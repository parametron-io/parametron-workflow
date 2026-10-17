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
	"time"
)

// These fixtures are GraphQL wire objects, not typed closure evidence. Only the
// production IssueClosure method constructs the evidence asserted below.
var closureWireTime = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)

func closureWireActor() map[string]any {
	return map[string]any{"__typename": "User", "id": "OPERATOR", "login": "operator"}
}

func closureWireIssue() map[string]any {
	i := identityJSON("I", 9)
	i["__typename"] = "Issue"
	return i
}

func closureWireComment(id string, database int) map[string]any {
	return map[string]any{
		"__typename": "IssueComment", "id": id, "databaseId": database,
		"body": "Force-Close: explicit exception", "createdAt": closureWireTime,
		"updatedAt": closureWireTime, "lastEditedAt": nil,
		"author": closureWireActor(), "editor": nil, "issue": identityJSON("I", 9),
	}
}

func closureWireEvent(id, kind string) map[string]any {
	e := map[string]any{"__typename": kind, "id": id, "createdAt": closureWireTime,
		"actor": closureWireActor(), "closable": closureWireIssue()}
	if kind == "ClosedEvent" {
		e["closer"] = nil
	}
	return e
}

func closureWireEdges(nodes ...map[string]any) []map[string]any {
	edges := make([]map[string]any, len(nodes))
	for n, node := range nodes {
		edges[n] = map[string]any{"cursor": fmt.Sprintf("cursor-%03d", n), "node": node}
	}
	return edges
}

func closureWirePair() []map[string]any {
	return closureWireEdges(closureWireComment("COMMENT", 71), closureWireEvent("CLOSE", "ClosedEvent"))
}

// Put the command at edge 100 and the close at edge 101. A real first:100
// connection therefore needs two pages; no short-page pagination is invented.
func closureWirePaginated() []map[string]any {
	nodes := make([]map[string]any, 0, 101)
	for n := 0; n < 99; n++ {
		// The production selection requests only typename/id for opaque events.
		nodes = append(nodes, map[string]any{"__typename": "LabeledEvent", "id": fmt.Sprintf("LABEL-%03d", n)})
	}
	nodes = append(nodes, closureWireComment("COMMENT", 71), closureWireEvent("CLOSE", "ClosedEvent"))
	return closureWireEdges(nodes...)
}

func closureWirePage(edges []map[string]any, total int, next bool) map[string]any {
	start, end := "", ""
	if len(edges) != 0 {
		start = edges[0]["cursor"].(string)
		end = edges[len(edges)-1]["cursor"].(string)
	}
	return map[string]any{"totalCount": total, "updatedAt": closureWireTime,
		"pageInfo": map[string]any{"hasNextPage": next, "startCursor": start, "endCursor": end}, "edges": edges}
}

type closureWireFixture struct {
	t        *testing.T
	edges    []map[string]any
	state    string
	reads    int
	pages    int
	pass     int
	offset   int
	mutate   func(pass, page int, node, connection map[string]any)
	onNative func(read int, node map[string]any)
}

