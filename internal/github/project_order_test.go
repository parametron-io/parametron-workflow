package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func orderedItem(id, kind, content string, number int, archived bool) map[string]any {
	typ := map[string]string{"Issue": "ISSUE", "PullRequest": "PULL_REQUEST", "DraftIssue": "DRAFT_ISSUE", "": "REDACTED"}[kind]
	w := map[string]any{"id": id, "type": typ, "isArchived": archived, "project": map[string]any{"id": "P"}, "content": nil}
	if kind != "" {
		c := identityJSON(content, number)
		c["__typename"] = kind
		w["content"] = c
	}
	return w
}
func orderedPage(nodes []any, next bool, cursor string) any {
	return map[string]any{"node": map[string]any{"__typename": "ProjectV2", "id": "P", "items": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}}}
}
func TestProjectOrderConnectionOrder(t *testing.T) {
	for _, pages := range []int{1, 2} {
		t.Run(fmt.Sprint(pages), func(t *testing.T) {
			calls := 0
			nodes := []any{orderedItem("Z", "Issue", "I9", 9, false), orderedItem("A", "PullRequest", "PR", 5, false), orderedItem("Y", "DraftIssue", "D", 0, true), orderedItem("B", "", "", 0, false)}
			client := adapter(t, func(q request) any {
				calls++
				if q.Variables["id"] != "P" || !strings.Contains(q.Query, "orderBy:{field:POSITION,direction:ASC}") || !strings.Contains(q.Query, "archivedStates:[ARCHIVED,NOT_ARCHIVED]") {
					t.Error("missing explicit ordered read")
				}
				if pages == 1 {
					return orderedPage(nodes, false, "")
				}
				if calls == 1 {
					if q.Variables["cursor"] != nil {
						t.Error("first cursor")
					}
					return orderedPage(nodes[:2], true, "next")
				}
				if q.Variables["cursor"] != "next" {
					t.Error("continuation cursor")
				}
				return orderedPage(nodes[2:], false, "")
			})
			got, err := client.ListProjectItemsInOrder(context.Background(), "P")
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for _, i := range got {
				ids = append(ids, i.ID)
			}
			if !reflect.DeepEqual(ids, []string{"Z", "A", "Y", "B"}) || calls != pages {
				t.Fatalf("order/calls %v %d", ids, calls)
			}
			if got[0].Issue == nil || *got[0].Issue != (Identity{ID: "I9", Number: 9, Repository: Repository{ID: "R", Owner: "org", Name: "repo"}}) {
				t.Fatal("Issue evidence lost")
			}
			if got[1].Issue != nil || got[1].ContentKind != "PullRequest" || got[1].ContentID != "PR" || got[2].Issue != nil || got[2].ContentKind != "DraftIssue" || !got[2].Archived || got[3].ContentKind != "" || got[3].Type != "REDACTED" {
				t.Fatalf("opaque/archive lost %+v", got)
			}
		})
	}
}
func TestProjectOrderRejectsMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]any) []any
	}{
		{"duplicate item", func(n []any) []any { return append(n, n[0]) }},
		{"duplicate content", func(n []any) []any { return append(n, orderedItem("other", "Issue", "I", 1, true)) }},
		{"duplicate issue identity", func(n []any) []any { return append(n, orderedItem("other", "Issue", "different", 1, false)) }},
		{"issue number", func(n []any) []any { n[0].(map[string]any)["content"].(map[string]any)["number"] = 0; return n }},
		{"issue repository", func(n []any) []any { n[0].(map[string]any)["content"].(map[string]any)["repository"] = nil; return n }},
		{"project evidence", func(n []any) []any { n[0].(map[string]any)["project"] = map[string]any{"id": "wrong"}; return n }},
		{"archive missing", func(n []any) []any { delete(n[0].(map[string]any), "isArchived"); return n }},
		{"null item", func(n []any) []any { return []any{nil} }},
		{"kind mismatch", func(n []any) []any { n[0].(map[string]any)["type"] = "PULL_REQUEST"; return n }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := tc.edit([]any{orderedItem("item", "Issue", "I", 1, false)})
			client := adapter(t, func(request) any { return orderedPage(nodes, false, "") })
			out, err := client.ListProjectItemsInOrder(context.Background(), "P")
			category(t, err, Malformed)
			if out != nil {
				t.Fatal("partial output")
			}
		})
	}
}
func TestProjectOrderPaginationFailures(t *testing.T) {
	for _, mode := range []string{"missing cursor", "cycle", "missing metadata", "wrong project", "wrong type", "missing project"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := adapter(t, func(request) any {
				calls++
				response := orderedPage([]any{}, true, "cursor").(map[string]any)
				n := response["node"].(map[string]any)
				items := n["items"].(map[string]any)
				switch mode {
				case "missing cursor":
					items["pageInfo"] = map[string]any{"hasNextPage": true}
				case "missing metadata":
					delete(items, "pageInfo")
				case "wrong project":
					n["id"] = "other"
				case "wrong type":
					n["__typename"] = "Issue"
				case "missing project":
					response["node"] = nil
				}
				return response
			})
			_, err := client.ListProjectItemsInOrder(context.Background(), "P")
			want := Malformed
			if mode == "missing project" {
				want = NotFound
			}
			category(t, err, want)
			expected := 1
			if mode == "cycle" {
				expected = 2
			}
			if calls != expected {
				t.Fatalf("retried or failed pagination: %d", calls)
			}
		})
	}
}
func TestProjectOrderErrorsAndFake(t *testing.T) {
	for _, kind := range []Category{NotFound, Unauthorized, Forbidden, RateLimited, Conflict, Malformed, Transient, Permanent} {
		calls := 0
		client, err := NewTransport("", nil, TokenFunc(func(context.Context) (string, error) { calls++; return "", &Error{Category: kind} }))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ListProjectItemsInOrder(context.Background(), "P")
		category(t, err, kind)
		if calls != 1 {
			t.Fatal("retry")
		}
		_, err = client.ListProjectItemsInOrder(context.Background(), " \t")
		category(t, err, Permanent)
		if calls != 1 {
			t.Fatal("blank project read")
		}
	}
	f := &ProjectOrderFake{}
	_, err := f.ListProjectItemsInOrder(context.Background(), "P")
	category(t, err, Permanent)
	sentinel := &Error{Category: Conflict}
	f.ListProjectItemsInOrderFunc = func(_ context.Context, id string) ([]OrderedProjectItem, error) {
		if id != "P" {
			t.Fatal(id)
		}
		return nil, sentinel
	}
	_, err = f.ListProjectItemsInOrder(context.Background(), "P")
	if err != sentinel {
		t.Fatal("fake error lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, _ := NewTransport("", nil, TokenFunc(func(context.Context) (string, error) { t.Fatal("cancelled token read"); return "", nil }))
	_, err = client.ListProjectItemsInOrder(ctx, "P")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestProjectOrderDuplicateAcrossPages(t *testing.T) {
	for _, sameItem := range []bool{false, true} {
		t.Run(fmt.Sprint(sameItem), func(t *testing.T) {
			calls := 0
			client := adapter(t, func(request) any {
				calls++
				if calls == 1 {
					return orderedPage([]any{orderedItem("one", "Issue", "I", 1, false)}, true, "next")
				}
				id := "two"
				if sameItem {
					id = "one"
				}
				return orderedPage([]any{orderedItem(id, "Issue", "I", 1, true)}, false, "")
			})
			got, err := client.ListProjectItemsInOrder(context.Background(), "P")
			category(t, err, Malformed)
			if got != nil || calls != 2 {
				t.Fatal("partial result or retry")
			}
		})
	}
}
func TestProjectOrderOpaqueAndDeletedContent(t *testing.T) {
	opaque := orderedItem("opaque", "DraftIssue", "opaque-content", 0, false)
	opaque["content"].(map[string]any)["__typename"] = "OpaqueContent"
	deleted := orderedItem("deleted", "Issue", "deleted-issue", 2, false)
	deleted["content"] = nil
	client := adapter(t, func(request) any { return orderedPage([]any{opaque, deleted}, false, "") })
	got, err := client.ListProjectItemsInOrder(context.Background(), "P")
	if err != nil || len(got) != 2 || got[0].ContentKind != "OpaqueContent" || got[0].Issue != nil || got[1].Type != "ISSUE" || got[1].ContentID != "" || got[1].Issue != nil {
		t.Fatal(got, err)
	}
}
