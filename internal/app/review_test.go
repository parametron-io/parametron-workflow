package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prreview"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type reviewRuntimeClient struct {
	*onboardingRuntimeClient
	facts         github.ReviewFacts
	now           *time.Time
	effects       []string
	failure       error
	statusFailure string
}

func (f *reviewRuntimeClient) NativeLifecycle(ctx context.Context, id github.Identity, kind string) (github.NativeLifecycle, error) {
	if id != f.pr.Identity || kind != "pull_request" || f.pr.State != "OPEN" {
		return github.NativeLifecycle{}, fmt.Errorf("unexpected native lifecycle read: %+v/%s", id, kind)
	}
	return github.NativeLifecycle{Resource: id, Kind: kind, State: f.pr.State}, ctx.Err()
}

func (f *reviewRuntimeClient) PullRequestReviewFacts(ctx context.Context, pr github.Identity) (github.ReviewFacts, error) {
	if pr != f.pr.Identity {
		return github.ReviewFacts{}, fmt.Errorf("wrong native facts identity %+v", pr)
	}
	if f.failure != nil {
		return github.ReviewFacts{}, f.failure
	}
	return f.facts, ctx.Err()
}
func (f *reviewRuntimeClient) CurrentProjectItem(ctx context.Context, id string) (github.CurrentProjectItem, error) {
	if id == "ISSUEITEM" {
		i := f.issue.Identity
		return github.CurrentProjectItem{ID: id, ProjectID: "engineering", ContentKind: "Issue", Issue: &i}, ctx.Err()
	}
	return f.onboardingRuntimeClient.CurrentProjectItem(ctx, id)
}
func (f *reviewRuntimeClient) SetProjectOption(ctx context.Context, p, id, field, option string) error {
	if p != "engineering" || field != "status" || (id != "PRITEM" && id != "ISSUEITEM") {
		return fmt.Errorf("unauthorized field target %s/%s/%s", p, id, field)
	}
	if id == f.statusFailure && option == "in_progress" {
		return &github.Error{Category: github.Transient}
	}
	f.effects = append(f.effects, id+":"+option)
	for node, item := range f.items {
		if item.ID == id {
			item.Values[field] = github.FieldValue{Kind: config.SingleSelect, OptionID: option}
			f.items[node] = item
			return ctx.Err()
		}
	}
	return errors.New("missing item")
}
func (f *reviewRuntimeClient) ConvertPullRequestToDraft(ctx context.Context, id string) error {
	if id != "PR47" {
		return fmt.Errorf("unexpected Draft identity: %s", id)
	}
	f.drafts++
	f.effects = append(f.effects, "draft:PR47")
	f.pr.Draft = true
	if len(f.facts.Transitions) > 0 {
		f.facts.Transitions = append(f.facts.Transitions, github.ReviewTransition{ID: fmt.Sprintf("DRAFT%d", f.drafts), Kind: "ConvertToDraftEvent", PR: f.pr.Identity, CreatedAt: *f.now})
	}
	return ctx.Err()
}
func reviewReceipt(t *testing.T, r *runtime, id, event string, payload any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/webhooks/github", bytes.NewReader(body))
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
}
func TestSignedReviewLifecycleProductionWorkerAndRestart(t *testing.T) {
	c, base := onboardingRuntimeFixture(t)
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	c.Clock = func() time.Time { return now }
	c.Runner = nil
	f := &reviewRuntimeClient{onboardingRuntimeClient: base, now: &now, facts: github.ReviewFacts{PR: base.pr.Identity}}
	c.Client = f
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "branch", "issues")
	step(t, r)
	state(t, r, "branch", storage.Completed, 1)
	onboardingNotification(t, r, f, "onboard", "pull_request")
	step(t, r)
	state(t, r, "onboard", storage.Completed, 1)
	if f.drafts != 1 || f.adds != 1 {
		t.Fatal("initial onboarding absent")
	}
	f.effects = nil
	repository := map[string]any{"owner": map[string]any{"login": f.repo.Owner}, "name": f.repo.Name}
	a := func(actor github.Actor) map[string]any {
		return map[string]any{"node_id": actor.ID, "login": actor.Login, "type": actor.Type}
	}
	var oldReady, oldRework map[string]any
	for n := 1; n <= 2; n++ {
		now = now.Add(time.Minute)
		nativeAt := now
		f.pr.Draft = false
		f.facts.Transitions = append(f.facts.Transitions, github.ReviewTransition{ID: fmt.Sprintf("READY%d", n), Kind: "ReadyForReviewEvent", PR: f.pr.Identity, Actor: &f.user, CreatedAt: nativeAt})
		now = now.Add(time.Second)
		ready := map[string]any{"action": "ready_for_review", "repository": repository, "pull_request": map[string]any{"node_id": "PR47", "number": 47, "updated_at": nativeAt}, "sender": a(f.user)}
		id := fmt.Sprintf("ready%d", n)
		reviewReceipt(t, r, id, "pull_request", ready)
		reviewReceipt(t, r, id, "pull_request", ready)
		step(t, r)
		e := state(t, r, id, storage.Completed, 1)
		if e.Resource == nil || e.Resource.Kind != "pull_request" || e.Resource.Number != 47 {
			t.Fatal(e)
		}
		for _, node := range []string{"PR47", "I46"} {
			if f.items[node].Values["status"].OptionID != "in_review" {
				t.Fatal("did not enter review", f.items)
			}
		}
		now = now.Add(time.Minute)
		reviewer := github.Actor{ID: "REV", Login: "reviewer", Type: "User"}
		review := github.ReviewSubmission{ID: fmt.Sprintf("REWORK%d", n), PR: f.pr.Identity, Author: &reviewer, State: "CHANGES_REQUESTED", SubmittedAt: now}
		f.facts.Submissions = append(f.facts.Submissions, review)
		f.facts.LatestOpinionated = []github.ReviewSubmission{review}
		now = now.Add(time.Second)
		rework := map[string]any{"action": "submitted", "repository": repository, "pull_request": map[string]any{"node_id": "PR47", "number": 47}, "review": map[string]any{"node_id": review.ID, "state": "changes_requested", "submitted_at": review.SubmittedAt, "user": a(reviewer)}}
		id = fmt.Sprintf("rework%d", n)
		reviewReceipt(t, r, id, "pull_request_review", rework)
		step(t, r)
		state(t, r, id, storage.Completed, 1)
		reviewReceipt(t, r, id, "pull_request_review", rework)
		step(t, r)
		if !f.pr.Draft || f.drafts != n+1 {
			t.Fatal("draft replay", f.drafts)
		}
		if err := r.store.Close(); err != nil {
			t.Fatal(err)
		}
		r = openRuntime(t, c)
		if n == 2 {
			before := len(f.effects)
			reviewReceipt(t, r, "delayed-ready", "pull_request", oldReady)
			reviewReceipt(t, r, "delayed-rework", "pull_request_review", oldRework)
			step(t, r)
			step(t, r)
			state(t, r, "delayed-ready", storage.Completed, 1)
			state(t, r, "delayed-rework", storage.Completed, 1)
			if len(f.effects) != before {
				t.Fatal("old cycle wrote")
			}
		}
		oldReady, oldRework = ready, rework
	}
	want := []string{"PRITEM:in_review", "ISSUEITEM:in_review", "draft:PR47", "PRITEM:in_progress", "ISSUEITEM:in_progress", "PRITEM:in_review", "ISSUEITEM:in_review", "draft:PR47", "PRITEM:in_progress", "ISSUEITEM:in_progress"}
	if !reflect.DeepEqual(f.effects, want) {
		t.Fatal(f.effects, want)
	}
	if f.adds != 1 || f.creates != 1 || f.pr.Body != "Target: #46\nClassification: false" {
		t.Fatal("changed onboarding or binding")
	}
	before := len(f.effects)
	bad := map[string]any{"action": "submitted", "repository": repository, "pull_request": map[string]any{"node_id": "PR47", "number": 47}, "review": map[string]any{"node_id": "UNKNOWN", "state": "changes_requested", "submitted_at": now, "user": a(f.user)}}
	reviewReceipt(t, r, "unsupported", "pull_request_review", map[string]any{"action": "edited"})
	step(t, r)
	state(t, r, "unsupported", storage.Completed, 1)
	// Selector node disagreement cannot consume the freshly resolved PR authority.
	bad["pull_request"] = map[string]any{"node_id": "OTHER", "number": 47}
	reviewReceipt(t, r, "wrong-source", "pull_request_review", bad)
	step(t, r)
	e := state(t, r, "wrong-source", storage.Retryable, 1)
	if e.State.ErrorCategory != "pr_review_changed" || len(f.effects) != before {
		t.Fatal(e.State, f.effects)
	}
}
func TestReviewFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		err      error
		category string
		retry    bool
	}{
		{prreview.ErrAmbiguous, "pr_review_ambiguous", false}, {prreview.ErrSuperseded, "pr_review_superseded", false}, {prreview.ErrCorrelation, "pr_review_correlation", false}, {prreview.ErrProvenance, "pr_review_provenance", false}, {prreview.ErrStorage, "pr_review_storage", false}, {prreview.ErrRejected, "pr_review_rejected", false}, {prreview.ErrInvalid, "pr_review_invalid", false}, {prreview.ErrProject, "pr_review_project", false}, {prreview.ErrChanged, "pr_review_changed", true}, {github.ErrReviewFactsChanged, "pr_review_changed", true},
	} {
		for _, err := range []error{tc.err, fmt.Errorf("wrapped: %w", tc.err)} {
			got := classify(err)
			if got.Category != tc.category || got.Retryable != tc.retry {
				t.Fatal(got, tc)
			}
		}
	}
}

