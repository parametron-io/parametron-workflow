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

func commentResource(kind string) CommentResource {
	return CommentResource{Identity: nativeTestIdentity(), Kind: kind}
}
func commentJSON(kind string, id string, db int, body string) map[string]any {
	path := "issues"
	if kind == "pull_request" {
		path = "pull"
	}
	return map[string]any{"__typename": "IssueComment", "id": id, "databaseId": db, "body": body, "url": fmt.Sprintf("https://github.com/org/repo/%s/9#issuecomment-%d", path, db), "author": map[string]any{"__typename": "Bot", "id": "BOT", "login": "controller[bot]"}}
}
func commentPage(kind string, nodes []any, total int, next bool, cursor string) map[string]any {
	p := identityJSON("I", 9)
	typ := "Issue"
	if kind == "pull_request" {
		typ = "PullRequest"
	}
	p["__typename"] = typ
	p["comments"] = map[string]any{"nodes": nodes, "totalCount": total, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}
	return map[string]any{"node": p}
}
func TestCommentURLRequiresGitHubOrigin(t *testing.T) {
	r := commentResource("issue")
	for _, host := range []string{"evil.example", "github.com.evil.example", "github.com:444", "github.com."} {
		t.Run(host, func(t *testing.T) {
			raw := commentJSON("issue", "C", 1, "body")
			raw["url"] = fmt.Sprintf("https://%s/org/repo/issues/9#issuecomment-1", host)
			b, _ := json.Marshal(raw)
			var w wireComment
			if e := json.Unmarshal(b, &w); e != nil {
				t.Fatal(e)
			}
			_, e := w.normalized(r)
			if e == nil {
				t.Fatal("foreign origin accepted as exact GitHub comment identity")
			}
			category(t, e, Malformed)
		})
	}
}
func TestCommentExactIdentity(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		for _, variant := range []string{"valid", "review", "missing id", "database", "missing body", "missing author", "deleted author", "author type", "author id", "author login", "scheme", "query", "fragment", "resource", "repository", "surface", "userinfo"} {
			t.Run(kind+"/"+variant, func(t *testing.T) {
				raw := commentJSON(kind, "C", 1, "body")
				switch variant {
				case "review":
					raw["__typename"] = "PullRequestReviewComment"
				case "missing id":
					raw["id"] = ""
				case "database":
					raw["databaseId"] = 0
				case "missing body":
					raw["body"] = nil
				case "missing author":
					raw["author"] = nil
				case "deleted author":
					raw["author"] = map[string]any{}
				case "author type":
					raw["author"].(map[string]any)["__typename"] = "Organization"
				case "author id":
					raw["author"].(map[string]any)["id"] = ""
				case "author login":
					raw["author"].(map[string]any)["login"] = "bad login"
				case "scheme":
					raw["url"] = "http://github.com/org/repo/issues/9#issuecomment-1"
				case "query":
					raw["url"] = raw["url"].(string) + "?secret"
				case "fragment":
					raw["url"] = strings.ReplaceAll(raw["url"].(string), "issuecomment-1", "discussion_r1")
				case "resource":
					raw["url"] = strings.ReplaceAll(raw["url"].(string), "/9#", "/10#")
				case "repository":
					raw["url"] = strings.ReplaceAll(raw["url"].(string), "/org/", "/other/")
				case "surface":
					raw["url"] = "https://github.com/org/repo/pull/9/files#issuecomment-1"
				case "userinfo":
					raw["url"] = "https://user@github.com/org/repo/issues/9#issuecomment-1"
				}
				b, _ := json.Marshal(raw)
				var w wireComment
				if e := json.Unmarshal(b, &w); e != nil {
					t.Fatal(e)
				}
				c, e := w.normalized(commentResource(kind))
				if variant == "valid" {
					if e != nil || c.ID != "C" || c.DatabaseID != 1 || c.Resource.Kind != kind {
						t.Fatal(c, e)
					}
				} else {
					category(t, e, Malformed)
				}
			})
		}
	}
}
func TestCommentPaginationAndExactOperations(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		t.Run(kind, func(t *testing.T) {
			pages, preflights := 0, 0
			r := commentResource(kind)
			c := mutationAdapter(t, func(q request) any {
				if strings.Contains(q.Query, "repository(owner:") {
					preflights++
					field := "issue"
					if kind == "pull_request" {
						field = "pullRequest"
					}
					if !strings.Contains(q.Query, "result:"+field+"(number:$number)") {
						t.Fatal(q)
					}
					return map[string]any{"repository": map[string]any{"result": nativeJSON("I", "OPEN")}}
				}
				pages++
				typ := "Issue"
				if kind == "pull_request" {
					typ = "PullRequest"
				}
				want := `query($id:ID!,$cursor:String){node(id:$id){__typename ... on ` + typ + `{` + identitySelection + ` comments(first:100,after:$cursor){totalCount nodes{` + commentSelection + `} pageInfo{hasNextPage endCursor}}}}}`
				cursor := any(nil)
				if pages == 2 {
					cursor = "next"
				}
				if q.Query != want || !reflect.DeepEqual(q.Variables, map[string]any{"id": "I", "cursor": cursor}) {
					t.Fatal(q)
				}
				return commentPage(kind, []any{commentJSON(kind, fmt.Sprintf("C%d", pages), pages, "body")}, 2, pages == 1, "next")
			})
			list, e := c.IssueComments(context.Background(), r)
			if e != nil || !list.Complete || len(list.Comments) != 2 || pages != 2 || preflights != 2 {
				t.Fatal(list, e, pages, preflights)
			}
		})
	}
}
func TestCommentPaginationRejectsPartialOrContradictoryEvidence(t *testing.T) {
	for _, variant := range []string{"missing page", "null next", "empty cursor", "cycle", "zero continuation", "inconsistent total", "excess nodes", "incomplete final", "negative total", "excess total", "excess page", "duplicate node", "duplicate database", "wrong parent", "wrong type", "missing node", "missing connection", "null nodes", "cancel between pages"} {
		t.Run(variant, func(t *testing.T) {
			pages := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := mutationAdapter(t, func(q request) any {
				if strings.Contains(q.Query, "repository(owner:") {
					return map[string]any{"repository": map[string]any{"result": nativeJSON("I", "OPEN")}}
				}
				pages++
				out := commentPage("issue", []any{commentJSON("issue", fmt.Sprintf("C%d", pages), pages, "body")}, 3, true, "cursor")
				node := out["node"].(map[string]any)
				p := node["comments"].(map[string]any)
				switch variant {
				case "missing page":
					delete(p, "pageInfo")
				case "null next":
					p["pageInfo"] = map[string]any{"hasNextPage": nil}
				case "empty cursor":
					p["pageInfo"].(map[string]any)["endCursor"] = ""
				case "cycle":
				case "zero continuation":
					p["nodes"] = []any{}
				case "inconsistent total":
					if pages == 2 {
						p["totalCount"] = 4
					}
				case "excess nodes":
					p["totalCount"] = 0
				case "incomplete final":
					p["pageInfo"].(map[string]any)["hasNextPage"] = false
				case "negative total":
					p["totalCount"] = -1
				case "excess total":
					p["totalCount"] = 100001
				case "excess page":
					p["nodes"] = make([]any, 101)
				case "duplicate node", "duplicate database":
					p["totalCount"] = 2
					if pages == 2 {
						p["pageInfo"].(map[string]any)["hasNextPage"] = false
						if variant == "duplicate node" {
							p["nodes"] = []any{commentJSON("issue", "C1", 2, "body")}
						}
					} else if variant == "duplicate database" {
						p["nodes"] = []any{commentJSON("issue", "C1", 2, "body")}
					}
				case "wrong parent":
					node["number"] = 10
				case "wrong type":
					node["__typename"] = "PullRequest"
				case "missing node":
					out["node"] = nil
				case "missing connection":
					node["comments"] = nil
				case "null nodes":
					p["nodes"] = nil
				case "cancel between pages":
					cancel()
				}
				return out
			})
			list, e := c.IssueComments(ctx, commentResource("issue"))
			if e == nil || list.Complete || len(list.Comments) != 0 {
				t.Fatal("partial evidence escaped", list, e)
			}
			if variant == "cancel between pages" {
				if !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
			} else {
				var typed *Error
				if !errors.As(e, &typed) {
					t.Fatal(e)
				}
			}
		})
	}
}
func TestCommentLookupAndMutationReceipts(t *testing.T) {
	for _, op := range []string{"lookup", "create", "update"} {
		for _, variant := range []string{"valid", "missing", "wrong id", "wrong body", "wrong author", "preflight changed"} {
			t.Run(op+"/"+variant, func(t *testing.T) {
				r := commentResource("pull_request")
				writes := 0
				raw := commentJSON(r.Kind, "C", 1, "old")
				b, _ := json.Marshal(raw)
				var w wireComment
				_ = json.Unmarshal(b, &w)
				existing, e := w.normalized(r)
				if e != nil {
					t.Fatal(e)
				}
				c := mutationAdapter(t, func(q request) any {
					if strings.Contains(q.Query, "repository(owner:") {
						return map[string]any{"repository": map[string]any{"result": nativeJSON("I", "OPEN")}}
					}
					if !strings.Contains(q.Query, "mutation") {
						want := `query($id:ID!){node(id:$id){... on IssueComment{` + commentSelection + `}}}`
						if q.Query != want || !reflect.DeepEqual(q.Variables, map[string]any{"id": "C"}) {
							t.Fatal(q)
						}
						v := commentJSON(r.Kind, "C", 1, "old")
						if op == "update" {
							if variant == "preflight changed" {
								v["body"] = "edited"
							}
							return map[string]any{"node": v}
						}
						switch variant {
						case "missing":
							return map[string]any{"node": nil}
						case "wrong id":
							v["id"] = "OTHER"
						case "wrong author":
							v["author"] = nil
						}
						return map[string]any{"node": v}
					}
					writes++
					input := map[string]any{"subjectId": "I", "body": "new"}
					want := `mutation($input:AddCommentInput!){result:addComment(input:$input){commentEdge{node{` + commentSelection + `}}}}`
					if op == "update" {
						input = map[string]any{"id": "C", "body": "new"}
						want = `mutation($input:UpdateIssueCommentInput!){result:updateIssueComment(input:$input){issueComment{` + commentSelection + `}}}`
					}
					if q.Query != want || !reflect.DeepEqual(q.Variables, map[string]any{"input": input}) {
						t.Fatal(q)
					}
					v := commentJSON(r.Kind, "C", 1, "new")
					switch variant {
					case "missing":
						return map[string]any{}
					case "wrong id":
						v["id"] = "OTHER"
					case "wrong body":
						v["body"] = "wrong"
					case "wrong author":
						v["author"] = nil
					}
					if op == "create" {
						return map[string]any{"result": map[string]any{"commentEdge": map[string]any{"node": v}}}
					}
					return map[string]any{"result": map[string]any{"issueComment": v}}
				})
				switch op {
				case "lookup":
					_, e = c.IssueComment(context.Background(), r, "C")
				case "create":
					_, e = c.CreateIssueComment(context.Background(), r, "new")
				case "update":
					_, e = c.UpdateIssueComment(context.Background(), existing, "new")
				}
				bad := variant == "missing" || variant == "wrong author" || (variant == "wrong id" && op != "create") || (variant == "wrong body" && op != "lookup") || (variant == "preflight changed" && op == "update")
				if bad {
					if e == nil {
						t.Fatal("invalid receipt accepted")
					}
				} else if e != nil {
					t.Fatal(e)
				}
				if variant == "preflight changed" && op == "update" && writes != 0 {
					t.Fatal("mutation after stale preflight")
				}
			})
		}
	}
}
func TestCommentTransportTypedFailuresAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   Category
	}{{401, "secret", Unauthorized}, {403, "secret", Forbidden}, {404, "secret", NotFound}, {409, "secret", Conflict}, {429, "secret", RateLimited}, {503, "secret", Transient}, {200, "{", Malformed}, {200, `{"errors":[{"type":"FORBIDDEN"}]}`, Forbidden}} {
		t.Run(string(tc.want), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c, e := NewTransport(s.URL, s.Client(), TokenFunc(func(context.Context) (string, error) { return "token", nil }))
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.CreateIssueComment(context.Background(), commentResource("issue"), "body")
			category(t, e, tc.want)
			if calls != 1 || strings.Contains(e.Error(), "secret") {
				t.Fatal(calls, e)
			}
		})
	}
	calls := 0
	c := mutationAdapter(t, func(request) any { calls++; return nil })
	for _, body := range []string{"", strings.Repeat("x", 60001)} {
		_, e := c.CreateIssueComment(context.Background(), commentResource("issue"), body)
		category(t, e, Permanent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := c.CreateIssueComment(ctx, commentResource("issue"), "body")
	if !errors.Is(e, context.Canceled) || calls != 0 {
		t.Fatal(e, calls)
	}
}

func TestCommentInvalidResourceAndRequestIdentity(t *testing.T) {
	calls := 0
	c := mutationAdapter(t, func(request) any { calls++; return nil })
	r := commentResource("issue")
	for _, change := range []func(*CommentResource){func(r *CommentResource) { r.Kind = "review" }, func(r *CommentResource) { r.Identity.ID = "" }, func(r *CommentResource) { r.Identity.Number = 0 }, func(r *CommentResource) { r.Identity.Repository.Owner = "bad owner" }, func(r *CommentResource) { r.Identity.Repository.Name = "../repo" }, func(r *CommentResource) { r.Identity.Repository.ID = "" }} {
		bad := r
		change(&bad)
		_, e := c.IssueComments(context.Background(), bad)
		category(t, e, Permanent)
		_, e = c.CreateIssueComment(context.Background(), bad, "body")
		category(t, e, Permanent)
	}
	for _, id := range []string{"", "bad id", "C\n"} {
		_, e := c.IssueComment(context.Background(), r, id)
		category(t, e, Permanent)
	}
	if calls != 0 {
		t.Fatal("invalid evidence sent requests", calls)
	}
}
func TestCommentAuthorStableIdentity(t *testing.T) {
	a := Actor{ID: "U", Login: "developer", Type: "User"}
	b := a
	b.Login = "DEVELOPER"
	if !SameCommentAuthor(a, b) {
		t.Fatal("case equivalent login rejected")
	}
	for _, change := range []func(*Actor){func(a *Actor) { a.ID = "OTHER" }, func(a *Actor) { a.Login = "other" }, func(a *Actor) { a.Type = "Bot" }, func(a *Actor) { a.ID = "" }} {
		b = a
		change(&b)
		if SameCommentAuthor(a, b) {
			t.Fatal("contradictory author accepted", b)
		}
	}
	for _, a := range []Actor{{ID: "U", Login: "user[bot]", Type: "User"}, {ID: "B", Login: "bot[bot]", Type: "Bot"}, {ID: "U", Login: "user", Type: "User"}} {
		want := a.Type == "Bot" || a.Login == "user"
		if ValidCommentAuthor(a) != want {
			t.Fatal(a)
		}
	}
}

func TestCommentBotAuthorRepresentations(t *testing.T) {
	bot := func(id, login string) Actor { return Actor{ID: id, Login: login, Type: "Bot"} }
	rest := bot("SYNTHETIC_BOT", "synthetic-controller[bot]")
	graph := bot(rest.ID, "synthetic-controller")
	for _, tc := range []struct {
		name string
		a, b Actor
		want bool
	}{
		{"cross form", rest, graph, true},
		{"REST form", rest, rest, true},
		{"GraphQL form", graph, graph, true},
		{"REST case", rest, bot(rest.ID, "SYNTHETIC-CONTROLLER[bot]"), true},
		{"GraphQL case", graph, bot(rest.ID, "SYNTHETIC-CONTROLLER"), true},
		{"User case", Actor{ID: "USER", Login: "developer", Type: "User"}, Actor{ID: "USER", Login: "DEVELOPER", Type: "User"}, true},
		{"REST different node", rest, bot("OTHER", rest.Login), false},
		{"GraphQL different node", graph, bot("OTHER", graph.Login), false},
		{"cross form different node", rest, bot("OTHER", graph.Login), false},
		{"node case", rest, bot("synthetic_bot", graph.Login), false},
		{"User impersonation", rest, Actor{ID: rest.ID, Login: graph.Login, Type: "User"}, false},
		{"User bot suffix", graph, Actor{ID: graph.ID, Login: rest.Login, Type: "User"}, false},
		{"Users cross form", Actor{ID: "USER", Login: "developer", Type: "User"}, Actor{ID: "USER", Login: "developer[bot]", Type: "User"}, false},
		{"unrelated", rest, bot(rest.ID, "unrelated"), false},
		{"lookalike", rest, bot(rest.ID, "synthetic-contro11er"), false},
		{"missing node", rest, bot("", graph.Login), false},
		{"malformed node", rest, bot("bad node", graph.Login), false},
		{"both malformed nodes", bot("bad node", rest.Login), bot("bad node", graph.Login), false},
		{"empty login", rest, bot(rest.ID, ""), false},
		{"invalid type", rest, Actor{ID: rest.ID, Login: graph.Login, Type: "Organization"}, false},
		{"arbitrary suffix", graph, bot(rest.ID, graph.Login+"[app]"), false},
		{"partial suffix", graph, bot(rest.ID, graph.Login+"[bot"), false},
		{"malformed suffix", graph, bot(rest.ID, graph.Login+"bot]"), false},
		{"additional suffix", rest, bot(rest.ID, rest.Login+"[bot]"), false},
		{"trailing suffix", graph, bot(rest.ID, rest.Login+"-extra"), false},
		{"cross form case", rest, bot(rest.ID, "SYNTHETIC-CONTROLLER"), false},
		{"suffix case", graph, bot(rest.ID, graph.Login+"[BOT]"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if SameCommentAuthor(tc.a, tc.b) != tc.want || SameCommentAuthor(tc.b, tc.a) != tc.want {
				t.Fatalf("symmetric ownership comparison: %+v %+v want %t", tc.a, tc.b, tc.want)
			}
		})
	}
}

func TestCommentGraphQLBotReadbackPreservesAuthor(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		t.Run(kind, func(t *testing.T) {
			graph := Actor{ID: "SYNTHETIC_BOT_NODE", Login: "synthetic-controller", Type: "Bot"}
			verified := graph
			verified.Login += "[bot]"
			transport := mutationAdapter(t, func(q request) any {
				if strings.Contains(q.Query, "repository(owner:") {
					return map[string]any{"repository": map[string]any{"result": nativeJSON("I", "OPEN")}}
				}
				raw := commentJSON(kind, "C", 1, "body")
				raw["author"] = map[string]any{"__typename": graph.Type, "id": graph.ID, "login": graph.Login}
				return map[string]any{"node": raw}
			})
			comment, err := transport.IssueComment(context.Background(), commentResource(kind), "C")
			if err != nil || comment.Author != graph || !SameCommentAuthor(verified, comment.Author) {
				t.Fatal("provider author changed or verified ownership rejected", comment, err)
			}
		})
	}
}
