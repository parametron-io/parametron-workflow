package prreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var injected = errors.New("injected failure")

type fixture struct {
	t          *testing.T
	in         Input
	pr, target observe.ObservedState
	store      *storage.Store
	path       string
	cfg        Config
	c          *Controller
	now        time.Time
	writes     []string
	drafts     int
	bindCalls  int
	bindHook   func(int)
	statusHook func(string) error
	draftHook  func() error
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, in: policyInput(t)}
	f.now = f.in.Onboarding.CompletedAt
	f.path = filepath.Join(t.TempDir(), "review.sqlite")
	var err error
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	projects := func(item string) []observe.ProjectState {
		return []observe.ProjectState{{ID: "ENG", Profile: config.Engineering, Items: []observe.ProjectItem{{ID: item, Fields: map[config.FieldRole]observe.FieldValue{config.Status: {FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "PROGRESS"}, StatusRole: config.InProgress}, config.StartDate: {FieldValue: github.FieldValue{Kind: config.Date, Date: "2026-10-01"}}, config.RoadmapOrder: {FieldValue: github.FieldValue{Kind: config.Number, Number: 17}}}}}}, {ID: "BUG", Profile: config.BugTracker}}
	}
	f.pr = observe.ObservedState{Resource: resource(f.in.PR.Identity, "pull_request"), Presence: observe.Present, PullRequest: &f.in.PR, Projects: projects("PRITEM")}
	f.target = observe.ObservedState{Resource: resource(f.in.Binding.Evidence.Target, "issue"), Presence: observe.Present, Issue: &github.Issue{Identity: f.in.Binding.Evidence.Target, State: "OPEN", Body: "", Assignees: []github.Actor{f.in.Binding.Evidence.Developer}}, Projects: projects("TARGETITEM")}
	f.cfg = Config{Deployment: config.ResolvedConfig{Organization: config.Organization{ID: "ORG", Login: "parametron-io"}, Repositories: []config.Repository{f.in.PR.Repository}, Engineering: config.ResolvedProject{ID: "ENG", Fields: map[config.FieldRole]string{config.Status: "STATUS"}, StatusOptions: map[config.StatusRole]string{config.InProgress: "PROGRESS", config.InReview: "REVIEW"}}, BugTracker: config.ResolvedProject{ID: "BUG"}}, Binding: f, Onboarding: f, Reader: f, Reviews: f, PRItems: f, Items: f, Status: f, Draft: f, Store: f.store, Clock: func() time.Time { return f.now }}
	f.rebuild()
	return f
}
func (f *fixture) rebuild() {
	f.t.Helper()
	var err error
	f.c, err = New(f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
}
func clone[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err = json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func (f *fixture) Evaluate(ctx context.Context, r storage.Resource) (prbinding.Decision, error) {
	f.bindCalls++
	if f.bindHook != nil {
		f.bindHook(f.bindCalls)
	}
	return clone(f.t, f.in.Binding), ctx.Err()
}
func (f *fixture) Completed(ctx context.Context, r storage.Resource) (pronboarding.Completion, error) {
	return clone(f.t, f.in.Onboarding), ctx.Err()
}
func (f *fixture) ReadCurrent(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	if r == f.pr.Resource {
		return clone(f.t, f.pr), ctx.Err()
	}
	if r == f.target.Resource {
		return clone(f.t, f.target), ctx.Err()
	}
	f.t.Fatalf("unexpected resource %+v", r)
	return observe.ObservedState{}, nil
}
func (f *fixture) PullRequestReviewFacts(ctx context.Context, pr github.Identity) (github.ReviewFacts, error) {
	if pr != f.in.PR.Identity {
		f.t.Fatal(pr)
	}
	return clone(f.t, f.in.Facts), ctx.Err()
}
func (f *fixture) CurrentPullRequestProjectItem(ctx context.Context, id string) (github.PullRequestProjectItem, error) {
	if id != "PRITEM" {
		f.t.Fatal(id)
	}
	return github.PullRequestProjectItem{ID: id, ProjectID: "ENG", ContentKind: "PullRequest", PullRequest: &f.in.Binding.Evidence.PR}, ctx.Err()
}
func (f *fixture) CurrentProjectItem(ctx context.Context, id string) (github.CurrentProjectItem, error) {
	if id != "TARGETITEM" {
		f.t.Fatal(id)
	}
	return github.CurrentProjectItem{ID: id, ProjectID: "ENG", ContentKind: "Issue", Issue: &f.in.Binding.Evidence.Target}, ctx.Err()
}
func (f *fixture) SetProjectOption(ctx context.Context, p, item, field, option string) error {
	if p != "ENG" || field != "STATUS" || (item != "PRITEM" && item != "TARGETITEM") || (option != "REVIEW" && option != "PROGRESS") {
		f.t.Fatalf("wrong write %s %s %s %s", p, item, field, option)
	}
	f.writes = append(f.writes, item+":"+option)
	o := &f.pr
	if item == "TARGETITEM" {
		o = &f.target
	}
	role := config.InProgress
	if option == "REVIEW" {
		role = config.InReview
	}
	o.Projects[0].Items[0].Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: option}, StatusRole: role}
	if f.statusHook != nil {
		return f.statusHook(item)
	}
	return ctx.Err()
}
func (f *fixture) ConvertPullRequestToDraft(ctx context.Context, id string) error {
	if id != "PR" {
		f.t.Fatal(id)
	}
	f.drafts++
	f.writes = append(f.writes, "draft:PR")
	f.in.PR.Draft = true
	f.in.Facts.Transitions = append(f.in.Facts.Transitions, github.ReviewTransition{ID: fmt.Sprintf("DRAFT%d", f.drafts), Kind: "ConvertToDraftEvent", PR: f.in.PR.Identity, CreatedAt: f.now})
	if f.draftHook != nil {
		return f.draftHook()
	}
	return ctx.Err()
}
func (f *fixture) event(id string, c Candidate) storage.Event {
	f.t.Helper()
	f.now = c.At.Add(time.Second)
	a := map[string]any{"node_id": c.Actor.ID, "login": c.Actor.Login, "type": c.Actor.Type}
	p := map[string]any{"node_id": c.PRID, "number": c.Number, "updated_at": c.At}
	root := map[string]any{"action": "ready_for_review", "pull_request": p, "sender": a}
	name := "pull_request"
	if c.Kind == "review" {
		name = "pull_request_review"
		root["action"] = "submitted"
		root["review"] = map[string]any{"node_id": c.ReviewID, "state": c.State, "submitted_at": c.At, "user": a}
	}
	b, _ := json.Marshal(root)
	_, err := f.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: name, Payload: b, ReceivedAt: f.now, Resource: &f.pr.Resource})
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := f.store.Event(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}
func (f *fixture) ready(id string, at time.Time) storage.Event {
	dev := f.in.Binding.Evidence.Developer
	r := github.ReviewTransition{ID: id, Kind: "ReadyForReviewEvent", PR: f.in.PR.Identity, Actor: &dev, CreatedAt: at}
	f.in.PR.Draft = false
	if id != "READY1" {
		f.in.Facts.Transitions = append(f.in.Facts.Transitions, r)
	} else {
		f.in.Facts.Transitions[0] = r
	}
	return f.event(id, Candidate{Kind: "ready", PRID: "PR", Number: 49, Actor: dev, At: at})
}
func (f *fixture) review(id, state string, at time.Time) storage.Event {
	a := github.Actor{ID: "REV", Login: "reviewer", Type: "User"}
	r := github.ReviewSubmission{ID: id, PR: f.in.PR.Identity, Author: &a, State: state, SubmittedAt: at}
	f.in.Facts.Submissions = append(f.in.Facts.Submissions, r)
	f.in.Facts.LatestOpinionated = []github.ReviewSubmission{r}
	return f.event(id, Candidate{Kind: "review", PRID: "PR", Number: 49, ReviewID: id, State: state, Actor: a, At: at})
}
func (f *fixture) handle(e storage.Event) {
	f.t.Helper()
	if err := f.c.Handle(context.Background(), e); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) restart() {
	f.t.Helper()
	if err := f.store.Close(); err != nil {
		f.t.Fatal(err)
	}
	var err error
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.cfg.Store = f.store
	if f.cfg.Onboarding != f {
		f.cfg.Onboarding, err = pronboarding.New(pronboarding.Config{Deployment: f.cfg.Deployment, Binding: f, Reader: f, Projects: f, Items: f, Draft: f, Store: f.store, Clock: f.cfg.Clock})
		if err != nil {
			f.t.Fatal(err)
		}
	}
	f.rebuild()
}

