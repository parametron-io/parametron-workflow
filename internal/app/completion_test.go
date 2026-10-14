package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type completionRuntimeClient struct {
	*reviewRuntimeClient
	native          map[string]github.NativeLifecycle
	children        []github.Issue
	closes, reopens int
}

func (f *completionRuntimeClient) NativeLifecycle(ctx context.Context, id github.Identity, k string) (github.NativeLifecycle, error) {
	if n, ok := f.native[id.ID]; ok {
		if n.Resource != id || n.Kind != k {
			f.t.Fatal("native identity mismatch")
		}
		return n, ctx.Err()
	}
	if id == f.issue.Identity || id == f.pr.Identity {
		return github.NativeLifecycle{Resource: id, Kind: k, State: "OPEN"}, ctx.Err()
	}
	f.t.Fatal("unrelated lifecycle", id)
	return github.NativeLifecycle{}, nil
}
func (f *completionRuntimeClient) CloseIssue(ctx context.Context, id github.Identity) (github.IssueStateReceipt, error) {
	if id != f.issue.Identity {
		f.t.Fatal("unrelated close", id)
	}
	f.closes++
	f.issue.State = "CLOSED"
	return github.IssueStateReceipt{Issue: id, State: "CLOSED"}, ctx.Err()
}
func (f *completionRuntimeClient) ReopenIssue(ctx context.Context, id github.Identity) (github.IssueStateReceipt, error) {
	if id != f.issue.Identity {
		f.t.Fatal("unrelated reopen", id)
	}
	f.reopens++
	f.issue.State = "OPEN"
	return github.IssueStateReceipt{Issue: id, State: "OPEN"}, ctx.Err()
}
func (f *completionRuntimeClient) ListIssues(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
	out, err := f.onboardingRuntimeClient.ListIssues(ctx, r)
	for _, i := range f.children {
		out = append(out, github.ListedIssue{Identity: i.Identity, Body: i.Body, State: i.State, Type: i.Type})
	}
	return out, err
}
func (f *completionRuntimeClient) CurrentProjectItem(ctx context.Context, id string) (github.CurrentProjectItem, error) {
	if id == "CHILDITEM" && len(f.children) > 0 {
		i := f.children[0].Identity
		return github.CurrentProjectItem{ID: id, ProjectID: "engineering", ContentKind: "Issue", Issue: &i}, ctx.Err()
	}
	return f.reviewRuntimeClient.CurrentProjectItem(ctx, id)
}
func (f *completionRuntimeClient) SetProjectOption(ctx context.Context, p, id, field, option string) error {
	if id == "CHILDITEM" {
		if p != "engineering" || field != "status" {
			f.t.Fatal(p, id, field)
		}
		item := f.items[f.children[0].ID]
		item.Values[field] = github.FieldValue{Kind: config.SingleSelect, OptionID: option}
		f.items[f.children[0].ID] = item
		return ctx.Err()
	}
	return f.reviewRuntimeClient.SetProjectOption(ctx, p, id, field, option)
}
func completionRuntimeFixture(t *testing.T) (Config, *completionRuntimeClient, *time.Time) {
	c, base := onboardingRuntimeFixture(t)
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	c.Clock = func() time.Time { return now }
	c.Runner = nil
	f := &completionRuntimeClient{reviewRuntimeClient: &reviewRuntimeClient{onboardingRuntimeClient: base, now: &now, facts: github.ReviewFacts{PR: base.pr.Identity}}, native: map[string]github.NativeLifecycle{}}
	original := base.IssueFunc
	base.IssueFunc = func(ctx context.Context, r github.Ref) (github.Issue, error) {
		for _, i := range f.children {
			if i.Number == r.Number && i.Repository.Owner == r.Owner && i.Repository.Name == r.Repository {
				return i, ctx.Err()
			}
		}
		return original(ctx, r)
	}
	c.Client = f
	return c, f, &now
}
func runtimeOption(f *completionRuntimeClient, node string) string {
	return f.items[node].Values["status"].OptionID
}

func (f *completionRuntimeClient) ClearProjectNumber(ctx context.Context, project, id, field string) error {
	if project != "engineering" || field != "roadmap_order" || id != "ISSUEITEM" && id != "CHILDITEM" {
		f.t.Fatalf("unrelated roadmap clear: %s/%s/%s", project, id, field)
	}
	node := f.issue.ID
	if id == "CHILDITEM" {
		node = f.children[0].ID
	}
	item := f.items[node]
	delete(item.Values, field)
	f.items[node] = item
	return ctx.Err()
}

