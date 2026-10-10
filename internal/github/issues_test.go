package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func listedJSON(id string, n int, state string) map[string]any {
	w := identityJSON(id, n)
	w["__typename"] = "Issue"
	w["title"] = ""
	w["body"] = ""
	w["state"] = state
	return w
}
func TestListIssuesPagination(t *testing.T) {
	calls := 0
	c := adapter(t, func(q request) any {
		calls++
		if strings.Contains(q.Query, "repository(owner:") {
			return map[string]any{"repository": repoJSON()}
		}
		if !strings.Contains(q.Query, "result:issues(first:100,after:$cursor)") || strings.Contains(q.Query, "search") || strings.Contains(q.Query, "pullRequest") || q.Variables["id"] != "R" {
			t.Fatal("not Issue connection", q)
		}
		if q.Variables["cursor"] == nil {
			return connectionJSON("Repository", []any{listedJSON("I2", 2, "CLOSED")}, true, "next")
		}
		if q.Variables["cursor"] != "next" {
			t.Fatal(q)
		}
		return connectionJSON("Repository", []any{listedJSON("I1", 1, "OPEN")}, false, "")
	})
	got, err := c.ListIssues(context.Background(), Repository{"R", "org", "repo"})
	if err != nil || len(got) != 2 || got[0].Number != 1 || got[1].State != "CLOSED" || calls != 3 {
		t.Fatal(got, err, calls)
	}
}
func TestListIssuesRejectsMalformed(t *testing.T) {
	for _, name := range []string{"null", "pr", "missing title", "missing body", "missing state", "bad state", "zero", "blank id", "wrong repository", "duplicate number", "duplicate id", "missing metadata", "cursor cycle", "wrong repository id"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			c := adapter(t, func(q request) any {
				calls++
				if strings.Contains(q.Query, "repository(owner:") {
					repo := repoJSON()
					if name == "wrong repository id" {
						repo["id"] = "other"
					}
					return map[string]any{"repository": repo}
				}
				w := listedJSON("I", 1, "OPEN")
				nodes := []any{w}
				switch name {
				case "null":
					nodes = []any{nil}
				case "pr":
					w["__typename"] = "PullRequest"
				case "missing title":
					delete(w, "title")
				case "missing body":
					w["body"] = nil
				case "missing state":
					delete(w, "state")
				case "bad state":
					w["state"] = "MERGED"
				case "zero":
					w["number"] = 0
				case "blank id":
					w["id"] = " "
				case "wrong repository":
					repo := repoJSON()
					repo["name"] = "other"
					w["repository"] = repo
				case "duplicate number":
					nodes = append(nodes, listedJSON("I2", 1, "OPEN"))
				case "duplicate id":
					nodes = append(nodes, listedJSON("I", 2, "OPEN"))
				case "missing metadata":
					return map[string]any{"node": map[string]any{"__typename": "Repository", "result": map[string]any{"nodes": nodes}}}
				case "cursor cycle":
					return connectionJSON("Repository", []any{}, true, "same")
				}
				return connectionJSON("Repository", nodes, false, "")
			})
			_, err := c.ListIssues(context.Background(), Repository{"R", "org", "repo"})
			category(t, err, Malformed)
			if calls > 3 {
				t.Fatal("unexpected retry", calls)
			}
		})
	}
}
func TestListIssuesErrorsNoRetry(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 409, 500, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(status) }))
			defer s.Close()
			c, _ := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
			_, err := c.ListIssues(context.Background(), Repository{"R", "org", "repo"})
			want := map[int]Category{401: Unauthorized, 403: Forbidden, 404: NotFound, 429: RateLimited, 409: Conflict, 500: Transient, 422: Permanent}[status]
			category(t, err, want)
			if calls != 1 {
				t.Fatal("retry", calls)
			}
		})
	}
	_, err := (&ListerFake{}).ListIssues(context.Background(), Repository{})
	category(t, err, Permanent)
}

func TestListIssuesGraphQLErrorOnPage(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Write([]byte(`{"data":{"repository":{"id":"R","name":"repo","owner":{"login":"org"}}}}`))
			return
		}
		w.Write([]byte(`{"data":{"node":null},"errors":[{"type":"RATE_LIMITED","message":"private diagnostic"}]}`))
	}))
	defer s.Close()
	c, _ := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
	_, err := c.ListIssues(context.Background(), Repository{"R", "org", "repo"})
	category(t, err, RateLimited)
	if calls != 2 || strings.Contains(err.Error(), "private") {
		t.Fatal(calls, err)
	}
}
func TestListIssuesRequiresDiscoveredIdentity(t *testing.T) {
	c := adapter(t, func(q request) any { t.Fatal("unexpected HTTP"); return nil })
	for _, r := range []Repository{{Owner: "org", Name: "repo"}, {ID: "R", Name: "repo"}, {ID: "R", Owner: "org"}} {
		_, err := c.ListIssues(context.Background(), r)
		category(t, err, Permanent)
	}
}