func (f *closureWireFixture) respond(q request) any {
	f.t.Helper()
	if strings.Contains(q.Query, "result:issue(number:$number)") {
		f.reads++
		if !reflect.DeepEqual(q.Variables, map[string]any{"owner": "org", "name": "repo", "number": float64(9)}) {
			f.t.Errorf("native identity arguments: %#v", q.Variables)
		}
		node := nativeJSON("I", f.state)
		if f.onNative != nil {
			f.onNative(f.reads, node)
		}
		return map[string]any{"repository": map[string]any{"result": node}}
	}
	if !strings.Contains(q.Query, "node(id:$id)") || !strings.Contains(q.Query, "timelineItems(first:100,after:$cursor)") || strings.Contains(q.Query, "itemTypes:") || q.Variables["id"] != "I" {
		f.t.Errorf("unexpected closure query: %#v", q)
		return nil
	}
	for _, selection := range []string{"totalCount updatedAt", "startCursor", "edges{cursor node{", "lastEditedAt", "editor{", "closer{", "closable{"} {
		if !strings.Contains(q.Query, selection) {
			f.t.Errorf("missing closure selection %q", selection)
		}
	}
	cursor, supplied := q.Variables["cursor"]
	if !supplied {
		f.t.Error("initial cursor argument omitted")
	}
	if cursor == nil {
		f.pass++
		f.offset = 0
		if f.reads != 2*f.pass-1 {
			f.t.Errorf("acquisition %d not bracketed by native reads: %d", f.pass, f.reads)
		}
	} else if f.offset == 0 || cursor != f.edges[f.offset-1]["cursor"] {
		f.t.Errorf("unexpected continuation cursor: %v at offset %d", cursor, f.offset)
		return nil
	}
	f.pages++
	end := min(f.offset+100, len(f.edges))
	connection := closureWirePage(f.edges[f.offset:end], len(f.edges), end < len(f.edges))
	page := f.offset/100 + 1
	f.offset = end
	node := closureWireIssue()
	node["state"], node["createdAt"], node["timelineItems"] = f.state, closureWireTime.Add(-time.Hour), connection
	if f.mutate != nil {
		f.mutate(f.pass, page, node, connection)
	}
	return map[string]any{"node": node}
}

func closureWireClient(t *testing.T, edges []map[string]any, state string) (*Transport, *closureWireFixture) {
	t.Helper()
	f := &closureWireFixture{t: t, edges: edges, state: state}
	return adapter(t, f.respond), f
}

func assertClosureWireFailure(t *testing.T, got IssueClosure, err, want error) {
	t.Helper()
	if err == nil || want != nil && !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if !reflect.DeepEqual(got, IssueClosure{}) {
		t.Fatalf("failure leaked closure evidence: %+v", got)
	}
}

func TestIssueClosureTransportComplete(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		t.Run(fmt.Sprintf("paginated=%v", paginated), func(t *testing.T) {
			edges := closureWirePair()
			if paginated {
				edges = closureWirePaginated()
			}
			c, f := closureWireClient(t, edges, "CLOSED")
			got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
			if err != nil || got.Validate(nativeTestIdentity(), "CLOSED") != nil {
				t.Fatal("complete observation rejected", err)
			}
			if got.Issue != nativeTestIdentity() || got.State != "CLOSED" || !got.Complete || got.Order != ClosureCursorOrder || !got.CreatedAt.Equal(closureWireTime.Add(-time.Hour)) || !got.UpdatedAt.Equal(closureWireTime) || len(got.Items) != len(edges) {
				t.Fatalf("incorrect normalized envelope: %+v", got)
			}
			if f.reads != 4 || f.pass != 2 || f.pages != map[bool]int{false: 2, true: 4}[paginated] {
				t.Fatal("missing complete independent acquisitions", f.reads, f.pass, f.pages)
			}
			for n, item := range got.Items {
				node := edges[n]["node"].(map[string]any)
				if item.ID != node["id"] || item.Cursor != edges[n]["cursor"] || item.Kind != node["__typename"] || item.Issue != got.Issue {
					t.Fatalf("connection sequence changed at %d: %+v", n, item)
				}
			}
			comment, close := got.Items[len(got.Items)-2], got.Items[len(got.Items)-1]
			wantActor := &Actor{ID: "OPERATOR", Login: "operator", Type: "User"}
			wantComment := &ClosureComment{ID: "COMMENT", DatabaseID: 71, Issue: nativeTestIdentity(), Body: "Force-Close: explicit exception", Author: wantActor, CreatedAt: closureWireTime, UpdatedAt: closureWireTime}
			if !reflect.DeepEqual(comment.Comment, wantComment) || !reflect.DeepEqual(close.Actor, wantActor) || !comment.CreatedAt.Equal(close.CreatedAt) || close.ID != "CLOSE" || close.CloserID != "" || close.CloserKind != "" {
				t.Fatal("same-second adjacent facts changed", comment, close)
			}
			latest, boundary, index, ok := got.LatestClose()
			if !ok || latest.ID != "CLOSE" || boundary.ID != "" || index != len(edges)-1 {
				t.Fatal("latest close lost", latest, boundary, index, ok)
			}
		})
	}
}

