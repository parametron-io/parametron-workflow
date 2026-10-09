package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/parametron-io/parametron-workflow/internal/config"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type request struct {
	Query     string
	Variables map[string]any
}

func adapter(t *testing.T, handler func(request) any) *Transport {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("User-Agent") != "parametron-workflow" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "" || r.Header.Get("Authorization") == "" {
			t.Error("missing transport headers")
		}
		var q request
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		if strings.Contains(q.Query, "mutation") {
			t.Error("unexpected mutation")
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": handler(q)}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewTransport(server.URL, server.Client(), TokenFunc(func(context.Context) (string, error) { return t.Name(), nil }))
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func repoJSON() map[string]any {
	return map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}}
}
func identityJSON(id string, n int) map[string]any {
	return map[string]any{"id": id, "number": n, "repository": repoJSON()}
}
func connectionJSON(typ string, nodes []any, next bool, cursor string) any {
	return map[string]any{"node": map[string]any{"__typename": typ, "result": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}}}
}
func category(t *testing.T, err error, want Category) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Category != want {
		t.Fatalf("got %v, want %s", err, want)
	}
}

func TestRepository(t *testing.T) {
	client := adapter(t, func(q request) any {
		if q.Variables["owner"] != "org" || q.Variables["name"] != "repo" {
			t.Error("lookup arguments")
		}
		return map[string]any{"repository": repoJSON()}
	})
	got, err := client.Repository(context.Background(), "org", "repo")
	if err != nil || got != (Repository{ID: "R", Owner: "org", Name: "repo"}) {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestIssue(t *testing.T) {
	calls := []string{}
	client := adapter(t, func(q request) any {
		calls = append(calls, q.Query)
		if strings.Contains(q.Query, "result:issue(") {
			w := identityJSON("I", 9)
			w["title"] = "title"
			w["body"] = "Parent: untouched"
			w["state"] = "OPEN"
			w["author"] = map[string]any{"login": "author"}
			w["issueType"] = map[string]any{"id": "T", "name": "Bug"}
			w["parent"] = identityJSON("P", 1)
			return map[string]any{"repository": map[string]any{"result": w}}
		}
		switch {
		case strings.Contains(q.Query, "result:labels"):
			if q.Variables["cursor"] == nil {
				return connectionJSON("Issue", []any{map[string]any{"name": "bug"}}, true, "next")
			}
			if q.Variables["cursor"] != "next" {
				t.Error("cursor")
			}
			return connectionJSON("Issue", []any{map[string]any{"name": "security"}}, false, "")
		case strings.Contains(q.Query, "result:assignees"):
			return connectionJSON("Issue", []any{map[string]any{"login": "dev"}}, false, "")
		default:
			return connectionJSON("Issue", []any{identityJSON("related", 2)}, false, "")
		}
	})
	got, err := client.Issue(context.Background(), Ref{Owner: "org", Repository: "repo", Number: 9})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "I" || got.Repository.ID != "R" || got.Title != "title" || got.Body != "Parent: untouched" || got.State != "OPEN" || got.Type.Name != "Bug" || got.Author.Login != "author" || got.Parent.ID != "P" || len(got.SubIssues) != 1 || len(got.BlockedBy) != 1 || len(got.Blocking) != 1 || len(got.LinkedPullRequests) != 1 || !reflect.DeepEqual(got.Labels, []string{"bug", "security"}) || got.Assignees[0].Login != "dev" {
		t.Fatalf("%+v", got)
	}
	if len(calls) < 7 {
		t.Fatal("missing reads")
	}
}
func TestPullRequest(t *testing.T) {
	for _, state := range []string{"OPEN", "CLOSED", "MERGED"} {
		t.Run(state, func(t *testing.T) {
			client := adapter(t, func(q request) any {
				if strings.Contains(q.Query, "result:closingIssuesReferences") {
					return connectionJSON("PullRequest", []any{identityJSON("I", 1)}, false, "")
				}
				if strings.Contains(q.Query, "result:labels") {
					return connectionJSON("PullRequest", []any{map[string]any{"name": "docs"}}, false, "")
				}
				w := identityJSON("PR", 9)
				for k, v := range map[string]any{"title": "title", "body": "Target: untouched", "state": state, "isDraft": true, "headRefName": "task/9", "headRefOid": "head", "baseRefName": "main", "baseRefOid": "base", "author": map[string]any{"login": "dev"}} {
					w[k] = v
				}
				return map[string]any{"repository": map[string]any{"result": w}}
			})
			got, err := client.PullRequest(context.Background(), Ref{"org", "repo", 9})
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != "PR" || got.State != state || !got.Draft || got.HeadRef != "task/9" || got.HeadSHA != "head" || got.BaseRef != "main" || got.BaseSHA != "base" || got.Author.Login != "dev" || got.Labels[0] != "docs" {
				t.Fatalf("%+v", got)
			}
		})
	}
}
func TestMissingResources(t *testing.T) {
	client := adapter(t, func(request) any { return map[string]any{"repository": nil, "node": nil} })
	_, err := client.Repository(context.Background(), "org", "repo")
	category(t, err, NotFound)
	_, err = client.Issue(context.Background(), Ref{"org", "repo", 9})
	category(t, err, NotFound)
	_, err = client.PullRequest(context.Background(), Ref{"org", "repo", 9})
	category(t, err, NotFound)
	_, err = client.ProjectItems(context.Background(), "I", "P", nil)
	category(t, err, NotFound)
}
func TestMalformedReads(t *testing.T) {
	for _, payload := range []any{map[string]any{"id": ""}, map[string]any{"id": "R", "name": "repo"}} {
		client := adapter(t, func(request) any { return map[string]any{"repository": payload} })
		_, err := client.Repository(context.Background(), "org", "repo")
		category(t, err, Malformed)
	}
	for _, kind := range []string{"issue", "pullRequest"} {
		client := adapter(t, func(request) any { return map[string]any{"repository": map[string]any{"result": identityJSON("I", 9)}} })
		var err error
		if kind == "issue" {
			_, err = client.Issue(context.Background(), Ref{"org", "repo", 9})
		} else {
			_, err = client.PullRequest(context.Background(), Ref{"org", "repo", 9})
		}
		category(t, err, Malformed)
	}
}
func TestHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		status           int
		remaining, retry string
		want             Category
	}{{401, "", "", Unauthorized}, {403, "", "", Forbidden}, {404, "", "", NotFound}, {403, "0", "", RateLimited}, {403, "", "1", RateLimited}, {429, "", "", RateLimited}, {409, "", "", Conflict}, {412, "", "", Conflict}, {500, "", "", Transient}, {503, "", "", Transient}, {422, "", "", Permanent}} {
		t.Run(fmt.Sprint(tc.status, tc.remaining, tc.retry), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", tc.remaining)
				w.Header().Set("Retry-After", tc.retry)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, r.Header.Get("Authorization"))
			}))
			defer server.Close()
			client, _ := NewTransport(server.URL, server.Client(), TokenFunc(func(context.Context) (string, error) { return t.Name(), nil }))
			_, err := client.Repository(context.Background(), "org", "repo")
			category(t, err, tc.want)
			if strings.Contains(err.Error(), t.Name()) {
				t.Fatal("credential leaked")
			}
		})
	}
}
func TestResponseErrors(t *testing.T) {
	for _, tc := range []struct {
		body string
		want Category
	}{{`oops`, Malformed}, {`{}`, Malformed}, {`{"data":null}`, Malformed}, {`{"errors":[{"type":"NOT_FOUND"}],"data":{"repository":{}}}`, NotFound}, {`{"errors":[{"type":"RATE_LIMITED"}]}`, RateLimited}, {`{"errors":[{"type":"FORBIDDEN"}]}`, Forbidden}, {`{"errors":[{"extensions":{"code":"INTERNAL_SERVER_ERROR"}}]}`, Transient}, {`{"errors":[{"message":"private"}]}`, Permanent}, {strings.Repeat("x", (8<<20)+1), Malformed}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
		client, _ := NewTransport(server.URL, server.Client(), TokenFunc(func(context.Context) (string, error) { return t.Name(), nil }))
		_, err := client.Repository(context.Background(), "org", "repo")
		category(t, err, tc.want)
		server.Close()
	}
}
func TestAuthAndContext(t *testing.T) {
	calls := 0
	client := adapter(t, func(request) any { calls++; return map[string]any{"repository": repoJSON()} })
	client.tokens = TokenFunc(func(context.Context) (string, error) { return "", errors.New(t.Name()) })
	_, err := client.Repository(context.Background(), "org", "repo")
	category(t, err, Unauthorized)
	if strings.Contains(err.Error(), t.Name()) || calls != 0 {
		t.Fatal("provider error exposed or request sent")
	}
	client.tokens = TokenFunc(func(context.Context) (string, error) { return t.Name(), nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Repository(ctx, "org", "repo")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Time{})
	defer cancel()
	_, err = client.Repository(ctx, "org", "repo")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestNetworkAndRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
	}))
	client, _ := NewTransport(server.URL, server.Client(), TokenFunc(func(context.Context) (string, error) { return t.Name(), nil }))
	_, err := client.Repository(context.Background(), "org", "repo")
	category(t, err, Permanent)
	server.Close()
	_, err = client.Repository(context.Background(), "org", "repo")
	category(t, err, Transient)
}
func TestInvalidTransportAndRequests(t *testing.T) {
	for _, endpoint := range []string{"file:///tmp/api", "http://user:password@example.com/graphql", "https://example.com/graphql?token=private", "https://example.com/graphql#private"} {
		_, err := NewTransport(endpoint, nil, TokenFunc(func(context.Context) (string, error) { return "", nil }))
		category(t, err, Permanent)
	}
	_, err := NewTransport("", nil, nil)
	category(t, err, Permanent)
	client := adapter(t, func(request) any { t.Error("unexpected network call"); return nil })
	_, err = client.Repository(context.Background(), "", "repo")
	category(t, err, Permanent)
	_, err = client.Issue(context.Background(), Ref{"org", "repo", 0})
	category(t, err, Permanent)
	_, err = client.ProjectItems(context.Background(), "I", "P", map[string]config.FieldKind{"F": "text"})
	category(t, err, Permanent)
	client.tokens = TokenFunc(func(context.Context) (string, error) { return "", nil })
	_, err = client.Repository(context.Background(), "org", "repo")
	category(t, err, Unauthorized)
}

func TestClassifiedTokenFailure(t *testing.T) {
	client := adapter(t, func(request) any { t.Error("unexpected request"); return nil })
	client.tokens = TokenFunc(func(context.Context) (string, error) {
		return "", &Error{Category: Transient, Cause: errors.New(t.Name())}
	})
	_, err := client.Repository(context.Background(), "org", "repo")
	category(t, err, Transient)
	if strings.Contains(err.Error(), t.Name()) || errors.Unwrap(err) != nil {
		t.Fatal("provider diagnostic leaked")
	}
}
