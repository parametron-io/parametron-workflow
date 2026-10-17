package app

import (
	"context"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// Notification identity is deliberately a locator. Production Resolver discards
// payload node IDs; current observation must supply the exact PR identity.
func TestOnboardingNotificationConvergesFromFreshIdentity(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "identity-branch", "issues")
	step(t, r)
	state(t, r, "identity-branch", storage.Completed, 1)
	onboardingNotification(t, r, f, "identity-pr", "pull_request")
	step(t, r)
	event, err := r.store.Event(context.Background(), "identity-pr")
	if err != nil {
		t.Fatal(err)
	}
	if event.Resource == nil || event.Resource.NodeID != "" {
		t.Fatal("notification locator unexpectedly became node authority")
	}
	completed, err := r.store.ProvenanceRecords(context.Background(), "pr_onboarding_completed_v1")
	if err != nil || len(completed) != 1 || f.drafts != 1 || f.adds != 1 {
		t.Fatal("onboarding did not complete before continuous convergence", err, len(completed), f.drafts, f.adds)
	}
	if event.State.Status != storage.Completed {
		t.Fatalf("valid PR durably onboarded, but current convergence failed: status=%s category=%s", event.State.Status, event.State.ErrorCategory)
	}
}
