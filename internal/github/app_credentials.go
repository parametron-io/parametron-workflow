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
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// AppCredentialConfig contains explicit deployment bindings, never inferred from
// an opaque token. Account is the installation owner, not the comment author.
// PrivateKeyFile is read only when minting; secrets remain in process memory.
type AppCredentialConfig struct {
	AppID, InstallationID, AccountID                         int64
	ClientID, Slug, AccountLogin, AccountNodeID, AccountType string
	PrivateKeyFile                                           string
	Repositories                                             []string
	Author                                                   Actor
}

// AppCredentials owns both installation-token minting and comment-author
// verification. It never accepts externally minted installation tokens. Every
// refresh revalidates the same App, installation, account, repositories and Bot.
// The transport and author reader share this exact instance.
type AppCredentials struct {
	cfg       AppCredentialConfig
	transport *Transport
	lane      chan struct{}
	token     string
	expires   time.Time
}

type appBearer string

func (s appBearer) Token(ctx context.Context) (string, error) { return string(s), ctx.Err() }

func NewAppCredentials(c AppCredentialConfig, endpoint string, client *http.Client) (*AppCredentials, error) {
	if c.AppID <= 0 || c.InstallationID <= 0 || c.AccountID <= 0 || !validIDs(c.ClientID, c.AccountNodeID) || !validLogin(c.Slug) || !validLogin(c.AccountLogin) || (c.AccountType != "Organization" && c.AccountType != "User") || strings.TrimSpace(c.PrivateKeyFile) == "" || len(c.Repositories) == 0 || len(c.Repositories) > 500 || !ValidCommentAuthor(c.Author) || c.Author.Type != "Bot" || c.Author.Login != c.Slug+"[bot]" {
		return nil, failure(Permanent)
	}
	seen := map[string]bool{}
	for _, name := range c.Repositories {
		if name == "" || strings.ContainsAny(name, "/\\?#\r\n\x00 ") || name == "." || name == ".." || seen[strings.ToLower(name)] {
			return nil, failure(Permanent)
		}
		seen[strings.ToLower(name)] = true
	}
	t, err := NewTransport(endpoint, client, appBearer("unused"))
	if err != nil {
		return nil, err
	}
	c.Repositories = append([]string(nil), c.Repositories...)
	return &AppCredentials{cfg: c, transport: t, lane: make(chan struct{}, 1)}, nil
}

func (s *AppCredentials) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.lane <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *AppCredentials) unlock() { <-s.lane }

// jwt follows GitHub's RS256 contract: backdate iat by 60 seconds, expire
// within ten minutes, use the distinct client ID as issuer. No key/JWT content
// or underlying parser errors are ever included in diagnostics.
// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app
func (s *AppCredentials) jwt(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.Open(s.cfg.PrivateKeyFile)
	if err != nil {
		return "", failure(Unauthorized)
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	closeErr := f.Close()
	if err != nil || closeErr != nil || len(data) > 65536 {
		return "", failure(Unauthorized)
	}
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 || len(block.Headers) != 0 {
		return "", failure(Unauthorized)
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		key, _ = parsed.(*rsa.PrivateKey)
	default:
		return "", failure(Unauthorized)
	}
	if err != nil || key == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return "", failure(Unauthorized)
	}
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": s.cfg.ClientID})
	encode := base64.RawURLEncoding.EncodeToString
	unsigned := encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", failure(Unauthorized)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return unsigned + "." + encode(signature), nil
}

// appAPI reuses bounded, redacted transport mechanics without exposing arbitrary
// GraphQL or following response-provided URLs. Its bearer is request-local.
func appAPI(ctx context.Context, t *Transport, method, path string, input, out any) error {
	endpoint, _ := url.Parse(t.endpoint)
	prefix := strings.TrimSuffix(endpoint.Path, "/graphql")
	if prefix == "/api" {
		prefix = "/api/v3"
	}
	relative, err := url.Parse(path)
	if err != nil {
		return failure(Permanent)
	}
	endpoint.Path = prefix + relative.Path
	endpoint.RawQuery = relative.RawQuery
	var body []byte
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return failure(Permanent)
		}
	}
	data, err := t.request(ctx, method, endpoint.String(), body)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, out) != nil {
		return failure(Malformed)
	}
	return ctx.Err()
}

