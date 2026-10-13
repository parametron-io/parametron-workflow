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

func TestReviewFactsPaginationAndStates(t *testing.T) {
	for _, state := range []string{"PENDING", "COMMENTED", "APPROVED", "CHANGES_REQUESTED", "DISMISSED"} {
		t.Run(state, func(t *testing.T) {
			calls := map[string]int{}
			actor := map[string]any{"id": "U", "login": "dev", "__typename": "User"}
			at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
			event := func(id, kind string, at time.Time) any {
				return map[string]any{"id": id, "__typename": kind, "createdAt": at, "actor": actor, "pullRequest": identityJSON("PR", 9)}
			}
			review := map[string]any{"id": "REV", "state": state, "author": actor, "pullRequest": identityJSON("PR", 9), "submittedAt": at}
			if state == "PENDING" {
				review["submittedAt"] = nil
			}
			client := adapter(t, func(q request) any {
				if strings.Contains(q.Query, "result:pullRequest(") {
					calls["identity"]++
					return map[string]any{"repository": map[string]any{"result": identityJSON("PR", 9)}}
				}
				for _, name := range []string{"timelineItems", "reviews", "latestOpinionatedReviews"} {
					if strings.Contains(q.Query, "result:"+name+"(") {
						calls[name]++
						if q.Variables["id"] != "PR" {
							t.Error(q.Variables)
						}
						if !strings.Contains(q.Query, "first:100") {
							t.Error("unbounded query", q.Query)
						}
						if calls[name] == 1 {
							return connectionJSON("PullRequest", []any{}, true, "NEXT")
						}
						if q.Variables["cursor"] != "NEXT" {
							t.Error("cursor lost", q.Variables)
						}
						switch name {
						case "timelineItems":
							if !strings.Contains(q.Query, "... on ReadyForReviewEvent") || !strings.Contains(q.Query, "... on ConvertToDraftEvent") || !strings.Contains(q.Query, "... on Bot{id login}") {
								t.Error(q.Query)
							}
							return connectionJSON("PullRequest", []any{event("READY", "ReadyForReviewEvent", at), event("DRAFT", "ConvertToDraftEvent", at.Add(time.Minute))}, false, "")
						case "reviews":
							return connectionJSON("PullRequest", []any{review}, false, "")
						default:
							if state == "PENDING" {
								return connectionJSON("PullRequest", []any{}, false, "")
							}
							return connectionJSON("PullRequest", []any{review}, false, "")
						}
					}
				}
				t.Error("unexpected query", q.Query)
				return nil
			})
			pr := Identity{ID: "PR", Number: 9, Repository: Repository{ID: "R", Owner: "org", Name: "repo"}}
			got, err := client.PullRequestReviewFacts(context.Background(), pr)
			if err != nil {
				t.Fatal(err)
			}
			if got.PR != pr || len(got.Transitions) != 2 || got.Transitions[0].ID != "READY" || got.Transitions[1].ID != "DRAFT" || len(got.Submissions) != 1 || got.Submissions[0].State != state {
				t.Fatal(got)
			}
			if state == "PENDING" && !got.Submissions[0].SubmittedAt.IsZero() {
				t.Fatal(got)
			}
			if !reflect.DeepEqual(calls, map[string]int{"identity": 1, "timelineItems": 2, "reviews": 2, "latestOpinionatedReviews": 2}) {
				t.Fatal(calls)
			}
		})
	}
}
func TestReviewFactsRejectMalformedAndConflictingConnections(t *testing.T) {
	for _, name := range []string{"wrong_identity", "null_event", "duplicate_event", "wrong_event_pr", "missing_actor_id", "nullable_actor", "bot_actor", "missing_submission_time", "pending_with_time", "unsupported_state", "conflicting_latest", "duplicate_latest", "cursor_cycle", "missing_page_info", "empty"} {
		t.Run(name, func(t *testing.T) {
			at := "2026-10-13T10:00:00Z"
			actor := map[string]any{"id": "U", "login": "dev", "__typename": "User"}
			event := map[string]any{"id": "READY", "__typename": "ReadyForReviewEvent", "createdAt": at, "actor": actor, "pullRequest": identityJSON("PR", 9)}
			review := map[string]any{"id": "REV", "state": "CHANGES_REQUESTED", "submittedAt": at, "author": actor, "pullRequest": identityJSON("PR", 9)}
			switch name {
			case "wrong_event_pr":
				event["pullRequest"] = identityJSON("OTHER", 9)
			case "missing_actor_id":
				delete(actor, "id")
			case "nullable_actor":
				event["actor"] = nil
				review["author"] = nil
			case "bot_actor":
				actor["__typename"] = "Bot"
			case "missing_submission_time":
				delete(review, "submittedAt")
			case "pending_with_time":
				review["state"] = "PENDING"
			case "unsupported_state":
				review["state"] = "UNKNOWN"
			}
			client := adapter(t, func(q request) any {
				if strings.Contains(q.Query, "result:pullRequest(") {
					id := "PR"
					if name == "wrong_identity" {
						id = "OTHER"
					}
					return map[string]any{"repository": map[string]any{"result": identityJSON(id, 9)}}
				}
				if strings.Contains(q.Query, "result:timelineItems(") {
					nodes := []any{event}
					switch name {
					case "null_event":
						nodes = []any{nil}
					case "duplicate_event":
						nodes = append(nodes, event)
					case "empty":
						nodes = []any{}
					case "cursor_cycle":
						return connectionJSON("PullRequest", nodes, true, "REPEAT")
					case "missing_page_info":
						return map[string]any{"node": map[string]any{"__typename": "PullRequest", "result": map[string]any{"nodes": nodes}}}
					}
					return connectionJSON("PullRequest", nodes, false, "")
				}
				if strings.Contains(q.Query, "result:reviews(") {
					nodes := []any{review}
					if name == "empty" {
						nodes = []any{}
					}
					return connectionJSON("PullRequest", nodes, false, "")
				}
				if strings.Contains(q.Query, "result:latestOpinionatedReviews(") {
					nodes := []any{review}
					if name == "empty" {
						nodes = []any{}
					}
					if name == "conflicting_latest" {
						r := map[string]any{}
						for k, v := range review {
							r[k] = v
						}
						r["state"] = "APPROVED"
						nodes = []any{r}
					}
					if name == "duplicate_latest" {
						nodes = append(nodes, review)
					}
					return connectionJSON("PullRequest", nodes, false, "")
				}
				t.Error(q.Query)
				return nil
			})
			got, err := client.PullRequestReviewFacts(context.Background(), Identity{ID: "PR", Number: 9, Repository: Repository{ID: "R", Owner: "org", Name: "repo"}})
			switch name {
			case "nullable_actor", "bot_actor", "empty":
				if err != nil {
					t.Fatal(err)
				}
				if name == "nullable_actor" && (got.Transitions[0].Actor != nil || got.Submissions[0].Author != nil) {
					t.Fatal(got)
				}
				if name == "bot_actor" && got.Transitions[0].Actor.Type != "Bot" {
					t.Fatal(got)
				}
			case "conflicting_latest":
				if !errors.Is(err, ErrReviewFactsChanged) {
					t.Fatal(err)
				}
			default:
				category(t, err, Malformed)
			}
			if err != nil && !reflect.DeepEqual(got, ReviewFacts{}) {
				t.Fatal("partial facts returned", got)
			}
		})
	}
}
func TestReviewFakeRequiresConfiguredCapability(t *testing.T) {
	f := &Fake{}
	_, err := f.PullRequestReviewFacts(context.Background(), Identity{})
	if err == nil {
		t.Fatal("unconfigured fake invented review evidence")
	}
}

func TestReviewTransportProviderFailuresAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want Category
	}{
		{"unauthorized", 401, `{}`, Unauthorized}, {"forbidden", 403, `{}`, Forbidden}, {"conflict", 409, `{}`, Conflict}, {"rate_limit", 429, `{}`, RateLimited}, {"transient", 503, `{}`, Transient}, {"malformed", 200, `{`, Malformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewTransport(server.URL, server.Client(), TokenFunc(func(context.Context) (string, error) { return "test", nil }))
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.PullRequestReviewFacts(context.Background(), Identity{ID: "PR", Number: 9, Repository: Repository{ID: "R", Owner: "org", Name: "repo"}})
			category(t, err, tc.want)
			if calls != 1 || !reflect.DeepEqual(got, ReviewFacts{}) {
				t.Fatal("retried or returned partial evidence", calls, got)
			}
		})
	}
	for _, stage := range []string{"before_read", "during_timeline", "during_reviews", "during_latest"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			client := adapter(t, func(q request) any {
				calls++
				if strings.Contains(q.Query, "result:pullRequest(") {
					return map[string]any{"repository": map[string]any{"result": identityJSON("PR", 9)}}
				}
				if stage == "during_timeline" && strings.Contains(q.Query, "result:timelineItems(") || stage == "during_reviews" && strings.Contains(q.Query, "result:reviews(") || stage == "during_latest" && strings.Contains(q.Query, "result:latestOpinionatedReviews(") {
					cancel()
				}
				return connectionJSON("PullRequest", []any{}, false, "")
			})
			if stage == "before_read" {
				cancel()
			}
			_, err := client.PullRequestReviewFacts(ctx, Identity{ID: "PR", Number: 9, Repository: Repository{ID: "R", Owner: "org", Name: "repo"}})
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if stage == "before_read" && calls != 0 {
				t.Fatal("cancelled read sent request")
			}
		})
	}
}