func (f *fixture) AddProjectItem(context.Context, string, string) (string, error) {
	f.t.Fatal("unexpected membership mutation")
	return "", nil
}

func (f *fixture) onboard() {
	f.t.Helper()
	f.in.PR.Draft = true
	c, err := pronboarding.New(pronboarding.Config{Deployment: f.cfg.Deployment, Binding: f, Reader: f, Projects: f, Items: f, Draft: f, Store: f.store, Clock: f.cfg.Clock})
	if err != nil {
		f.t.Fatal(err)
	}
	_, err = f.store.InsertDelivery(context.Background(), storage.Delivery{ID: "onboard", EventName: "pull_request", Payload: []byte(`{"action":"opened","pull_request":{"node_id":"PR","number":49}}`), ReceivedAt: f.now, Resource: &f.pr.Resource})
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := f.store.Event(context.Background(), "onboard")
	if err != nil {
		f.t.Fatal(err)
	}
	if err = c.Handle(context.Background(), e); err != nil {
		f.t.Fatal(err)
	}
	f.in.Onboarding, err = c.Completed(context.Background(), f.pr.Resource)
	if err != nil || !f.in.Onboarding.Completed {
		f.t.Fatal(f.in.Onboarding, err)
	}
	f.cfg.Onboarding = c
	f.rebuild()
}
func TestControllerThreeCyclesRestartAndStaleDelivery(t *testing.T) {
	f := setup(t)
	f.onboard()
	base := f.in.Onboarding.CompletedAt
	unchangedPR := clone(t, f.in.PR)
	unchangedTarget := clone(t, *f.target.Issue)
	var old []storage.Event
	for n := 1; n <= 3; n++ {
		ready := f.ready(fmt.Sprintf("READY%d", n), base.Add(time.Duration(n*10)*time.Minute))
		f.handle(ready)
		f.handle(ready)
		approval := f.review(fmt.Sprintf("APPROVAL%d", n), "APPROVED", base.Add(time.Duration(n*10+1)*time.Minute))
		before := len(f.writes)
		f.handle(approval)
		if len(f.writes) != before {
			t.Fatal("approval wrote")
		}
		comment := f.review(fmt.Sprintf("COMMENT%d", n), "COMMENTED", base.Add(time.Duration(n*10+2)*time.Minute))
		f.handle(comment)
		if len(f.writes) != before {
			t.Fatal("comment wrote")
		}
		rework := f.review(fmt.Sprintf("REWORK%d", n), "CHANGES_REQUESTED", base.Add(time.Duration(n*10+3)*time.Minute))
		f.handle(rework)
		f.handle(rework)
		old = append(old, ready, rework)
		f.restart()
		before = len(f.writes)
		for _, e := range old {
			f.handle(e)
		}
		if len(f.writes) != before || f.drafts != n {
			t.Fatal("old delivery replayed", f.writes)
		}
	}
	want := []string{}
	for n := 0; n < 3; n++ {
		want = append(want, "PRITEM:REVIEW", "TARGETITEM:REVIEW", "draft:PR", "PRITEM:PROGRESS", "TARGETITEM:PROGRESS")
	}
	if !reflect.DeepEqual(f.writes, want) {
		t.Fatalf("effect order %v", f.writes)
	}
	records, err := f.c.load(context.Background())
	if err != nil || len(records) != 3 {
		t.Fatal(records, err)
	}
	last, err := latest(records, f.in.PR.Identity)
	if err != nil || last.start.Previous != "READY2" || last.attempt.Review.ID != "REWORK3" {
		t.Fatal(last, err)
	}
	for _, ns := range []string{startNS, attemptNS, doneNS} {
		ps, err := f.store.ProvenanceRecords(context.Background(), ns)
		if err != nil || len(ps) != 3 {
			t.Fatal(ns, ps, err)
		}
		for n, p := range ps {
			want := fmt.Sprintf("READY%d", n+1)
			if ns != startNS {
				want = fmt.Sprintf("REWORK%d", n+1)
			}
			if p.DeliveryID != want {
				t.Fatal("lost original delivery", p)
			}
		}
	}
	unchangedPR.Draft = true
	if !reflect.DeepEqual(f.in.PR, unchangedPR) || !reflect.DeepEqual(*f.target.Issue, unchangedTarget) {
		t.Fatal("unrelated identity state changed")
	}
	for _, o := range []observe.ObservedState{f.pr, f.target} {
		fields := o.Projects[0].Items[0].Fields
		if fields[config.StartDate].Date != "2026-10-01" || fields[config.RoadmapOrder].Number != 17 {
			t.Fatal(fields)
		}
	}
}
func TestControllerOwnership(t *testing.T) {
	for _, pr := range []bool{true, false} {
		for _, target := range []bool{true, false} {
			t.Run(fmt.Sprintf("%t/%t", pr, target), func(t *testing.T) {
				f := setup(t)
				if !pr {
					f.in.PR.Body = "Set-Status: false"
				}
				if !target {
					f.target.Issue.Body = "Set-Status: false"
				}
				f.handle(f.ready("READY1", f.in.Candidate.At))
				f.handle(f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute)))
				want := []string{}
				if pr {
					want = append(want, "PRITEM:REVIEW")
				}
				if target {
					want = append(want, "TARGETITEM:REVIEW")
				}
				want = append(want, "draft:PR")
				if pr {
					want = append(want, "PRITEM:PROGRESS")
				}
				if target {
					want = append(want, "TARGETITEM:PROGRESS")
				}
				if !reflect.DeepEqual(f.writes, want) {
					t.Fatal(f.writes, want)
				}
			})
		}
	}
}
func TestControllerRejectsAuthorizationDrift(t *testing.T) {
	for _, name := range []string{"missing_completion", "changed_target", "closed_target", "closed_pr", "missing_pr", "missing_target", "changed_author", "changed_branch", "changed_head", "changed_project", "wrong_item", "binding_rejected", "between_reads"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			e := f.ready("READY1", f.in.Candidate.At)
			switch name {
			case "missing_completion":
				f.in.Onboarding.Completed = false
			case "changed_target":
				f.in.Onboarding.Binding.Target.ID = "OTHER"
			case "closed_target":
				f.target.Issue.State = "CLOSED"
			case "closed_pr":
				f.in.PR.State = "CLOSED"
			case "missing_target":
				f.target.Presence = observe.Missing
			case "changed_author":
				f.in.PR.Author = &github.Actor{ID: "OTHER", Login: "outsider", Type: "User"}
			case "changed_branch":
				f.in.PR.HeadRef = "manual"
			case "changed_head":
				f.in.PR.HeadSHA = "changed"
			case "changed_project":
				f.in.Onboarding.Project = "OTHER"
			case "wrong_item":
				f.pr.Projects[0].Items[0].Archived = true
			case "missing_pr":
				f.pr.Presence = observe.Missing
			case "binding_rejected":
				f.in.Binding.Outcome = prbinding.Rejected
			case "between_reads":
				f.bindHook = func(n int) {
					if n == 2 {
						f.in.Binding.Outcome = prbinding.Rejected
					}
				}
			}
			if err := f.c.Handle(context.Background(), e); err == nil {
				t.Fatal("accepted drift")
			}
			if len(f.writes) != 0 {
				t.Fatal(f.writes)
			}
		})
	}
}
func TestPartialStatusRecoveryAndSupersession(t *testing.T) {
	for _, item := range []string{"PRITEM", "TARGETITEM"} {
		t.Run(item, func(t *testing.T) {
			f := setup(t)
			e := f.ready("READY1", f.in.Candidate.At)
			f.statusHook = func(id string) error {
				if id == item {
					return injected
				}
				return nil
			}
			if err := f.c.Handle(context.Background(), e); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			f.statusHook = nil
			f.restart()
			f.handle(e)
			if len(f.writes) != 2 {
				t.Fatal("did not converge", f.writes)
			}
			rework := f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute))
			f.statusHook = func(id string) error {
				if id == item {
					return injected
				}
				return nil
			}
			if err := f.c.Handle(context.Background(), rework); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			f.statusHook = nil
			next := f.ready("READY2", f.in.Candidate.At.Add(3*time.Minute))
			before := len(f.writes)
			if err := f.c.Converge(context.Background(), f.pr.Resource); err != nil {
				t.Fatal(err)
			}
			f.handle(rework)
			if len(f.writes) != before {
				t.Fatal("older rework reverted newer readiness")
			}
			f.handle(next)
			if f.drafts != 1 {
				t.Fatal("replayed draft")
			}
		})
	}
}
func TestAmbiguousDraftNeverReplayed(t *testing.T) {
	for _, name := range []string{"provider_error_after_effect", "cancel_after_effect", "receipt_failure", "attempt_failure"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.handle(f.ready("READY1", f.in.Candidate.At))
			e := f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "provider_error_after_effect":
				f.draftHook = func() error { return injected }
			case "cancel_after_effect":
				f.draftHook = func() error { cancel(); return ctx.Err() }
			case "receipt_failure":
				f.cfg.Store = failStore{Store: f.store, ns: doneNS}
				f.rebuild()
			case "attempt_failure":
				f.cfg.Store = failStore{Store: f.store, ns: attemptNS}
				f.rebuild()
			}
			if err := f.c.Handle(ctx, e); err == nil {
				t.Fatal("expected interruption")
			}
			before := len(f.writes)
			f.cfg.Store = f.store
			f.draftHook = nil
			f.restart()
			err := f.c.Handle(context.Background(), e)
			if name == "attempt_failure" {
				if err != nil || f.drafts != 1 {
					t.Fatal(err, f.drafts)
				}
			} else {
				if !errors.Is(err, ErrAmbiguous) || len(f.writes) != before || f.drafts != 1 {
					t.Fatal("ambiguous replay", err, f.writes)
				}
			}
		})
	}
}