type appMetadata struct {
	ID       int64
	NodeID   string `json:"node_id"`
	ClientID string `json:"client_id"`
	Slug     string
}
type appInstallation struct {
	ID                  int64
	AppID               int64  `json:"app_id"`
	ClientID            string `json:"client_id"`
	AppSlug             string `json:"app_slug"`
	Account             restUser
	TargetID            int64  `json:"target_id"`
	TargetType          string `json:"target_type"`
	RepositorySelection string `json:"repository_selection"`
	Permissions         map[string]string
	SuspendedAt         json.RawMessage `json:"suspended_at"`
}

func (s *AppCredentials) installationValid(i appInstallation) bool {
	c := s.cfg
	return i.ID == c.InstallationID && i.AppID == c.AppID && i.ClientID == c.ClientID && i.AppSlug == c.Slug && i.Account.ID == c.AccountID && i.Account.NodeID == c.AccountNodeID && i.Account.Login == c.AccountLogin && i.Account.Type == c.AccountType && i.TargetID == c.AccountID && i.TargetType == c.AccountType && (i.RepositorySelection == "all" || i.RepositorySelection == "selected") && string(i.SuspendedAt) == "null" && i.Permissions["issues"] == "write" && i.Permissions["pull_requests"] == "write" && i.Permissions["metadata"] == "read"
}

