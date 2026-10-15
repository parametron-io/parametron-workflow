package prcompanionreconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	er "github.com/parametron-io/parametron-workflow/internal/engineeringreconcile"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	pc "github.com/parametron-io/parametron-workflow/internal/prcompanion"
	"github.com/parametron-io/parametron-workflow/internal/prdiscovery"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type primaryFunc func(context.Context, storage.Resource) (observe.ObservedState, error)

func (f primaryFunc) ReadPrimary(c context.Context, r storage.Resource) (observe.ObservedState, error) {
	return f(c, r)
}

type currentFunc func(context.Context, storage.Resource) (observe.ObservedState, error)

func (f currentFunc) ReadCurrent(c context.Context, r storage.Resource) (observe.ObservedState, error) {
	return f(c, r)
}

type discoveryFunc func(context.Context) (prdiscovery.Result, error)

func (f discoveryFunc) Discover(c context.Context) (prdiscovery.Result, error) { return f(c) }

type snapshotFunc func(context.Context) (ec.Snapshot, error)

func (f snapshotFunc) ResolveSnapshot(c context.Context) (ec.Snapshot, error) { return f(c) }

type numbers struct {
	set   func(context.Context, string, string, string, float64) error
	clear func(context.Context, string, string, string) error
}

func (f numbers) SetProjectNumber(c context.Context, p, i, k string, v float64) error {
	return f.set(c, p, i, k, v)
}
func (f numbers) ClearProjectNumber(c context.Context, p, i, k string) error {
	return f.clear(c, p, i, k)
}

type harness struct {
	t           *testing.T
	c           *Controller
	store       *storage.Store
	path        string
	issues      []github.Issue
	entries     []prdiscovery.Entry
	writes      []string
	discoveries int
	hook        func(int)
	writeHook   func(context.Context) error
	afterRead   func()
}