func TestWorkerNewReadyProgressesPastOlderPartialRework(t *testing.T) {
	c, base := onboardingRuntimeFixture(t)
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	c.Clock = func() time.Time { return now }
	f := &reviewRuntimeClient{onboardingRuntimeClient: base, now: &now, facts: github.ReviewFacts{PR: base.pr.Identity}}
	c.Client = f
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "branch", "issues")
	step(t, r)
	onboardingNotification(t, r, f, "onboard", "pull_request")
	step(t, r)
	state(t, r, "onboard", storage.Completed, 1)
	repository := map[string]any{"owner": map[string]any{"login": f.repo.Owner}, "name": f.repo.Name}
	actor := map[string]any{"node_id": f.user.ID, "login": f.user.Login, "type": f.user.Type}
	readyPayload := func(at time.Time) any {
		return map[string]any{"action": "ready_for_review", "repository": repository, "pull_request": map[string]any{"node_id": "PR47", "number": 47, "updated_at": at}, "sender": actor}
	}
	now = now.Add(time.Minute)
	first := now
	f.pr.Draft = false
	f.facts.Transitions = []github.ReviewTransition{{ID: "READY1", Kind: "ReadyForReviewEvent", PR: f.pr.Identity, Actor: &f.user, CreatedAt: first}}
	now = now.Add(time.Second)
	reviewReceipt(t, r, "ready1", "pull_request", readyPayload(first))
	step(t, r)
	state(t, r, "ready1", storage.Completed, 1)
	now = now.Add(time.Minute)
	review := github.ReviewSubmission{ID: "REWORK1", PR: f.pr.Identity, Author: &f.user, State: "CHANGES_REQUESTED", SubmittedAt: now}
	f.facts.Submissions = []github.ReviewSubmission{review}
	f.facts.LatestOpinionated = []github.ReviewSubmission{review}
	now = now.Add(time.Second)
	f.statusFailure = "ISSUEITEM"
	payload := map[string]any{"action": "submitted", "repository": repository, "pull_request": map[string]any{"node_id": "PR47", "number": 47}, "review": map[string]any{"node_id": review.ID, "state": "changes_requested", "submitted_at": review.SubmittedAt, "user": actor}}
	reviewReceipt(t, r, "partial-rework", "pull_request_review", payload)
	step(t, r)
	state(t, r, "partial-rework", storage.Retryable, 1)
	if f.drafts != 2 || f.items["PR47"].Values["status"].OptionID != "in_progress" || f.items["I46"].Values["status"].OptionID != "in_review" {
		t.Fatal("unexpected partial state", f.effects)
	}
	now = now.Add(time.Minute)
	second := now
	f.pr.Draft = false
	f.facts.Transitions = append(f.facts.Transitions, github.ReviewTransition{ID: "READY2", Kind: "ReadyForReviewEvent", PR: f.pr.Identity, Actor: &f.user, CreatedAt: second})
	now = now.Add(c.RetryDelay)
	f.statusFailure = ""
	before := len(f.effects)
	// A new Ready delivery shares the old delivery's real worker resource key.
	reviewReceipt(t, r, "ready2", "pull_request", readyPayload(second))
	step(t, r)
	state(t, r, "partial-rework", storage.Completed, 2)
	step(t, r)
	state(t, r, "ready2", storage.Completed, 1)
	if f.drafts != 2 || f.pr.Draft || f.items["PR47"].Values["status"].OptionID != "in_review" || f.items["I46"].Values["status"].OptionID != "in_review" {
		t.Fatal("older rework blocked or reverted new cycle", f.effects)
	}
	if !reflect.DeepEqual(f.effects[before:], []string{"PRITEM:in_review"}) {
		t.Fatal("stale target effect", f.effects[before:])
	}
}
