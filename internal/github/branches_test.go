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
	"sync/atomic"
	"testing"
)

var branchRepo = Repository{ID: "R", Owner: "org", Name: "repo"}

const branchSHA = "0123456789abcdef0123456789abcdef01234567"

func branchWire(name string) map[string]any {
	return map[string]any{"name": name, "prefix": "refs/heads/", "repository": map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}}, "target": map[string]any{"__typename": "Commit", "oid": branchSHA}}
}
func TestBranchExactRequests(t *testing.T) {
	for _, op := range []string{"default", "lookup", "create"} {
		t.Run(op, func(t *testing.T) {
			calls := 0
			c := mutationAdapter(t, func(q request) any {
				calls++
				name := "development/issue-37"
				switch op {
				case "default":
					name = "trunk"
					want := `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){id name owner{login} ref:defaultBranchRef{` + branchSelection + `}}}`
					if q.Query != want || !reflect.DeepEqual(q.Variables, map[string]any{"owner": "org", "name": "repo"}) {
						t.Error(q)
					}
				case "lookup":
					want := `query($owner:String!,$name:String!,$ref:String!){repository(owner:$owner,name:$name){id name owner{login} ref:ref(qualifiedName:$ref){` + branchSelection + `}}}`
					if q.Query != want || !reflect.DeepEqual(q.Variables, map[string]any{"owner": "org", "name": "repo", "ref": "refs/heads/" + name}) {
						t.Error(q)
					}
				case "create":
					want := `mutation($input:CreateRefInput!){result:createRef(input:$input){ref{` + branchSelection + `}}}`
					if q.Query != want || !reflect.DeepEqual(q.Variables, map[string]any{"input": map[string]any{"repositoryId": "R", "name": "refs/heads/" + name, "oid": branchSHA}}) {
						t.Error(q)
					}
					return map[string]any{"result": map[string]any{"ref": branchWire(name)}}
				}
				return map[string]any{"repository": map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}, "ref": branchWire(name)}}
			})
			b, err := branchCall(context.Background(), c, op)
			want := "development/issue-37"
			if op == "default" {
				want = "trunk"
			}
			if err != nil || b != (Branch{branchRepo, want, branchSHA}) || calls != 1 {
				t.Fatal(b, err, calls)
			}
		})
	}
}
func branchCall(ctx context.Context, c *Transport, op string) (Branch, error) {
	switch op {
	case "default":
		return c.DefaultBranch(ctx, branchRepo)
	case "lookup":
		return c.Branch(ctx, branchRepo, "development/issue-37")
	default:
		return c.CreateBranch(ctx, branchRepo, "development/issue-37", branchSHA)
	}
}
func TestBranchResponseValidation(t *testing.T) {
	for _, op := range []string{"default", "lookup", "create"} {
		for _, bad := range []string{"missing", "name", "prefix", "repository", "owner", "repo-name", "target", "target-kind", "sha", "creation-sha"} {
			if bad == "creation-sha" && op != "create" {
				continue
			}
			t.Run(op+"/"+bad, func(t *testing.T) {
				c := mutationAdapter(t, func(request) any {
					ref := branchWire("development/issue-37")
					switch bad {
					case "missing":
						return map[string]any{"repository": map[string]any{"id": "R"}}
					case "name":
						ref["name"] = "../invalid"
					case "prefix":
						ref["prefix"] = "refs/tags/"
					case "repository":
						ref["repository"].(map[string]any)["id"] = "wrong"
					case "owner":
						ref["repository"].(map[string]any)["owner"] = map[string]any{"login": "wrong"}
					case "repo-name":
						ref["repository"].(map[string]any)["name"] = "wrong"
					case "target":
						ref["target"] = nil
					case "target-kind":
						ref["target"].(map[string]any)["__typename"] = "Tag"
					case "sha":
						ref["target"].(map[string]any)["oid"] = "moving-name"
					case "creation-sha":
						ref["target"].(map[string]any)["oid"] = strings.Repeat("a", 40)
					}
					if op == "create" {
						return map[string]any{"result": map[string]any{"ref": ref}}
					}
					return map[string]any{"repository": map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}, "ref": ref}}
				})
				_, err := branchCall(context.Background(), c, op)
				category(t, err, Malformed)
			})
		}
	}
	for _, response := range []any{map[string]any{"repository": nil}, map[string]any{"repository": map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}, "ref": nil}}} {
		c := mutationAdapter(t, func(request) any { return response })
		for _, op := range []string{"default", "lookup"} {
			_, err := branchCall(context.Background(), c, op)
			category(t, err, NotFound)
		}
	}
	// A well-formed but different exact name must be rejected too.
	c := mutationAdapter(t, func(request) any { return map[string]any{"result": map[string]any{"ref": branchWire("other")}} })
	_, err := branchCall(context.Background(), c, "create")
	category(t, err, Malformed)
}
func TestBranchFailuresNoHiddenRetry(t *testing.T) {
	for _, op := range []string{"default", "lookup", "create"} {
		for _, tc := range []struct {
			status int
			body   string
			want   Category
		}{{404, "private", NotFound}, {409, "private", Conflict}, {412, "private", Conflict}, {429, "private", RateLimited}, {503, "private", Transient}, {200, `{`, Malformed}, {200, `{"errors":[{"type":"RATE_LIMITED","message":"private"}]}`, RateLimited}} {
			t.Run(fmt.Sprintf("%s/%d/%s", op, tc.status, tc.want), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}))
				defer s.Close()
				c, _ := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "secret", nil }))
				_, err := branchCall(context.Background(), c, op)
				category(t, err, tc.want)
				if calls != 1 || strings.Contains(err.Error(), "private") {
					t.Fatal(calls, err)
				}
			})
		}
		t.Run(op+"/cancel", func(t *testing.T) {
			calls := 0
			c := mutationAdapter(t, func(request) any { calls++; return nil })
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := branchCall(ctx, c, op)
			if !errors.Is(err, context.Canceled) || calls != 0 {
				t.Fatal(err, calls)
			}
		})
	}
}
func TestBranchInvalidArgumentsAndFake(t *testing.T) {
	calls := 0
	c := mutationAdapter(t, func(request) any { calls++; return nil })
	for _, name := range []string{"", "refs/heads/main", "../main", "a..b", "a.lock", "a//b", "a/", "-a", "a b", "a@{b", "a\n", "a.", "a?", strings.Repeat("a", 256)} {
		if ValidBranchName(name) {
			t.Fatal(name)
		}
		_, err := c.Branch(context.Background(), branchRepo, name)
		category(t, err, Permanent)
		_, err = c.CreateBranch(context.Background(), branchRepo, name, branchSHA)
		category(t, err, Permanent)
	}
	for _, sha := range []string{"main", strings.Repeat("A", 40), strings.Repeat("g", 40), strings.Repeat("a", 39)} {
		_, err := c.CreateBranch(context.Background(), branchRepo, "valid", sha)
		category(t, err, Permanent)
	}
	for _, repo := range []Repository{{}, {ID: "R", Owner: "org/other", Name: "repo"}, {ID: " R", Owner: "org", Name: "repo"}} {
		_, err := c.DefaultBranch(context.Background(), repo)
		category(t, err, Permanent)
	}
	if calls != 0 {
		t.Fatal(calls)
	}
	f := BranchFake{}
	_, a := f.DefaultBranch(context.Background(), branchRepo)
	_, b := f.Branch(context.Background(), branchRepo, "main")
	_, d := f.CreateBranch(context.Background(), branchRepo, "valid", branchSHA)
	for _, err := range []error{a, b, d} {
		category(t, err, Permanent)
	}
}
func TestBranchCancellationDuringRequest(t *testing.T) {
	// Cancellation after admission retains errors.Is and makes only one request.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cancel()
		<-release
	}))
	defer s.Close()
	c, _ := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "secret", nil }))
	_, err := c.CreateBranch(ctx, branchRepo, "valid", branchSHA)
	close(release)
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
}
func TestBranchOuterRepositoryValidation(t *testing.T) {
	for _, field := range []string{"id", "name", "owner"} {
		c := mutationAdapter(t, func(request) any {
			repo := map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}, "ref": branchWire("main")}
			repo[field] = nil
			return map[string]any{"repository": repo}
		})
		_, err := c.DefaultBranch(context.Background(), branchRepo)
		category(t, err, Malformed)
	}
	// Keep wire fixtures JSON-representable rather than relying on decoder aliases.
	if _, err := json.Marshal(branchWire("main")); err != nil {
		t.Fatal(err)
	}
}

func TestBranchMissingFieldsCannotProveAbsence(t *testing.T) {
	for _, response := range []any{map[string]any{}, map[string]any{"repository": map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}}}} {
		c := mutationAdapter(t, func(request) any { return response })
		for _, op := range []string{"default", "lookup"} {
			_, err := branchCall(context.Background(), c, op)
			category(t, err, Malformed)
		}
	}
}

func TestExactBranchAbsenceEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response any
		missing  bool
	}{
		{"validated ref absent", map[string]any{"repository": map[string]any{"id": "R", "name": "repo", "owner": map[string]any{"login": "org"}, "ref": nil}}, true},
		{"repository absent", map[string]any{"repository": nil}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mutationAdapter(t, func(request) any { return tc.response })
			_, err := c.Branch(context.Background(), branchRepo, "development/issue-37")
			category(t, err, NotFound)
			var missing *MissingBranch
			if errors.As(err, &missing) != tc.missing {
				t.Fatal(err)
			}
			if tc.missing && (missing.Repository != branchRepo || missing.Name != "development/issue-37") {
				t.Fatal(missing)
			}
		})
	}
}
