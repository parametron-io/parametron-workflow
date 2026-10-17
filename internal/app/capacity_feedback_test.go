package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	wf "github.com/parametron-io/parametron-workflow/internal/workflowfeedback"
)

type capacityRuntimeClient struct {
	*completionRuntimeClient
	prs []github.PullRequest
}

func (f *capacityRuntimeClient) ListPullRequests(ctx context.Context, repo github.Repository) ([]github.ListedPullRequest, error) {
	if repo != f.repo {
		return nil, fmt.Errorf("unrelated PR enumeration: %v", repo)
	}
	out := []github.ListedPullRequest{{Identity: f.pr.Identity, State: f.pr.State}}
	for _, pr := range f.prs {
		out = append(out, github.ListedPullRequest{Identity: pr.Identity, State: pr.State})
	}
	return out, ctx.Err()
}
func (f *capacityRuntimeClient) CurrentPullRequestProjectItem(ctx context.Context, id string) (github.PullRequestProjectItem, error) {
	for _, pr := range f.prs {
		if f.items[pr.ID].ID == id {
			identity := pr.Identity
			return github.PullRequestProjectItem{ID: id, ProjectID: "engineering", ContentKind: "PullRequest", PullRequest: &identity}, ctx.Err()
		}
	}
	return f.completionRuntimeClient.CurrentPullRequestProjectItem(ctx, id)
}

// Pure capacity tests already prove #48's allocation. This closes the narrower
// composition gap: ten real accepted bindings must reach typed managed feedback
// without a partial numeric acquisition, including on equivalent delivery.
func TestCompanionCapacityFeedbackProductionWorker(t *testing.T) {
	c, base, _ := completionRuntimeFixture(t)
	r := openRuntime(t, c)
	onboardingNotification(t, r, base, "branch", "issues")
	step(t, r)
	state(t, r, "branch", storage.Completed, 1)
	primary := base.items[base.issue.ID].Values["roadmap_order"]
	r.store.Close()
	f := &capacityRuntimeClient{completionRuntimeClient: base}
	for n := 0; n < 9; n++ {
		pr := base.pr
		pr.ID = fmt.Sprintf("EXTRA_PR_%d", n)
		pr.Number = 100 + n
		f.prs = append(f.prs, pr)
		f.items[pr.ID] = github.ProjectItem{ID: fmt.Sprintf("EXTRA_ITEM_%d", n), ProjectID: "engineering", Content: &github.Content{ID: pr.ID, Kind: "PullRequest"}, Values: map[string]github.FieldValue{}}
	}
	read := base.PullRequestFunc
	base.PullRequestFunc = func(ctx context.Context, ref github.Ref) (github.PullRequest, error) {
		for _, pr := range f.prs {
			if ref.Owner == pr.Repository.Owner && ref.Repository == pr.Repository.Name && ref.Number == pr.Number {
				return pr, ctx.Err()
			}
		}
		return read(ctx, ref)
	}
	writes := 0
	f.numberHook = func(ctx context.Context, id string) error {
		if id != "ISSUEITEM" {
			writes++
		}
		return ctx.Err()
	}
	c.Client = f
	r = openRuntime(t, c)
	for _, delivery := range []string{"overflow", "equivalent-overflow"} {
		onboardingNotification(t, r, f, delivery, "pull_request")
		step(t, r)
		e := state(t, r, delivery, storage.Failed, 1)
		if e.State.ErrorCategory != "pr_companion_capacity" {
			t.Fatal(e.State)
		}
		for _, pr := range append([]github.PullRequest{f.pr}, f.prs...) {
			if _, present := f.items[pr.ID].Values["roadmap_order"]; present {
				t.Fatal("partial companion slot written", pr.ID)
			}
			list, err := f.IssueComments(context.Background(), github.CommentResource{Identity: pr.Identity, Kind: "pull_request"})
			if err != nil || len(list.Comments) != 1 || list.Comments[0].Author != runtimeBot || !strings.HasPrefix(list.Comments[0].Body, wf.Marker+"\n") || !strings.Contains(list.Comments[0].Body, "10") {
				t.Fatal("overflow lacks one owned explanation", pr.ID, err)
			}
		}
		if writes != 0 || f.items[f.issue.ID].Values["roadmap_order"] != primary || f.commentCreates != 10 || f.commentUpdates != 0 || f.drafts != 1 || f.adds != 1 {
			t.Fatal("overflow changed primary, replayed ingress, or duplicated feedback", writes, f.commentCreates, f.commentUpdates, f.drafts, f.adds)
		}
	}
}
