package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/prdiscovery"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	wf "github.com/parametron-io/parametron-workflow/internal/workflowfeedback"
)

// Selection is deliberately independent of accepted binding. Concrete discovery
// validates the result's provider evidence; this boundary must still reject an
// ambiguous locator and must never substitute a payload node ID.
func TestContinuousPRTriggerIdentityBoundaries(t *testing.T) {
	repo := github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}
	canonical := storage.Resource{Owner: repo.Owner, Repository: repo.Name, Kind: "pull_request", Number: 47, NodeID: "PR47"}
	for _, name := range []string{"partial", "full", "wrong-node", "other-repository", "other-owner", "other-number", "wrong-number-with-node", "wrong-kind", "duplicate", "conflicting-node", "missing-node", "incomplete", "unstable", "absent", "cross-repository-lookalike", "cross-repository-node", "same-number-other-repository", "rejected-binding", "canceled"} {
		t.Run(name, func(t *testing.T) {
			current := prdiscovery.Result{Repositories: []github.Repository{repo}, Completeness: prdiscovery.Complete, Freshness: prdiscovery.StableReads, Entries: []prdiscovery.Entry{{Resource: canonical}}}
			locator := canonical
			locator.NodeID = ""
			ctx := context.Background()
			var want error
			found := false
			switch name {
			case "partial":
				found = true
			case "full":
				locator = canonical
				found = true
			case "wrong-node":
				locator.NodeID = "OTHER"
				want = prdiscovery.ErrChanged
			case "other-repository":
				locator.Repository = "other"
				want = prdiscovery.ErrInvalid
			case "other-owner":
				locator.Owner = "other"
				want = prdiscovery.ErrInvalid
			case "other-number":
				locator.Number++
			case "wrong-number-with-node":
				locator = canonical
				locator.Number++
				want = prdiscovery.ErrChanged
			case "wrong-kind":
				locator.Kind = "issue"
				want = prdiscovery.ErrInvalid
			case "duplicate":
				current.Entries = append(current.Entries, current.Entries[0])
				want = prdiscovery.ErrInvalid
			case "conflicting-node":
				other := current.Entries[0]
				other.Resource.NodeID = "OTHER"
				current.Entries = append(current.Entries, other)
				want = prdiscovery.ErrInvalid
			case "missing-node":
				current.Entries[0].Resource.NodeID = ""
				want = prdiscovery.ErrInvalid
			case "incomplete":
				current.Completeness = ""
				want = prdiscovery.ErrIncomplete
			case "unstable":
				current.Freshness = ""
				want = prdiscovery.ErrIncomplete
			case "absent":
				current.Entries = nil
			case "cross-repository-lookalike":
				other := repo
				other.ID = "OTHER_REPO"
				other.Name = "other"
				current.Repositories = append(current.Repositories, other)
				current.Entries[0].Resource.Repository = other.Name
				current.Entries[0].Resource.NodeID = "OTHER_PR"
			case "cross-repository-node", "same-number-other-repository":
				other := repo
				other.ID, other.Name = "OTHER_REPO", "other"
				current.Repositories = append(current.Repositories, other)
				entry := current.Entries[0]
				entry.Resource.Repository, entry.Resource.NodeID = other.Name, "OTHER_PR"
				current.Entries = append(current.Entries, entry)
				if name == "cross-repository-node" {
					locator.Repository, locator.NodeID = other.Name, canonical.NodeID
					want = prdiscovery.ErrChanged
				} else {
					found = true
				}
			case "rejected-binding":
				current.Entries[0].Binding = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingPR}
				found = true
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			got, ok, err := continuousPRTrigger(ctx, current, locator)
			if !errors.Is(err, want) || ok != found {
				t.Fatalf("found=%v error=%v; want found=%v error=%v", ok, err, found, want)
			}
			if found && got != canonical {
				t.Fatal("selected wrong canonical identity", got)
			}
			if !found && got != (storage.Resource{}) {
				t.Fatal("failed selection leaked identity", got)
			}
		})
	}
}

