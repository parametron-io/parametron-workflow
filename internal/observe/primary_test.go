package observe

import (
	"context"
	"errors"
	"testing"

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
