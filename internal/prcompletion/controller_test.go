package prcompletion

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type completionFixture struct {
	t          *testing.T
	in         Input
	project    config.ResolvedProject
	cfg        Config
	controller *Controller
	store      *storage.Store
	path       string
	status     map[string]config.StatusRole
	closes     int
	writes     []string
	closeHook  func() error
	recordHook func(string) error
	readHook   func()
}

func newCompletionFixture(t *testing.T) *completionFixture {
	t.Helper()
	f := &completionFixture{t: t, in: completionInput(), status: map[string]config.StatusRole{"PRITEM": config.InProgress, "TARGETITEM": config.InProgress}, path: filepath.Join(t.TempDir(), "completion.sqlite")}
	f.in.Target.Body = "Classification: false"
	f.in.Target.Type = &github.IssueType{ID: "type-Task", Name: "Task"}
	f.in.Target.Assignees = []github.Actor{f.in.Onboarding.Binding.Developer}
	f.project = config.ResolvedProject{ID: "ENG", Fields: map[config.FieldRole]string{config.Status: "STATUS"}, StatusOptions: map[config.StatusRole]string{}}
	for _, s := range []config.StatusRole{config.Backlog, config.Blocked, config.Ready, config.InProgress, config.InReview, config.Done} {
		f.project.StatusOptions[s] = string(s)
	}
	d := config.ResolvedConfig{Organization: config.Organization{ID: "ORG", Login: "parametron-io"}, Repositories: []config.Repository{f.in.Target.Repository}, Engineering: f.project, BugTracker: config.ResolvedProject{ID: "BUG"}, IssueTypes: map[string]string{"Task": "type-Task", "Phase": "type-Phase", "Feature": "type-Feature", "Bug": "type-Bug"}}
	var err error
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	resolver, err := ec.New(ec.Config{Deployment: d, Primary: f, Lister: f, Accepted: f})
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = Config{Deployment: d, Reader: f, Snapshots: resolver, Onboarding: f, Binding: f, Review: f, Native: f, Issues: f, Developers: f, Items: f, PRItems: f, Fields: f, Store: f, Clock: func() time.Time { return f.in.Native.Transitions[0].CreatedAt.Add(time.Hour) }}
	f.rebuild()
	f.delivery("merge")
	return f
}
func (f *completionFixture) rebuild() {
	f.t.Helper()
	var err error
	f.controller, err = New(f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *completionFixture) delivery(id string) storage.Event {
	f.t.Helper()
	r := resource(f.in.PR.Identity, "pull_request")
	_, err := f.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: "pull_request", Payload: []byte(`{}`), ReceivedAt: f.cfg.Clock(), Resource: &r})
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := f.store.Event(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}
func (f *completionFixture) ReadPrimary(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	i := f.in.Target
	return observe.ObservedState{Resource: resource(i.Identity, "issue"), Presence: observe.Present, Issue: &i}, ctx.Err()
}
func (f *completionFixture) ReadCurrent(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	if f.readHook != nil {
		f.readHook()
	}
	var o observe.ObservedState
	item := "TARGETITEM"
	if r == resource(f.in.PR.Identity, "pull_request") {
		p := f.in.PR
		o = observe.ObservedState{Resource: r, Presence: observe.Present, PullRequest: &p}
		item = "PRITEM"
	} else if r == resource(f.in.Target.Identity, "issue") {
		o, _ = f.ReadPrimary(ctx, r)
	} else {
		f.t.Fatalf("unrelated resource %+v", r)
	}
	s := f.status[item]
	o.Projects = []observe.ProjectState{{ID: "ENG", Profile: config.Engineering, Items: []observe.ProjectItem{{ID: item, Fields: map[config.FieldRole]observe.FieldValue{config.Status: {FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: f.project.StatusOptions[s]}, StatusRole: s}}}}}, {ID: "BUG", Profile: config.BugTracker}}
	return o, ctx.Err()
}
func (f *completionFixture) ListIssues(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
	i := f.in.Target
	return []github.ListedIssue{{Identity: i.Identity, Body: i.Body, State: i.State, Type: i.Type}}, ctx.Err()
}
func (f *completionFixture) AcceptedIssue(context.Context, storage.Resource) (sp.AcceptedIssue, bool, error) {
	return sp.AcceptedIssue{}, false, nil
}
func (f *completionFixture) Completed(ctx context.Context, r storage.Resource) (pronboarding.Completion, error) {
	return f.in.Onboarding, ctx.Err()
}
func (f *completionFixture) Completions(ctx context.Context) ([]pronboarding.Completion, error) {
	return []pronboarding.Completion{f.in.Onboarding}, ctx.Err()
}
func (f *completionFixture) Evaluate(ctx context.Context, r storage.Resource) (prbinding.Decision, error) {
	return prbinding.Decision{Outcome: prbinding.Accepted, Evidence: f.in.Onboarding.Binding}, ctx.Err()
}
func (f *completionFixture) PRStatus(ctx context.Context, r storage.Resource) (config.StatusRole, error) {
	return config.InReview, ctx.Err()
}
func (f *completionFixture) CanonicalTargetStatus(ctx context.Context, r storage.Resource) (config.StatusRole, bool, error) {
	return "", false, ctx.Err()
}
func (f *completionFixture) NativeLifecycle(ctx context.Context, i github.Identity, k string) (github.NativeLifecycle, error) {
	if i == f.in.PR.Identity {
		return f.in.Native, ctx.Err()
	}
	if i != f.in.Target.Identity || k != "issue" {
		f.t.Fatal("unrelated native read", i, k)
	}
	return github.NativeLifecycle{Resource: i, Kind: k, State: f.in.Target.State}, ctx.Err()
}
func (f *completionFixture) CloseIssue(ctx context.Context, i github.Identity) (github.IssueStateReceipt, error) {
	if i != f.in.Target.Identity {
		f.t.Fatal("unrelated close", i)
	}
	if err := ctx.Err(); err != nil {
		return github.IssueStateReceipt{}, err
	}
	f.closes++
	f.in.Target.State = "CLOSED"
	if f.closeHook != nil {
		if err := f.closeHook(); err != nil {
			return github.IssueStateReceipt{}, err
		}
	}
	return github.IssueStateReceipt{Issue: i, State: "CLOSED"}, nil
}
func (f *completionFixture) ReopenIssue(context.Context, github.Identity) (github.IssueStateReceipt, error) {
	f.t.Fatal("unexpected reopen")
	return github.IssueStateReceipt{}, nil
}
func (f *completionFixture) User(ctx context.Context, login string) (github.Actor, error) {
	return f.in.Onboarding.Binding.Developer, ctx.Err()
}
func (f *completionFixture) OrganizationMembership(ctx context.Context, o config.Organization, login string) (github.Membership, error) {
	return github.Membership{Organization: o, User: f.in.Onboarding.Binding.Developer, State: "active"}, ctx.Err()
}
func (f *completionFixture) RepositoryPermission(ctx context.Context, r github.Repository, login string) (github.Permission, error) {
	return github.Permission{Repository: r, User: f.in.Onboarding.Binding.Developer, Base: "write"}, ctx.Err()
}
func (f *completionFixture) CurrentProjectItem(ctx context.Context, id string) (github.CurrentProjectItem, error) {
	if id != "TARGETITEM" {
		f.t.Fatal(id)
	}
	i := f.in.Target.Identity
	return github.CurrentProjectItem{ID: id, ProjectID: "ENG", ContentKind: "Issue", Issue: &i}, ctx.Err()
}
func (f *completionFixture) CurrentPullRequestProjectItem(ctx context.Context, id string) (github.PullRequestProjectItem, error) {
	if id != "PRITEM" {
		f.t.Fatal(id)
	}
	i := f.in.PR.Identity
	return github.PullRequestProjectItem{ID: id, ProjectID: "ENG", ContentKind: "PullRequest", PullRequest: &i}, ctx.Err()
}
func (f *completionFixture) SetProjectOption(ctx context.Context, p, id, field, option string) error {
	if p != "ENG" || field != "STATUS" {
		f.t.Fatal(p, id, field)
	}
	if _, ok := f.status[id]; !ok {
		f.t.Fatal("unrelated item", id)
	}
	f.status[id] = config.StatusRole(option)
	f.writes = append(f.writes, id+":"+option)
	return ctx.Err()
}
func (f *completionFixture) Event(ctx context.Context, id string) (storage.Event, error) {
	return f.store.Event(ctx, id)
}
func (f *completionFixture) Provenance(ctx context.Context, ns, key string) (storage.Provenance, error) {
	return f.store.Provenance(ctx, ns, key)
}
func (f *completionFixture) RecordProvenance(ctx context.Context, p storage.Provenance) (bool, error) {
	if f.recordHook != nil {
		if err := f.recordHook(p.Namespace); err != nil {
			return false, err
		}
	}
	return f.store.RecordProvenance(ctx, p)
}