func TestContinuousPRTriggerRevalidatesEstablishedIdentity(t *testing.T) {
	resource := storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: "pull_request", Number: 47, NodeID: "ORIGINAL"}
	current := prdiscovery.Result{Repositories: []github.Repository{{ID: "R", Owner: resource.Owner, Name: resource.Repository}}, Completeness: prdiscovery.Complete, Freshness: prdiscovery.StableReads, Entries: []prdiscovery.Entry{{Resource: resource}}}
	locator := resource
	locator.NodeID = ""
	trigger, found, err := continuousPRTrigger(context.Background(), current, locator)
	if err != nil || !found || trigger != resource {
		t.Fatal("initial identity unavailable", err)
	}
	current.Entries[0].Resource.NodeID = "REPLACEMENT"
	if _, _, err := continuousPRTrigger(context.Background(), current, trigger); !errors.Is(err, prdiscovery.ErrChanged) {
		t.Fatal("new stable identity replaced established identity", err)
	}
	current.Entries = nil
	if got, found, err := continuousPRTrigger(context.Background(), current, trigger); err != nil || found || got != (storage.Resource{}) {
		t.Fatal("absent refresh returned a consumer", err)
	}
	// Converge treats this false result as ErrChanged after a prior match; initial
	// absence instead selects no consumer. Neither supplies historical authority.
}

func TestRejectedPRNotificationFeedbackHasNoLifecycleAuthority(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "branch", "issues")
	step(t, r)
	state(t, r, "branch", storage.Completed, 1)
	f.pr.Author = &github.Actor{ID: "UNAUTHORIZED", Login: "outsider", Type: "User"}
	onboardingNotification(t, r, f, "rejected", "pull_request")
	step(t, r)
	e := state(t, r, "rejected", storage.Failed, 1)
	if e.State.ErrorCategory != "pr_onboarding_rejected" {
		t.Fatal(e.State)
	}
	resource := github.CommentResource{Identity: f.pr.Identity, Kind: "pull_request"}
	comments, err := f.IssueComments(context.Background(), resource)
	if err != nil || len(comments.Comments) != 1 || !strings.HasPrefix(comments.Comments[0].Body, wf.Marker+"\n") || comments.Comments[0].Author != runtimeBot {
		t.Fatal("rejected binding did not reach owned feedback", err)
	}
	if f.adds != 0 || f.drafts != 0 || f.closes != 0 || f.reopens != 0 || len(f.links[f.pr.Identity]) != 0 {
		t.Fatal("selection conferred lifecycle authority")
	}
	before := comments.Comments[0]
	onboardingNotification(t, r, f, "repeated-rejection", "pull_request")
	step(t, r)
	state(t, r, "repeated-rejection", storage.Failed, 1)
	comments, err = f.IssueComments(context.Background(), resource)
	if err != nil || len(comments.Comments) != 1 || comments.Comments[0] != before || f.commentCreates != 1 || f.commentUpdates != 0 {
		t.Fatal("equivalent rejection duplicated feedback", err)
	}
}

// The provider changes only after complete discovery has selected a current
// Target and the terminal owner has verified its explicit Phase closure. This
// exercises Converge's second discovery through the real composed worker.
type routingRefreshClient struct {
	*completionRuntimeClient
	lists   int
	changed bool
	mode    string
}

func (f *routingRefreshClient) ListPullRequests(ctx context.Context, r github.Repository) ([]github.ListedPullRequest, error) {
	f.lists++
	if f.changed && f.mode == "absent" {
		return []github.ListedPullRequest{}, ctx.Err()
	}
	if f.changed && f.mode == "incomplete" {
		return nil, github.ErrPullRequestIncomplete
	}
	return f.completionRuntimeClient.ListPullRequests(ctx, r)
}
func (f *routingRefreshClient) IssueClosure(ctx context.Context, id github.Identity) (github.IssueClosure, error) {
	facts, err := f.completionRuntimeClient.IssueClosure(ctx, id)
	if err == nil && f.lists > 0 && !f.changed {
		f.changed = true
		if f.mode == "node" {
			f.pr.ID = "REPLACEMENT_PR"
		}
	}
	return facts, err
}

