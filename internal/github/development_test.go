package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDevelopmentReferencesValidate(t *testing.T) {
	pr := nativeTestIdentity()
	pr.ID = "PR"
	issue := nativeTestIdentity()
	for _, tc := range []struct {
		name   string
		change func(*DevelopmentReferences)
		bad    bool
	}{
		{"empty", func(r *DevelopmentReferences) { r.Manual = nil }, false},
		{"manual", func(r *DevelopmentReferences) {}, false},
		{"keyword", func(r *DevelopmentReferences) { r.Keyword = r.Manual; r.Manual = nil }, false},
		{"overlap", func(r *DevelopmentReferences) { r.Keyword = r.Manual }, false},
		{"incomplete", func(r *DevelopmentReferences) { r.Complete = false }, true},
		{"missing pr", func(r *DevelopmentReferences) { r.PR = Identity{} }, true},
		{"wrong pr", func(r *DevelopmentReferences) { r.PR.ID = "OTHER" }, true},
		{"invalid issue", func(r *DevelopmentReferences) { r.Manual[0].Number = 0 }, true},
		{"duplicate manual", func(r *DevelopmentReferences) { r.Manual = append(r.Manual, r.Manual[0]) }, true},
		{"duplicate keyword", func(r *DevelopmentReferences) { r.Keyword = []Identity{issue, issue} }, true},
		{"node conflict", func(r *DevelopmentReferences) { i := issue; i.Number++; r.Keyword = []Identity{i} }, true},
		{"number conflict", func(r *DevelopmentReferences) { i := issue; i.ID = "OTHER"; r.Keyword = []Identity{i} }, true},
		{"repository name conflict", func(r *DevelopmentReferences) {
			i := issue
			i.ID = "OTHER"
			i.Number++
			i.Repository.ID = "OTHER"
			r.Keyword = []Identity{i}
		}, true},
		{"repository node conflict", func(r *DevelopmentReferences) {
			i := issue
			i.ID = "OTHER"
			i.Number++
			i.Repository.Name = "other"
			r.Keyword = []Identity{i}
		}, true},
		{"pr repository conflict", func(r *DevelopmentReferences) { r.Manual[0].Repository.ID = "OTHER" }, true},
		{"pr node used as issue", func(r *DevelopmentReferences) { r.Manual[0] = pr }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := DevelopmentReferences{PR: pr, Complete: true, Manual: []Identity{issue}}
			tc.change(&r)
			err := r.Validate(pr)
			if (err != nil) != tc.bad {
				t.Fatalf("err=%v bad=%v", err, tc.bad)
			}
		})
	}
}