func TestIssueClosureTransportPreservesOpaqueAndCycles(t *testing.T) {
	for _, name := range []string{"intervening", "cycles", "missing-command", "open-comment-only", "empty-open"} {
		t.Run(name, func(t *testing.T) {
			pair := closureWirePair()
			nodes := []map[string]any{pair[0]["node"].(map[string]any), pair[1]["node"].(map[string]any)}
			state := "CLOSED"
			switch name {
			case "intervening":
				nodes = []map[string]any{nodes[0], {"__typename": "LabeledEvent", "id": "INTERVENING"}, nodes[1]}
			case "cycles":
				nodes = append(nodes, closureWireEvent("REOPEN", "ReopenedEvent"), closureWireComment("NEW-COMMENT", 72), closureWireEvent("NEW-CLOSE", "ClosedEvent"))
			case "missing-command":
				nodes = nodes[1:]
			case "open-comment-only":
				nodes, state = nodes[:1], "OPEN"
			case "empty-open":
				nodes, state = nil, "OPEN"
			}
			edges := closureWireEdges(nodes...)
			c, _ := closureWireClient(t, edges, state)
			got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
			if err != nil || !got.Complete || len(got.Items) != len(nodes) {
				t.Fatal("valid observation rejected", err, got)
			}
			for n, item := range got.Items {
				if item.ID != nodes[n]["id"] || item.Kind != nodes[n]["__typename"] || item.Cursor != edges[n]["cursor"] {
					t.Fatal("native sequence changed", n, item)
				}
			}
			close, boundary, index, ok := got.LatestClose()
			switch name {
			case "intervening":
				if !ok || index != 2 || got.Items[index-1].Kind != "LabeledEvent" || got.Items[1].Comment != nil || got.Items[1].Actor != nil {
					t.Fatal("opaque event was lost or false adjacency created", got.Items)
				}
			case "cycles":
				if !ok || close.ID != "NEW-CLOSE" || boundary.ID != "REOPEN" || index != 4 || got.Items[index-1].Comment.ID != "NEW-COMMENT" || got.Items[index-1].Comment.DatabaseID != 72 {
					t.Fatal("closure cycles conflated", got.Items)
				}
				if boundary.Issue != got.Issue || !boundary.CreatedAt.Equal(closureWireTime) || boundary.Actor == nil || *boundary.Actor != (Actor{ID: "OPERATOR", Login: "operator", Type: "User"}) {
					t.Fatal("reopen identity/actor/time changed", boundary)
				}
			case "missing-command":
				if !ok || index != 0 || got.Items[0].Comment != nil {
					t.Fatal("missing command fabricated", got.Items)
				}
			default:
				if ok {
					t.Fatal("comment/empty timeline manufactured a close", got.Items)
				}
			}
		})
	}
}

