package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	wf "github.com/parametron-io/parametron-workflow/internal/workflowfeedback"
)

func TestTerminalCorrectionFeedbackProductionRestartLostResponses(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "done-drift", true: "native-close"}[native], func(t *testing.T) {
			c, f, now := completionRuntimeFixture(t)
			f.issue.Body += "\nSet-Status: false"
			if native {
				f.issue.State = "CLOSED"
			}
			item := f.items[f.issue.ID]
			item.Values["status"] = github.FieldValue{Kind: config.SingleSelect, OptionID: "done"}
			f.items[f.issue.ID] = item
			f.lostCreate = true
			r := openRuntime(t, c)
			onboardingNotification(t, r, f, "correction", "issues")
			step(t, r)
			state(t, r, "correction", storage.Retryable, 1)
			if f.issue.State != "OPEN" || runtimeOption(f, f.issue.ID) != "ready" || f.closes != 0 || f.reopens != map[bool]int{true: 1, false: 0}[native] {
				t.Fatal("canonical correction failed before feedback response", f.issue.State, f.reopens, f.items)
			}
			resource := github.CommentResource{Identity: f.issue.Identity, Kind: "issue"}
			list, _ := f.IssueComments(context.Background(), resource)
			if f.commentCreates != 1 || len(list.Comments) != 1 || !strings.HasPrefix(list.Comments[0].Body, wf.Marker+"\n") || list.Comments[0].Author != runtimeBot {
				t.Fatal("typed correction did not reach owned managed feedback")
			}
			if native && !strings.Contains(list.Comments[0].Body, "terminal authorization was missing") {
				t.Fatal("native correction explanation missing")
			}
			if !native && !strings.Contains(list.Comments[0].Body, "natively open") {
				t.Fatal("Done correction explanation missing")
			}
			before := list.Comments[0]
			r.store.Close()
			*now = now.Add(c.RetryDelay)
			r = openRuntime(t, c)
			step(t, r)
			state(t, r, "correction", storage.Completed, 2)
			if f.commentCreates != 1 || f.commentUpdates != 0 {
				t.Fatal("lost create response duplicated feedback")
			}
			f.lostUpdate = true
			// Project-item resolution supplies an exact current identity through
			// production routing. The separate Issue-notification regression below
			// preserves the unresolved locator/identity defect on that route.
			if _, err := r.store.InsertDelivery(context.Background(), storage.Delivery{ID: "resolution", EventName: "projects_v2_item", Payload: []byte(`{"action":"edited","projects_v2_item":{"node_id":"ISSUEITEM","project_node_id":"engineering"}}`), ReceivedAt: *now}); err != nil {
				t.Fatal(err)
			}
			step(t, r)
			state(t, r, "resolution", storage.Retryable, 1)
			r.store.Close()
			*now = now.Add(c.RetryDelay)
			r = openRuntime(t, c)
			step(t, r)
			state(t, r, "resolution", storage.Completed, 2)
			after, _ := f.IssueComments(context.Background(), resource)
			if len(after.Comments) != 1 || after.Comments[0].ID != before.ID || after.Comments[0].DatabaseID != before.DatabaseID || !strings.Contains(after.Comments[0].Body, "constraints are resolved") || f.commentCreates != 1 || f.commentUpdates != 1 {
				t.Fatal("resolution did not reuse exact managed ownership", f.commentCreates, f.commentUpdates)
			}
			onboardingNotification(t, r, f, "quiet", "issues")
			step(t, r)
			state(t, r, "quiet", storage.Completed, 1)
			if f.commentCreates != 1 || f.commentUpdates != 1 {
				t.Fatal("routine convergence repeated feedback")
			}
		})
	}
}
func TestTerminalFeedbackDoesNotAdoptForeignComments(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	f.issue.State = "CLOSED"
	resource := github.CommentResource{Identity: f.issue.Identity, Kind: "issue"}
	human := github.IssueComment{ID: "HUMAN", DatabaseID: 201, Resource: resource, Author: github.Actor{ID: "OTHER_USER", Login: "human", Type: "User"}, Body: wf.Marker + "\nhuman copy"}
	other := github.IssueComment{ID: "OTHER_BOT", DatabaseID: 202, Resource: resource, Author: github.Actor{ID: "OTHER_BOT", Login: "other[bot]", Type: "Bot"}, Body: wf.Marker + "\nother app copy"}
	f.comments[resource] = []github.IssueComment{human, other}
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "foreign", "issues")
	step(t, r)
	state(t, r, "foreign", storage.Completed, 1)
	list, _ := f.IssueComments(context.Background(), resource)
	if len(list.Comments) != 3 || list.Comments[0] != human || list.Comments[1] != other || list.Comments[2].Author != runtimeBot || f.commentCreates != 1 || f.commentUpdates != 0 {
		t.Fatal("foreign comment adopted or modified")
	}
}
func TestVerifiedAuthorStartupAndChangedIdentityFailClosed(t *testing.T) {
	t.Run("startup", func(t *testing.T) {
		c, f, _ := completionRuntimeFixture(t)
		f.author = github.Actor{ID: "OTHER", Login: "other[bot]", Type: "Bot"}
		r, err := compose(context.Background(), c)
		if r != nil || !errors.Is(err, wf.ErrOwnership) || f.commentCreates != 0 || f.reopens != 0 {
			t.Fatal("unverified author admitted", err)
		}
	})
	t.Run("after-startup", func(t *testing.T) {
		c, f, _ := completionRuntimeFixture(t)
		r := openRuntime(t, c)
		f.author = github.Actor{ID: "OTHER", Login: "other[bot]", Type: "Bot"}
		f.issue.State = "CLOSED"
		onboardingNotification(t, r, f, "changed-author", "issues")
		step(t, r)
		state(t, r, "changed-author", storage.Failed, 1)
		if f.issue.State != "OPEN" || f.reopens != 1 || f.closes != 0 || f.commentCreates != 0 || f.commentUpdates != 0 {
			t.Fatal("feedback identity failure authorized closure or comment mutation")
		}
	})
}
func TestPhaseForceCloseCannotBypassChildGateProduction(t *testing.T) {
	c, f, now := completionRuntimeFixture(t)
	f.issue.Type = &github.IssueType{ID: "type-Phase", Name: "Phase"}
	f.issue.Body += "\nSet-Status: false"
	f.issue.State = "CLOSED"
	f.children = []github.Issue{{Identity: github.Identity{ID: "CHILD", Repository: f.repo, Number: 48}, State: "OPEN", Body: "Classification: false\nParent: #46", Type: &github.IssueType{ID: "type-Task", Name: "Task"}}}
	f.items["CHILD"] = github.ProjectItem{ID: "CHILDITEM", ProjectID: "engineering", Content: &github.Content{ID: "CHILD", Kind: "Issue"}, Values: map[string]github.FieldValue{}}
	if err := f.closureComment(f.issue.Identity, "Force-Close: cannot skip child work"); err != nil {
		t.Fatal(err)
	}
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "phase-force", "issues")
	step(t, r)
	state(t, r, "phase-force", storage.Completed, 1)
	if f.issue.State != "OPEN" || f.children[0].State != "OPEN" || f.reopens != 1 || f.closes != 0 || runtimeOption(f, f.issue.ID) == "done" {
		t.Fatal("Phase command bypassed direct-child gate")
	}
	*now = now.Add(time.Minute)
	f.children[0].State = "CLOSED"
	if err := f.closureComment(f.children[0].Identity, "Force-Close: child explicitly abandoned"); err != nil {
		t.Fatal(err)
	}
	onboardingNotification(t, r, f, "last-child", "issues")
	step(t, r)
	state(t, r, "last-child", storage.Completed, 1)
	if f.issue.State != "OPEN" || f.children[0].State != "CLOSED" || f.closes != 0 {
		t.Fatal("last child cascaded Phase close")
	}
	*now = now.Add(time.Minute)
	f.issue.State = "CLOSED"
	onboardingNotification(t, r, f, "explicit-phase", "issues")
	step(t, r)
	state(t, r, "explicit-phase", storage.Completed, 1)
	if f.issue.State != "CLOSED" || runtimeOption(f, f.issue.ID) != "done" || f.reopens != 1 {
		t.Fatal("eligible explicit normal Phase close rejected")
	}
	for resource, comments := range f.comments {
		if resource.Identity.ID == "CHILD" {
			for _, comment := range comments {
				if comment.Author == runtimeBot {
					t.Fatal("parent lifecycle posted child completion feedback")
				}
			}
		}
	}
	*now = now.Add(time.Minute)
	f.children[0].State = "OPEN"
	if _, err := r.store.InsertDelivery(context.Background(), storage.Delivery{ID: "child-reopened", EventName: "issues", Payload: []byte(`{"repository":{"owner":{"login":"parametron-io"},"name":"repo"},"issue":{"number":48}}`), ReceivedAt: *now}); err != nil {
		t.Fatal(err)
	}
	step(t, r)
	state(t, r, "child-reopened", storage.Completed, 1)
	if f.issue.State != "OPEN" || runtimeOption(f, f.issue.ID) == "done" || f.children[0].State != "OPEN" || f.reopens != 2 || f.closes != 0 {
		t.Fatal("reopened child did not reopen Phase")
	}
	// The child's own explicit reopen can correct its stale Done projection;
	// such feedback must describe that correction, never synthetic completion.
	for resource, comments := range f.comments {
		if resource.Identity.ID == "CHILD" {
			for _, comment := range comments {
				if comment.Author == runtimeBot && !strings.Contains(comment.Body, "Project Done was selected for correction") {
					t.Fatal("synthetic child completion feedback")
				}
			}
		}
	}
}

func TestReportedTerminalFeedbackResolvesOnIssueNotification(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	f.issue.State = "CLOSED"
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "reject-close", "issues")
	step(t, r)
	state(t, r, "reject-close", storage.Completed, 1)
	resource := github.CommentResource{Identity: f.issue.Identity, Kind: "issue"}
	before, err := f.IssueComments(context.Background(), resource)
	if err != nil || len(before.Comments) != 1 || !strings.Contains(before.Comments[0].Body, "terminal authorization was missing") {
		t.Fatal("owner correction feedback was not established", err)
	}
	onboardingNotification(t, r, f, "refresh-resolved", "issues")
	step(t, r)
	state(t, r, "refresh-resolved", storage.Completed, 1)
	after, err := f.IssueComments(context.Background(), resource)
	if err != nil || len(after.Comments) != 1 || after.Comments[0].ID != before.Comments[0].ID {
		t.Fatal("managed ownership changed", err)
	}
	if !strings.Contains(after.Comments[0].Body, "constraints are resolved") {
		t.Fatal("reported historical correction remains active after a fresh Issue notification confirms canonical open state")
	}
}