func newHarness(t *testing.T, n int) *harness {
	t.Helper()
	h := &harness{t: t, path: filepath.Join(t.TempDir(), "ownership.sqlite")}
	var err error
	h.store, err = storage.Open(context.Background(), h.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.store.Close() })
	repo := github.Repository{ID: "REPO", Owner: "parametron-io", Name: "engine"}
	target := github.Identity{ID: "ISSUE", Repository: repo, Number: 46}
	h.issues = []github.Issue{{Identity: target, State: "OPEN", Body: "Classification: false", Type: &github.IssueType{ID: "task", Name: "Task"}}}
	origin := resource(target, "issue")
	if _, err = h.store.InsertDelivery(context.Background(), storage.Delivery{ID: "delivery", EventName: "issues", Payload: []byte("{}"), ReceivedAt: time.Unix(1, 0), Resource: &origin}); err != nil {
		t.Fatal(err)
	}
	deployment := config.ResolvedConfig{Organization: config.Organization{ID: "ORG", Login: "parametron-io"}, Repositories: []config.Repository{repo}, IssueTypes: map[string]string{"Task": "task", "Feature": "feature", "Phase": "phase", "Bug": "bug"}}
	loader, _ := semanticflow.NewAcceptedLoader(h)
	resolver, err := ec.New(ec.Config{Deployment: deployment, Accepted: loader, Primary: primaryFunc(func(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
		for _, i := range h.issues {
			if int64(i.Number) == r.Number {
				return observe.ObservedState{Resource: resource(i.Identity, "issue"), Presence: observe.Present, Issue: &i}, ctx.Err()
			}
		}
		return observe.ObservedState{}, ec.ErrMissing
	}), Lister: &github.ListerFake{ListIssuesFunc: func(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
		out := []github.ListedIssue{}
		for _, i := range h.issues {
			out = append(out, github.ListedIssue{Identity: i.Identity, Body: i.Body, State: i.State, Type: i.Type})
		}
		return out, ctx.Err()
	}}})
	if err != nil {
		t.Fatal(err)
	}
	project := func(id string) []observe.ProjectState {
		return []observe.ProjectState{{ID: "ENG", Profile: config.Engineering, Items: []observe.ProjectItem{{ID: id, Fields: map[config.FieldRole]observe.FieldValue{}}}}}
	}
	user := github.Actor{ID: "USER", Login: "developer", Type: "User"}
	for i := 0; i < n; i++ {
		id := github.Identity{ID: fmt.Sprint("PR", i), Repository: repo, Number: 100 + i}
		targetObs := observe.ObservedState{Resource: origin, Presence: observe.Present, Issue: &h.issues[0], Projects: project("TARGETITEM")}
		h.entries = append(h.entries, prdiscovery.Entry{Resource: resource(id, "pull_request"), PR: observe.ObservedState{Resource: resource(id, "pull_request"), Presence: observe.Present, PullRequest: &github.PullRequest{Identity: id, State: "OPEN"}, Projects: project(fmt.Sprint("ITEM", i))}, Target: &targetObs, Binding: prbinding.Decision{Outcome: prbinding.Accepted, Evidence: prbinding.Evidence{PR: id, Target: target, Kind: "Task", Policy: "active_org_member_repository_write_assignee_v1", Developer: user, Branch: github.Branch{Repository: repo, Name: "development/issue-46", SHA: strings.Repeat("a", 40)}}}})
	}
	cfg := Config{Repositories: []github.Repository{repo}, Project: config.ResolvedProject{ID: "ENG", Fields: map[config.FieldRole]string{config.RoadmapOrder: "NUMBER"}}, BugProject: "BUG", Store: h.store, Clock: func() time.Time { return time.Unix(100, 0).UTC() }, Snapshots: resolver}
	cfg.Discovery = discoveryFunc(func(ctx context.Context) (prdiscovery.Result, error) {
		h.discoveries++
		if h.hook != nil {
			h.hook(h.discoveries)
		}
		// Current Target primary evidence tracks the snapshot; numbers have no policy authority.
		for i := range h.entries {
			if h.entries[i].Target != nil {
				for j := range h.issues {
					if h.entries[i].Target.Resource.NodeID == h.issues[j].ID {
						v := h.issues[j]
						h.entries[i].Target.Issue = &v
					}
				}
			}
		}
		raw, _ := json.Marshal(h.entries)
		var entries []prdiscovery.Entry
		_ = json.Unmarshal(raw, &entries)
		return prdiscovery.Result{Repositories: []github.Repository{repo}, Project: "ENG", BugProject: "BUG", Completeness: prdiscovery.Complete, Freshness: prdiscovery.StableReads, Entries: entries}, ctx.Err()
	})
	cfg.Reader = currentFunc(func(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
		if h.afterRead != nil {
			h.afterRead()
		}
		for _, e := range h.entries {
			if e.Resource == r {
				return e.PR, ctx.Err()
			}
		}
		return observe.ObservedState{}, ErrIncomplete
	})
	effect := func(ctx context.Context, p, item, field string, v *float64) error {
		if p != "ENG" || field != "NUMBER" {
			t.Fatalf("foreign write %s/%s", p, field)
		}
		for i := range h.entries {
			it := &h.entries[i].PR.Projects[0].Items[0]
			if it.ID == item {
				h.writes = append(h.writes, item)
				if v == nil {
					delete(it.Fields, config.RoadmapOrder)
				} else {
					it.Fields[config.RoadmapOrder] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Number, Number: *v}}
				}
				if h.writeHook != nil {
					return h.writeHook(ctx)
				}
				return ctx.Err()
			}
		}
		t.Fatal("non-PR write", item)
		return ErrProject
	}
	cfg.Numbers = numbers{set: func(ctx context.Context, p, i, f string, v float64) error { return effect(ctx, p, i, f, &v) }, clear: func(ctx context.Context, p, i, f string) error { return effect(ctx, p, i, f, nil) }}
	h.c, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func (h *harness) run() error { return h.c.Reconcile(context.Background(), "delivery") }
