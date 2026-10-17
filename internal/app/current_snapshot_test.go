package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func TestContinuousIssueCanonicalSnapshotIdentity(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	r := openRuntime(t, c)
	p, err := Prepare(context.Background(), c.Source, f)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := observe.NewProcessor(*p.Deployment, f, consumerFunc(func(context.Context, observe.PolicyInput) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := semanticflow.NewAcceptedLoader(r.store)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := ec.New(ec.Config{Deployment: *p.Deployment, Primary: reader, Lister: f, Accepted: accepted})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := resolver.ResolveSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	locator := storage.Resource{Owner: f.repo.Owner, Repository: f.repo.Name, Kind: "issue", Number: 46}
	for _, name := range []string{"locator", "exact", "wrong-node", "cross-repository", "cross-owner", "missing", "wrong-kind"} {
		t.Run(name, func(t *testing.T) {
			x := locator
			valid := false
			switch name {
			case "locator":
				valid = true
			case "exact":
				x.NodeID = f.issue.ID
				valid = true
			case "wrong-node":
				x.NodeID = "OTHER"
			case "cross-repository":
				x.Repository = "other"
			case "cross-owner":
				x.Owner = "other"
			case "missing":
				x.Number = 99
			case "wrong-kind":
				x.Kind = "pull_request"
			}
			graph, err := continuousIssueContext(snapshot, x)
			if valid {
				if err != nil || graph.Root.NodeID != f.issue.ID || graph.Root.Number != 46 {
					t.Fatal(graph.Root, err)
				}
			} else if err == nil {
				t.Fatal("unverified locator resolved")
			}
		})
	}
	graph, err := continuousIssueContext(snapshot, locator)
	if err != nil {
		t.Fatal(err)
	}
	f.issue.ID = "REPLACEMENT"
	changed, err := resolver.ResolveSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = continuousIssueContext(changed, graph.Root); !errors.Is(err, ec.ErrChanged) {
		t.Fatal("identity changed across snapshots", err)
	}
	if f.closes != 0 || f.reopens != 0 || f.commentCreates != 0 {
		t.Fatal("identity resolution performed effects")
	}
}

func TestUnrelatedIssueDoesNotResolveHistoricalFeedback(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	f.issue.State = "CLOSED"
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "rejected", "issues")
	step(t, r)
	state(t, r, "rejected", storage.Completed, 1)
	resource := github.CommentResource{Identity: f.issue.Identity, Kind: "issue"}
	before, _ := f.IssueComments(context.Background(), resource)
	if len(before.Comments) != 1 || !strings.Contains(before.Comments[0].Body, "terminal authorization was missing") {
		t.Fatal("missing original correction")
	}
	unrelated := f.issue
	unrelated.ID = "UNRELATED"
	unrelated.Number = 48
	unrelated.State = "OPEN"
	unrelated.Body = "Classification: false\nCreate-Branch: false"
	f.children = []github.Issue{unrelated}
	f.items[unrelated.ID] = github.ProjectItem{ID: "CHILDITEM", ProjectID: "engineering", Content: &github.Content{ID: unrelated.ID, Kind: "Issue"}, Values: map[string]github.FieldValue{"status": {Kind: config.SingleSelect, OptionID: "ready"}}}
	_, err := r.store.InsertDelivery(context.Background(), storage.Delivery{ID: "unrelated", EventName: "issues", Payload: []byte(`{"repository":{"owner":{"login":"parametron-io"},"name":"repo"},"issue":{"number":48}}`), ReceivedAt: c.Clock()})
	if err != nil {
		t.Fatal(err)
	}
	step(t, r)
	state(t, r, "unrelated", storage.Completed, 1)
	after, _ := f.IssueComments(context.Background(), resource)
	if len(after.Comments) != 1 || after.Comments[0] != before.Comments[0] || f.commentUpdates != 0 {
		t.Fatal("unrelated notification refreshed historical correction")
	}
	onboardingNotification(t, r, f, "relevant", "issues")
	step(t, r)
	state(t, r, "relevant", storage.Completed, 1)
	after, _ = f.IssueComments(context.Background(), resource)
	if len(after.Comments) != 1 || !strings.Contains(after.Comments[0].Body, "constraints are resolved") || f.commentUpdates != 1 {
		t.Fatal("relevant resolution failed")
	}
}

func TestForceCloseCurrentCyclesProductionWorker(t *testing.T) {
	c, f, now := completionRuntimeFixture(t)
	r := openRuntime(t, c)
	feedbackCreates := 0
	for cycle := 0; cycle < 2; cycle++ {
		f.issue.State = "CLOSED"
		if err := f.closureComment(f.issue.Identity, "Force-Close: explicit completion"); err != nil {
			t.Fatal(err)
		}
		id := []string{"first", "second"}[cycle]
		onboardingNotification(t, r, f, id, "issues")
		step(t, r)
		state(t, r, id, storage.Completed, 1)
		if f.issue.State != "CLOSED" || f.reopens != 0 || f.closes != 0 || runtimeOption(f, f.issue.ID) != "done" {
			t.Fatal("valid same-second command rejected")
		}
		if f.commentCreates != feedbackCreates {
			t.Fatal("successful Force-Close posted routine feedback")
		}
		rows, err := r.store.ProvenanceRecords(context.Background(), "force_close_comment_v1")
		if err != nil || len(rows) != cycle+1 {
			t.Fatal("cycle acceptance missing", len(rows), err)
		}
		r.store.Close()
		r = openRuntime(t, c)
		if cycle == 0 {
			*now = now.Add(time.Minute)
			f.issue.State = "OPEN"
			onboardingNotification(t, r, f, "reopened", "issues")
			step(t, r)
			state(t, r, "reopened", storage.Completed, 1)
			// The explicit reopen corrects stale Project Done and may report that
			// typed correction. It must not be mistaken for Force-Close feedback.
			feedbackCreates = f.commentCreates
			*now = now.Add(time.Minute)
		}
	}
}

func TestUnexpectedTerminalMutationSettlesWorker(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	earlier := f.issue
	earlier.ID = "EARLIER"
	earlier.Number = 45
	earlier.State = "CLOSED"
	f.children = []github.Issue{earlier}
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "unexpected-reopen", "issues")
	step(t, r)
	state(t, r, "unexpected-reopen", storage.Failed, 1)
	if f.children[0].State != "CLOSED" || f.reopens != 0 {
		t.Fatal("unexpected mutation swallowed")
	}
	if _, err := f.ReopenIssue(context.Background(), earlier.Identity); err == nil || !strings.Contains(err.Error(), "unrelated reopen") || !strings.Contains(err.Error(), "EARLIER") {
		t.Fatal("unexpected resource diagnostic missing", err)
	}
}
