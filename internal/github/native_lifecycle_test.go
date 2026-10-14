package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func nativeTestIdentity() Identity {
	return Identity{ID: "I", Repository: Repository{ID: "R", Owner: "org", Name: "repo"}, Number: 9}
}
func nativeJSON(id string, state string) map[string]any {
	out := identityJSON(id, 9)
	out["state"] = state
	return out
}

func TestNativeIssueStateExactTransport(t *testing.T) {
	for _, close := range []bool{true, false} {
		t.Run(map[bool]string{true: "close", false: "reopen"}[close], func(t *testing.T) {
			from, to, op, input := "OPEN", "CLOSED", "closeIssue", "CloseIssueInput"
			if !close {
				from, to, op, input = "CLOSED", "OPEN", "reopenIssue", "ReopenIssueInput"
			}
			for _, bad := range []string{"", "identity", "state", "missing", "preflight"} {
				t.Run("response/"+bad, func(t *testing.T) {
					calls := 0
					mutations := 0
					c := mutationAdapter(t, func(q request) any {
						calls++
						if calls == 1 {
							if !strings.Contains(q.Query, "result:issue(number:$number)") || q.Variables["owner"] != "org" || q.Variables["name"] != "repo" || q.Variables["number"] != float64(9) {
								t.Error("incorrect preflight", q)
							}
							s := from
							if bad == "preflight" {
								s = to
							}
							return map[string]any{"repository": map[string]any{"result": nativeJSON("I", s)}}
						}
						mutations++
						want := `mutation($input:` + input + `!){result:` + op + `(input:$input){issue{` + identitySelection + ` state}}}`
						if q.Query != want || !reflect.DeepEqual(q.Variables, map[string]any{"input": map[string]any{"issueId": "I"}}) {
							t.Error("unbounded mutation", q)
						}
						if bad == "missing" {
							return map[string]any{}
						}
						id, s := "I", to
						if bad == "identity" {
							id = "OTHER"
						}
						if bad == "state" {
							s = from
						}
						return map[string]any{"result": map[string]any{"issue": nativeJSON(id, s)}}
					})
					var got IssueStateReceipt
					var err error
					if close {
						got, err = c.CloseIssue(context.Background(), nativeTestIdentity())
					} else {
						got, err = c.ReopenIssue(context.Background(), nativeTestIdentity())
					}
					if bad == "" {
						if err != nil || got.Issue != nativeTestIdentity() || got.State != to {
							t.Fatal(got, err)
						}
					} else if bad == "preflight" {
						if !errors.Is(err, ErrNativeLifecycleChanged) || mutations != 0 {
							t.Fatal(got, err, mutations)
						}
					} else {
						var e *Error
						if !errors.As(err, &e) || e.Category != Malformed {
							t.Fatal(err)
						}
					}
					if calls > 2 || mutations > 1 {
						t.Fatal("hidden retry", calls, mutations)
					}
				})
			}
		})
	}
}

