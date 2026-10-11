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

const activityHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func comparisonGraph() map[string]any {
	repo := map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}}
	commit := func(s string) any { return map[string]any{"__typename": "Commit", "oid": s, "repository": repo} }
	return map[string]any{"repository": map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}, "base": commit(branchSHA), "head": commit(activityHead)}}
}
func comparisonREST(endpoint, status string) map[string]any {
	a, b := 0, 0
	merge := branchSHA
	switch status {
	case "ahead":
		a = 2
	case "behind":
		b = 2
		merge = activityHead
	case "diverged":
		a = 2
		b = 2
		merge = strings.Repeat("c", 40)
	}
	return map[string]any{"url": endpoint + "/repos/org/repo/compare/" + branchSHA + "..." + activityHead, "status": status, "ahead_by": a, "behind_by": b, "base_commit": map[string]any{"sha": branchSHA}, "merge_base_commit": map[string]any{"sha": merge}}
}
func TestComparisonExactAndFailClosed(t *testing.T) {
	for _, bad := range []string{"ahead", "behind", "diverged", "repo", "owner", "name", "base", "head", "kind", "commit-repo", "null", "url", "missing-count", "negative", "status", "inconsistent", "merge", "base-rest", "invalid-json", "unrelated"} {
		t.Run(bad, func(t *testing.T) {
			calls := 0
			var endpoint string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
					t.Error("headers")
				}
				if r.Method == http.MethodPost {
					var q request
					if json.NewDecoder(r.Body).Decode(&q) != nil {
						t.Error("request")
					}
					if !reflect.DeepEqual(q.Variables, map[string]any{"owner": "org", "name": "repo", "base": branchSHA, "head": activityHead}) || !strings.Contains(q.Query, "object(expression:$base)") || strings.Contains(q.Query, "history") {
						t.Error(q)
					}
					data := comparisonGraph()
					repo := data["repository"].(map[string]any)
					switch bad {
					case "repo":
						repo["id"] = "wrong"
					case "owner":
						repo["owner"] = map[string]any{"login": "wrong"}
					case "name":
						repo["name"] = "wrong"
					case "base":
						repo["base"].(map[string]any)["oid"] = activityHead
					case "head":
						repo["head"].(map[string]any)["oid"] = branchSHA
					case "kind":
						repo["head"].(map[string]any)["__typename"] = "Tag"
					case "commit-repo":
						repo["head"].(map[string]any)["repository"] = nil
					case "null":
						data["repository"] = nil
					}
					json.NewEncoder(w).Encode(map[string]any{"data": data})
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/repos/org/repo/compare/"+branchSHA+"..."+activityHead || r.URL.RawQuery != "per_page=1" {
					t.Error(r.Method, r.URL)
				}
				if bad == "unrelated" {
					w.WriteHeader(404)
					return
				}
				if bad == "invalid-json" {
					fmt.Fprint(w, "null")
					return
				}
				status := "ahead"
				if bad == "behind" || bad == "diverged" {
					status = bad
				}
				data := comparisonREST(endpoint, status)
				switch bad {
				case "url":
					data["url"] = endpoint + "/repos/other/repo/compare/" + branchSHA + "..." + activityHead
				case "missing-count":
					delete(data, "ahead_by")
				case "negative":
					data["ahead_by"] = -1
				case "status":
					data["status"] = "unknown"
				case "inconsistent":
					data["behind_by"] = 1
				case "merge":
					data["merge_base_commit"] = map[string]any{"sha": activityHead}
				case "base-rest":
					data["base_commit"] = map[string]any{"sha": activityHead}
				}
				json.NewEncoder(w).Encode(data)
			}))
			defer server.Close()
			endpoint = server.URL
			c, _ := NewTransport(endpoint, server.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
			got, err := c.CompareCommits(context.Background(), branchRepo, branchSHA, activityHead)
			if bad == "ahead" || bad == "behind" || bad == "diverged" {
				if err != nil || got.Status != bad || got.Repository != branchRepo || got.BaseSHA != branchSHA || got.HeadSHA != activityHead || calls != 2 {
					t.Fatal(got, err, calls)
				}
			} else {
				want := Malformed
				if bad == "null" || bad == "unrelated" {
					want = NotFound
				}
				category(t, err, want)
				if calls > 2 {
					t.Fatal("hidden retry", calls)
				}
			}
		})
	}
}
func TestDateMutationExactAndValidation(t *testing.T) {
	for _, bad := range []string{"valid", "item", "project", "null"} {
		t.Run(bad, func(t *testing.T) {
			calls := 0
			c := mutationAdapter(t, func(q request) any {
				calls++
				want := map[string]any{"input": map[string]any{"projectId": "P", "itemId": "ITEM", "fieldId": "F", "value": map[string]any{"date": "2026-10-10"}}}
				if !reflect.DeepEqual(q.Variables, want) || !strings.Contains(q.Query, "updateProjectV2ItemFieldValue") {
					t.Error(q)
				}
				item, project := "ITEM", "P"
				if bad == "item" {
					item = "wrong"
				}
				if bad == "project" {
					project = "wrong"
				}
				if bad == "null" {
					return map[string]any{"result": nil}
				}
				return map[string]any{"result": map[string]any{"projectV2Item": map[string]any{"id": item, "project": map[string]any{"id": project}}}}
			})
			err := c.SetProjectDate(context.Background(), "P", "ITEM", "F", "2026-10-10")
			if bad == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				category(t, err, Malformed)
			}
			if calls != 1 {
				t.Fatal(calls)
			}
			for _, date := range []string{"2026-1-01", "2026-02-30", "2026-10-10T00:00:00Z", "", "0000-01-01"} {
				category(t, c.SetProjectDate(context.Background(), "P", "ITEM", "F", date), Permanent)
			}
			for _, ids := range [][3]string{{"", "ITEM", "F"}, {"P", " ", "F"}, {"P", "ITEM", "bad field"}} {
				category(t, c.SetProjectDate(context.Background(), ids[0], ids[1], ids[2], "2026-10-10"), Permanent)
			}
			if calls != 1 {
				t.Fatal("invalid inputs sent", calls)
			}
		})
	}
}
func TestActivityTransportErrorsAndCancellation(t *testing.T) {
	for _, op := range []string{"date", "compare"} {
		for _, status := range []int{404, 409, 429, 503} {
			t.Run(fmt.Sprintf("%s/%d", op, status), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if op == "compare" && r.Method == http.MethodPost {
						json.NewEncoder(w).Encode(map[string]any{"data": comparisonGraph()})
						return
					}
					w.WriteHeader(status)
					fmt.Fprint(w, "private-token-details")
				}))
				defer s.Close()
				c, _ := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
				var err error
				if op == "date" {
					err = c.SetProjectDate(context.Background(), "P", "ITEM", "F", "2026-10-10")
				} else {
					_, err = c.CompareCommits(context.Background(), branchRepo, branchSHA, activityHead)
				}
				expected := map[int]Category{404: NotFound, 409: Conflict, 429: RateLimited, 503: Transient}[status]
				category(t, err, expected)
				want := 1
				if op == "compare" {
					want = 2
				}
				if calls != want || strings.Contains(err.Error(), "private") {
					t.Fatal(err, calls)
				}
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c, _ := NewTransport("http://unused", nil, TokenFunc(func(context.Context) (string, error) { t.Fatal("canceled token request"); return "", nil }))
		var err error
		if op == "date" {
			err = c.SetProjectDate(ctx, "P", "ITEM", "F", "2026-10-10")
		} else {
			_, err = c.CompareCommits(ctx, branchRepo, branchSHA, activityHead)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	c := mutationAdapter(t, func(request) any { t.Fatal("invalid comparison sent"); return nil })
	for _, pair := range [][2]string{{"main", activityHead}, {branchSHA, "HEAD"}, {strings.ToUpper(branchSHA), activityHead}} {
		_, err := c.CompareCommits(context.Background(), branchRepo, pair[0], pair[1])
		category(t, err, Permanent)
	}
}

func TestIdenticalComparison(t *testing.T) {
	var endpoint string
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == http.MethodPost {
			data := comparisonGraph()
			data["repository"].(map[string]any)["head"].(map[string]any)["oid"] = branchSHA
			json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"url": endpoint + "/repos/org/repo/compare/" + branchSHA + "..." + branchSHA, "status": "identical", "ahead_by": 0, "behind_by": 0, "base_commit": map[string]any{"sha": branchSHA}, "merge_base_commit": map[string]any{"sha": branchSHA}})
	}))
	defer s.Close()
	endpoint = s.URL
	c, _ := NewTransport(endpoint, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
	got, err := c.CompareCommits(context.Background(), branchRepo, branchSHA, branchSHA)
	if err != nil || got.Status != "identical" || got.AheadBy != 0 || got.BehindBy != 0 || calls != 2 {
		t.Fatal(got, err, calls)
	}
}
