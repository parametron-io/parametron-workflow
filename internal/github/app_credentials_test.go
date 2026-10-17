package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func syntheticAppConfig(t *testing.T) (AppCredentialConfig, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("generate synthetic key")
	}
	path := filepath.Join(t.TempDir(), "synthetic.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal("write synthetic key")
	}
	return AppCredentialConfig{AppID: 101, InstallationID: 202, AccountID: 303, ClientID: "synthetic-client", Slug: "synthetic-controller", AccountLogin: "synthetic-org", AccountNodeID: "SYNTHETIC_ORG", AccountType: "Organization", PrivateKeyFile: path, Repositories: []string{"repo"}, Author: Actor{ID: "SYNTHETIC_BOT", Login: "synthetic-controller[bot]", Type: "Bot"}}, key
}

func TestAppJWTSignatureAndKeyFormats(t *testing.T) {
	cfg, key := syntheticAppConfig(t)
	for _, format := range []string{"PKCS1", "PKCS8"} {
		t.Run(format, func(t *testing.T) {
			block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
			if format == "PKCS8" {
				block.Type = "PRIVATE KEY"
				var err error
				block.Bytes, err = x509.MarshalPKCS8PrivateKey(key)
				if err != nil {
					t.Fatal("encode synthetic key")
				}
			}
			if err := os.WriteFile(cfg.PrivateKeyFile, pem.EncodeToMemory(block), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := NewAppCredentials(cfg, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now()
			jwt, err := s.jwt(context.Background())
			after := time.Now()
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(jwt, ".")
			if len(parts) != 3 {
				t.Fatal("JWT segment count")
			}
			decode := func(v string) []byte {
				b, e := base64.RawURLEncoding.DecodeString(v)
				if e != nil {
					t.Fatal("JWT encoding")
				}
				return b
			}
			var header map[string]string
			if json.Unmarshal(decode(parts[0]), &header) != nil || !reflect.DeepEqual(header, map[string]string{"alg": "RS256", "typ": "JWT"}) {
				t.Fatal("JWT header")
			}
			var claims struct {
				Issuer  string `json:"iss"`
				Issued  int64  `json:"iat"`
				Expires int64  `json:"exp"`
			}
			if json.Unmarshal(decode(parts[1]), &claims) != nil || claims.Issuer != cfg.ClientID || claims.Issued < before.Add(-time.Minute).Unix() || claims.Issued > after.Add(-time.Minute).Unix() || claims.Expires > after.Add(10*time.Minute).Unix() || claims.Expires < before.Add(8*time.Minute).Unix() {
				t.Fatal("JWT claims outside contract")
			}
			digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], decode(parts[2])) != nil {
				t.Fatal("invalid JWT signature")
			}
		})
	}
}

func TestAppJWTRejectsInvalidKeysAndCancellation(t *testing.T) {
	cfg, key := syntheticAppConfig(t)
	valid := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	for name, data := range map[string][]byte{"empty": nil, "malformed": []byte("synthetic-secret-marker"), "truncated": valid[:len(valid)/2], "oversized": []byte(strings.Repeat("x", 65537)), "unsupported": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)}), "trailing": append(append([]byte{}, valid...), []byte("synthetic-secret-marker")...)} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(cfg.PrivateKeyFile, data, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := NewAppCredentials(cfg, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			token, err := s.jwt(context.Background())
			if err == nil || token != "" {
				t.Fatal("invalid key authenticated")
			}
			if strings.Contains(err.Error(), "synthetic-secret-marker") || strings.Contains(err.Error(), "PRIVATE KEY") {
				t.Fatal("key material in error")
			}
		})
	}
	s, _ := NewAppCredentials(cfg, "", nil)
	if err := os.Remove(cfg.PrivateKeyFile); err != nil {
		t.Fatal(err)
	}
	if token, err := s.jwt(context.Background()); err == nil || token != "" {
		t.Fatal("missing key authenticated")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if token, err := s.jwt(ctx); !errors.Is(err, context.Canceled) || token != "" {
		t.Fatal("canceled signing authenticated")
	}
}