func developmentWire(id string, n int) map[string]any {
	w := identityJSON(id, n)
	w["__typename"] = "Issue"
	return w
}
func TestDevelopmentReadAndMutationWire(t *testing.T) {
	pr := nativeTestIdentity()
	pr.ID = "PR"
	issue := nativeTestIdentity()
	for _, tc := range []struct {
		name                  string
		add, present, keyword bool
		receipt               string
		bad                   bool
	}{
		{"add", true, false, false, "I", false}, {"remove", false, true, false, "I", false},
		{"present add", true, true, false, "I", false}, {"absent remove", false, false, false, "I", false},
		{"keyword remove", false, false, true, "I", false}, {"wrong receipt", true, false, false, "OTHER", true},
		{"missing receipt", true, false, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutations := 0
			manual, keyword := 0, 0
			c := mutationAdapter(t, func(q request) any {
				if strings.HasPrefix(q.Query, "mutation") {
					mutations++
					op, input := "removeCloseIssueReferences", "RemoveCloseIssueReferencesInput"
					if tc.add {
						op, input = "addCloseIssueReferences", "AddCloseIssueReferencesInput"
					}
					want := `mutation($input:` + input + `!){result:` + op + `(input:$input){issue{` + identitySelection + `}}}`
					if q.Query != want {
						t.Errorf("query=%s", q.Query)
					}
					if !reflect.DeepEqual(q.Variables["input"], map[string]any{"issueId": "I", "pullRequestIds": []any{"PR"}}) {
						t.Errorf("input=%v", q.Variables)
					}
					if tc.receipt == "" {
						return map[string]any{"result": nil}
					}
					return map[string]any{"result": map[string]any{"issue": identityJSON(tc.receipt, 9)}}
				}
				if strings.Contains(q.Query, "closingIssuesReferences") {
					nodes := []any{}
					if strings.Contains(q.Query, "userLinkedOnly:true") {
						manual++
						if tc.present {
							nodes = append(nodes, developmentWire("I", 9))
						}
					} else if strings.Contains(q.Query, "excludeUserLinked:true") {
						keyword++
						if tc.keyword {
							nodes = append(nodes, developmentWire("I", 9))
						}
					} else {
						t.Errorf("untyped connection: %s", q.Query)
					}
					return connectionJSON("PullRequest", nodes, false, "")
				}
				id := "PR"
				if strings.Contains(q.Query, "result:issue(") {
					id = "I"
				}
				return map[string]any{"repository": map[string]any{"result": nativeJSON(id, "OPEN")}}
			})
			var err error
			if tc.add {
				err = c.AddDevelopmentReference(context.Background(), pr, issue)
			} else {
				err = c.RemoveDevelopmentReference(context.Background(), pr, issue)
			}
			if (err != nil) != tc.bad {
				t.Fatalf("err=%v", err)
			}
			want := 1
			if tc.present == tc.add {
				want = 0
			}
			if mutations != want || manual != 1 || keyword != 1 {
				t.Fatalf("mutations=%d manual=%d keyword=%d", mutations, manual, keyword)
			}
		})
	}
}
func TestDevelopmentReadPaginationAndMalformed(t *testing.T) {
	pr := nativeTestIdentity()
	pr.ID = "PR"
	for _, bad := range []string{"", "null", "type", "missing identity", "pageInfo", "cycle", "duplicate"} {
		t.Run(bad, func(t *testing.T) {
			pages := map[bool]int{}
			c := mutationAdapter(t, func(q request) any {
				if !strings.Contains(q.Query, "closingIssuesReferences") {
					return map[string]any{"repository": map[string]any{"result": nativeJSON("PR", "OPEN")}}
				}
				manual := strings.Contains(q.Query, "userLinkedOnly:true")
				pages[manual]++
				n := pages[manual]
				node := any(developmentWire("I", 9))
				switch bad {
				case "null":
					node = nil
				case "type":
					w := developmentWire("I", 9)
					w["__typename"] = "PullRequest"
					node = w
				case "missing identity":
					node = map[string]any{"__typename": "Issue"}
				}
				if bad == "pageInfo" {
					return map[string]any{"node": map[string]any{"__typename": "PullRequest", "result": map[string]any{"nodes": []any{node}}}}
				}
				if bad == "cycle" {
					return connectionJSON("PullRequest", []any{}, true, "same")
				}
				if bad == "duplicate" {
					return connectionJSON("PullRequest", []any{node, node}, false, "")
				}
				if n == 1 {
					return connectionJSON("PullRequest", []any{node}, true, "next")
				}
				return connectionJSON("PullRequest", []any{}, false, "")
			})
			r, err := c.DevelopmentReferences(context.Background(), pr)
			if bad != "" {
				if err == nil {
					t.Fatalf("accepted malformed %s: %+v", bad, r)
				}
				return
			}
			if err != nil || len(r.Manual) != 1 || len(r.Keyword) != 1 || pages[true] != 2 || pages[false] != 2 {
				t.Fatalf("r=%+v pages=%v err=%v", r, pages, err)
			}
		})
	}
}
func TestDevelopmentCancellationAndInvalidIdentity(t *testing.T) {
	calls := 0
	c := mutationAdapter(t, func(q request) any { calls++; return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pr := nativeTestIdentity()
	issue := pr
	pr.ID = "PR"
	if err := c.AddDevelopmentReference(ctx, pr, issue); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if _, err := c.DevelopmentReferences(context.Background(), Identity{}); err == nil {
		t.Fatal("invalid identity accepted")
	}
	if calls != 0 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestDevelopmentMutationRejectsFreshClosedPR(t *testing.T) {
	pr := nativeTestIdentity()
	pr.ID = "PR"
	issue := nativeTestIdentity()
	reads, mutations := 0, 0
	c := mutationAdapter(t, func(q request) any {
		if strings.HasPrefix(q.Query, "mutation") {
			mutations++
			return map[string]any{"result": map[string]any{"issue": identityJSON("I", 9)}}
		}
		if strings.Contains(q.Query, "closingIssuesReferences") {
			return connectionJSON("PullRequest", []any{}, false, "")
		}
		id, state := "PR", "OPEN"
		if strings.Contains(q.Query, "result:issue(") {
			id = "I"
		} else {
			reads++
			if reads == 3 {
				state = "CLOSED"
			}
		}
		return map[string]any{"repository": map[string]any{"result": nativeJSON(id, state)}}
	})
	err := c.AddDevelopmentReference(context.Background(), pr, issue)
	if err == nil || mutations != 0 {
		t.Fatalf("closed before mutation: err=%v mutations=%d", err, mutations)
	}
}

func TestDevelopmentMutationPreflightIdentity(t *testing.T) {
	for _, mode := range []string{"closed", "merged", "pr node", "pr number", "pr repository", "target node", "target repository", "invalid pr", "invalid target"} {
		t.Run(mode, func(t *testing.T) {
			pr := nativeTestIdentity()
			pr.ID = "PR"
			issue := nativeTestIdentity()
			mutations := 0
			c := mutationAdapter(t, func(q request) any {
				if strings.HasPrefix(q.Query, "mutation") {
					mutations++
					return nil
				}
				if strings.Contains(q.Query, "closingIssuesReferences") {
					return connectionJSON("PullRequest", []any{}, false, "")
				}
				isTarget := strings.Contains(q.Query, "result:issue(")
				id := "PR"
				if isTarget {
					id = "I"
				}
				w := nativeJSON(id, "OPEN")
				switch mode {
				case "closed":
					if !isTarget {
						w["state"] = "CLOSED"
					}
				case "merged":
					if !isTarget {
						w["state"] = "MERGED"
					}
				case "pr node":
					if !isTarget {
						w["id"] = "OTHER"
					}
				case "pr number":
					if !isTarget {
						w["number"] = 10
					}
				case "pr repository":
					if !isTarget {
						r := repoJSON()
						r["id"] = "OTHER"
						w["repository"] = r
					}
				case "target node":
					if isTarget {
						w["id"] = "OTHER"
					}
				case "target repository":
					if isTarget {
						r := repoJSON()
						r["id"] = "OTHER"
						w["repository"] = r
					}
				}
				return map[string]any{"repository": map[string]any{"result": w}}
			})
			if mode == "invalid pr" {
				pr.ID = ""
			}
			if mode == "invalid target" {
				issue.Number = 0
			}
			if err := c.AddDevelopmentReference(context.Background(), pr, issue); err == nil || mutations != 0 {
				t.Fatalf("err=%v mutations=%d", err, mutations)
			}
		})
	}
}

func TestDevelopmentWireFailuresNoRetry(t *testing.T) {
	for _, operation := range []string{"read", "add", "remove"} {
		for _, tc := range []struct {
			name   string
			status int
			body   string
			want   Category
		}{
			{"unauthorized", 401, `{}`, Unauthorized}, {"forbidden", 403, `{}`, Forbidden}, {"rate", 429, `{}`, RateLimited}, {"conflict", 409, `{}`, Conflict}, {"transient", 503, `{}`, Transient}, {"not found", 404, `{}`, NotFound}, {"permanent", 422, `{}`, Permanent}, {"malformed", 200, `{`, Malformed}, {"empty envelope", 200, `{}`, Malformed}, {"null data", 200, `{"data":null}`, Malformed}, {"graphql error", 200, `{"errors":[{"type":"FORBIDDEN"}]}`, Forbidden},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				failures, mutations := 0, 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var q request
					if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
						t.Error(err)
					}
					mutation := strings.HasPrefix(q.Query, "mutation")
					if mutation {
						mutations++
					}
					if mutation || operation == "read" {
						failures++
						w.WriteHeader(tc.status)
						w.Write([]byte(tc.body))
						return
					}
					var data any
					if strings.Contains(q.Query, "closingIssuesReferences") {
						nodes := []any{}
						if operation == "remove" && strings.Contains(q.Query, "userLinkedOnly:true") {
							nodes = append(nodes, developmentWire("I", 9))
						}
						data = connectionJSON("PullRequest", nodes, false, "")
					} else {
						id := "PR"
						if strings.Contains(q.Query, "result:issue(") {
							id = "I"
						}
						data = map[string]any{"repository": map[string]any{"result": nativeJSON(id, "OPEN")}}
					}
					json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				defer s.Close()
				c, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
				if err != nil {
					t.Fatal(err)
				}
				pr := nativeTestIdentity()
				pr.ID = "PR"
				issue := nativeTestIdentity()
				switch operation {
				case "read":
					_, err = c.DevelopmentReferences(context.Background(), pr)
				case "add":
					err = c.AddDevelopmentReference(context.Background(), pr, issue)
				case "remove":
					err = c.RemoveDevelopmentReference(context.Background(), pr, issue)
				}
				category(t, err, tc.want)
				want := 1
				if operation == "read" {
					want = 0
				}
				if failures != 1 || mutations != want {
					t.Fatalf("failures=%d mutations=%d", failures, mutations)
				}
			})
		}
	}
}
func TestDevelopmentInFlightCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
			defer s.Close()
			c, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
			if err != nil {
				t.Fatal(err)
			}
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.DevelopmentReferences(ctx, nativeTestIdentity()); done <- err }()
			<-entered
			if !deadline {
				cancel()
			}
			err = <-done
			close(release)
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestDevelopmentReadCrossRepositoryExactIdentity(t *testing.T) {
	pr := nativeTestIdentity()
	pr.ID = "PR"
	other := Identity{ID: "CROSS", Number: 9, Repository: Repository{ID: "R2", Owner: "org", Name: "other"}}
	c := adapter(t, func(q request) any {
		if strings.Contains(q.Query, "closingIssuesReferences") {
			extra := ",userLinkedOnly:true"
			if strings.Contains(q.Query, "excludeUserLinked:true") {
				extra = ",excludeUserLinked:true"
			}
			want := `query($id:ID!,$cursor:String){node(id:$id){__typename ... on PullRequest {result:closingIssuesReferences(first:100,after:$cursor` + extra + `){nodes{__typename ` + identitySelection + `} pageInfo{hasNextPage endCursor}}}}}`
			if q.Query != want || q.Variables["id"] != "PR" || q.Variables["cursor"] != nil {
				t.Errorf("query=%s variables=%v", q.Query, q.Variables)
			}
			w := developmentWire("CROSS", 9)
			w["repository"] = map[string]any{"id": "R2", "name": "other", "owner": map[string]any{"login": "org"}}
			return connectionJSON("PullRequest", []any{w}, false, "")
		}
		if q.Variables["owner"] != "org" || q.Variables["name"] != "repo" || q.Variables["number"] != float64(9) {
			t.Errorf("preflight variables=%v", q.Variables)
		}
		return map[string]any{"repository": map[string]any{"result": nativeJSON("PR", "OPEN")}}
	})
	got, err := c.DevelopmentReferences(context.Background(), pr)
	want := DevelopmentReferences{PR: pr, Complete: true, Manual: []Identity{other}, Keyword: []Identity{other}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
func TestDevelopmentMutationInFlightCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	mutations := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q request
		json.NewDecoder(r.Body).Decode(&q)
		if strings.HasPrefix(q.Query, "mutation") {
			mutations++
			close(entered)
			<-release
			return
		}
		var data any
		if strings.Contains(q.Query, "closingIssuesReferences") {
			data = connectionJSON("PullRequest", []any{}, false, "")
		} else {
			id := "PR"
			if strings.Contains(q.Query, "result:issue(") {
				id = "I"
			}
			data = map[string]any{"repository": map[string]any{"result": nativeJSON(id, "OPEN")}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer s.Close()
	c, err := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	pr := nativeTestIdentity()
	pr.ID = "PR"
	go func() { done <- c.AddDevelopmentReference(ctx, pr, nativeTestIdentity()) }()
	<-entered
	cancel()
	err = <-done
	close(release)
	if !errors.Is(err, context.Canceled) || mutations != 1 {
		t.Fatalf("err=%v mutations=%d", err, mutations)
	}
}
