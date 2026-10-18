package engineeringcontext

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func TestConfiguredOrganizationConstructor(t *testing.T) {
	for _, org := range []string{"parametron-io", "prm-workflow", "PRM-Workflow"} {
		for _, tc := range []string{"valid", "foreign owner", "invalid owner", "duplicate name", "duplicate id", "invalid id", "missing dependency", "invalid organization"} {
			t.Run(org+"/"+tc, func(t *testing.T) {
				f := setup(t, issue(engine, 45, ""))
				c := f.resolver.cfg
				c.Deployment = deployment()
				c.Deployment.Organization.Login = org
				for i := range c.Deployment.Repositories {
					c.Deployment.Repositories[i].Owner = org
				}
				switch tc {
				case "foreign owner":
					c.Deployment.Repositories[0].Owner = "other-org"
				case "invalid owner":
					c.Deployment.Repositories[0].Owner = "bad/owner"
				case "duplicate name":
					r := c.Deployment.Repositories[0]
					r.ID = "OTHER"
					c.Deployment.Repositories = append(c.Deployment.Repositories, r)
				case "duplicate id":
					r := c.Deployment.Repositories[0]
					r.Name = "other"
					c.Deployment.Repositories = append(c.Deployment.Repositories, r)
				case "invalid id":
					c.Deployment.Repositories[0].ID = "bad id"
				case "missing dependency":
					c.Primary = nil
				case "invalid organization":
					c.Deployment.Organization.Login = "bad/owner"
				}
				_, err := New(c)
				if tc == "valid" {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrInvalid) {
					t.Fatalf("expected invalid: %v", err)
				}
			})
		}
	}
}

func TestConfiguredOrganizationGraphReferences(t *testing.T) {
	for _, org := range []string{"parametron-io", "prm-workflow"} {
		for _, tc := range []struct {
			name, body string
			want       error
		}{
			{"configured cross repository", "Blocked-By: " + org + "/workflow_repo_2#7", nil},
			{"unconfigured repository", "Blocked-By: " + org + "/unconfigured#7", ErrDeployment},
			{"foreign organization", "Blocked-By: other-org/workflow_repo_2#7", ErrInvalid},
		} {
			t.Run(org+"/"+tc.name, func(t *testing.T) {
				d := deployment()
				d.Organization.Login = org
				d.Repositories[0].Owner = org
				d.Repositories[0].Name = "workflow_repo_1"
				d.Repositories[1].Owner = org
				d.Repositories[1].Name = "workflow_repo_2"
				issues := []github.Issue{
					{Identity: github.Identity{ID: "I10", Repository: d.Repositories[0], Number: 10}, State: "OPEN", Body: "Classification: false\n" + tc.body, Type: &github.IssueType{ID: d.IssueTypes["Task"], Name: "Task"}},
					{Identity: github.Identity{ID: "I7", Repository: d.Repositories[1], Number: 7}, State: "OPEN", Body: "Classification: false", Type: &github.IssueType{ID: d.IssueTypes["Task"], Name: "Task"}},
				}
				key := func(i github.Issue) storage.Resource {
					return storage.Resource{Owner: org, Repository: i.Repository.Name, Kind: "issue", Number: int64(i.Number), NodeID: i.ID}
				}
				c := Config{Deployment: d, Accepted: snapshotAcceptedFunc(func(context.Context, storage.Resource) (sp.AcceptedIssue, bool, error) {
					return sp.AcceptedIssue{}, false, nil
				})}
				c.Primary = snapshotPrimaryFunc(func(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
					for _, i := range issues {
						if i.Repository.Owner == r.Owner && i.Repository.Name == r.Repository && int64(i.Number) == r.Number {
							return observe.ObservedState{Resource: key(i), Presence: observe.Present, Issue: &i}, ctx.Err()
						}
					}
					t.Fatalf("read outside configured identities: %+v", r)
					return observe.ObservedState{}, nil
				})
				c.Lister = &github.ListerFake{ListIssuesFunc: func(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
					for _, i := range issues {
						if i.Repository == r {
							return []github.ListedIssue{{Identity: i.Identity, State: i.State, Body: i.Body, Type: i.Type}}, ctx.Err()
						}
					}
					t.Fatalf("listed unconfigured repository: %+v", r)
					return nil, nil
				}}
				resolver, err := New(c)
				if err != nil {
					t.Fatal(err)
				}
				root := key(issues[0])
				out, err := resolver.Resolve(context.Background(), root)
				if tc.want != nil {
					if !errors.Is(err, tc.want) || len(out.Nodes) != 0 {
						t.Fatalf("%+v %v", out, err)
					}
					return
				}
				if err != nil || len(out.Nodes) != 2 || len(out.Dependencies) != 1 {
					t.Fatalf("%+v %v", out, err)
				}
				first, _ := json.Marshal(out)
				again, err := resolver.Resolve(context.Background(), root)
				second, _ := json.Marshal(again)
				if err != nil || string(first) != string(second) {
					t.Fatalf("unstable output: %v", err)
				}
				root.Owner = "other-org"
				if _, err := resolver.Resolve(context.Background(), root); !errors.Is(err, ErrDeployment) {
					t.Fatal(err)
				}
			})
		}
	}
}