// Shapes follow github/rest-api-description installation and user schemas and
// octokit/graphql-schema Bot fields; all identities and credentials are synthetic.
type appFixture struct {
	cfg                                                 AppCredentialConfig
	key                                                 *rsa.PrivateKey
	app, installation, minted, repositories, bot, graph map[string]any
	calls                                               []string
	mu                                                  sync.Mutex
	hook                                                func(*http.Request)
	mutationStatus                                      int
	mutationRateLimit                                   bool
}

func newAppFixture(t *testing.T) (*appFixture, *AppCredentials) {
	t.Helper()
	cfg, key := syntheticAppConfig(t)
	permissions := map[string]string{"issues": "write", "pull_requests": "write", "metadata": "read"}
	owner := map[string]any{"id": cfg.AccountID, "node_id": cfg.AccountNodeID, "login": cfg.AccountLogin, "type": cfg.AccountType}
	f := &appFixture{cfg: cfg, key: key,
		app:          map[string]any{"id": cfg.AppID, "client_id": cfg.ClientID, "slug": cfg.Slug, "node_id": "SYNTHETIC_APP"},
		installation: map[string]any{"id": cfg.InstallationID, "app_id": cfg.AppID, "client_id": cfg.ClientID, "app_slug": cfg.Slug, "account": owner, "target_id": cfg.AccountID, "target_type": cfg.AccountType, "repository_selection": "selected", "permissions": permissions, "suspended_at": nil},
		minted:       map[string]any{"token": "synthetic-installation-token", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repository_selection": "selected", "permissions": permissions},
		repositories: map[string]any{"total_count": 1, "repositories": []any{map[string]any{"id": 404, "node_id": "SYNTHETIC_REPO", "name": "repo", "owner": owner}}},
		bot:          map[string]any{"id": 505, "node_id": cfg.Author.ID, "login": cfg.Author.Login, "type": "Bot"},
		graph:        map[string]any{"data": map[string]any{"node": map[string]any{"__typename": "Bot", "id": cfg.Author.ID, "login": cfg.Slug, "databaseId": 505}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
		if f.hook != nil {
			f.hook(r)
		}
		var out any
		switch r.URL.Path {
		case "/app":
			out = f.app
		case "/app/installations/202":
			out = f.installation
		case "/app/installations/202/access_tokens":
			var body map[string][]string
			if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body, map[string][]string{"repositories": {"repo"}}) {
				t.Error("mint lacks explicit repository restriction")
			}
			out = f.minted
		case "/installation/repositories":
			out = f.repositories
		case "/users/" + cfg.Slug + "[bot]":
			out = f.bot
		case "/graphql":
			out = f.graph
		case "/mutate":
			if f.mutationRateLimit {
				w.Header().Set("X-RateLimit-Remaining", "0")
			}
			w.WriteHeader(f.mutationStatus)
			return
		default:
			t.Error("unexpected credential endpoint")
			http.Error(w, "", 404)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/app") {
			bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			p := strings.Split(bearer, ".")
			if len(p) != 3 {
				t.Error("App endpoint lacks signed JWT")
			} else {
				sig, e := base64.RawURLEncoding.DecodeString(p[2])
				digest := sha256.Sum256([]byte(p[0] + "." + p[1]))
				if e != nil || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig) != nil {
					t.Error("App endpoint signature invalid")
				}
			}
		} else if r.Header.Get("Authorization") != "Bearer synthetic-installation-token" {
			t.Error("installation endpoint credential mismatch")
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error("encode synthetic response")
		}
	}))
	t.Cleanup(server.Close)
	s, err := NewAppCredentials(cfg, server.URL+"/graphql", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f, s
}
func TestAppMintCacheRefreshAndVerifiedAuthor(t *testing.T) {
	f, s := newAppFixture(t)
	ctx := context.Background()
	if token, err := s.Token(ctx); err != nil || token == "" {
		t.Fatal("fresh mint failed", err)
	}
	want := []string{"GET /app", "GET /app/installations/202", "POST /app/installations/202/access_tokens", "GET /installation/repositories?per_page=100&page=1", "GET /users/synthetic-controller%5Bbot%5D", "POST /graphql", "GET /app/installations/202"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatal("credential verification order differs", f.calls)
	}
	for range 3 {
		if _, err := s.Token(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.calls) != len(want) {
		t.Fatal("valid cache reminted")
	}
	transport, _ := NewTransport(s.transport.endpoint, s.transport.http, s)
	if author, err := transport.AuthenticatedCommentAuthor(ctx); err != nil || author != f.cfg.Author {
		t.Fatal("verified author mismatch", err)
	}
	for _, expiry := range []time.Time{time.Now().Add(time.Minute), time.Now().Add(-time.Minute)} {
		s.expires = expiry
		if _, err := s.Token(ctx); err != nil {
			t.Fatal("refresh failed", err)
		}
	}
	s.invalidate(ctx, s.token)
	f.installation["suspended_at"] = "2026-01-01T00:00:00Z"
	if token, err := s.Token(ctx); err == nil || token != "" || s.token != "" {
		t.Fatal("failed refresh retained credential")
	}
	f.installation["suspended_at"] = nil
	if token, err := s.Token(ctx); err != nil || token == "" {
		t.Fatal("refresh did not recover", err)
	}
}
func TestAppMintRejectsIdentityAndScopeDrift(t *testing.T) {
	for _, name := range []string{"app-id", "client-id", "app-slug", "app-node-is-bot", "installation-id", "installation-app", "account-id", "account-node", "account-login", "account-type", "suspended", "permissions", "missing-token", "expiry", "token-permissions", "extra-scope", "missing-scope", "wrong-repository", "cross-account", "bot-type", "bot-id", "bot-login", "graphql-disagreement", "graphql-missing"} {
		t.Run(name, func(t *testing.T) {
			f, s := newAppFixture(t)
			switch name {
			case "app-id":
				f.app["id"] = 999
			case "client-id":
				f.app["client_id"] = "other"
			case "app-slug":
				f.app["slug"] = "other"
			case "app-node-is-bot":
				f.app["node_id"] = f.cfg.Author.ID
			case "installation-id":
				f.installation["id"] = 999
			case "installation-app":
				f.installation["app_id"] = 999
			case "account-id":
				f.installation["account"].(map[string]any)["id"] = 999
			case "account-node":
				f.installation["account"].(map[string]any)["node_id"] = "OTHER"
			case "account-login":
				f.installation["account"].(map[string]any)["login"] = "other"
			case "account-type":
				f.installation["account"].(map[string]any)["type"] = "User"
			case "suspended":
				f.installation["suspended_at"] = "2026-01-01T00:00:00Z"
			case "permissions":
				f.installation["permissions"] = map[string]string{"metadata": "read"}
			case "missing-token":
				delete(f.minted, "token")
			case "expiry":
				f.minted["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
			case "token-permissions":
				f.minted["permissions"] = map[string]string{"issues": "read"}
			case "extra-scope":
				f.repositories["total_count"] = 2
			case "missing-scope":
				f.repositories["repositories"] = []any{}
			case "wrong-repository":
				f.repositories["repositories"].([]any)[0].(map[string]any)["name"] = "other"
			case "cross-account":
				f.repositories["repositories"].([]any)[0].(map[string]any)["owner"] = map[string]any{"id": 999, "node_id": "OTHER", "login": "other", "type": "Organization"}
			case "bot-type":
				f.bot["type"] = "User"
			case "bot-id":
				f.bot["id"] = 0
			case "bot-login":
				f.bot["login"] = "other[bot]"
			case "graphql-disagreement":
				f.graph["data"].(map[string]any)["node"].(map[string]any)["databaseId"] = 999
			case "graphql-missing":
				f.graph["data"] = map[string]any{"node": nil}
			}
			if token, err := s.Token(context.Background()); err == nil || token != "" || s.token != "" {
				t.Fatal("unverified credential accepted")
			}
			for _, call := range f.calls {
				if call == "GET /user" {
					t.Fatal("installation used user identity endpoint")
				}
			}
			if strings.HasPrefix(name, "app-") || name == "client-id" || strings.HasPrefix(name, "installation-") || strings.HasPrefix(name, "account-") || name == "suspended" || name == "permissions" {
				for _, call := range f.calls {
					if strings.Contains(call, "access_tokens") {
						t.Fatal("mint occurred before authority verification")
					}
				}
			}
		})
	}
}
func TestAppConcurrentCacheAndCanceledWaiter(t *testing.T) {
	f, s := newAppFixture(t)
	var wg sync.WaitGroup
	var failures atomic.Int32
	for range 16 {
		wg.Go(func() {
			if token, err := s.Token(context.Background()); err != nil || token == "" {
				failures.Add(1)
			}
		})
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatal("concurrent mint failed")
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 7 {
		t.Fatal("concurrent callers duplicated mint", calls)
	}
	if err := s.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Token(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("wait cancellation lost")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter ignored cancellation")
	}
	s.unlock()
}
func TestAppCredentialRequestBudget(t *testing.T) {
	_, s := newAppFixture(t)
	if token, err := s.Token(WithRequestLimit(context.Background(), 2)); !errors.Is(err, ErrRequestLimit) || token != "" || s.token != "" {
		t.Fatal("budget exhaustion authenticated", err)
	}
}

func TestAppInstallationChangedDuringVerification(t *testing.T) {
	for _, name := range []string{"removed", "suspended", "identity", "permission"} {
		t.Run(name, func(t *testing.T) {
			f, s := newAppFixture(t)
			reads := 0
			f.hook = func(r *http.Request) {
				if r.URL.Path != "/app/installations/202" {
					return
				}
				reads++
				if reads != 2 {
					return
				}
				switch name {
				case "removed":
					f.installation["id"] = 0
				case "suspended":
					f.installation["suspended_at"] = "2026-01-01T00:00:00Z"
				case "identity":
					f.installation["account"].(map[string]any)["node_id"] = "CHANGED"
				case "permission":
					f.installation["permissions"] = map[string]string{"issues": "read"}
				}
			}
			if token, err := s.Token(context.Background()); err == nil || token != "" || s.token != "" {
				t.Fatal("installation changed during mint authenticated")
			}
		})
	}
}
func TestAppTransportAuthenticationFailureNeverReplaysMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		limited    bool
		want       Category
		invalidate bool
	}{
		{"unauthorized", 401, false, Unauthorized, true}, {"forbidden", 403, false, Forbidden, true}, {"rate-limited-403", 403, true, RateLimited, true}, {"rate-limited-429", 429, true, RateLimited, false}, {"transient", 503, false, Transient, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s := newAppFixture(t)
			f.mutationStatus = tc.status
			f.mutationRateLimit = tc.limited
			transport, err := NewTransport(s.transport.endpoint, s.transport.http, s)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := strings.TrimSuffix(s.transport.endpoint, "/graphql") + "/mutate"
			_, err = transport.request(context.Background(), http.MethodPost, endpoint, []byte(`{"synthetic":"mutation"}`))
			category(t, err, tc.want)
			if (s.token == "") != tc.invalidate {
				t.Fatal("incorrect credential invalidation")
			}
			mutations := 0
			for _, call := range f.calls {
				if call == "POST /mutate" {
					mutations++
				}
			}
			if mutations != 1 {
				t.Fatal("uncertain mutation replayed", mutations)
			}
		})
	}
}

