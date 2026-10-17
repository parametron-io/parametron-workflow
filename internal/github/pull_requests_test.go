package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func prListNode(id string, n int, state string) map[string]any {
	w := identityJSON(id, n)
	w["__typename"] = "PullRequest"
	w["state"] = state
	return w
}
func prPage(nodes []any, total int, next bool, cursor string) map[string]any {
	r := repoJSON()
	r["__typename"] = "Repository"
	r["result"] = map[string]any{"nodes": nodes, "totalCount": total, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}
	return map[string]any{"node": r}
}
func TestPullRequestEnumeration(t *testing.T) {
	for _, size := range []int{0, 1, 3, 100, 101, 10000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			page, reads := 0, 0
			tr := adapter(t, func(q request) any {
				if strings.Contains(q.Query, "repository(owner:") {
					reads++
					return map[string]any{"repository": repoJSON()}
				}
				for _, selection := range []string{"node(id:$id)", "... on Repository", "result:pullRequests(first:100,after:$cursor,states:[OPEN,CLOSED,MERGED])", "totalCount", "__typename", "id number repository{id name owner{login}}", "state", "pageInfo{hasNextPage endCursor}"} {
					if !strings.Contains(q.Query, selection) {
						t.Errorf("missing %q: %s", selection, q.Query)
					}
				}
				if q.Variables["id"] != "R" {
					t.Error(q)
				}
				if page == 0 && q.Variables["cursor"] != nil || page > 0 && q.Variables["cursor"] != fmt.Sprint(page) {
					t.Error(q)
				}
				start := page * 100
				end := start + 100
				if end > size {
					end = size
				}
				nodes := []any{}
				for j := start; j < end; j++ {
					n := size - j
					nodes = append(nodes, prListNode(fmt.Sprint("PR", n), n, []string{"OPEN", "CLOSED", "MERGED"}[j%3]))
				}
				page++
				return prPage(nodes, size, end < size, fmt.Sprint(page))
			})
			got, err := tr.ListPullRequests(context.Background(), Repository{ID: "R", Owner: "org", Name: "repo"})
			if err != nil || len(got) != size || reads != 2 {
				t.Fatal(len(got), err, reads)
			}
			for i, p := range got {
				if p.Number != i+1 || p.ID != fmt.Sprint("PR", i+1) || p.Repository.ID != "R" {
					t.Fatal(p)
				}
			}
		})
	}
	for _, state := range []string{"CLOSED", "MERGED"} {
		tr := adapter(t, func(q request) any {
			if strings.Contains(q.Query, "repository(owner:") {
				return map[string]any{"repository": repoJSON()}
			}
			return prPage([]any{prListNode("PR", 1, state)}, 1, false, "")
		})
		got, err := tr.ListPullRequests(context.Background(), Repository{ID: "R", Owner: "org", Name: "repo"})
		if err != nil || len(got) != 1 || got[0].State != state {
			t.Fatal(got, err)
		}
	}
}
func TestPullRequestMalformedEnumeration(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(map[string]any, map[string]any, map[string]any)
		sentinel error
		category Category
	}{
		{"missing repository", func(p, n, c map[string]any) { p["node"] = nil }, nil, NotFound},
		{"wrong typename", func(p, n, c map[string]any) { n["__typename"] = "Issue" }, nil, Malformed},
		{"wrong repo", func(p, n, c map[string]any) { n["id"] = "other" }, nil, Malformed},
		{"missing connection", func(p, n, c map[string]any) { n["result"] = nil }, ErrPullRequestIncomplete, ""},
		{"null total", func(p, n, c map[string]any) { c["totalCount"] = nil }, ErrPullRequestIncomplete, ""},
		{"negative total", func(p, n, c map[string]any) { c["totalCount"] = -1 }, ErrPullRequestIncomplete, ""},
		{"oversize total", func(p, n, c map[string]any) { c["totalCount"] = 10001 }, ErrPullRequestLimit, ""},
		{"null nodes", func(p, n, c map[string]any) { c["nodes"] = nil }, ErrPullRequestIncomplete, ""},
		{"null node", func(p, n, c map[string]any) { c["nodes"] = []any{nil} }, nil, Malformed},
		{"wrong PR type", func(p, n, c map[string]any) { c["nodes"].([]any)[0].(map[string]any)["__typename"] = "Issue" }, nil, Malformed},
		{"missing ID", func(p, n, c map[string]any) { c["nodes"].([]any)[0].(map[string]any)["id"] = "" }, nil, Malformed},
		{"bad number", func(p, n, c map[string]any) { c["nodes"].([]any)[0].(map[string]any)["number"] = 0 }, nil, Malformed},
		{"bad state", func(p, n, c map[string]any) { c["nodes"].([]any)[0].(map[string]any)["state"] = "UNKNOWN" }, nil, Malformed},
		{"wrong PR repo", func(p, n, c map[string]any) {
			r := repoJSON()
			r["id"] = "other"
			c["nodes"].([]any)[0].(map[string]any)["repository"] = r
		}, nil, Malformed},
		{"duplicate ID", func(p, n, c map[string]any) {
			c["nodes"] = []any{prListNode("PR", 1, "OPEN"), prListNode("PR", 2, "OPEN")}
			c["totalCount"] = 2
		}, nil, Malformed},
		{"duplicate number", func(p, n, c map[string]any) {
			c["nodes"] = []any{prListNode("PR", 1, "OPEN"), prListNode("other", 1, "OPEN")}
			c["totalCount"] = 2
		}, nil, Malformed},
		{"missing page", func(p, n, c map[string]any) { c["pageInfo"] = nil }, ErrPullRequestIncomplete, ""},
		{"null continuation", func(p, n, c map[string]any) { c["pageInfo"] = map[string]any{"hasNextPage": nil} }, ErrPullRequestIncomplete, ""},
		{"missing cursor", func(p, n, c map[string]any) { c["totalCount"] = 2; c["pageInfo"] = map[string]any{"hasNextPage": true} }, ErrPullRequestIncomplete, ""},
		{"empty continuation", func(p, n, c map[string]any) {
			c["nodes"] = []any{}
			c["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "next"}
		}, ErrPullRequestIncomplete, ""},
		{"premature terminal", func(p, n, c map[string]any) { c["totalCount"] = 2 }, ErrPullRequestIncomplete, ""},
		{"excess nodes", func(p, n, c map[string]any) { c["totalCount"] = 0 }, ErrPullRequestIncomplete, ""},
		{"oversize page", func(p, n, c map[string]any) {
			nodes := []any{}
			for i := 1; i <= 101; i++ {
				nodes = append(nodes, prListNode(fmt.Sprint(i), i, "OPEN"))
			}
			c["nodes"] = nodes
			c["totalCount"] = 101
		}, nil, Malformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			tr := adapter(t, func(q request) any {
				calls++
				if strings.Contains(q.Query, "repository(owner:") {
					return map[string]any{"repository": repoJSON()}
				}
				p := prPage([]any{prListNode("PR", 1, "OPEN")}, 1, false, "")
				n := p["node"].(map[string]any)
				c := n["result"].(map[string]any)
				tc.mutate(p, n, c)
				return p
			})
			got, err := tr.ListPullRequests(context.Background(), Repository{ID: "R", Owner: "org", Name: "repo"})
			if got != nil || err == nil {
				t.Fatal("partial success", got, err)
			}
			if tc.sentinel != nil {
				if !errors.Is(err, tc.sentinel) {
					t.Fatal(err)
				}
			} else {
				category(t, err, tc.category)
			}
			if calls != 2 {
				t.Fatal("retry", calls)
			}
		})
	}
}
func TestPullRequestPaginationFailures(t *testing.T) {
	for _, name := range []string{"changed total", "repeated cursor", "cursor cycle", "page limit"} {
		t.Run(name, func(t *testing.T) {
			pages := 0
			tr := adapter(t, func(q request) any {
				if strings.Contains(q.Query, "repository(owner:") {
					return map[string]any{"repository": repoJSON()}
				}
				pages++
				total := 200
				cursor := fmt.Sprint(pages)
				if name == "changed total" && pages == 2 {
					total++
				}
				if name == "repeated cursor" {
					cursor = "same"
				}
				if name == "cursor cycle" {
					cursor = fmt.Sprint(pages % 2)
				}
				return prPage([]any{prListNode(fmt.Sprint("PR", pages), pages, "OPEN")}, total, true, cursor)
			})
			got, err := tr.ListPullRequests(context.Background(), Repository{ID: "R", Owner: "org", Name: "repo"})
			want := ErrPullRequestIncomplete
			if name == "changed total" {
				want = ErrPullRequestChanged
			}
			if name == "page limit" {
				want = ErrPullRequestLimit
			}
			if got != nil || !errors.Is(err, want) || pages > 100 {
				t.Fatal(got, err, pages)
			}
		})
	}
}
func TestPullRequestRepositoryFreshness(t *testing.T) {
	for _, name := range []string{"stable", "replaced", "disappeared", "forbidden", "not found"} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			tr := adapter(t, func(q request) any {
				if strings.Contains(q.Query, "repository(owner:") {
					reads++
					r := repoJSON()
					if reads == 2 {
						switch name {
						case "replaced":
							r["id"] = "new"
						case "disappeared":
							return map[string]any{"repository": nil}
						}
					}
					return map[string]any{"repository": r}
				}
				return prPage([]any{}, 0, false, "")
			})
			if name == "forbidden" || name == "not found" {
				tr.tokens = TokenFunc(func(context.Context) (string, error) {
					return "", failure(map[string]Category{"forbidden": Forbidden, "not found": NotFound}[name])
				})
			}
			got, err := tr.ListPullRequests(context.Background(), Repository{ID: "R", Owner: "org", Name: "repo"})
			switch name {
			case "stable":
				if err != nil || len(got) != 0 {
					t.Fatal(got, err)
				}
			case "replaced":
				if !errors.Is(err, ErrPullRequestChanged) || got != nil {
					t.Fatal(got, err)
				}
			default:
				if err == nil || got != nil {
					t.Fatal(got, err)
				}
			}
		})
	}
}

