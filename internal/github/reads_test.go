package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestPullRequestBindingIdentityNormalization(t *testing.T) {
	for _, tc := range []struct {
		name      string
		head      any
		actor     any
		malformed bool
	}{
		{"exact", repoJSON(), map[string]any{"login": "dev", "id": "U", "type": "User"}, false},
		{"deleted head", nil, map[string]any{"login": "dev", "id": "U", "type": "User"}, false},
		{"fork", map[string]any{"id": "fork", "name": "repo", "owner": map[string]any{"login": "other"}}, map[string]any{"login": "dev", "id": "U", "type": "User"}, false},
		{"Bot preserved", repoJSON(), map[string]any{"login": "bot", "id": "B", "type": "Bot"}, false},
		{"missing author", repoJSON(), nil, false},
		{"missing head node", map[string]any{"name": "repo", "owner": map[string]any{"login": "org"}}, nil, true},
		{"missing head owner", map[string]any{"id": "R", "name": "repo"}, nil, true},
		{"missing head name", map[string]any{"id": "R", "owner": map[string]any{"login": "org"}}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := adapter(t, func(q request) any {
				if strings.Contains(q.Query, "result:labels") {
					return connectionJSON("PullRequest", []any{map[string]any{"name": "workflow"}}, false, "")
				}
				if strings.Contains(q.Query, "result:closingIssuesReferences") {
					return connectionJSON("PullRequest", []any{identityJSON("I", 1)}, false, "")
				}
				if !strings.Contains(q.Query, "author{login ... on Node{id} type:__typename}") || !strings.Contains(q.Query, "headRepository{id name owner{login}}") {
					t.Error("stable identity selection missing", q.Query)
				}
				w := identityJSON("PR", 9)
				for k, v := range map[string]any{"title": "title", "body": "Target: #1", "state": "OPEN", "isDraft": true, "headRefName": "development/issue-1", "headRefOid": strings.Repeat("a", 40), "baseRefName": "main", "baseRefOid": strings.Repeat("b", 40), "author": tc.actor, "headRepository": tc.head} {
					w[k] = v
				}
				return map[string]any{"repository": map[string]any{"result": w}}
			})
			p, err := client.PullRequest(context.Background(), Ref{Owner: "org", Repository: "repo", Number: 9})
			if tc.malformed {
				category(t, err, Malformed)
				if !reflect.DeepEqual(p, PullRequest{}) {
					t.Fatal("partial PR", p)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.ID != "PR" || p.Number != 9 || p.Repository != developerRepo || p.Title != "title" || p.Body != "Target: #1" || p.State != "OPEN" || !p.Draft || p.HeadRef != "development/issue-1" || p.HeadSHA != strings.Repeat("a", 40) || p.BaseRef != "main" || p.BaseSHA != strings.Repeat("b", 40) || !reflect.DeepEqual(p.Labels, []string{"workflow"}) || len(p.ClosingIssues) != 1 {
				t.Fatal("existing fields lost", p)
			}
			if tc.head == nil {
				if p.HeadRepository != (Repository{}) {
					t.Fatal("deleted head invented", p)
				}
			} else {
				w := tc.head.(map[string]any)
				if p.HeadRepository.ID != w["id"] || p.HeadRepository.Owner != w["owner"].(map[string]any)["login"] || p.HeadRepository.Name != w["name"] {
					t.Fatal(p)
				}
			}
			if tc.actor == nil {
				if p.Author != nil {
					t.Fatal(p)
				}
			} else {
				w := tc.actor.(map[string]any)
				if p.Author == nil || p.Author.ID != w["id"] || p.Author.Type != w["type"] || p.Author.Login != w["login"] {
					t.Fatal(p)
				}
			}
		})
	}
}
func TestIssueAssigneeStableIdentityNormalization(t *testing.T) {
	for _, typ := range []string{"User", "Bot", "Organization", "Mannequin"} {
		t.Run(typ, func(t *testing.T) {
			client := adapter(t, func(q request) any {
				if strings.Contains(q.Query, "result:issue(") {
					w := identityJSON("I", 9)
					w["title"] = "title"
					w["body"] = "Classification: false"
					w["state"] = "OPEN"
					return map[string]any{"repository": map[string]any{"result": w}}
				}
				if strings.Contains(q.Query, "result:assignees") {
					if !strings.Contains(q.Query, "login id type:__typename") {
						t.Error("assignee stable identity not queried")
					}
					return connectionJSON("Issue", []any{map[string]any{"login": "dev", "id": "U", "type": typ}}, false, "")
				}
				return connectionJSON("Issue", []any{}, false, "")
			})
			i, err := client.Issue(context.Background(), Ref{Owner: "org", Repository: "repo", Number: 9})
			if err != nil || !reflect.DeepEqual(i.Assignees, []Actor{{ID: "U", Login: "dev", Type: typ}}) || i.Body != "Classification: false" || i.ID != "I" || i.State != "OPEN" {
				t.Fatal(i, err)
			}
			if ValidHuman(i.Assignees[0]) != (typ == "User") {
				t.Fatal("unsupported actor accepted as human")
			}
		})
	}
}