func TestCompletionProductionWorkerRestartAndReopen(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(fmt.Sprintf("merged_%v", merged), func(t *testing.T) {
			c, f, now := completionRuntimeFixture(t)
			r := openRuntime(t, c)
			onboardingNotification(t, r, "branch", "issues")
			step(t, r)
			state(t, r, "branch", storage.Completed, 1)
			onboardingNotification(t, r, "onboard", "pull_request")
			step(t, r)
			state(t, r, "onboard", storage.Completed, 1)
			f.children = []github.Issue{{Identity: github.Identity{ID: "CHILD", Repository: f.repo, Number: 48}, State: "OPEN", Body: "Classification: false\nBlocked-By: #46", Type: &github.IssueType{ID: "type-Task", Name: "Task"}}}
			f.items["CHILD"] = github.ProjectItem{ID: "CHILDITEM", ProjectID: "engineering", Content: &github.Content{ID: "CHILD", Kind: "Issue"}, Values: map[string]github.FieldValue{"status": {Kind: config.SingleSelect, OptionID: "blocked"}}}
			*now = now.Add(time.Hour)
			kind, stateName := "ClosedEvent", "CLOSED"
			if merged {
				kind, stateName = "MergedEvent", "MERGED"
			}
			f.pr.State = stateName
			f.native[f.pr.ID] = github.NativeLifecycle{Resource: f.pr.Identity, Kind: "pull_request", State: stateName, Transitions: []github.NativeTransition{{ID: "END", Kind: kind, Resource: f.pr.Identity, CreatedAt: *now}}}
			onboardingNotification(t, r, "terminal", "pull_request")
			step(t, r)
			state(t, r, "terminal", storage.Completed, 1)
			if runtimeOption(f, f.pr.ID) != "done" {
				t.Fatal(f.items)
			}
			if merged {
				if runtimeOption(f, "CHILD") != "ready" {
					t.Fatal("downstream eligibility did not converge", f.items)
				}
				if f.closes != 1 || f.issue.State != "CLOSED" || runtimeOption(f, f.issue.ID) != "done" {
					t.Fatal(f.closes, f.issue.State, f.items)
				}
			} else if f.closes != 0 || f.issue.State != "OPEN" || runtimeOption(f, f.issue.ID) != "in_progress" {
				t.Fatal(f.closes, f.issue.State, f.items)
			}
			if err := r.store.Close(); err != nil {
				t.Fatal(err)
			}
			r = openRuntime(t, c)
			if merged {
				f.issue.State = "OPEN"
				*now = now.Add(time.Hour)
				f.native[f.issue.ID] = github.NativeLifecycle{Resource: f.issue.Identity, Kind: "issue", State: "OPEN", Transitions: []github.NativeTransition{{ID: "TARGETCLOSE", Kind: "ClosedEvent", Resource: f.issue.Identity, CreatedAt: now.Add(-time.Minute)}, {ID: "TARGETREOPEN", Kind: "ReopenedEvent", Resource: f.issue.Identity, CreatedAt: *now}}}
			}
			onboardingNotification(t, r, "delayed-terminal", "pull_request")
			step(t, r)
			state(t, r, "delayed-terminal", storage.Completed, 1)
			if f.issue.State != "OPEN" || f.closes != map[bool]int{true: 1, false: 0}[merged] || runtimeOption(f, f.issue.ID) == "done" {
				t.Fatal("reclosed or retained Done", f.closes, f.issue.State, f.items)
			}
		})
	}
}

func TestManualDoneDriftProductionWorker(t *testing.T) {
	for _, directive := range []string{"", "Set-Status: true", "Set-Status: false"} {
		t.Run(directive, func(t *testing.T) {
			c, f, _ := completionRuntimeFixture(t)
			f.issue.Body += "\n" + directive
			item := f.items[f.issue.ID]
			item.Values["status"] = github.FieldValue{Kind: config.SingleSelect, OptionID: "done"}
			f.items[f.issue.ID] = item
			r := openRuntime(t, c)
			for _, id := range []string{"manual-done", "delayed-after-reopen", "duplicate-drift"} {
				if id == "manual-done" {
					onboardingNotification(t, r, id, "issues")
				} else {
					_, err := r.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: "projects_v2_item", Payload: []byte(`{"action":"edited","projects_v2_item":{"node_id":"ISSUEITEM","project_node_id":"engineering"}}`), ReceivedAt: time.Unix(1700000000, 0)})
					if err != nil {
						t.Fatal(err)
					}
				}
				step(t, r)
				state(t, r, id, storage.Completed, 1)
				if id == "manual-done" {
					if err := r.store.Close(); err != nil {
						t.Fatal(err)
					}
					r = openRuntime(t, c)
				}
			}
			if f.issue.State != "OPEN" || f.closes != 0 || runtimeOption(f, f.issue.ID) != "ready" {
				t.Fatal(f.issue.State, f.closes, f.items)
			}
		})
	}
}