func TestPullRequestOperationalFailuresNoPartialList(t *testing.T) {
	for _, at := range []int{1, 2, 3} {
		for _, status := range []int{401, 403, 404, 429, 500} {
			t.Run(fmt.Sprint(at, "/", status), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if calls == at {
						w.WriteHeader(status)
						return
					}
					data := map[string]any{"repository": repoJSON()}
					if calls == 2 {
						data = prPage([]any{}, 0, false, "")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				defer s.Close()
				tr, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
				if err != nil {
					t.Fatal(err)
				}
				got, err := tr.ListPullRequests(context.Background(), Repository{ID: "R", Owner: "org", Name: "repo"})
				category(t, err, map[int]Category{401: Unauthorized, 403: Forbidden, 404: NotFound, 429: RateLimited, 500: Transient}[status])
				if got != nil || calls != at {
					t.Fatal("partial success or retry", got, calls)
				}
			})
		}
	}
}
func TestPullRequestPaginationEquivalentPartitions(t *testing.T) {
	var baseline []ListedPullRequest
	for _, width := range []int{1, 2, 3} {
		offset := 0
		tr := adapter(t, func(q request) any {
			if strings.Contains(q.Query, "repository(owner:") {
				return map[string]any{"repository": repoJSON()}
			}
			end := offset + width
			if end > 3 {
				end = 3
			}
			nodes := []any{}
			for n := offset; n < end; n++ {
				nodes = append(nodes, prListNode(fmt.Sprint("PR", 3-n), 3-n, []string{"OPEN", "CLOSED", "MERGED"}[n]))
			}
			offset = end
			return prPage(nodes, 3, end < 3, fmt.Sprint(end))
		})
		got, err := tr.ListPullRequests(context.Background(), Repository{ID: "R", Owner: "org", Name: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		if baseline == nil {
			baseline = got
		} else if !reflect.DeepEqual(baseline, got) {
			t.Fatal("page boundary affected result", got)
		}
	}
}
