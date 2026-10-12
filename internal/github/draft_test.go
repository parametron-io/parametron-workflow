package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDraftConversionExactNarrowTransport(t *testing.T) {
	calls := 0
	c := mutationAdapter(t, func(q request) any {
		calls++
		if q.Query != `mutation($input:ConvertPullRequestToDraftInput!){result:convertPullRequestToDraft(input:$input){pullRequest{id isDraft}}}` || !reflect.DeepEqual(q.Variables, map[string]any{"input": map[string]any{"pullRequestId": "PR"}}) {
			t.Fatal(q)
		}
		return map[string]any{"result": map[string]any{"pullRequest": map[string]any{"id": "PR", "isDraft": true}}}
	})
	var narrow DraftConverter = c
	if err := narrow.ConvertPullRequestToDraft(context.Background(), "PR"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", " ", "bad id", "PR\n", "\tPR"} {
		category(t, narrow.ConvertPullRequestToDraft(context.Background(), id), Permanent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := narrow.ConvertPullRequestToDraft(ctx, "PR"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("invalid input, cancellation or transport retry sent a request", calls)
	}
}

func TestDraftConversionRejectsMissingOrMismatchedResponse(t *testing.T) {
	for _, body := range []any{
		map[string]any{}, map[string]any{"result": nil}, map[string]any{"result": map[string]any{}},
		map[string]any{"result": map[string]any{"pullRequest": nil}},
		map[string]any{"result": map[string]any{"pullRequest": map[string]any{"id": "OTHER", "isDraft": true}}},
		map[string]any{"result": map[string]any{"pullRequest": map[string]any{"id": "PR", "isDraft": false}}},
		map[string]any{"result": map[string]any{"pullRequest": map[string]any{"id": "PR"}}},
		map[string]any{"result": map[string]any{"pullRequest": map[string]any{"id": "PR", "isDraft": "true"}}},
	} {
		c := mutationAdapter(t, func(request) any { return body })
		category(t, c.ConvertPullRequestToDraft(context.Background(), "PR"), Malformed)
	}
}

func TestDraftConversionTypedErrorsAndProtections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   Category
	}{
		{"unauthorized", 401, "secret-token", Unauthorized}, {"forbidden", 403, "secret-token", Forbidden},
		{"conflict", 409, "secret-token", Conflict}, {"precondition", 412, "secret-token", Conflict},
		{"rate_limit", 429, "secret-token", RateLimited}, {"transient", 503, "secret-token", Transient},
		{"redirect", 302, "secret-token", Permanent}, {"malformed", 200, "{", Malformed},
		{"missing_data", 200, `{"data":null}`, Malformed},
		{"graphql_permission", 200, `{"errors":[{"type":"FORBIDDEN"}]}`, Forbidden},
		{"graphql_rate_limit", 200, `{"errors":[{"type":"RATE_LIMITED"}]}`, RateLimited},
		{"graphql_transient", 200, `{"errors":[{"type":"INTERNAL"}]}`, Transient},
		{"oversized", 200, strings.Repeat("x", (8<<20)+1), Malformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/redirect")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "secret-token", nil }))
			if err != nil {
				t.Fatal(err)
			}
			err = c.ConvertPullRequestToDraft(context.Background(), "PR")
			category(t, err, tc.want)
			if calls != 1 || strings.Contains(err.Error(), "secret-token") {
				t.Fatal("retry or credential leak", err, calls)
			}
		})
	}
	t.Run("token_cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		c, err := NewTransport("http://127.0.0.1:1", nil, TokenFunc(func(context.Context) (string, error) { cancel(); return "", ctx.Err() }))
		if err != nil {
			t.Fatal(err)
		}
		if err = c.ConvertPullRequestToDraft(ctx, "PR"); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("transport_failure", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		s.Close()
		c, _ := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
		category(t, c.ConvertPullRequestToDraft(context.Background(), "PR"), Transient)
	})
}

func TestPullRequestProjectItemExactCurrentIdentity(t *testing.T) {
	for _, variant := range []string{"valid", "missing", "wrong_item", "wrong_project", "wrong_type", "missing_content", "wrong_content_type", "malformed_identity", "issue"} {
		t.Run(variant, func(t *testing.T) {
			c := mutationAdapter(t, func(q request) any {
				if strings.Contains(q.Query, "mutation") || !reflect.DeepEqual(q.Variables, map[string]any{"id": "ITEM"}) {
					t.Fatal(q)
				}
				content := map[string]any{"__typename": "PullRequest", "id": "PR", "number": 47, "repository": map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}}}
				node := map[string]any{"__typename": "ProjectV2Item", "id": "ITEM", "project": map[string]any{"id": "P"}, "content": content}
				switch variant {
				case "missing":
					return map[string]any{"node": nil}
				case "wrong_item":
					node["id"] = "OTHER"
				case "wrong_project":
					node["project"] = nil
				case "wrong_type":
					node["__typename"] = "Issue"
				case "missing_content":
					node["content"] = nil
				case "wrong_content_type":
					content["__typename"] = "Repository"
				case "malformed_identity":
					content["number"] = 0
				case "issue":
					content["__typename"] = "Issue"
				}
				return map[string]any{"node": node}
			})
			i, err := c.CurrentPullRequestProjectItem(context.Background(), "ITEM")
			switch variant {
			case "valid":
				if err != nil || i.ID != "ITEM" || i.ProjectID != "P" || i.PullRequest == nil || i.PullRequest.ID != "PR" || i.PullRequest.Repository.ID != "R" || i.Issue != nil {
					t.Fatal(i, err)
				}
			case "missing":
				category(t, err, NotFound)
			case "missing_content":
				if err != nil || i.PullRequest != nil {
					t.Fatal(i, err)
				}
			case "issue":
				if err != nil || i.Issue == nil || i.PullRequest != nil {
					t.Fatal(i, err)
				}
			default:
				category(t, err, Malformed)
			}
		})
	}
}

func TestDraftConversionInFlightCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	release, done := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		calls++
		cancel()
		// Keep the response unavailable until the client observes cancellation.
		// Returning here would race an empty HTTP 200 against the canceled request.
		<-release
	}))
	defer s.Close()
	c, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	err = c.ConvertPullRequestToDraft(ctx, "PR")
	close(release)
	<-done
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