func TestIssueClosureTransportCommentAndCloserDecoding(t *testing.T) {
	for _, name := range []string{"edited", "bot", "nullable-actors", "pull-request-closer", "commit-closer"} {
		t.Run(name, func(t *testing.T) {
			edges := closureWirePair()
			comment, close := edges[0]["node"].(map[string]any), edges[1]["node"].(map[string]any)
			switch name {
			case "edited":
				comment["body"] = "Force-Close: current edited reason"
				comment["updatedAt"], comment["lastEditedAt"] = closureWireTime.Add(time.Second), closureWireTime.Add(time.Second)
				comment["editor"] = map[string]any{"__typename": "User", "id": "EDITOR", "login": "editor"}
			case "bot":
				comment["author"], close["actor"] = map[string]any{"__typename": "Bot", "id": "BOT", "login": "controller[bot]"}, map[string]any{"__typename": "Bot", "id": "BOT", "login": "controller[bot]"}
			case "nullable-actors":
				comment["author"], close["actor"] = nil, nil
			case "pull-request-closer":
				close["closer"] = map[string]any{"__typename": "PullRequest", "id": "IMPLEMENTATION"}
			case "commit-closer":
				close["closer"] = map[string]any{"__typename": "Commit", "id": "IMPLEMENTATION"}
			}
			c, _ := closureWireClient(t, edges, "CLOSED")
			got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
			if err != nil {
				t.Fatal(err)
			}
			p, e := got.Items[0].Comment, got.Items[1]
			if p.ID != "COMMENT" || p.DatabaseID != 71 || p.Body != comment["body"] || p.Issue != got.Issue || e.Issue != got.Issue || e.ID != "CLOSE" {
				t.Fatal("comment/event identity or current body changed", got)
			}
			switch name {
			case "edited":
				if p.LastEditedAt == nil || !p.LastEditedAt.Equal(closureWireTime.Add(time.Second)) || !p.UpdatedAt.Equal(*p.LastEditedAt) || p.Editor == nil || *p.Editor != (Actor{ID: "EDITOR", Login: "editor", Type: "User"}) {
					t.Fatal("editing evidence erased", p)
				}
			case "bot":
				if p.Author == nil || *p.Author != (Actor{ID: "BOT", Login: "controller[bot]", Type: "Bot"}) || !reflect.DeepEqual(p.Author, e.Actor) {
					t.Fatal("Bot identity changed", got)
				}
			case "nullable-actors":
				if p.Author != nil || e.Actor != nil || p.Editor != nil {
					t.Fatal("missing actors inferred", got)
				}
			default:
				want := close["closer"].(map[string]any)
				if e.CloserID != want["id"] || e.CloserKind != want["__typename"] {
					t.Fatal("native closer erased", e)
				}
			}
		})
	}
}