func (h *harness) value(i int) (float64, bool) {
	v, ok := h.entries[i].PR.Projects[0].Items[0].Fields[config.RoadmapOrder]
	return v.Number, ok
}
func (h *harness) drift(i int, v float64) {
	h.entries[i].PR.Projects[0].Items[0].Fields[config.RoadmapOrder] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Number, Number: v}}
}
func (h *harness) restart() {
	h.t.Helper()
	h.store.Close()
	var err error
	h.store, err = storage.Open(context.Background(), h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	h.c.cfg.Store = h.store
}
func (h *harness) expected() pc.Plan {
	h.t.Helper()
	s, err := h.c.cfg.Snapshots.ResolveSnapshot(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, err := s.CanonicalContext()
	if err != nil {
		h.t.Fatal(err)
	}
	_, _, primary, err := er.Compute(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	in := pc.Input{Primary: primary}
	for _, e := range h.entries {
		in.PullRequests = append(in.PullRequests, pc.PullRequest{Resource: e.Resource, Identity: e.PR.PullRequest.Identity, State: pc.State(e.PR.PullRequest.State), Binding: e.Binding})
	}
	plan, err := pc.Evaluate(in)
	if err != nil {
		h.t.Fatal(err)
	}
	return plan
}

func TestFirstAcquisitionRequiresWriteEvenForMatchingValue(t *testing.T) {
	for _, v := range []float64{-1, 0, 1.5, 10001, 999999} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			h := newHarness(t, 1)
			if v >= 0 {
				h.drift(0, v)
			}
			want := h.expected().Assignments[0].RoadmapOrder
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if len(h.writes) != 1 {
				t.Fatal(h.writes)
			}
			rows, err := h.c.ownership(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			o := rows[h.entries[0].Resource]
			if o.Acquisition.CompletedAt.IsZero() || o.Revision != 2 {
				t.Fatal(o)
			}
			h.restart()
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if len(h.writes) != 1 {
				t.Fatal("matching owned value rewritten")
			}
			h.drift(0, 0.25)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			got, _ := h.value(0)
			if got != float64(want) || len(h.writes) != 2 {
				t.Fatal(got, h.writes)
			}
		})
	}
}

func TestUncertainAcquisitionNeverAdoptsMatchingObservation(t *testing.T) {
	for _, stage := range []string{"lost response", "post-write read", "completion save", "pre-write change"} {
		t.Run(stage, func(t *testing.T) {
			h := newHarness(t, 1)
			transient := &github.Error{Category: github.Transient}
			switch stage {
			case "lost response":
				h.writeHook = func(context.Context) error { return transient }
			case "post-write read":
				h.afterRead = func() { h.entries[0].PR.Presence = observe.Missing }
			case "completion save":
				h.c.cfg.Store = &storeHook{OwnershipStore: h.store, save: func(prev int64) error {
					if prev == 1 {
						return errors.New("disk failure")
					}
					return nil
				}}
			case "pre-write change":
				h.hook = func(n int) {
					if n == 4 {
						h.drift(0, 12)
					}
				}
			}
			if err := h.run(); err == nil {
				t.Fatal("expected interruption")
			}
			if stage == "pre-write change" && len(h.writes) != 0 {
				t.Fatal(h.writes)
			}
			h.hook = nil
			h.afterRead = nil
			h.writeHook = nil
			h.entries[0].PR.Presence = observe.Present
			h.restart()
			count := len(h.writes)
			if err := h.run(); !errors.Is(err, ErrAmbiguous) {
				t.Fatal(err)
			}
			if len(h.writes) != count {
				t.Fatal("ambiguous write replayed")
			}
		})
	}
}

type storeHook struct {
	OwnershipStore
	save func(int64) error
}

func (s *storeHook) SaveCompanionField(ctx context.Context, p int64, r storage.CompanionField) error {
	if err := s.save(p); err != nil {
		return err
	}
	return s.OwnershipStore.SaveCompanionField(ctx, p, r)
}

func TestVerifiedRecoveryRecomputesAfterLostResponse(t *testing.T) {
	h := newHarness(t, 2)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	h.drift(0, 0)
	h.writeHook = func(context.Context) error { return &github.Error{Category: github.Transient} }
	if err := h.run(); err == nil {
		t.Fatal("lost response")
	}
	h.writeHook = nil
	h.restart()
	h.entries[0].PR.PullRequest.State = "CLOSED"
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if _, present := h.value(0); present {
		t.Fatal("clear set zero")
	}
	want := h.expected().Assignments[0].RoadmapOrder
	if got, _ := h.value(1); got != float64(want) {
		t.Fatal(got, want)
	}
	h.entries[0].PR.PullRequest.State = "OPEN"
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, a := range h.expected().Assignments {
		for i, e := range h.entries {
			if e.Resource == a.PR {
				if got, _ := h.value(i); got != float64(a.RoadmapOrder) {
					t.Fatal(got, a)
				}
			}
		}
	}
}

func TestCompleteInvalidPlanHasNoEffects(t *testing.T) {
	for _, kind := range []string{"overflow", "unknown binding", "wrong project", "archived", "duplicate item", "missing item", "wrong number type", "missing delivery"} {
		t.Run(kind, func(t *testing.T) {
			n := 2
			if kind == "overflow" {
				n = 10
			}
			h := newHarness(t, n)
			switch kind {
			case "unknown binding":
				h.entries[1].Binding = prbinding.Decision{}
			case "wrong project":
				h.entries[1].PR.Projects[0].ID = "BUG"
			case "archived":
				h.entries[1].PR.Projects[0].Items[0].Archived = true
			case "duplicate item":
				h.entries[1].PR.Projects[0].Items[0].ID = h.entries[0].PR.Projects[0].Items[0].ID
			case "missing item":
				h.entries[1].PR.Projects[0].Items = nil
			case "wrong number type":
				h.entries[1].PR.Projects[0].Items[0].Fields[config.RoadmapOrder] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "human"}}
			case "missing delivery":
				if err := h.c.Reconcile(context.Background(), "missing"); !errors.Is(err, ErrOwnership) {
					t.Fatal(err)
				}
				return
			}
			if err := h.run(); err == nil {
				t.Fatal("invalid plan accepted")
			}
			if len(h.writes) != 0 {
				t.Fatal(h.writes)
			}
			rows, err := h.store.CompanionFields(context.Background(), "ENG", "NUMBER")
			if err != nil || len(rows) != 0 {
				t.Fatal(rows, err)
			}
		})
	}
}
func TestFreshnessBeforeAndBetweenEffects(t *testing.T) {
	for _, at := range []int{2, 3, 4, 5} {
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			h := newHarness(t, 2)
			h.hook = func(n int) {
				if n == at {
					h.entries[1].Binding = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.InvalidDeveloper}
				}
			}
			if err := h.run(); err == nil {
				t.Fatal("changed plan accepted")
			}
			want := 0
			if at == 5 {
				want = 1
			}
			if len(h.writes) != want {
				t.Fatal(at, h.writes)
			}
		})
	}
}
func TestReplacementItemCannotInheritOwnership(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	h.entries[0].PR.Projects[0].Items[0].ID = "replacement"
	h.restart()
	if err := h.run(); !errors.Is(err, ErrOwnership) {
		t.Fatal(err)
	}
	if len(h.writes) != 1 {
		t.Fatal(h.writes)
	}
}
func TestCancellationBoundaries(t *testing.T) {
	for _, stage := range []string{"before", "durable", "remote", "verified"} {
		t.Run(stage, func(t *testing.T) {
			h := newHarness(t, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "before":
				cancel()
			case "durable":
				h.hook = func(n int) {
					if n == 4 {
						cancel()
					}
				}
			case "remote":
				h.writeHook = func(context.Context) error { cancel(); return ctx.Err() }
			case "verified":
				h.c.cfg.Store = &storeHook{OwnershipStore: h.store, save: func(prev int64) error {
					if prev == 1 {
						cancel()
						return ctx.Err()
					}
					return nil
				}}
			}
			if err := h.c.Reconcile(ctx, "delivery"); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			h.hook = nil
			h.writeHook = nil
			h.restart()
			err := h.run()
			if stage == "before" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrAmbiguous) {
				t.Fatal(err)
			}
		})
	}
}

