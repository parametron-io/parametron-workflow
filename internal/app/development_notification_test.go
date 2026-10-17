package app

import (
	"context"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// A PR notification selects current reconciliation; neither the notification
// locator nor a pre-existing native link grants Development authority. The
// composed #46/#47 owners must supply that authority after onboarding.
func TestPRNotificationConvergesDevelopmentFromFreshIdentity(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "branch", "issues")
	step(t, r)
	state(t, r, "branch", storage.Completed, 1)
	onboardingNotification(t, r, f, "onboard", "pull_request")
	step(t, r)
	state(t, r, "onboard", storage.Completed, 1)
	links, err := f.DevelopmentReferences(context.Background(), f.pr.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !links.Complete || len(links.Manual) != 1 || links.Manual[0] != f.issue.Identity {
		t.Errorf("completed PR notification did not converge verified Development Target: got %v", links.Manual)
	}
	// Positive control: the same production owners and fake converge the exact
	// link on an Issue locator. This excludes missing binding/onboarding facts
	// or an incapable fake as the cause of the PR notification failure.
	onboardingNotification(t, r, f, "issue-control", "issues")
	step(t, r)
	state(t, r, "issue-control", storage.Completed, 1)
	links, err = f.DevelopmentReferences(context.Background(), f.pr.Identity)
	if err != nil || len(links.Manual) != 1 || links.Manual[0] != f.issue.Identity {
		t.Fatal("positive Issue control did not converge", err, links.Manual)
	}
	// A later PR-only notification must also repair drift without replaying
	// onboarding or requiring an unrelated Issue notification.
	f.links[f.pr.Identity] = nil
	onboardingNotification(t, r, f, "drift", "pull_request")
	step(t, r)
	state(t, r, "drift", storage.Completed, 1)
	links, err = f.DevelopmentReferences(context.Background(), f.pr.Identity)
	if err != nil || len(links.Manual) != 1 || links.Manual[0] != f.issue.Identity || f.drafts != 1 || f.adds != 1 {
		t.Fatal("PR drift convergence failed or replayed onboarding", err, links.Manual, f.drafts, f.adds)
	}
}