func TestIssueClosureTransportRejectsInvalidEvidence(t *testing.T) {
	for _, name := range []string{
		"missing-count", "negative-count", "over-limit-count", "final-count", "missing-page-info", "missing-next",
		"wrong-start", "wrong-end", "empty-continuation", "duplicate-cursor", "duplicate-node", "missing-node-id",
		"missing-kind", "null-node", "wrong-issue", "wrong-repository", "wrong-number", "missing-creation", "missing-update",
		"missing-connection", "missing-body", "missing-comment-issue", "wrong-comment-issue", "wrong-comment-repository",
		"missing-database", "missing-comment-time", "invalid-comment-time", "missing-author-id", "malformed-editor",
		"missing-event-time", "missing-closable", "wrong-closable", "malformed-actor", "unsupported-actor",
		"missing-closer-id", "unsupported-closer", "duplicate-database", "native-state-disagreement",
	} {
		t.Run(name, func(t *testing.T) {
			edges := closureWirePair()
			c, f := closureWireClient(t, edges, "CLOSED")
			want := error(ErrClosureIncomplete)
			if name == "missing-author-id" || name == "malformed-editor" || name == "malformed-actor" {
				want = nil // These are the shared decoder's typed Malformed errors.
			}
			if name == "native-state-disagreement" {
				want = ErrClosureChanged
			}
			f.mutate = func(_, _ int, node, p map[string]any) {
				comment, close := edges[0]["node"].(map[string]any), edges[1]["node"].(map[string]any)
				info := p["pageInfo"].(map[string]any)
				switch name {
				case "missing-count":
					delete(p, "totalCount")
				case "negative-count":
					p["totalCount"] = -1
				case "over-limit-count":
					p["totalCount"] = MaxClosureItems + 1
				case "final-count":
					p["totalCount"] = 3
				case "missing-page-info":
					delete(p, "pageInfo")
				case "missing-next":
					delete(info, "hasNextPage")
				case "wrong-start":
					info["startCursor"] = "WRONG"
				case "wrong-end":
					info["endCursor"] = "WRONG"
				case "empty-continuation":
					p["edges"] = []any{}
					info["hasNextPage"] = true
				case "duplicate-cursor":
					edges[1]["cursor"] = edges[0]["cursor"]
					info["endCursor"] = edges[0]["cursor"]
				case "duplicate-node":
					close["id"] = comment["id"]
				case "missing-node-id":
					delete(close, "id")
				case "missing-kind":
					delete(close, "__typename")
				case "null-node":
					edges[0]["node"] = nil
				case "wrong-issue":
					node["id"] = "OTHER"
				case "wrong-repository":
					node["repository"] = map[string]any{"id": "OTHER", "name": "repo", "owner": map[string]any{"login": "org"}}
				case "wrong-number":
					node["number"] = 10
				case "missing-creation":
					delete(node, "createdAt")
				case "missing-update":
					delete(p, "updatedAt")
				case "missing-connection":
					delete(node, "timelineItems")
				case "missing-body":
					delete(comment, "body")
				case "missing-comment-issue":
					delete(comment, "issue")
				case "wrong-comment-issue":
					comment["issue"] = identityJSON("OTHER", 9)
				case "wrong-comment-repository":
					comment["issue"].(map[string]any)["repository"].(map[string]any)["id"] = "OTHER"
				case "missing-database":
					delete(comment, "databaseId")
				case "missing-comment-time":
					delete(comment, "createdAt")
				case "invalid-comment-time":
					comment["updatedAt"] = "not-a-time"
				case "missing-author-id":
					delete(comment["author"].(map[string]any), "id")
				case "malformed-editor":
					comment["editor"] = map[string]any{"__typename": "User", "login": "editor"}
				case "missing-event-time":
					delete(close, "createdAt")
				case "missing-closable":
					delete(close, "closable")
				case "wrong-closable":
					close["closable"] = identityJSON("OTHER", 9)
				case "malformed-actor":
					close["actor"] = map[string]any{"__typename": "User", "id": "USER"}
				case "unsupported-actor":
					close["actor"] = map[string]any{"__typename": "Organization", "id": "ORG", "login": "org"}
				case "missing-closer-id":
					close["closer"] = map[string]any{"__typename": "PullRequest"}
				case "unsupported-closer":
					close["closer"] = map[string]any{"__typename": "Issue", "id": "OTHER"}
				case "duplicate-database":
					other := closureWireComment("SECOND-COMMENT", 71)
					p["edges"] = closureWireEdges(comment, other, close)
					p["totalCount"] = 3
					info["endCursor"] = "cursor-002"
				case "native-state-disagreement":
					p["edges"] = closureWireEdges(comment)
					p["totalCount"] = 1
					info["endCursor"] = "cursor-000"
				}
			}
			got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
			assertClosureWireFailure(t, got, err, want)
			if want == nil {
				category(t, err, Malformed)
			}
			if f.pass > 1 {
				t.Fatal("invalid first acquisition reached second pass")
			}
		})
	}
}

func TestIssueClosureTransportBetweenPageChanges(t *testing.T) {
	for _, name := range []string{"count", "updated", "created", "identity", "state", "repeated-cursor", "duplicate-node", "empty-next", "truncated", "wrong-typename"} {
		t.Run(name, func(t *testing.T) {
			edges := closureWirePaginated()
			c, f := closureWireClient(t, edges, "CLOSED")
			want := error(ErrClosureIncomplete)
			if name == "count" || name == "updated" || name == "created" {
				want = ErrClosureChanged
			}
			f.mutate = func(_, page int, node, p map[string]any) {
				if page != 2 {
					return
				}
				info := p["pageInfo"].(map[string]any)
				switch name {
				case "count":
					p["totalCount"] = len(edges) + 1
				case "updated":
					p["updatedAt"] = closureWireTime.Add(time.Second)
				case "created":
					node["createdAt"] = closureWireTime.Add(-2 * time.Hour)
				case "identity":
					node["id"] = "OTHER"
				case "state":
					node["state"] = "OPEN"
				case "repeated-cursor":
					p["edges"] = []map[string]any{{"cursor": edges[99]["cursor"], "node": edges[100]["node"]}}
					info["startCursor"], info["endCursor"] = edges[99]["cursor"], edges[99]["cursor"]
				case "duplicate-node":
					edges[100]["node"].(map[string]any)["id"] = "COMMENT"
				case "empty-next":
					p["edges"] = []any{}
					info["hasNextPage"] = true
				case "truncated":
					p["edges"] = []any{}
				case "wrong-typename":
					// Non-Issue nodes cannot supply fields selected inside ... on Issue.
					for k := range node {
						delete(node, k)
					}
					node["__typename"] = "PullRequest"
				}
			}
			got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
			assertClosureWireFailure(t, got, err, want)
			if f.pages != 2 || f.pass != 1 {
				t.Fatal("did not reach second page", f.pages, f.pass)
			}
		})
	}
}