func (h *harness) Provenance(ctx context.Context, n, k string) (storage.Provenance, error) {
	return h.store.Provenance(ctx, n, k)
}
func (h *harness) Event(ctx context.Context, id string) (storage.Event, error) {
	return h.store.Event(ctx, id)
}

func TestFreshPlanAfterPartialProgressAndBudget(t *testing.T) {
	for _, limit := range []error{github.ErrRequestLimit, prdiscovery.ErrLimit} {
		t.Run(limit.Error(), func(t *testing.T) {
			h := newHarness(t, 2)
			// First resource completes (initial + three loads); next preparation is bounded out.
			previous := h.c.cfg.Discovery
			h.c.cfg.Discovery = discoveryFunc(func(ctx context.Context) (prdiscovery.Result, error) {
				if h.discoveries == 4 {
					return prdiscovery.Result{}, limit
				}
				return previous.Discover(ctx)
			})
			if err := h.run(); !errors.Is(err, ErrChanged) {
				t.Fatal(err)
			}
			if len(h.writes) != 1 {
				t.Fatal(h.writes)
			}
			h.c.cfg.Discovery = previous
			h.restart()
			// Closing an earlier canonical PR frees its slot; retry must recompute.
			h.entries[0].PR.PullRequest.State = "CLOSED"
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if _, ok := h.value(0); ok {
				t.Fatal("terminal number retained")
			}
			if got, _ := h.value(1); got != float64(h.expected().Assignments[0].RoadmapOrder) {
				t.Fatal(got)
			}
		})
	}
}
func TestFirstObservationLimitHasNoPartialOwnership(t *testing.T) {
	h := newHarness(t, 1)
	h.c.cfg.Discovery = discoveryFunc(func(context.Context) (prdiscovery.Result, error) { return prdiscovery.Result{}, prdiscovery.ErrLimit })
	if err := h.run(); !errors.Is(err, prdiscovery.ErrLimit) {
		t.Fatal(err)
	}
	rows, _ := h.store.CompanionFields(context.Background(), "ENG", "NUMBER")
	if len(h.writes) != 0 || len(rows) != 0 {
		t.Fatal(h.writes, rows)
	}
}