func TestInvalidPhaseCloseProductionWorker(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	f.issue.Type = &github.IssueType{ID: "type-Phase", Name: "Phase"}
	f.issue.State = "CLOSED"
	f.issue.Body += "\nSet-Status: false"
	f.children = []github.Issue{{Identity: github.Identity{ID: "CHILD", Repository: f.repo, Number: 48}, State: "OPEN", Body: "Classification: false\nParent: #46", Type: &github.IssueType{ID: "type-Task", Name: "Task"}}}
	f.items["CHILD"] = github.ProjectItem{ID: "CHILDITEM", ProjectID: "engineering", Content: &github.Content{ID: "CHILD", Kind: "Issue"}, Values: map[string]github.FieldValue{}}
	item := f.items[f.issue.ID]
	item.Values["status"] = github.FieldValue{Kind: config.SingleSelect, OptionID: "done"}
	f.items[f.issue.ID] = item
	r := openRuntime(t, c)
	onboardingNotification(t, r, "invalid-phase-close", "issues")
	step(t, r)
	state(t, r, "invalid-phase-close", storage.Completed, 1)
	if f.issue.State != "OPEN" || f.reopens != 1 || f.closes != 0 || runtimeOption(f, f.issue.ID) == "done" {
		t.Fatal(f.issue.State, f.reopens, f.items)
	}
	onboardingNotification(t, r, "duplicate-phase-close", "issues")
	step(t, r)
	state(t, r, "duplicate-phase-close", storage.Completed, 1)
	if f.reopens != 1 {
		t.Fatal("repeat reopen")
	}
}

func TestDirectPhaseMergeNeverClosesObjective(t *testing.T) {
	for _, childClosed := range []bool{false, true} {
		t.Run(fmt.Sprintf("closed_children_%v", childClosed), func(t *testing.T) {
			c, f, now := completionRuntimeFixture(t)
			f.issue.Type = &github.IssueType{ID: "type-Phase", Name: "Phase"}
			f.issue.Body += "\nCreate-Branch: true"
			r := openRuntime(t, c)
			onboardingNotification(t, r, "phase-branch", "issues")
			step(t, r)
			state(t, r, "phase-branch", storage.Completed, 1)
			onboardingNotification(t, r, "phase-onboard", "pull_request")
			step(t, r)
			state(t, r, "phase-onboard", storage.Completed, 1)
			childState := "OPEN"
			if childClosed {
				childState = "CLOSED"
			}
			f.children = []github.Issue{{Identity: github.Identity{ID: "CHILD", Repository: f.repo, Number: 48}, State: childState, Body: "Classification: false\nParent: #46", Type: &github.IssueType{ID: "type-Task", Name: "Task"}}}
			f.items["CHILD"] = github.ProjectItem{ID: "CHILDITEM", ProjectID: "engineering", Content: &github.Content{ID: "CHILD", Kind: "Issue"}, Values: map[string]github.FieldValue{}}
			*now = now.Add(time.Hour)
			f.pr.State = "MERGED"
			f.native[f.pr.ID] = github.NativeLifecycle{Resource: f.pr.Identity, Kind: "pull_request", State: "MERGED", Transitions: []github.NativeTransition{{ID: "PHASEMERGE", Kind: "MergedEvent", Resource: f.pr.Identity, CreatedAt: *now}}}
			onboardingNotification(t, r, "phase-merge", "pull_request")
			step(t, r)
			state(t, r, "phase-merge", storage.Completed, 1)
			if f.closes != 0 || f.issue.State != "OPEN" || runtimeOption(f, f.issue.ID) == "done" || runtimeOption(f, f.pr.ID) != "done" {
				t.Fatal("implementation completed Phase objective", f.closes, f.issue.State, f.items)
			}
		})
	}
}