func TestIssueClosureTransportTwoPassStability(t *testing.T) {
	for _, name := range []string{"body", "edit-time", "actor", "comment-id", "event-id", "cursor", "cursor-order", "insertion", "deletion", "metadata"} {
		t.Run(name, func(t *testing.T) {
			edges := closureWirePair()
			c, f := closureWireClient(t, edges, "CLOSED")
			f.mutate = func(pass, _ int, _, p map[string]any) {
				if pass != 2 {
					return
				}
				comment, close := edges[0]["node"].(map[string]any), edges[1]["node"].(map[string]any)
				info := p["pageInfo"].(map[string]any)
				switch name {
				case "body":
					comment["body"] = "Force-Close: changed current reason"
				case "edit-time":
					comment["lastEditedAt"], comment["updatedAt"] = closureWireTime.Add(time.Second), closureWireTime.Add(time.Second)
				case "actor":
					close["actor"] = map[string]any{"__typename": "User", "id": "OTHER", "login": "other"}
				case "comment-id":
					comment["id"] = "OTHER-COMMENT"
				case "event-id":
					close["id"] = "OTHER-CLOSE"
				case "cursor":
					edges[0]["cursor"], edges[1]["cursor"] = "changed-comment-cursor", "changed-close-cursor"
					info["startCursor"], info["endCursor"] = edges[0]["cursor"], edges[1]["cursor"]
				case "cursor-order":
					// Equal timestamps must not make a reversed server sequence
					// compare equal through local timestamp/ID sorting.
					p["edges"] = []map[string]any{edges[1], edges[0]}
					info["startCursor"], info["endCursor"] = edges[1]["cursor"], edges[0]["cursor"]
				case "insertion":
					p["edges"] = closureWireEdges(comment, map[string]any{"__typename": "LabeledEvent", "id": "INSERTED"}, close)
					p["totalCount"], info["endCursor"] = 3, "cursor-002"
				case "deletion":
					p["edges"] = closureWireEdges(close)
					p["totalCount"], info["endCursor"] = 1, "cursor-000"
				case "metadata":
					p["updatedAt"] = closureWireTime.Add(time.Second)
				}
			}
			got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
			assertClosureWireFailure(t, got, err, ErrClosureChanged)
			if f.pass != 2 || f.pages != 2 || f.reads != 4 {
				t.Fatal("two complete acquisitions not exercised", f.pass, f.pages, f.reads)
			}
		})
	}
}

func TestIssueClosureTransportNativeIdentityBarriers(t *testing.T) {
	for _, name := range []string{"initial-id", "initial-repository", "initial-state", "final-state", "final-id"} {
		t.Run(name, func(t *testing.T) {
			c, f := closureWireClient(t, closureWirePair(), "CLOSED")
			f.onNative = func(read int, node map[string]any) {
				switch {
				case name == "initial-id" && read == 1, name == "final-id" && read == 2:
					node["id"] = "OTHER"
				case name == "initial-repository" && read == 1:
					node["repository"].(map[string]any)["id"] = "OTHER"
				case name == "initial-state" && read == 1:
					node["state"] = "MERGED"
				case name == "final-state" && read == 2:
					node["state"] = "OPEN"
				}
			}
			got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
			if name == "final-state" {
				assertClosureWireFailure(t, got, err, ErrClosureChanged)
			} else {
				assertClosureWireFailure(t, got, err, nil)
				category(t, err, Malformed)
			}
			if f.pass > 1 {
				t.Fatal("invalid native barrier admitted another acquisition")
			}
		})
	}
}

