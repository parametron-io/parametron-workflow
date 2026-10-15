package workflowfeedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/github"
)

// Only conversation comment operations are implemented. Any lifecycle, Project,
// branch, review, Draft or Development operation fails this fixture immediately.
func TestTypedFeedbackThroughGitHubTransport(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		for _, fault := range []string{"none", "lost create", "lost update", "malformed receipt", "canceled write", "deleted update"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				f := remote(kind)
				r := f.current.Resource
				var stored map[string]any
				writes := 0
				faulted := false
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				identity := func() map[string]any {
					return map[string]any{"id": r.Identity.ID, "number": r.Identity.Number, "repository": map[string]any{"id": r.Identity.Repository.ID, "name": r.Identity.Repository.Name, "owner": map[string]any{"login": r.Identity.Repository.Owner}}}
				}
				makeComment := func(body string) map[string]any {
					surface := "issues"
					if kind == "pull_request" {
						surface = "pull"
					}
					return map[string]any{"__typename": "IssueComment", "id": "C", "databaseId": 1, "body": body, "url": fmt.Sprintf("https://github.com/%s/%s/%s/%d#issuecomment-1", r.Identity.Repository.Owner, r.Identity.Repository.Name, surface, r.Identity.Number), "author": map[string]any{"__typename": f.author.Type, "id": f.author.ID, "login": f.author.Login}}
				}
				if fault == "lost update" || fault == "deleted update" {
					stored = makeComment(Marker + "\nold constraints")
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					var q struct {
						Query     string
						Variables map[string]any
					}
					if e := json.NewDecoder(req.Body).Decode(&q); e != nil {
						t.Error(e)
						w.WriteHeader(400)
						return
					}
					var data any
					switch {
					case strings.Contains(q.Query, "repository(owner:"):
						node := identity()
						node["state"] = "OPEN"
						data = map[string]any{"repository": map[string]any{"result": node}}
					case strings.Contains(q.Query, "comments(first:100"):
						node := identity()
						typ := "Issue"
						if kind == "pull_request" {
							typ = "PullRequest"
						}
						node["__typename"] = typ
						nodes := []any{}
						if stored != nil {
							nodes = append(nodes, stored)
						}
						node["comments"] = map[string]any{"totalCount": len(nodes), "nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
						data = map[string]any{"node": node}
					case strings.HasPrefix(q.Query, "query($id:ID!){node(id:$id){... on IssueComment"):
						if fault == "deleted update" && !faulted {
							f.lookups++
							if f.lookups == 3 {
								stored = nil
								faulted = true
							}
						}
						data = map[string]any{"node": stored}
					case strings.Contains(q.Query, "result:addComment") || strings.Contains(q.Query, "result:updateIssueComment"):
						writes++
						input := q.Variables["input"].(map[string]any)
						body, ok := input["body"].(string)
						if !ok {
							t.Error("missing body")
						}
						if strings.Contains(q.Query, "result:addComment") {
							if input["subjectId"] != r.Identity.ID {
								t.Error(input)
							}
						} else if input["id"] != "C" {
							t.Error(input)
						}
						stored = makeComment(body)
						if !faulted && (fault == "lost create" || fault == "lost update") {
							faulted = true
							conn, _, e := w.(http.Hijacker).Hijack()
							if e != nil {
								t.Error(e)
							} else {
								_ = conn.Close()
							}
							return
						}
						if !faulted && fault == "canceled write" {
							faulted = true
							cancel()
							<-req.Context().Done()
							return
						}
						if !faulted && fault == "malformed receipt" {
							faulted = true
							data = map[string]any{}
							break
						}
						if strings.Contains(q.Query, "result:addComment") {
							data = map[string]any{"result": map[string]any{"commentEdge": map[string]any{"node": stored}}}
						} else {
							data = map[string]any{"result": map[string]any{"issueComment": stored}}
						}
					default:
						t.Errorf("forbidden or unexpected capability: %s", q.Query)
						w.WriteHeader(400)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				defer server.Close()
				transport, e := github.NewTransport(server.URL, server.Client(), github.TokenFunc(func(context.Context) (string, error) { return "fixture-token", nil }))
				if e != nil {
					t.Fatal(e)
				}
				config := Config{Refresh: f, Author: f, Reader: transport, Writer: transport}
				c, e := New(config)
				if e != nil {
					t.Fatal(e)
				}
				e = c.Reconcile(ctx, r)
				if fault == "none" {
					if e != nil {
						t.Fatal(e)
					}
				} else {
					if e == nil {
						t.Fatal("injected failure succeeded")
					}
					var typed *github.Error
					if !errors.As(e, &typed) && !errors.Is(e, context.Canceled) {
						t.Fatal("untyped failure", e)
					}
				}
				if fault == "deleted update" && writes != 0 {
					t.Fatal("deleted comment was overwritten", writes)
				}
				// Recreate the controller and retry using live remote state, never the old plan.
				c, e = New(config)
				if e != nil {
					t.Fatal(e)
				}
				if e = c.Reconcile(context.Background(), r); e != nil {
					t.Fatal(e)
				}
				if e = c.Reconcile(context.Background(), r); e != nil {
					t.Fatal(e)
				}
				if writes != 1 {
					t.Fatalf("remote writes=%d want=1", writes)
				}
			})
		}
	}
}