func TestPRRoutingRejectsChangedDiscoveryAfterTerminalSelection(t *testing.T) {
	for _, mode := range []string{"absent", "node", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			c, base, _ := completionRuntimeFixture(t)
			base.issue.Type = &github.IssueType{ID: "type-Phase", Name: "Phase"}
			base.issue.State = "CLOSED"
			base.issue.Body += "\nCreate-Branch: false"
			item := base.items[base.issue.ID]
			item.Values["status"] = github.FieldValue{Kind: config.SingleSelect, OptionID: "done"}
			base.items[base.issue.ID] = item
			f := &routingRefreshClient{completionRuntimeClient: base, mode: mode}
			c.Client = f
			r := openRuntime(t, c)
			onboardingNotification(t, r, f, "refresh", "pull_request")
			step(t, r)
			e := state(t, r, "refresh", storage.Retryable, 1)
			want := "pr_discovery_changed"
			if mode == "incomplete" {
				want = "pr_discovery_incomplete"
			}
			if !f.changed || f.lists < 4 || e.State.ErrorCategory != want {
				t.Fatal("fresh discovery barrier was not reached", f.changed, f.lists, e.State)
			}
			if f.adds != 0 || f.drafts != 0 || f.closes != 0 || f.reopens != 0 || len(f.links) != 0 || f.commentCreates != 0 {
				t.Fatal("changed discovery allowed a speculative effect")
			}
		})
	}
}

func TestPRNotificationWithdrawsFormerTargetFeedbackFromOwnedCompanion(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "branch", "issues")
	step(t, r)
	state(t, r, "branch", storage.Completed, 1)
	onboardingNotification(t, r, f, "onboard", "pull_request")
	step(t, r)
	state(t, r, "onboard", storage.Completed, 1)
	item := f.items[f.issue.ID]
	item.Values["status"] = github.FieldValue{Kind: config.SingleSelect, OptionID: "done"}
	f.items[f.issue.ID] = item
	onboardingNotification(t, r, f, "done-drift", "issues")
	step(t, r)
	state(t, r, "done-drift", storage.Completed, 1)
	old := github.CommentResource{Identity: f.issue.Identity, Kind: "issue"}
	before, err := f.IssueComments(context.Background(), old)
	if err != nil || len(before.Comments) != 1 || !strings.Contains(before.Comments[0].Body, "natively open") {
		t.Fatal("former Target correction was not reported", err)
	}
	other := f.issue
	other.ID = "NEW_TARGET"
	other.Number = 48
	other.Body = "Classification: false\nCreate-Branch: false"
	f.children = []github.Issue{other}
	f.items[other.ID] = github.ProjectItem{ID: "CHILDITEM", ProjectID: "engineering", Content: &github.Content{ID: other.ID, Kind: "Issue"}, Values: map[string]github.FieldValue{"status": {Kind: config.SingleSelect, OptionID: "ready"}}}
	// The existing authorized branch still belongs to #46. A Target edit selects
	// fresh rejection feedback, never authorization to rebind or move its link.
	f.pr.Body = "Target: #48\nClassification: false"
	onboardingNotification(t, r, f, "retarget", "pull_request")
	step(t, r)
	e, err := r.store.Event(context.Background(), "retarget")
	if err != nil || e.State.Status != storage.Failed || e.State.ErrorCategory != "pr_onboarding_rejected" {
		t.Fatal(e.State, err)
	}
	after, err := f.IssueComments(context.Background(), old)
	if err != nil || len(after.Comments) != 1 || after.Comments[0].ID != before.Comments[0].ID || !strings.Contains(after.Comments[0].Body, "constraints are resolved") {
		t.Fatal("canonical PR did not refresh former owned Target", err)
	}
	if f.adds != 1 || f.drafts != 1 || f.closes != 0 || f.reopens != 0 || len(f.links[f.pr.Identity]) != 1 || f.links[f.pr.Identity][0] != f.issue.Identity {
		t.Fatal("historical ownership conferred new lifecycle authority")
	}
}

func TestPRNotificationBugTargetRemainsIsolated(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	f.issue.Type = &github.IssueType{ID: "type-Bug", Name: "Bug"}
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "bug-pr", "pull_request")
	step(t, r)
	e, err := r.store.Event(context.Background(), "bug-pr")
	if err != nil || e.State.Status != storage.Failed {
		t.Fatal("unsupported Bug Target was accepted", e.State, err)
	}
	if f.adds != 0 || f.drafts != 0 || f.closes != 0 || f.reopens != 0 || f.commentCreates != 0 || len(f.links) != 0 {
		t.Fatal("Engineering effects escaped to Bug Target")
	}
}