func TestIssueClosureTransportCancellation(t *testing.T) {
	for _, name := range []string{"before", "paginated", "between-acquisitions"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, f := closureWireClient(t, closureWirePaginated(), "CLOSED")
			switch name {
			case "before":
				cancel()
			case "paginated", "between-acquisitions":
				// The first read has two timeline pages and two identity reads.
				// Cancel synchronously before the next page or pass two's first
				// request. No server timing or sleep determines the boundary.
				stop := 5
				if name == "paginated" {
					stop = 3
				}
				requests := 0
				c.tokens = TokenFunc(func(context.Context) (string, error) {
					requests++
					if requests == stop {
						cancel()
						return "", ctx.Err()
					}
					return "synthetic", nil
				})
			}
			got, err := c.IssueClosure(ctx, nativeTestIdentity())
			assertClosureWireFailure(t, got, err, context.Canceled)
			if name == "before" && f.reads+f.pages != 0 {
				t.Fatal("canceled input made request")
			}
			if name == "paginated" && (f.pages != 1 || f.pass != 1) {
				t.Fatal("canceled pagination continued", f.pages, f.pass)
			}
			if name == "between-acquisitions" && (f.reads != 2 || f.pages != 2 || f.pass != 1) {
				t.Fatal("cancellation boundary missed", f.reads, f.pages, f.pass)
			}
		})
	}
}

func TestIssueClosureTransportInFlightCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	f := &closureWireFixture{t: t, edges: closureWirePaginated(), state: "CLOSED"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q request
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.Contains(q.Query, "timelineItems(") && q.Variables["cursor"] != nil {
			close(entered)
			<-r.Context().Done()
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": f.respond(q)}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	c, err := NewTransport(server.URL, server.Client(), TokenFunc(func(context.Context) (string, error) { return "synthetic", nil }))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		got IssueClosure
		err error
	}
	finished := make(chan result, 1)
	go func() { got, err := c.IssueClosure(ctx, nativeTestIdentity()); finished <- result{got, err} }()
	select {
	case <-entered:
		cancel()
	case early := <-finished:
		t.Fatalf("reader returned before in-flight page: %+v, %v", early.got, early.err)
	case <-t.Context().Done():
		t.Fatal("test ended before continuation request")
	}
	resultValue := <-finished
	assertClosureWireFailure(t, resultValue.got, resultValue.err, context.Canceled)
}

func TestIssueClosureTransportProviderFailures(t *testing.T) {
	for _, name := range []string{"http", "malformed-json", "graphql-error", "null-data", "truncated-connection"} {
		t.Run(name, func(t *testing.T) {
			f := &closureWireFixture{t: t, edges: closureWirePair(), state: "CLOSED"}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var q request
				if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if requests == 1 {
					if err := json.NewEncoder(w).Encode(map[string]any{"data": f.respond(q)}); err != nil {
						t.Error(err)
					}
					return
				}
				switch name {
				case "http":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "malformed-json":
					_, _ = w.Write([]byte(`{"data":`))
				case "graphql-error":
					_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED"}],"data":{"node":null}}`))
				case "null-data":
					_, _ = w.Write([]byte(`{"data":null}`))
				case "truncated-connection":
					_, _ = w.Write([]byte(`{"data":{"node":{"__typename":"Issue"}}}`))
				}
			}))
			t.Cleanup(server.Close)
			c, err := NewTransport(server.URL, server.Client(), TokenFunc(func(context.Context) (string, error) { return "synthetic", nil }))
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
			assertClosureWireFailure(t, got, err, nil)
			switch name {
			case "http":
				category(t, err, Transient)
			case "graphql-error":
				category(t, err, RateLimited)
			case "truncated-connection":
				if !errors.Is(err, ErrClosureIncomplete) {
					t.Fatal(err)
				}
			default:
				category(t, err, Malformed)
			}
			if requests != 2 {
				t.Fatal("unexpected retry", requests)
			}
		})
	}
}