type failStore struct {
	*storage.Store
	ns string
}

func (s failStore) RecordProvenance(ctx context.Context, p storage.Provenance) (bool, error) {
	if p.Namespace == s.ns {
		return false, injected
	}
	return s.Store.RecordProvenance(ctx, p)
}

type boundaryStore struct {
	*storage.Store
	hook func(string)
}

func (s boundaryStore) RecordProvenance(ctx context.Context, p storage.Provenance) (bool, error) {
	ok, err := s.Store.RecordProvenance(ctx, p)
	if err == nil {
		s.hook(p.Namespace)
	}
	return ok, err
}
func TestCancellationAndAuthorizationAfterPersistence(t *testing.T) {
	for _, stage := range []string{startNS, attemptNS, doneNS} {
		for _, kind := range []string{"cancel", "reject"} {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				f := setup(t)
				ready := f.ready("READY1", f.in.Candidate.At)
				e := ready
				if stage != startNS {
					f.handle(ready)
					e = f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute))
				}
				before := len(f.writes)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.cfg.Store = boundaryStore{Store: f.store, hook: func(ns string) {
					if ns == stage {
						if kind == "cancel" {
							cancel()
						} else {
							f.in.Binding.Outcome = prbinding.Rejected
						}
					}
				}}
				f.rebuild()
				err := f.c.Handle(ctx, e)
				want := ErrRejected
				if kind == "cancel" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatal(err, want)
				}
				allowed := before
				if stage == doneNS {
					allowed++
				}
				if len(f.writes) != allowed {
					t.Fatal("persistence supplied permanent authority", f.writes)
				}
				f.cfg.Store = f.store
				f.in.Binding.Outcome = prbinding.Accepted
				f.restart()
				err = f.c.Handle(context.Background(), e)
				if stage == attemptNS {
					if !errors.Is(err, ErrAmbiguous) || f.drafts != 0 {
						t.Fatal(err, f.drafts)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
func TestStatusAuthorizationDriftBetweenPRAndTarget(t *testing.T) {
	for _, rework := range []bool{false, true} {
		t.Run(fmt.Sprint(rework), func(t *testing.T) {
			f := setup(t)
			e := f.ready("READY1", f.in.Candidate.At)
			if rework {
				f.handle(e)
				e = f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute))
			}
			before := len(f.writes)
			f.statusHook = func(item string) error {
				if item == "PRITEM" {
					f.in.Binding.Outcome = prbinding.Rejected
				}
				return nil
			}
			err := f.c.Handle(context.Background(), e)
			if !errors.Is(err, ErrRejected) {
				t.Fatal(err)
			}
			want := before + 1
			if rework {
				want++
			}
			if len(f.writes) != want || f.writes[len(f.writes)-1][:6] != "PRITEM" {
				t.Fatal("unauthorized Target write", f.writes)
			}
		})
	}
}
func TestEquivalentExecutionsHaveEquivalentEffectsAndEvidence(t *testing.T) {
	run := func() ([]string, []storage.Provenance) {
		f := setup(t)
		f.handle(f.ready("READY1", f.in.Candidate.At))
		f.handle(f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute)))
		var ps []storage.Provenance
		for _, ns := range []string{startNS, attemptNS, doneNS} {
			p, err := f.store.ProvenanceRecords(context.Background(), ns)
			if err != nil {
				t.Fatal(err)
			}
			ps = append(ps, p...)
		}
		return f.writes, ps
	}
	a, pa := run()
	b, pb := run()
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(pa, pb) {
		t.Fatal("nondeterministic intended effects or provenance")
	}
}