// Challenge each identity invariant through minting and cached-author rechecks.
func TestAppBotIdentityRejectsUnverifiedResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*appFixture, map[string]any)
		want   Category
	}{
		{"rest-login", func(f *appFixture, n map[string]any) { f.bot["login"] = "other[bot]" }, Unauthorized},
		{"rest-node", func(f *appFixture, n map[string]any) { f.bot["node_id"] = "OTHER_BOT" }, Unauthorized},
		{"rest-empty-node", func(f *appFixture, n map[string]any) { f.bot["node_id"] = "" }, Unauthorized},
		{"rest-database", func(f *appFixture, n map[string]any) { f.bot["id"] = 999 }, Unauthorized},
		{"rest-missing-database", func(f *appFixture, n map[string]any) { delete(f.bot, "id") }, Unauthorized},
		{"rest-type", func(f *appFixture, n map[string]any) { f.bot["type"] = "User" }, Unauthorized},
		{"graphql-login", func(f *appFixture, n map[string]any) { n["login"] = "other" }, Unauthorized},
		{"graphql-rest-style-login", func(f *appFixture, n map[string]any) { n["login"] = f.cfg.Author.Login }, Unauthorized},
		{"graphql-case-folded-login", func(f *appFixture, n map[string]any) { n["login"] = strings.ToUpper(f.cfg.Slug) }, Unauthorized},
		{"graphql-empty-login", func(f *appFixture, n map[string]any) { n["login"] = "" }, Unauthorized},
		{"graphql-node", func(f *appFixture, n map[string]any) { n["id"] = "OTHER_BOT" }, Unauthorized},
		{"graphql-database", func(f *appFixture, n map[string]any) { n["databaseId"] = 999 }, Unauthorized},
		{"graphql-type", func(f *appFixture, n map[string]any) { n["__typename"] = "User" }, Unauthorized},
		{"graphql-missing-node", func(f *appFixture, n map[string]any) { f.graph["data"].(map[string]any)["node"] = nil }, Unauthorized},
		{"graphql-errors-with-valid-data", func(f *appFixture, n map[string]any) { f.graph["errors"] = []any{map[string]any{"type": "FORBIDDEN"}} }, Forbidden},
		{"graphql-malformed-database", func(f *appFixture, n map[string]any) { n["databaseId"] = "invalid" }, Malformed},
		{"graphql-malformed-node", func(f *appFixture, n map[string]any) { f.graph["data"].(map[string]any)["node"] = []any{} }, Malformed},
		{"lookalike-stable-identities", func(f *appFixture, n map[string]any) {
			f.bot["node_id"], f.bot["id"] = "LOOKALIKE_BOT", 999
			n["id"], n["databaseId"] = "LOOKALIKE_BOT", 999
		}, Unauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, phase := range []string{"mint", "author-recheck"} {
				t.Run(phase, func(t *testing.T) {
					f, s := newAppFixture(t)
					ctx := context.Background()
					if phase == "author-recheck" {
						if token, err := s.Token(ctx); err != nil || token == "" {
							t.Fatal("initial verified mint failed", err)
						}
					}
					tc.change(f, f.graph["data"].(map[string]any)["node"].(map[string]any))
					if phase == "mint" {
						token, err := s.Token(ctx)
						category(t, err, tc.want)
						if token != "" {
							t.Fatal("unverified token returned")
						}
					} else {
						transport, err := NewTransport(s.transport.endpoint, s.transport.http, s)
						if err != nil {
							t.Fatal(err)
						}
						author, err := transport.AuthenticatedCommentAuthor(ctx)
						category(t, err, tc.want)
						if author != (Actor{}) {
							t.Fatal("unverified author returned")
						}
					}
					if s.token != "" {
						t.Fatal("identity failure retained credential")
					}
				})
			}
		})
	}
}
