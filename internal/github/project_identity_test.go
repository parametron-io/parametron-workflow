package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCurrentProjectItemContentIdentityOnly(t *testing.T) {
	for _, kind := range []string{"Issue", "PullRequest", "DraftIssue", "null"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			c := adapter(t, func(q request) any {
				calls++
				if q.Variables["id"] != "item" || strings.Contains(q.Query, "fieldValues") || strings.Contains(q.Query, "status") {
					t.Error(q)
				}
				var content any
				if kind != "null" {
					content = map[string]any{"__typename": kind, "id": "content"}
				}
				if kind == "Issue" {
					w := identityJSON("issue", 39)
					w["__typename"] = kind
					content = w
				}
				return map[string]any{"node": map[string]any{"__typename": "ProjectV2Item", "id": "item", "project": map[string]any{"id": "project"}, "content": content}}
			})
			got, err := c.CurrentProjectItem(context.Background(), "item")
			if err != nil || got.ID != "item" || got.ProjectID != "project" || calls != 1 {
				t.Fatal(got, err, calls)
			}
			if kind == "Issue" {
				want := Identity{ID: "issue", Repository: Repository{ID: "R", Owner: "org", Name: "repo"}, Number: 39}
				if got.Issue == nil || *got.Issue != want {
					t.Fatal(got)
				}
			} else if got.Issue != nil {
				t.Fatal("invented Issue", got)
			}
			wantKind := kind
			if kind == "null" {
				wantKind = ""
			}
			if got.ContentKind != wantKind {
				t.Fatal(got)
			}
		})
	}
}

func TestCurrentProjectItemRejectsMalformedIdentity(t *testing.T) {
	for _, bad := range []string{"missing", "typename", "item ID", "missing Project", "invalid Project", "Issue ID", "Issue number", "Issue repository", "content kind"} {
		t.Run(bad, func(t *testing.T) {
			c := adapter(t, func(q request) any {
				i := identityJSON("issue", 39)
				i["__typename"] = "Issue"
				n := map[string]any{"__typename": "ProjectV2Item", "id": "item", "project": map[string]any{"id": "project"}, "content": i}
				switch bad {
				case "missing":
					return map[string]any{"node": nil}
				case "typename":
					n["__typename"] = "Issue"
				case "item ID":
					n["id"] = "other"
				case "missing Project":
					n["project"] = nil
				case "invalid Project":
					n["project"] = map[string]any{"id": " "}
				case "Issue ID":
					i["id"] = ""
				case "Issue number":
					i["number"] = 0
				case "Issue repository":
					i["repository"] = nil
				case "content kind":
					i["__typename"] = "Repository"
				}
				return map[string]any{"node": n}
			})
			_, err := c.CurrentProjectItem(context.Background(), "item")
			want := Malformed
			if bad == "missing" {
				want = NotFound
			}
			category(t, err, want)
		})
	}
	c := adapter(t, func(request) any { t.Error("invalid input performed IO"); return nil })
	_, err := c.CurrentProjectItem(context.Background(), " ")
	category(t, err, Permanent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.CurrentProjectItem(ctx, "item")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
