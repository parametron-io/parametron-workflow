package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
)

func developerUserJSON() map[string]any {
	return map[string]any{"login": "developer", "id": 42, "node_id": "U", "type": "User"}
}

var developerOrg = config.Organization{ID: "O", Login: "org"}
var developerRepo = Repository{ID: "R", Owner: "org", Name: "repo"}

func developerTransport(t *testing.T, prefix string, handler func(http.ResponseWriter, *http.Request)) *Transport {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer isolated-test-token" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") != "parametron-workflow" {
			t.Error("missing authentication or API headers")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewTransport(server.URL+prefix, server.Client(), TokenFunc(func(context.Context) (string, error) { return "isolated-test-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func writeDeveloperJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}
func repositoryIdentityResponse(t *testing.T, w http.ResponseWriter, r *http.Request, wrong bool) {
	t.Helper()
	if r.Method != "POST" {
		t.Error("repository identity requires GraphQL POST")
	}
	var q request
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		t.Error(err)
	}
	if q.Variables["owner"] != "org" || q.Variables["name"] != "repo" || strings.Contains(q.Query, "mutation") {
		t.Error("invalid repository identity request")
	}
	repo := repoJSON()
	if wrong {
		repo["id"] = "other"
	}
	writeDeveloperJSON(t, w, map[string]any{"data": map[string]any{"repository": repo}})
}
func TestDeveloperReadEndpointsAndEvidence(t *testing.T) {
	for _, mapping := range []struct{ endpoint, rest string }{{"/graphql", ""}, {"/api/graphql", "/api/v3"}, {"/injected/graphql", "/injected"}} {
		t.Run(mapping.endpoint, func(t *testing.T) {
			calls := []string{}
			client := developerTransport(t, mapping.endpoint, func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if r.URL.Path == mapping.endpoint {
					repositoryIdentityResponse(t, w, r, false)
					return
				}
				if r.Method != "GET" {
					t.Error("developer REST read must be GET")
				}
				switch r.URL.Path {
				case mapping.rest + "/users/developer":
					writeDeveloperJSON(t, w, developerUserJSON())
				case mapping.rest + "/orgs/org/memberships/developer":
					writeDeveloperJSON(t, w, map[string]any{"state": "active", "user": developerUserJSON(), "organization": map[string]any{"login": "org", "node_id": "O"}})
				case mapping.rest + "/repos/org/repo/collaborators/developer/permission":
					writeDeveloperJSON(t, w, map[string]any{"permission": "write", "role_name": "maintain", "user": developerUserJSON()})
				default:
					t.Error("unexpected endpoint", r.URL.Path)
				}
			})
			u, err := client.User(context.Background(), "developer")
			if err != nil || u != (Actor{ID: "U", Login: "developer", Type: "User"}) {
				t.Fatal(u, err)
			}
			m, err := client.OrganizationMembership(context.Background(), developerOrg, "developer")
			if err != nil || m != (Membership{Organization: developerOrg, User: u, State: "active"}) {
				t.Fatal(m, err)
			}
			p, err := client.RepositoryPermission(context.Background(), developerRepo, "developer")
			if err != nil || p != (Permission{Repository: developerRepo, User: u, Base: "write"}) {
				t.Fatal(p, err)
			}
			if len(calls) != 4 {
				t.Fatal("retries or extra reads", calls)
			}
		})
	}
	for _, tc := range []struct{ base, role string }{{"write", "maintain"}, {"write", "custom-writer"}, {"admin", "admin"}, {"read", "triage"}, {"read", "custom-reader"}, {"none", "custom-reader"}} {
		t.Run(tc.role+"/"+tc.base, func(t *testing.T) {
			client := developerTransport(t, "/graphql", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/graphql" {
					repositoryIdentityResponse(t, w, r, false)
					return
				}
				writeDeveloperJSON(t, w, map[string]any{"permission": tc.base, "role_name": tc.role, "user": developerUserJSON()})
			})
			p, err := client.RepositoryPermission(context.Background(), developerRepo, "developer")
			if err != nil || p.Base != tc.base {
				t.Fatal(p, err)
			}
		})
	}
}
func TestHumanAndStableUserIdentity(t *testing.T) {
	good := Actor{ID: "U", Login: "developer", Type: "User"}
	other := good
	other.Login = "DEVELOPER"
	if !ValidHuman(good) || !SameUser(good, other) {
		t.Fatal("valid canonical identity rejected")
	}
	for _, tc := range []Actor{{ID: "U", Login: "developer", Type: "Bot"}, {ID: "U", Login: "developer", Type: "Organization"}, {ID: "U", Login: "developer", Type: "Mannequin"}, {ID: "", Login: "developer", Type: "User"}, {ID: " U ", Login: "developer", Type: "User"}, {ID: "U", Login: "bad/login", Type: "User"}, {ID: "U", Login: "-dev", Type: "User"}, {ID: "U", Login: "dev-", Type: "User"}, {ID: "U", Login: "dev--name", Type: "User"}, {ID: "U", Login: strings.Repeat("a", 40), Type: "User"}} {
		if ValidHuman(tc) || SameUser(good, tc) {
			t.Fatal("invalid human identity", tc)
		}
	}
	other = good
	other.ID = "other"
	if SameUser(good, other) {
		t.Fatal("login substituted for stable ID")
	}
	other = good
	other.Login = "other"
	if SameUser(good, other) {
		t.Fatal("stable ID substituted for consistent current login")
	}
}
func TestDeveloperMalformedResponseEvidence(t *testing.T) {
	for _, boundary := range []string{"user", "membership", "permission"} {
		for _, bad := range []string{"empty", "invalid JSON", "missing numeric ID", "missing node ID", "wrong subject", "invalid login", "unknown actor", "missing type", "missing authority", "wrong organization", "wrong repository"} {
			if bad == "wrong organization" && boundary != "membership" || bad == "wrong repository" && boundary != "permission" || bad == "missing authority" && boundary == "user" {
				continue
			}
			t.Run(boundary+"/"+bad, func(t *testing.T) {
				calls := 0
				client := developerTransport(t, "/graphql", func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.URL.Path == "/graphql" {
						repositoryIdentityResponse(t, w, r, bad == "wrong repository")
						return
					}
					if r.Method != "GET" {
						t.Error(r.Method)
					}
					if bad == "invalid JSON" {
						w.Write([]byte(`{`))
						return
					}
					u := developerUserJSON()
					switch bad {
					case "missing numeric ID":
						delete(u, "id")
					case "missing node ID":
						delete(u, "node_id")
					case "wrong subject":
						u["login"] = "other"
					case "invalid login":
						u["login"] = "bad/login"
					case "unknown actor":
						u["type"] = "Alien"
					case "missing type":
						delete(u, "type")
					}
					var response any = u
					switch boundary {
					case "membership":
						org := map[string]any{"login": "org", "node_id": "O"}
						if bad == "wrong organization" {
							org["node_id"] = "other"
						}
						m := map[string]any{"state": "active", "user": u, "organization": org}
						if bad == "missing authority" {
							delete(m, "state")
						}
						response = m
					case "permission":
						p := map[string]any{"permission": "write", "user": u}
						if bad == "missing authority" {
							delete(p, "permission")
						}
						response = p
					}
					if bad == "empty" {
						response = map[string]any{}
					}
					writeDeveloperJSON(t, w, response)
				})
				var err error
				switch boundary {
				case "user":
					_, err = client.User(context.Background(), "developer")
				case "membership":
					_, err = client.OrganizationMembership(context.Background(), developerOrg, "developer")
				case "permission":
					_, err = client.RepositoryPermission(context.Background(), developerRepo, "developer")
				}
				category(t, err, Malformed)
				max := 1
				if boundary == "permission" {
					max = 2
				}
				if calls > max {
					t.Fatal("unexpected retry", calls)
				}
			})
		}
	}
}
func TestDeveloperFailureCategoriesAndNoRetries(t *testing.T) {
	for _, boundary := range []string{"user", "membership", "permission"} {
		for _, tc := range []struct {
			status int
			rate   bool
			want   Category
		}{{401, false, Unauthorized}, {403, false, Forbidden}, {403, true, RateLimited}, {404, false, NotFound}, {429, false, RateLimited}, {500, false, Transient}, {503, false, Transient}} {
			t.Run(fmt.Sprintf("%s/%d/%v", boundary, tc.status, tc.rate), func(t *testing.T) {
				calls := 0
				client := developerTransport(t, "/graphql", func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/graphql" {
						repositoryIdentityResponse(t, w, r, false)
						return
					}
					calls++
					if tc.rate {
						w.Header().Set("X-RateLimit-Remaining", "0")
					}
					w.WriteHeader(tc.status)
					w.Write([]byte("isolated-test-token private diagnostics"))
				})
				var err error
				switch boundary {
				case "user":
					_, err = client.User(context.Background(), "developer")
				case "membership":
					_, err = client.OrganizationMembership(context.Background(), developerOrg, "developer")
				case "permission":
					_, err = client.RepositoryPermission(context.Background(), developerRepo, "developer")
				}
				category(t, err, tc.want)
				if calls != 1 || strings.Contains(err.Error(), "isolated-test-token") || strings.Contains(err.Error(), "private") {
					t.Fatal("retry or credential/diagnostic disclosure", calls, err)
				}
			})
		}
	}
}
func TestDeveloperCancellationSizeAndCredentialIsolation(t *testing.T) {
	calls := 0
	client := developerTransport(t, "/graphql", func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(strings.Repeat("x", (8<<20)+1))) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.User(ctx, "developer")
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal(err, calls)
	}
	_, err = client.User(context.Background(), "developer")
	category(t, err, Malformed)
	client.tokens = TokenFunc(func(context.Context) (string, error) { return "", errors.New("isolated-test-token private") })
	_, err = client.User(context.Background(), "developer")
	category(t, err, Unauthorized)
	if calls != 1 || strings.Contains(err.Error(), "isolated-test-token") {
		t.Fatal("hidden credential fallback or disclosure", calls, err)
	}
	for _, login := range []string{"", "bad/login", "-bad", "bad--name"} {
		_, err := client.User(context.Background(), login)
		category(t, err, Permanent)
	}
	if calls != 1 {
		t.Fatal("invalid inputs made HTTP calls", calls)
	}
}

