package github

import (
	"context"
	"errors"
	"github.com/parametron-io/parametron-workflow/internal/config"
	"testing"
)

func TestFake(t *testing.T) {
	ctx := context.Background()
	ref := Ref{"org", "repo", 9}
	injected := errors.New("injected failure")
	calls := []string{}
	f := &Fake{
		IssueFunc: func(_ context.Context, r Ref) (Issue, error) {
			if r != ref {
				t.Fatal(r)
			}
			calls = append(calls, "issue")
			return Issue{Identity: Identity{ID: "I"}}, nil
		},
		PullRequestFunc: func(_ context.Context, r Ref) (PullRequest, error) {
			if r != ref {
				t.Fatal(r)
			}
			calls = append(calls, "pr")
			return PullRequest{Identity: Identity{ID: "PR"}}, nil
		},
		DiscoverSchemaFunc: func(_ context.Context, s config.SourceConfig) (config.Schema, error) {
			if s.Organization != "org" {
				t.Fatal(s)
			}
			calls = append(calls, "schema")
			return config.Schema{Organizations: []config.Organization{{ID: "O", Login: "org"}}}, nil
		},
		RepositoryFunc: func(context.Context, string, string) (Repository, error) { return Repository{}, injected },
		ProjectItemsFunc: func(context.Context, string, string, map[string]config.FieldKind) ([]ProjectItem, error) {
			return []ProjectItem{{ID: "item"}}, nil
		},
	}
	for range 2 {
		i, err := f.Issue(ctx, ref)
		if err != nil || i.ID != "I" {
			t.Fatal(i, err)
		}
		p, err := f.PullRequest(ctx, ref)
		if err != nil || p.ID != "PR" {
			t.Fatal(p, err)
		}
		s, err := f.DiscoverSchema(ctx, sourceConfig())
		if err != nil || s.Organizations[0].ID != "O" {
			t.Fatal(s, err)
		}
	}
	if len(calls) != 6 {
		t.Fatal(calls)
	}
	_, err := f.Repository(ctx, "org", "repo")
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	items, err := f.ProjectItems(ctx, "I", "P", nil)
	if err != nil || items[0].ID != "item" {
		t.Fatal(items, err)
	}
	f = &Fake{}
	_, err = f.Issue(ctx, ref)
	category(t, err, Permanent)
	_, err = f.PullRequest(ctx, ref)
	category(t, err, Permanent)
	_, err = f.DiscoverSchema(ctx, sourceConfig())
	category(t, err, Permanent)
	_, err = f.Repository(ctx, "org", "repo")
	category(t, err, Permanent)
	_, err = f.ProjectItems(ctx, "I", "P", nil)
	category(t, err, Permanent)
}