func TestNativeLifecyclePaginationAndValidation(t *testing.T) {
	for _, bad := range []string{"", "duplicate", "wrong identity", "missing time", "unsupported", "cursor cycle", "interrupted"} {
		t.Run(bad, func(t *testing.T) {
			pages := 0
			reads := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
			c := adapter(t, func(q request) any {
				if strings.Contains(q.Query, "result:pullRequest(") {
					reads++
					return map[string]any{"repository": map[string]any{"result": nativeJSON("I", "MERGED")}}
				}
				if !strings.Contains(q.Query, "first:100,after:$cursor,itemTypes:[CLOSED_EVENT,REOPENED_EVENT,MERGED_EVENT]") || q.Variables["id"] != "I" {
					t.Error(q)
				}
				pages++
				e := map[string]any{"id": "M", "__typename": "MergedEvent", "createdAt": at, "pullRequest": identityJSON("I", 9)}
				if pages == 1 {
					if bad == "interrupted" {
						cancel()
					}
					return connectionJSON("PullRequest", []any{}, true, "NEXT")
				}
				if q.Variables["cursor"] != "NEXT" {
					t.Error(q)
				}
				switch bad {
				case "wrong identity":
					e["pullRequest"] = identityJSON("OTHER", 9)
				case "missing time":
					delete(e, "createdAt")
				case "unsupported":
					e["__typename"] = "ReadyForReviewEvent"
				case "cursor cycle":
					return connectionJSON("PullRequest", []any{}, true, "NEXT")
				}
				nodes := []any{e}
				if bad == "duplicate" {
					nodes = append(nodes, e)
				}
				return connectionJSON("PullRequest", nodes, false, "")
			})
			got, err := c.NativeLifecycle(ctx, nativeTestIdentity(), "pull_request")
			if bad == "" {
				if err != nil || len(got.Transitions) != 1 || reads != 2 || pages != 2 {
					t.Fatal(got, err, reads, pages)
				}
			} else if bad == "interrupted" {
				if !errors.Is(err, context.Canceled) || pages != 1 {
					t.Fatal(err, pages)
				}
			} else {
				var e *Error
				if !errors.As(err, &e) || e.Category != Malformed {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestNativeLifecycleCancellationAndInvalidIdentity(t *testing.T) {
	calls := 0
	c := mutationAdapter(t, func(request) any { calls++; return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CloseIssue(ctx, nativeTestIdentity()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.ReopenIssue(ctx, nativeTestIdentity()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	id := nativeTestIdentity()
	id.ID = ""
	if _, err := c.NativeLifecycle(context.Background(), id, "issue"); err == nil {
		t.Fatal("accepted missing identity")
	}
	if calls != 0 {
		t.Fatal("invalid/cancelled call sent", calls)
	}
}

func TestNativeTransportErrorsHaveNoHiddenRetries(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
		want Category
	}{{401, `{}`, Unauthorized}, {403, `{}`, Forbidden}, {409, `{}`, Conflict}, {429, `{}`, RateLimited}, {503, `{}`, Transient}, {200, `{`, Malformed}} {
		for _, op := range []string{"read", "close", "reopen"} {
			t.Run(string(tc.want)+"/"+op, func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.WriteHeader(tc.code)
					w.Write([]byte(tc.body))
				}))
				defer s.Close()
				c, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "test", nil }))
				if err != nil {
					t.Fatal(err)
				}
				switch op {
				case "read":
					_, err = c.NativeLifecycle(context.Background(), nativeTestIdentity(), "issue")
				case "close":
					_, err = c.CloseIssue(context.Background(), nativeTestIdentity())
				case "reopen":
					_, err = c.ReopenIssue(context.Background(), nativeTestIdentity())
				}
				category(t, err, tc.want)
				if calls != 1 {
					t.Fatal("hidden retry", calls)
				}
			})
		}
	}
}

func TestNativePreflightRejectsEveryIdentityDimension(t *testing.T) {
	for _, field := range []string{"node", "number", "repository", "owner", "state", "missing"} {
		t.Run(field, func(t *testing.T) {
			calls := 0
			c := mutationAdapter(t, func(q request) any {
				calls++
				if strings.Contains(q.Query, "mutation") {
					t.Error("mutated after invalid identity")
				}
				i := nativeJSON("I", "OPEN")
				switch field {
				case "node":
					i["id"] = "OTHER"
				case "number":
					i["number"] = 10
				case "repository":
					i["repository"] = map[string]any{"id": "OTHER", "name": "repo", "owner": map[string]any{"login": "org"}}
				case "owner":
					i["repository"] = map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "other"}}
				case "state":
					i["state"] = "APPROVED"
				case "missing":
					delete(i, "state")
				}
				return map[string]any{"repository": map[string]any{"result": i}}
			})
			_, err := c.CloseIssue(context.Background(), nativeTestIdentity())
			category(t, err, Malformed)
			if calls != 1 {
				t.Fatal(calls)
			}
		})
	}
}