func TestIssueClosureTransportObservationBounds(t *testing.T) {
	t.Run("maximum-items", func(t *testing.T) {
		nodes := make([]map[string]any, MaxClosureItems)
		for n := range nodes {
			nodes[n] = map[string]any{"__typename": "LabeledEvent", "id": fmt.Sprintf("OPAQUE-%d", n)}
		}
		c, f := closureWireClient(t, closureWireEdges(nodes...), "OPEN")
		got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
		if err != nil || !got.Complete || len(got.Items) != MaxClosureItems || got.Items[MaxClosureItems-1].ID != fmt.Sprintf("OPAQUE-%d", MaxClosureItems-1) || f.pass != 2 || f.pages != 2*((MaxClosureItems+99)/100) || f.reads != 4 {
			t.Fatal("valid item boundary rejected or truncated", err, len(got.Items), f.pass, f.pages, f.reads)
		}
	})
	t.Run("endless-pages", func(t *testing.T) {
		pages := 0
		c := adapter(t, func(q request) any {
			if strings.Contains(q.Query, "result:issue(") {
				return map[string]any{"repository": map[string]any{"result": nativeJSON("I", "OPEN")}}
			}
			if q.Variables["id"] != "I" || pages == 0 && q.Variables["cursor"] != nil || pages > 0 && q.Variables["cursor"] != fmt.Sprintf("end-%d", pages) {
				t.Error("unexpected request", q)
			}
			pages++
			// Deliberately adversarial sparse continuation: the provider keeps
			// promising more of a bounded total, without ever finishing. This is
			// not a successful pagination fixture; it must hit the page guard.
			edges := []map[string]any{{"cursor": fmt.Sprintf("end-%d", pages), "node": map[string]any{"__typename": "LabeledEvent", "id": fmt.Sprintf("OPAQUE-%d", pages)}}}
			node := closureWireIssue()
			node["state"], node["createdAt"], node["timelineItems"] = "OPEN", closureWireTime.Add(-time.Hour), closureWirePage(edges, MaxClosureItems, true)
			return map[string]any{"node": node}
		})
		got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
		assertClosureWireFailure(t, got, err, ErrClosureLimit)
		if pages != 64 {
			t.Fatal("existing 64-page bound not enforced", pages)
		}
	})
	t.Run("request-budget", func(t *testing.T) {
		c, f := closureWireClient(t, closureWirePaginated(), "CLOSED")
		got, err := c.IssueClosure(WithRequestLimit(context.Background(), 2), nativeTestIdentity())
		assertClosureWireFailure(t, got, err, ErrRequestLimit)
		if f.reads != 1 || f.pages != 1 {
			t.Fatal("parent request budget not enforced", f.reads, f.pages)
		}
	})
	t.Run("oversized-page", func(t *testing.T) {
		edges := closureWirePaginated()
		c, f := closureWireClient(t, edges, "CLOSED")
		f.mutate = func(_, _ int, _, p map[string]any) { p["edges"] = edges }
		got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
		assertClosureWireFailure(t, got, err, ErrClosureLimit)
	})
	t.Run("evidence-bytes", func(t *testing.T) {
		// Each page stays below the shared HTTP 8 MiB limit; the accumulated
		// evidence crosses the closure-specific limit on page three.
		body := strings.Repeat("x", MaxClosureEvidenceBytes/250)
		nodes := make([]map[string]any, 300)
		for n := range nodes {
			nodes[n] = closureWireComment(fmt.Sprintf("COMMENT-%d", n), n+1)
			nodes[n]["body"] = body
		}
		c, f := closureWireClient(t, closureWireEdges(nodes...), "OPEN")
		got, err := c.IssueClosure(context.Background(), nativeTestIdentity())
		assertClosureWireFailure(t, got, err, ErrClosureLimit)
		if f.pages != 3 || f.pass != 1 {
			t.Fatal("evidence limit boundary missed", f.pages, f.pass)
		}
	})
}