func TestMalformedStableDeveloperNodeIdentity(t *testing.T) {
	for _, id := range []string{" U ", "U other", "U\tother", "U\nother"} {
		t.Run(id, func(t *testing.T) {
			a := Actor{ID: id, Login: "developer", Type: "User"}
			if ValidHuman(a) || SameUser(a, a) {
				t.Error("malformed stable node identity qualifies as human")
			}
			client := developerTransport(t, "/graphql", func(w http.ResponseWriter, r *http.Request) {
				u := developerUserJSON()
				u["node_id"] = id
				writeDeveloperJSON(t, w, u)
			})
			u, err := client.User(context.Background(), "developer")
			category(t, err, Malformed)
			if u != (Actor{}) {
				t.Error("partial malformed actor escapes", u)
			}
		})
	}
}

func TestDeveloperInFlightCancellationAndRedirectIsolation(t *testing.T) {
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := developerTransport(t, "/graphql", func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() })
	done := make(chan error, 1)
	go func() { _, err := client.User(ctx, "developer"); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	leaked := 0
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked++ }))
	defer other.Close()
	redirect := developerTransport(t, "/graphql", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) })
	_, err := redirect.User(context.Background(), "developer")
	category(t, err, Permanent)
	if leaked != 0 {
		t.Fatal("credential followed redirect")
	}
}
