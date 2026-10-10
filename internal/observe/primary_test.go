package observe

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func TestPrimaryReaderVerifiesIdentityWithoutProjects(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		t.Run(kind, func(t *testing.T) {
			id := identity(12)
			fake := &github.Fake{IssueFunc: func(context.Context, github.Ref) (github.Issue, error) {
				return github.Issue{Identity: id, Title: "current", Body: "Classification: false"}, nil
			}, PullRequestFunc: func(context.Context, github.Ref) (github.PullRequest, error) {
				return github.PullRequest{Identity: id, Title: "current", Body: "Classification: false"}, nil
			}}
			p := processor(t, fake, func(context.Context, PolicyInput) error { t.Error("primary read called policy"); return nil })
			r := storage.Resource{Owner: "ORG", Repository: "REPO", Kind: kind, Number: 12}
			o, err := p.ReadPrimary(context.Background(), r)
			if err != nil || o.Presence != Present || o.Projects != nil || o.Resource.Owner != "org" {
				t.Fatal(o, err)
			}
			for _, bad := range []github.Identity{{ID: "node", Number: 13, Repository: id.Repository}, {ID: "", Number: 12, Repository: id.Repository}, {ID: "node", Number: 12, Repository: github.Repository{Owner: "other", Name: "Repo", ID: "repo-id"}}, {ID: "node", Number: 12, Repository: github.Repository{Owner: "Org", Name: "other", ID: "repo-id"}}, {ID: "node", Number: 12, Repository: github.Repository{Owner: "Org", Name: "Repo", ID: "wrong-id"}}} {
				id = bad
				if _, err := p.ReadPrimary(context.Background(), r); !errors.Is(err, ErrObservation) {
					t.Fatal("identity mismatch accepted", bad, err)
				}
			}
		})
	}
}

func TestReadCurrentReusesFullObservationWithoutConsumer(t *testing.T) {
	d := deployment()
	d.IssueTypes = map[string]string{"Task": "TASK"}
	d.Engineering.FieldOptions = map[config.FieldRole]map[string]string{config.Priority: {"High": "HIGH"}}
	calls := []string{}
	fake := &github.Fake{IssueFunc: func(context.Context, github.Ref) (github.Issue, error) {
		calls = append(calls, "primary")
		return currentIssue(), nil
	}, ProjectItemsFunc: func(_ context.Context, id, p string, _ map[string]config.FieldKind) ([]github.ProjectItem, error) {
		calls = append(calls, p)
		return nil, nil
	}}
	p, err := NewProcessor(d, fake, consumerFunc(func(context.Context, PolicyInput) error { t.Error("ReadCurrent called policy"); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	d.IssueTypes["Task"] = "CHANGED"
	d.Engineering.FieldOptions[config.Priority]["High"] = "CHANGED"
	if p.deployment.IssueTypes["Task"] != "TASK" || p.deployment.Engineering.FieldOptions[config.Priority]["High"] != "HIGH" {
		t.Fatal("aliased discovery maps")
	}
	o, err := p.ReadCurrent(context.Background(), storage.Resource{Owner: "org", Repository: "repo", Kind: "issue", Number: 12, NodeID: "stale"})
	if err != nil || len(o.Projects) != 2 || o.Resource.NodeID != "current-node" || !reflect.DeepEqual(calls, []string{"primary", d.Engineering.ID, d.BugTracker.ID}) {
		t.Fatal(o, err, calls)
	}
}