type beforeStatusFailure struct {
	f     *fixture
	item  string
	calls []string
}

func (w *beforeStatusFailure) SetProjectOption(ctx context.Context, p, item, field, option string) error {
	w.calls = append(w.calls, item)
	if item == w.item {
		return injected
	}
	return w.f.SetProjectOption(ctx, p, item, field, option)
}
func TestStatusFailureBeforeWriteConvergesFromFreshState(t *testing.T) {
	for _, rework := range []bool{false, true} {
		for _, item := range []string{"PRITEM", "TARGETITEM"} {
			t.Run(fmt.Sprintf("%t/%s", rework, item), func(t *testing.T) {
				f := setup(t)
				e := f.ready("READY1", f.in.Candidate.At)
				if rework {
					f.handle(e)
					e = f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute))
				}
				writer := &beforeStatusFailure{f: f, item: item}
				f.cfg.Status = writer
				f.rebuild()
				if err := f.c.Handle(context.Background(), e); !errors.Is(err, injected) {
					t.Fatal(err)
				}
				wantCalls := []string{"PRITEM"}
				if item == "TARGETITEM" {
					wantCalls = append(wantCalls, "TARGETITEM")
				}
				if !reflect.DeepEqual(writer.calls, wantCalls) {
					t.Fatal(writer.calls)
				}
				f.cfg.Status = f
				f.restart()
				f.handle(e)
				option := "REVIEW"
				wantDrafts := 0
				if rework {
					option = "PROGRESS"
					wantDrafts = 1
				}
				if f.pr.Projects[0].Items[0].Fields[config.Status].OptionID != option || f.target.Projects[0].Items[0].Fields[config.Status].OptionID != option || f.drafts != wantDrafts {
					t.Fatal(f.writes)
				}
				before := len(f.writes)
				f.handle(e)
				if len(f.writes) != before {
					t.Fatal("duplicate convergence wrote")
				}
			})
		}
	}
}