func (s *AppCredentials) mint(ctx context.Context) error {
	// Clear before attempting refresh. A failed refresh never serves the old token.
	s.token = ""
	s.expires = time.Time{}
	jwt, err := s.jwt(ctx)
	if err != nil {
		return err
	}
	app := *s.transport
	app.tokens = appBearer(jwt)
	var metadata appMetadata
	if err = appAPI(ctx, &app, http.MethodGet, "/app", nil, &metadata); err != nil {
		return err
	}
	if metadata.ID != s.cfg.AppID || metadata.ClientID != s.cfg.ClientID || metadata.Slug != s.cfg.Slug || !validIDs(metadata.NodeID) || metadata.NodeID == s.cfg.Author.ID {
		return failure(Unauthorized)
	}
	path := "/app/installations/" + strconv.FormatInt(s.cfg.InstallationID, 10)
	var installation appInstallation
	if err = appAPI(ctx, &app, http.MethodGet, path, nil, &installation); err != nil {
		return err
	}
	if !s.installationValid(installation) {
		return failure(Unauthorized)
	}
	var minted struct {
		Token               string
		ExpiresAt           time.Time `json:"expires_at"`
		Permissions         map[string]string
		RepositorySelection string `json:"repository_selection"`
	}
	// GitHub restricts this token to the explicitly configured repositories. All
	// installed permissions are inherited and independently compared on response.
	if err = appAPI(ctx, &app, http.MethodPost, path+"/access_tokens", map[string]any{"repositories": s.cfg.Repositories}, &minted); err != nil {
		return err
	}
	now := time.Now()
	if strings.TrimSpace(minted.Token) != minted.Token || minted.Token == "" || strings.ContainsAny(minted.Token, "\r\n\x00") || !minted.ExpiresAt.After(now.Add(2*time.Minute)) || minted.ExpiresAt.After(now.Add(65*time.Minute)) || !reflect.DeepEqual(minted.Permissions, installation.Permissions) || minted.RepositorySelection != "selected" {
		return failure(Malformed)
	}
	writer := *s.transport
	writer.tokens = appBearer(minted.Token)
	if err = s.verifyRepositories(ctx, &writer); err != nil {
		return err
	}
	if err = s.verifyBot(ctx, &writer); err != nil {
		return err
	}
	// Recheck installation after mint and observations; concurrent removal,
	// suspension, permission or account changes cannot establish a valid session.
	var latest appInstallation
	if err = appAPI(ctx, &app, http.MethodGet, path, nil, &latest); err != nil {
		return err
	}
	if !s.installationValid(latest) || !reflect.DeepEqual(installation, latest) {
		return failure(Conflict)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !minted.ExpiresAt.After(time.Now().Add(2 * time.Minute)) {
		return failure(Conflict)
	}
	s.token = minted.Token
	s.expires = minted.ExpiresAt
	return nil
}

func (s *AppCredentials) verifyRepositories(ctx context.Context, t *Transport) error {
	expected := map[string]bool{}
	for _, name := range s.cfg.Repositories {
		expected[name] = true
	}
	ids := map[int64]bool{}
	nodes := map[string]bool{}
	total := -1
	count := 0
	for page := 1; page <= 5; page++ {
		var result struct {
			TotalCount   *int `json:"total_count"`
			Repositories []struct {
				ID     int64
				NodeID string `json:"node_id"`
				Name   string
				Owner  restUser
			}
		}
		if err := appAPI(ctx, t, http.MethodGet, "/installation/repositories?per_page=100&page="+strconv.Itoa(page), nil, &result); err != nil {
			return err
		}
		if result.TotalCount == nil || *result.TotalCount != len(s.cfg.Repositories) || result.Repositories == nil || len(result.Repositories) > 100 || (total >= 0 && total != *result.TotalCount) {
			return failure(Malformed)
		}
		total = *result.TotalCount
		for _, r := range result.Repositories {
			if !expected[r.Name] || r.ID <= 0 || !validIDs(r.NodeID) || ids[r.ID] || nodes[r.NodeID] || r.Owner.ID != s.cfg.AccountID || r.Owner.NodeID != s.cfg.AccountNodeID || r.Owner.Login != s.cfg.AccountLogin || r.Owner.Type != s.cfg.AccountType {
				return failure(Unauthorized)
			}
			delete(expected, r.Name)
			ids[r.ID] = true
			nodes[r.NodeID] = true
			count++
		}
		if count == total {
			return ctx.Err()
		}
		if len(result.Repositories) != 100 {
			return failure(Malformed)
		}
	}
	return failure(Malformed)
}

// GitHub's own create-github-app-token action documents resolving the App's
// Bot at /users/<authenticated-app-slug>[bot]. The slug is only a lookup key:
// JWT-verified App + App-owned token minting + REST Bot node/database identity +
// independent GraphQL Bot identity + exact expected actor establish the chain.
// App node IDs are never substituted for Bot node IDs.
// https://github.com/actions/create-github-app-token#configure-git-cli-for-an-apps-bot-user
func (s *AppCredentials) verifyBot(ctx context.Context, t *Transport) error {
	var user restUser
	if err := appAPI(ctx, t, http.MethodGet, "/users/"+url.PathEscape(s.cfg.Slug+"[bot]"), nil, &user); err != nil {
		return err
	}
	actor := Actor{ID: user.NodeID, Login: user.Login, Type: user.Type}
	if user.ID <= 0 || actor != s.cfg.Author || actor.Type != "Bot" {
		return failure(Unauthorized)
	}
	var data struct {
		Node *struct {
			Type       string `json:"__typename"`
			ID, Login  string
			DatabaseID int64
		}
	}
	if err := t.query(ctx, `query($id:ID!){node(id:$id){__typename ... on Bot{id login databaseId}}}`, map[string]any{"id": actor.ID}, &data); err != nil {
		return err
	}
	if data.Node == nil || data.Node.Type != "Bot" || data.Node.ID != actor.ID || data.Node.Login != actor.Login || data.Node.DatabaseID != user.ID {
		return failure(Unauthorized)
	}
	return ctx.Err()
}

func (s *AppCredentials) Token(ctx context.Context) (string, error) {
	if err := s.lock(ctx); err != nil {
		return "", err
	}
	defer s.unlock()
	if s.token == "" || !s.expires.After(time.Now().Add(2*time.Minute)) {
		if err := s.mint(ctx); err != nil {
			return "", err
		}
	}
	return s.token, ctx.Err()
}
func (s *AppCredentials) commentAuthor(ctx context.Context) (Actor, error) {
	if err := s.lock(ctx); err != nil {
		return Actor{}, err
	}
	defer s.unlock()
	if s.token == "" || !s.expires.After(time.Now().Add(2*time.Minute)) {
		if err := s.mint(ctx); err != nil {
			return Actor{}, err
		}
	}
	writer := *s.transport
	writer.tokens = appBearer(s.token)
	if err := s.verifyBot(ctx, &writer); err != nil {
		s.token = ""
		return Actor{}, err
	}
	return s.cfg.Author, ctx.Err()
}
func (s *AppCredentials) invalidate(ctx context.Context, token string) {
	if err := s.lock(ctx); err != nil {
		return
	}
	defer s.unlock()
	if s.token == token {
		s.token = ""
		s.expires = time.Time{}
	}
}