func TestMergeCloseReceiptSurvivesRestartAndReopen(t *testing.T) {
	f := newCompletionFixture(t)
	e := f.delivery("merge")
	r, err := f.controller.Handle(context.Background(), e)
	if err != nil || r.Outcome != Merged || f.closes != 1 || f.status["PRITEM"] != config.Done || f.in.Target.State != "CLOSED" {
		t.Fatal(r, err, f.closes, f.status)
	}
	for _, ns := range []string{attemptNS, completedNS} {
		if _, err := f.store.Provenance(context.Background(), ns, closeKey(*e.Resource)); err != nil {
			t.Fatal(ns, err)
		}
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.rebuild()
	f.in.Target.State = "OPEN"
	_, err = f.controller.Handle(context.Background(), f.delivery("delayed"))
	if err != nil || f.closes != 1 || f.in.Target.State != "OPEN" {
		t.Fatal("reclosed reopened target", err, f.closes)
	}
}

func TestIncompleteCloseNeverInfersReceiptOrRetries(t *testing.T) {
	for _, mode := range []string{"lost response", "lost receipt", "cancel after attempt", "cancel after close"} {
		t.Run(mode, func(t *testing.T) {
			f := newCompletionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := errors.New("injected")
			switch mode {
			case "lost response":
				f.closeHook = func() error { return injected }
			case "cancel after close":
				f.closeHook = func() error { cancel(); return nil }
			case "lost receipt":
				f.recordHook = func(ns string) error {
					if ns == completedNS {
						return injected
					}
					return nil
				}
			case "cancel after attempt":
				f.readHook = func() {
					if _, err := f.store.Provenance(context.Background(), attemptNS, closeKey(resource(f.in.PR.Identity, "pull_request"))); err == nil {
						cancel()
					}
				}
			}
			_, err := f.controller.Handle(ctx, f.delivery("merge"))
			if err == nil {
				t.Fatal("accepted incomplete effect")
			}
			f.closeHook = nil
			f.recordHook = nil
			f.readHook = nil
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.store, err = storage.Open(context.Background(), f.path)
			if err != nil {
				t.Fatal(err)
			}
			f.rebuild()
			before := f.closes
			_, err = f.controller.Handle(context.Background(), f.delivery("retry-current-state"))
			if !errors.Is(err, ErrAmbiguous) || f.closes != before {
				t.Fatal("current state manufactured a completion receipt", err, f.closes)
			}
			f.in.Target.State = "OPEN"
			_, err = f.controller.Handle(context.Background(), f.delivery("retry"))
			if !errors.Is(err, ErrAmbiguous) || f.closes != before || f.in.Target.State != "OPEN" {
				t.Fatal(err, f.closes, before)
			}
		})
	}
}

func TestCloseProvenanceAdversarial(t *testing.T) {
	for _, mode := range []string{"attempt only", "receipt only", "corrupt", "different target", "wrong namespace", "wrong key", "wrong delivery", "completed receipt"} {
		t.Run(mode, func(t *testing.T) {
			f := newCompletionFixture(t)
			e := f.delivery("merge")
			a, err := f.controller.fresh(context.Background(), *e.Resource)
			if err != nil {
				t.Fatal(err)
			}
			ev := closeEvidence{Version: 1, Onboarding: a.input.Onboarding, Merge: a.decision.Merge}
			p := storage.Provenance{Namespace: attemptNS, Key: closeKey(*e.Resource), DeliveryID: e.Delivery.ID, CreatedAt: f.cfg.Clock()}
			value := any(ev)
			want := ErrProvenance
			switch mode {
			case "attempt only":
				want = ErrAmbiguous
			case "receipt only":
				p.Namespace = completedNS
				value = closeReceipt{Version: 1, Evidence: ev, Disposition: "mutation", Receipt: &github.IssueStateReceipt{Issue: f.in.Target.Identity, State: "CLOSED"}}
			case "different target":
				ev.Onboarding.Binding.Target.ID = "OTHER"
				value = ev
			case "wrong namespace":
				p.Namespace = "other"
				want = nil
			case "wrong key":
				p.Key = "other"
				want = nil
			case "wrong delivery":
				other := f.delivery("other")
				r := *other.Resource
				r.Number++
				_, err = f.store.InsertDelivery(context.Background(), storage.Delivery{ID: "unrelated", EventName: "pull_request", Payload: []byte(`{}`), ReceivedAt: f.cfg.Clock(), Resource: &r})
				if err != nil {
					t.Fatal(err)
				}
				p.DeliveryID = "unrelated"
			case "completed receipt":
				p.Namespace = completedNS
				value = closeReceipt{Version: 1, Evidence: ev, Disposition: "already_closed"}
				want = nil
			}
			p.Metadata, _ = json.Marshal(value)
			if mode == "corrupt" {
				p.Metadata = []byte(`{"Version":`)
			}
			if _, err = f.store.RecordProvenance(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			_, err = f.controller.Handle(context.Background(), e)
			if !errors.Is(err, want) {
				t.Fatal(err, want)
			}
			if want != nil && f.closes != 0 {
				t.Fatal("unsafe mutation", f.closes)
			}
			if mode == "completed receipt" && f.closes != 0 {
				t.Fatal("receipt did not prevent repeat close")
			}
		})
	}
}

func TestCompletionClosedOpenAndPhase(t *testing.T) {
	for _, mode := range []string{"abandoned", "phase", "open", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newCompletionFixture(t)
			switch mode {
			case "abandoned":
				f.in.PR.State = "CLOSED"
				f.in.Native.State = "CLOSED"
				f.in.Native.Transitions[0].Kind = "ClosedEvent"
			case "phase":
				f.in.Target.Type = &github.IssueType{ID: "type-Phase", Name: "Phase"}
				f.in.Target.Body = "Classification: false\nCreate-Branch: true"
				f.in.Onboarding.Binding.Kind = sp.Phase
			case "open":
				f.in.PR.State = "OPEN"
			case "cancelled":
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			got, err := f.controller.Handle(ctx, f.delivery("merge"))
			if mode == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(got, err)
			}
			if f.closes != 0 || f.in.Target.State != "OPEN" {
				t.Fatal("unauthorized close", f.closes)
			}
			if mode == "phase" || mode == "abandoned" {
				if f.status["PRITEM"] != config.Done {
					t.Fatal(f.status)
				}
			}
		})
	}
}

func TestTerminalPRDoneBypassesOnlyTerminalOwnership(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{true: "merged", false: "abandoned"}[merged], func(t *testing.T) {
			f := newCompletionFixture(t)
			f.in.PR.Body += "\nSet-Status: false"
			f.in.Target.Body += "\nSet-Status: false"
			f.status["TARGETITEM"] = config.Ready
			if !merged {
				f.in.PR.State = "CLOSED"
				f.in.Native.State = "CLOSED"
				f.in.Native.Transitions[0].Kind = "ClosedEvent"
			}
			_, err := f.controller.Handle(context.Background(), f.delivery("merge"))
			if err != nil || f.status["PRITEM"] != config.Done {
				t.Fatal(err, f.status)
			}
			if merged {
				if f.closes != 1 {
					t.Fatal("terminal merge ownership blocked", f.closes)
				}
			} else if f.closes != 0 || f.status["TARGETITEM"] != config.Ready {
				t.Fatal("ordinary abandonment rollback bypassed ownership", f.closes, f.status)
			}
		})
	}
}