func TestPrewriteAuthorityMutationMatrix(t *testing.T) {
	mutations := map[string]func(*harness){
		"PR removed": func(h *harness) { h.entries = h.entries[:1] },
		"PR appears": func(h *harness) {
			e := h.entries[1]
			e.Resource.Number++
			e.Resource.NodeID = "NEWPR"
			pr := *e.PR.PullRequest
			pr.Number++
			pr.ID = "NEWPR"
			e.PR.PullRequest = &pr
			e.PR.Resource = e.Resource
			e.Binding.Evidence.PR = pr.Identity
			e.PR.Projects[0].Items[0].ID = "NEWITEM"
			h.entries = append(h.entries, e)
		},
		"closed": func(h *harness) { h.entries[1].PR.PullRequest.State = "CLOSED" },
		"merged": func(h *harness) { h.entries[1].PR.PullRequest.State = "MERGED" },
		"head":   func(h *harness) { h.entries[1].Binding.Evidence.Branch.SHA = strings.Repeat("b", 40) },
		"developer": func(h *harness) {
			h.entries[1].Binding = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.InvalidDeveloper}
		},
		"item":       func(h *harness) { h.entries[1].PR.Projects[0].Items[0].ID = "replacement" },
		"membership": func(h *harness) { h.entries[1].PR.Projects[0].Items = nil },
		"number":     func(h *harness) { h.drift(1, 0.5) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 2)
			h.hook = func(n int) {
				if n == 3 {
					mutate(h)
				}
			}
			if err := h.run(); err == nil {
				t.Fatal("stale effect accepted")
			}
			if len(h.writes) != 0 {
				t.Fatal(h.writes)
			}
		})
	}
}

func TestRealPrimaryChildCompactionAndReparenting(t *testing.T) {
	h := newHarness(t, 2)
	phase := h.issues[0]
	phase.Number = 45
	phase.ID = "PHASE"
	phase.Type = &github.IssueType{ID: "phase", Name: "Phase"}
	earlier := phase
	earlier.Number = 1
	earlier.ID = "EARLIER"
	h.issues[0].Body += "\nParent: #45"
	h.issues = append(h.issues, phase, earlier)
	check := func() {
		t.Helper()
		want := h.expected()
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		for _, a := range want.Assignments {
			for i, e := range h.entries {
				if e.Resource == a.PR {
					if got, _ := h.value(i); got != float64(a.RoadmapOrder) {
						t.Fatal(got, a)
					}
				}
			}
		}
	}
	check()
	if got, _ := h.value(0); got != 11011 {
		t.Fatal("fixture did not exercise child namespace", got)
	}
	h.issues[2].State = "CLOSED"
	check()
	if got, _ := h.value(0); got != 10011 {
		t.Fatal("child segment did not compact", got)
	}
	// Closed Phase retains its open child's segment.
	h.issues[1].State = "CLOSED"
	h.restart()
	check()
	// Removing Parent turns the Task into a standalone root under unchanged #36.
	h.issues[0].Body = "Classification: false"
	check()
	if got, _ := h.value(0); got != 10001 {
		t.Fatal("standalone root", got)
	}
	// Feature roots use the same primary authority.
	h.issues[0].Type = &github.IssueType{ID: "feature", Name: "Feature"}
	for i := range h.entries {
		h.entries[i].Binding.Evidence.Kind = "Feature"
	}
	check()
}

func TestOwnershipRevisionChangeDuringPreparation(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	h.drift(0, 0)
	start := h.discoveries
	h.hook = func(n int) {
		if n == start+3 {
			rows, err := h.store.CompanionFields(context.Background(), "ENG", "NUMBER")
			if err != nil {
				t.Fatal(err)
			}
			var o record
			_ = json.Unmarshal(rows[0].Metadata, &o)
			old := o.Revision
			o.Revision++
			rows[0].Revision = o.Revision
			rows[0].Metadata, _ = json.Marshal(o)
			if err := h.store.SaveCompanionField(context.Background(), old, rows[0]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := h.run(); !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
	if len(h.writes) != 1 {
		t.Fatal(h.writes)
	}
	h.hook = nil
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedWriteVisibleOnlyOnLaterRead(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	h.drift(0, 0)
	h.writeHook = func(context.Context) error { h.drift(0, 0); return nil }
	if err := h.run(); !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
	h.writeHook = nil
	h.drift(0, float64(h.expected().Assignments[0].RoadmapOrder))
	h.restart()
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if len(h.writes) != 2 {
		t.Fatal("verified match replayed", h.writes)
	}
}
