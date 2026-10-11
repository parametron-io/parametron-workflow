package engineeringflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	er "github.com/parametron-io/parametron-workflow/internal/engineeringreconcile"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

type noAcceptance struct{}

func (noAcceptance) AcceptedIssue(context.Context, storage.Resource) (sp.AcceptedIssue, bool, error) {
	return sp.AcceptedIssue{}, false, nil
}

type resolverFunc func(context.Context, storage.Event) (storage.Resource, error)

func (f resolverFunc) Resolve(ctx context.Context, e storage.Event) (storage.Resource, error) {
	return f(ctx, e)
}

type processorFunc func(context.Context, storage.Event) error

func (f processorFunc) Process(ctx context.Context, e storage.Event) error { return f(ctx, e) }

type enrichFunc func(context.Context, observe.PolicyInput) (semanticflow.PolicyInput, error)

func (f enrichFunc) Enrich(ctx context.Context, in observe.PolicyInput) (semanticflow.PolicyInput, error) {
	return f(ctx, in)
}

type semanticFunc func(context.Context, semanticflow.PolicyInput) error

func (f semanticFunc) Evaluate(ctx context.Context, in semanticflow.PolicyInput) error {
	return f(ctx, in)
}

type laneAttemptContext struct {
	context.Context
	attempt chan struct{}
	once    sync.Once
}

func (c *laneAttemptContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.attempt) })
	return c.Context.Done()
}

type flowFixture struct {
	t            *testing.T
	mu           sync.Mutex
	d            config.ResolvedConfig
	store        *storage.Store
	path         string
	flow         *Flow
	branch       *eb.Controller
	issues       map[int64]github.Issue
	items        map[int64]observe.ProjectItem
	remote       map[string]github.Branch
	snapshots    []ec.Snapshot
	branchInputs []ep.Input
	origins      []eb.Origin
	calls        []string
	fail         string
	snapshotHook func(int)
	effectHook   func(string)
	projectItem  github.CurrentProjectItem
	projectErr   error
	base, head   string
	reverse      bool
	at           time.Time
}

func (f *flowFixture) resource(n int64) storage.Resource {
	return storage.Resource{Owner: "parametron-io", Repository: "engine", Kind: "issue", Number: n, NodeID: fmt.Sprintf("I%d", n)}
}
func newFlowFixture(t *testing.T) *flowFixture {
	t.Helper()
	f := &flowFixture{t: t, issues: map[int64]github.Issue{}, items: map[int64]observe.ProjectItem{}, remote: map[string]github.Branch{}, base: strings.Repeat("a", 40), head: strings.Repeat("b", 40), at: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)}
	p := config.ResolvedProject{ID: "P", Fields: map[config.FieldRole]string{config.Status: "status", config.StartDate: "date", config.RoadmapOrder: "roadmap"}, StatusOptions: map[config.StatusRole]string{config.Backlog: "backlog", config.Blocked: "blocked", config.Ready: "ready", config.InProgress: "progress"}}
	for _, role := range []config.FieldRole{config.Priority, config.Effort, config.Estimate} {
		p.Fields[role] = string(role)
	}
	p.StatusOptions[config.InReview], p.StatusOptions[config.Done] = "review", "done"
	bugProject := config.ResolvedProject{ID: "B", Fields: map[config.FieldRole]string{}, StatusOptions: map[config.StatusRole]string{}}
	for _, role := range []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate, config.PriorityScore} {
		bugProject.Fields[role] = "bug-" + string(role)
	}
	for _, role := range []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done, config.ToTriage} {
		bugProject.StatusOptions[role] = "bug-" + string(role)
	}
	f.d = config.ResolvedConfig{Organization: config.Organization{Login: "parametron-io"}, Repositories: []config.Repository{{ID: "R", Owner: "parametron-io", Name: "engine"}}, Engineering: p, IssueTypes: map[string]string{"Phase": "type-Phase", "Task": "type-Task", "Feature": "type-Feature", "Bug": "type-Bug"}}
	f.d.BugTracker = bugProject
	var err error
	f.path = filepath.Join(t.TempDir(), "flow.sqlite")
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	f.branch, err = eb.New(eb.Config{Repositories: f.d.Repositories, Branches: f, Store: f.store, Clock: func() time.Time { return f.at }})
	if err != nil {
		t.Fatal(err)
	}
	activityCfg := ea.Config{Repositories: f.d.Repositories, Project: p, Provenance: f.branch, Branches: f, Comparison: f, Projects: f, Fields: f, Store: f.store}
	activity, err := ea.New(activityCfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := er.New(er.Config{Project: p, Projects: f, Activity: activity, Fields: f, Numbers: f})
	if err != nil {
		t.Fatal(err)
	}
	f.flow, err = New(Config{Deployment: f.d, Resolver: resolverFunc(func(ctx context.Context, e storage.Event) (storage.Resource, error) {
		if e.Resource == nil {
			return storage.Resource{}, observe.ErrBinding
		}
		return *e.Resource, ctx.Err()
	}), Reader: f, Foundation: processorFunc(func(context.Context, storage.Event) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "foundation")
		return nil
	}), Snapshot: f, Reconciler: reconciler, Activity: activityCfg, ProjectItems: f, Branches: f})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *flowFixture) add(n int64, typ, body, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.resource(n)
	f.issues[n] = github.Issue{Identity: github.Identity{ID: r.NodeID, Repository: f.d.Repositories[0], Number: int(n)}, State: state, Body: "Classification: false\n" + body, Type: &github.IssueType{ID: "type-" + typ, Name: typ}}
	f.items[n] = observe.ProjectItem{ID: fmt.Sprintf("item%d", n), Fields: map[config.FieldRole]observe.FieldValue{}}
}
func (f *flowFixture) change(n int64, body, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.issues[n]
	i.Body = "Classification: false\n" + body
	i.State = state
	f.issues[n] = i
}
func (f *flowFixture) ReadPrimary(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.issues[r.Number]
	if !ok {
		return observe.ObservedState{Resource: r, Presence: observe.Missing}, ctx.Err()
	}
	return observe.ObservedState{Resource: f.resource(r.Number), Presence: observe.Present, Issue: &i}, ctx.Err()
}
func (f *flowFixture) ReadCurrent(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	o, err := f.ReadPrimary(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.items[r.Number]
	fields := map[config.FieldRole]observe.FieldValue{}
	for k, v := range i.Fields {
		fields[k] = v
	}
	i.Fields = fields
	o.Projects = []observe.ProjectState{{Profile: config.Engineering, ID: "P", Items: []observe.ProjectItem{i}}}
	f.calls = append(f.calls, fmt.Sprintf("read:%d", r.Number))
	return o, err
}
func (f *flowFixture) ListIssues(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []github.ListedIssue{}
	for _, i := range f.issues {
		out = append(out, github.ListedIssue{Identity: i.Identity, Body: i.Body, State: i.State, Type: i.Type})
	}
	sort.Slice(out, func(i, j int) bool {
		if f.reverse {
			return out[i].Number > out[j].Number
		}
		return out[i].Number < out[j].Number
	})
	return out, ctx.Err()
}
func (f *flowFixture) ResolveSnapshot(ctx context.Context) (ec.Snapshot, error) {
	f.mu.Lock()
	n := len(f.snapshots)
	hook := f.snapshotHook
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	r, err := ec.New(ec.Config{Deployment: f.d, Primary: f, Lister: f, Accepted: noAcceptance{}})
	if err != nil {
		return ec.Snapshot{}, err
	}
	s, err := r.ResolveSnapshot(ctx)
	if err == nil {
		f.mu.Lock()
		f.snapshots = append(f.snapshots, s)
		f.calls = append(f.calls, "snapshot")
		f.mu.Unlock()
	}
	return s, err
}
func (f *flowFixture) DefaultBranch(ctx context.Context, r github.Repository) (github.Branch, error) {
	return github.Branch{Repository: r, Name: "main", SHA: f.base}, ctx.Err()
}
func (f *flowFixture) Branch(ctx context.Context, r github.Repository, n string) (github.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.remote[n]
	if !ok {
		return github.Branch{}, &github.MissingBranch{Repository: r, Name: n}
	}
	return b, ctx.Err()
}
func (f *flowFixture) CreateBranch(ctx context.Context, r github.Repository, n, sha string) (github.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "create:"+n)
	if f.fail == "create:"+n {
		return github.Branch{}, &github.Error{Category: github.Transient}
	}
	b := github.Branch{Repository: r, Name: n, SHA: sha}
	f.remote[n] = b
	return b, ctx.Err()
}
func (f *flowFixture) CompareCommits(ctx context.Context, r github.Repository, base, head string) (github.Comparison, error) {
	return github.Comparison{Repository: r, BaseSHA: base, HeadSHA: head, Status: "ahead", AheadBy: 1}, ctx.Err()
}
func (f *flowFixture) EnsureFrom(ctx context.Context, origin eb.Origin, in ep.Input) (eb.Result, error) {
	f.mu.Lock()
	f.branchInputs = append(f.branchInputs, in)
	f.origins = append(f.origins, origin)
	f.calls = append(f.calls, fmt.Sprintf("ensure:%d", in.Context.Root.Number))
	f.mu.Unlock()
	return f.branch.EnsureFrom(ctx, origin, in)
}
func (f *flowFixture) effect(ctx context.Context, item, kind string, value github.FieldValue) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	label := kind + ":" + item
	f.mu.Lock()
	f.calls = append(f.calls, label)
	if f.fail == label {
		f.mu.Unlock()
		return &github.Error{Category: github.Transient}
	}
	for n, i := range f.items {
		if i.ID == item {
			role := config.Status
			switch kind {
			case "date":
				role = config.StartDate
			case "set", "clear":
				role = config.RoadmapOrder
			}
			if kind == "clear" {
				delete(i.Fields, role)
			} else {
				i.Fields[role] = observe.FieldValue{FieldValue: value}
			}
			f.items[n] = i
		}
	}
	hook := f.effectHook
	f.mu.Unlock()
	if hook != nil {
		hook(label)
	}
	return nil
}
func (f *flowFixture) SetProjectOption(ctx context.Context, p, item, field, option string) error {
	if p != "P" || field != "status" {
		f.t.Fatal(p, field)
	}
	return f.effect(ctx, item, "status", github.FieldValue{Kind: config.SingleSelect, OptionID: option})
}
func (f *flowFixture) SetProjectDate(ctx context.Context, p, item, field, date string) error {
	if p != "P" || field != "date" {
		f.t.Fatal(p, field)
	}
	return f.effect(ctx, item, "date", github.FieldValue{Kind: config.Date, Date: date})
}
func (f *flowFixture) SetProjectNumber(ctx context.Context, p, item, field string, v float64) error {
	if p != "P" || field != "roadmap" {
		f.t.Fatal(p, field)
	}
	return f.effect(ctx, item, "set", github.FieldValue{Kind: config.Number, Number: v})
}
func (f *flowFixture) ClearProjectNumber(ctx context.Context, p, item, field string) error {
	if p != "P" || field != "roadmap" {
		f.t.Fatal(p, field)
	}
	return f.effect(ctx, item, "clear", github.FieldValue{})
}
func (f *flowFixture) CurrentProjectItem(ctx context.Context, id string) (github.CurrentProjectItem, error) {
	if err := ctx.Err(); err != nil {
		return github.CurrentProjectItem{}, err
	}
	return f.projectItem, f.projectErr
}
func (f *flowFixture) event(id, name string, n int64) storage.Event {
	f.t.Helper()
	r := f.resource(n)
	payload := []byte(`{}`)
	if name == "push" {
		payload = []byte(fmt.Sprintf(`{"repository":{"owner":{"login":"parametron-io"},"name":"engine"},"ref":"refs/heads/development/issue-%d"}`, n))
	}
	_, err := f.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: name, Payload: payload, ReceivedAt: f.at.Add(time.Hour), Resource: &r})
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := f.store.Event(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

func TestFlowFreshFullComputeBarrierBeforeBranchFanout(t *testing.T) {
	for _, change := range []string{"blocker", "inactive Phase", "disabled branch", "segment capacity", "child capacity", "hierarchy", "root order", "valid"} {
		t.Run(change, func(t *testing.T) {
			f := newFlowFixture(t)
			f.add(1, "Task", "", "OPEN")
			if change == "inactive Phase" {
				f.change(1, "Parent: #3", "OPEN")
				f.add(3, "Phase", "", "OPEN")
			}
			e := f.event("trigger", "issues", 1)
			f.snapshotHook = func(n int) {
				if n != 1 {
					return
				}
				switch change {
				case "blocker":
					f.add(2, "Bug", "", "OPEN")
					f.change(1, "Blocked-By: #2", "OPEN")
				case "inactive Phase":
					f.add(2, "Phase", "", "OPEN")
				case "disabled branch":
					f.change(1, "Create-Branch: false", "OPEN")
				case "segment capacity":
					for n := int64(2); n <= int64(eo.MaxRoadmapSegments)+1; n++ {
						f.add(n, "Task", "", "OPEN")
					}
				case "child capacity":
					f.add(2, "Phase", "", "OPEN")
					f.change(1, "Parent: #2", "OPEN")
					for n := int64(3); n <= int64(eo.MaxDirectChildren)+3; n++ {
						f.add(n, "Task", "Parent: #2", "OPEN")
					}
				case "hierarchy":
					f.add(2, "Task", "", "OPEN")
					f.change(1, "Parent: #2", "OPEN")
				case "root order":
					f.add(3, "Phase", "Blocked-By: #2", "OPEN")
					f.add(4, "Phase", "Blocked-By: #1", "OPEN")
					f.change(1, "Parent: #3", "OPEN")
					f.add(2, "Task", "Parent: #4", "OPEN")
				case "valid":
					f.add(2, "Feature", "", "OPEN")
				}
			}
			err := f.flow.Process(context.Background(), e)
			if change == "segment capacity" || change == "child capacity" {
				if !errors.Is(err, eo.ErrRoadmapCapacity) {
					t.Fatal(err)
				}
			} else if change == "hierarchy" {
				if !errors.Is(err, eo.ErrInvalid) {
					t.Fatal(err)
				}
			} else if change == "root order" {
				if !errors.Is(err, eo.ErrRoadmapHierarchy) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if change != "valid" {
				if len(f.branchInputs) != 0 {
					t.Fatal("stale pre-write eligibility reached #37", f.origins)
				}
				return
			}
			if len(f.branchInputs) != 2 || len(f.snapshots) != 2 {
				t.Fatal(f.branchInputs, len(f.snapshots))
			}
			fresh := f.snapshots[1]
			c, err := fresh.Context(f.resource(1))
			if err != nil {
				t.Fatal(err)
			}
			freshInput, _, _, err := er.Compute(c)
			if err != nil {
				t.Fatal(err)
			}
			for i, in := range f.branchInputs {
				wantRoot := f.resource(int64(i + 1))
				wantContext, err := fresh.Context(wantRoot)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(in.Context, wantContext) || !reflect.DeepEqual(in.PhaseOrder, freshInput.PhaseOrder) || f.origins[i] != (eb.Origin{DeliveryID: "trigger", Resource: c.Root}) {
					t.Fatal(in, f.origins[i])
				}
			}
		})
	}
}

func TestFlowCanonicalFanoutIndependentOfEnumeration(t *testing.T) {
	var baselineCalls []string
	var baselineInputs []ep.Input
	for _, reverse := range []bool{false, true} {
		f := newFlowFixture(t)
		f.reverse = reverse
		f.add(1, "Task", "", "OPEN")
		f.add(2, "Feature", "", "OPEN")
		f.add(3, "Phase", "Create-Branch: true", "OPEN")
		if err := f.flow.Process(context.Background(), f.event("origin", "issues", 1)); err != nil {
			t.Fatal(err)
		}
		if !reverse {
			baselineCalls = append([]string(nil), f.calls...)
			baselineInputs = f.branchInputs
		} else if !reflect.DeepEqual(f.calls, baselineCalls) || !reflect.DeepEqual(f.branchInputs, baselineInputs) {
			t.Fatal("enumeration changed mutation order", f.calls, baselineCalls)
		}
		if len(f.branchInputs) != 3 {
			t.Fatal(f.branchInputs)
		}
		for index, in := range f.branchInputs {
			if in.Context.Root.Number != int64(index+1) {
				t.Fatal("noncanonical fanout", f.branchInputs)
			}
		}
	}
}

func TestFlowAuthorizedPushFreshRecomputeAndResumeOriginalOrigin(t *testing.T) {
	f := newFlowFixture(t)
	f.add(1, "Task", "", "OPEN")
	if err := f.flow.Process(context.Background(), f.event("issue", "issues", 1)); err != nil {
		t.Fatal(err)
	}
	f.remote["development/issue-1"] = github.Branch{Repository: f.d.Repositories[0], Name: "development/issue-1", SHA: f.head}
	f.fail = "date:item1"
	push := f.event("original-push", "push", 1)
	if err := f.flow.Process(context.Background(), push); err == nil {
		t.Fatal("missing exact date failure")
	}
	a, err := ea.New(f.flow.cfg.Activity)
	if err != nil {
		t.Fatal(err)
	}
	state, err := a.ActivityState(context.Background(), f.resource(1))
	if err != nil || !state.Accepted || state.Completed || state.DeliveryID != "original-push" {
		t.Fatal(state, err)
	}
	f.fail = ""
	f.calls = nil
	f.snapshots = nil
	if err := f.flow.Process(context.Background(), f.event("ordinary-retry", "issues", 1)); err != nil {
		t.Fatal(err)
	}
	state, err = a.ActivityState(context.Background(), f.resource(1))
	if err != nil || !state.Completed || state.DeliveryID != "original-push" {
		t.Fatal(state, err)
	}
	if len(f.snapshots) != 3 || f.items[1].Fields[config.Status].OptionID != "progress" || f.items[1].Fields[config.StartDate].Date != "2026-10-11" {
		t.Fatal(len(f.snapshots), f.items, f.calls)
	}
	for _, call := range f.calls {
		if call == "status:item1" {
			t.Fatal("already-successful Status was replayed", f.calls)
		}
	}
	// Returned state cannot authorize arbitrary branch creation.
	state.WorkItem.Number = 999
	if _, err := f.branch.Completed(context.Background(), state.WorkItem); err != nil {
		t.Fatal(err)
	}
}

func TestFlowPRExcludesEngineeringAndProjectRouting(t *testing.T) {
	f := newFlowFixture(t)
	f.add(1, "Task", "", "OPEN")
	e := f.event("pr", "pull_request", 1)
	r := *e.Resource
	r.Kind = "pull_request"
	e.Resource = &r
	if err := f.flow.Process(context.Background(), e); err != nil || !reflect.DeepEqual(f.calls, []string{"foundation"}) || len(f.snapshots) != 0 || len(f.branchInputs) != 0 {
		t.Fatal(err, f.calls)
	}
	for _, action := range []string{"created", "edited", "reordered", "archived", "restored"} {
		i := f.issues[1].Identity
		f.projectItem = github.CurrentProjectItem{ID: "item", ProjectID: "P", ContentKind: "Issue", Issue: &i}
		e := storage.Event{Delivery: storage.Delivery{EventName: "projects_v2_item", Payload: []byte(fmt.Sprintf(`{"action":%q,"projects_v2_item":{"node_id":"item","project_node_id":"P","status":"untrusted","number":999}}`, action))}}
		got, err := f.flow.Resolve(context.Background(), e)
		if err != nil || got != f.resource(1) {
			t.Fatal(action, got, err)
		}
	}
	for _, kind := range []string{"PullRequest", "DraftIssue", ""} {
		f.projectItem = github.CurrentProjectItem{ID: "item", ProjectID: "P", ContentKind: kind}
		e := storage.Event{Delivery: storage.Delivery{EventName: "projects_v2_item", Payload: []byte(`{"action":"edited","projects_v2_item":{"node_id":"item","project_node_id":"P"}}`)}}
		got, err := f.flow.Resolve(context.Background(), e)
		if !errors.Is(err, worker.ErrNotApplicable) || got != (storage.Resource{}) {
			t.Fatal(kind, got, err)
		}
	}
}

func TestFlowLaneCancellationAndRelease(t *testing.T) {
	f := newFlowFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- f.flow.withLane(ctx, func() error { close(entered); <-release; return ctx.Err() }) }()
	<-entered
	waitCtx, waitCancel := context.WithCancel(context.Background())
	waitAttempt := &laneAttemptContext{Context: waitCtx, attempt: make(chan struct{})}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- f.flow.withLane(waitAttempt, func() error { t.Error("canceled waiter acquired lane"); return nil })
	}()
	<-waitAttempt.attempt
	waitCancel()
	if err := <-waitDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cancel()
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := f.flow.withLane(context.Background(), func() error { return nil }); err != nil {
		t.Fatal("holder failed to release lane", err)
	}
}

func TestFlowLaneTopologyChangeAndSemanticOverlap(t *testing.T) {
	f := newFlowFixture(t)
	f.add(1, "Task", "", "OPEN")
	f.add(2, "Feature", "", "OPEN")
	a := f.event("A", "issues", 1)
	b := f.event("B", "issues", 2)
	oldPlanWrites := make(chan struct{})
	releaseA := make(chan struct{})
	modelB := make(chan struct{})
	allowB := make(chan struct{})
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	f.effectHook = func(label string) {
		if label == "status:item1" {
			close(oldPlanWrites)
			<-releaseA
		}
	}
	f.flow.cfg.Enricher = enrichFunc(func(ctx context.Context, in observe.PolicyInput) (semanticflow.PolicyInput, error) {
		if in.DeliveryID == "B" {
			close(modelB)
			<-allowB
		}
		return semanticflow.PolicyInput{Current: in}, ctx.Err()
	})
	semanticOrder := []string{}
	f.flow.cfg.Semantic = semanticFunc(func(ctx context.Context, in semanticflow.PolicyInput) error {
		semanticOrder = append(semanticOrder, in.Current.DeliveryID)
		return ctx.Err()
	})
	go func() { doneA <- f.flow.Process(context.Background(), a) }()
	<-oldPlanWrites
	BContext := &laneAttemptContext{Context: context.Background(), attempt: make(chan struct{})}
	go func() { doneB <- f.flow.Process(BContext, b) }()
	<-modelB
	// B's model ran while A held the lane. Its downstream consumer cannot enter.
	if !reflect.DeepEqual(semanticOrder, []string{"A"}) {
		t.Fatal(semanticOrder)
	}
	close(allowB)
	// Done is evaluated as B attempts the lane select, while A still owns it.
	<-BContext.attempt
	f.change(1, "Create-Branch: false", "OPEN")
	f.change(2, "Blocked-By: #1", "OPEN")
	close(releaseA)
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
	if err := <-doneB; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(semanticOrder, []string{"A", "B"}) || f.items[2].Fields[config.Status].OptionID != "blocked" {
		t.Fatal(semanticOrder, f.items, f.calls)
	}
	if len(f.snapshots) != 4 {
		t.Fatal("each event must compute and freshly recheck", len(f.snapshots))
	}
	for _, in := range f.branchInputs {
		if in.Context.Root.Number == 2 {
			t.Fatal("stale Ready authorization", in)
		}
	}
}

func TestFlowRoutingNotApplicableAndMalformed(t *testing.T) {
	f := newFlowFixture(t)
	f.add(1, "Task", "", "OPEN")
	for _, tc := range []struct {
		name, event, payload string
		want                 error
	}{
		{"ordinary push", "push", `{"repository":{"owner":{"login":"parametron-io"},"name":"engine"},"ref":"refs/heads/main"}`, worker.ErrNotApplicable},
		{"outside push", "push", `{"repository":{"owner":{"login":"outside"},"name":"repo"},"ref":"refs/heads/development/issue-1"}`, worker.ErrNotApplicable},
		{"malformed push", "push", `{"repository":{"owner":{"login":"parametron-io"},"name":"engine"}}`, ea.ErrInvalid},
		{"noncanonical number", "push", `{"repository":{"owner":{"login":"parametron-io"},"name":"engine"},"ref":"refs/heads/development/issue-01"}`, ea.ErrInvalid},
		{"unrelated project", "projects_v2_item", `{"action":"edited","projects_v2_item":{"node_id":"item","project_node_id":"other"}}`, worker.ErrNotApplicable},
		{"unsupported action", "projects_v2_item", `{"action":"deleted"}`, worker.ErrNotApplicable},
		{"malformed exact item", "projects_v2_item", `{"action":"edited","projects_v2_item":{"node_id":" ","project_node_id":"P"}}`, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.flow.Resolve(context.Background(), storage.Event{Delivery: storage.Delivery{EventName: tc.event, Payload: []byte(tc.payload)}})
			if !errors.Is(err, tc.want) || got != (storage.Resource{}) {
				t.Fatal(got, err, tc.want)
			}
		})
	}
	f.projectItem = github.CurrentProjectItem{ID: "item", ProjectID: "P", ContentKind: "Issue", Issue: &github.Identity{ID: "I1", Repository: f.d.Repositories[0], Number: 1}}
	e := storage.Event{Delivery: storage.Delivery{EventName: "projects_v2_item", Payload: []byte(`{"action":"edited","projects_v2_item":{"node_id":"item","project_node_id":"P"}}`)}}
	f.projectErr = &github.Error{Category: github.NotFound}
	if _, err := f.flow.Resolve(context.Background(), e); !errors.Is(err, worker.ErrNotApplicable) {
		t.Fatal(err)
	}
	f.projectErr = nil
	f.projectItem.Issue.Repository.ID = "other"
	if _, err := f.flow.Resolve(context.Background(), e); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	f.projectItem.Issue.Repository = github.Repository{ID: "other", Owner: "outside", Name: "other"}
	if _, err := f.flow.Resolve(context.Background(), e); !errors.Is(err, worker.ErrNotApplicable) {
		t.Fatal(err)
	}
}

func TestFlowResumeClosedOrIneligibleDoesNotFabricateCompletion(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ineligible", true: "closed"}[closed], func(t *testing.T) {
			f := newFlowFixture(t)
			f.add(1, "Task", "", "OPEN")
			if err := f.flow.Process(context.Background(), f.event("issue", "issues", 1)); err != nil {
				t.Fatal(err)
			}
			f.remote["development/issue-1"] = github.Branch{Repository: f.d.Repositories[0], Name: "development/issue-1", SHA: f.head}
			f.fail = "date:item1"
			if err := f.flow.Process(context.Background(), f.event("push", "push", 1)); err == nil {
				t.Fatal("missing activity failure")
			}
			if closed {
				f.change(1, "", "CLOSED")
			} else {
				f.change(1, "Create-Branch: false", "OPEN")
			}
			f.fail = ""
			f.calls = nil
			f.snapshots = nil
			if err := f.flow.Process(context.Background(), f.event("retry", "issues", 1)); err != nil {
				t.Fatal(err)
			}
			a, err := ea.New(f.flow.cfg.Activity)
			if err != nil {
				t.Fatal(err)
			}
			state, err := a.ActivityState(context.Background(), f.resource(1))
			if err != nil || !state.Accepted || state.Completed || state.DeliveryID != "push" {
				t.Fatal(state, err)
			}
			for _, call := range f.calls {
				if call == "date:item1" {
					t.Fatal("ineligible completion effects", f.calls)
				}
			}
			if !closed && f.items[1].Fields[config.Status].OptionID != "ready" {
				t.Fatal("unfinished acceptance supplied In Progress overlay", f.items)
			}
			if closed {
				if _, ok := f.items[1].Fields[config.RoadmapOrder]; ok {
					t.Fatal("closed accepted work not cleared")
				}
			}
		})
	}
}

func TestFlowWorkerLocalEndToEndAndFreshRetry(t *testing.T) {
	f := newFlowFixture(t)
	f.add(1, "Task", "Blocked-By: #2", "OPEN")
	f.add(2, "Bug", "", "OPEN")
	resolver, err := observe.NewResolver(f.d)
	if err != nil {
		t.Fatal(err)
	}
	f.flow.cfg.Resolver = resolver
	w, err := worker.New(worker.Config{Store: f.store, Resolver: f.flow, Processor: f.flow, Concurrency: 2, Clock: func() time.Time { return f.at.Add(time.Hour) }, Classifier: func(err error) worker.Classification {
		return worker.Classification{Retryable: false, Category: "test_terminal"}
	}, RetrySchedule: func(_ worker.Classification, _ int64, now time.Time) time.Time { return now.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id, event string, payload []byte) {
		t.Helper()
		_, err := f.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: event, Payload: payload, ReceivedAt: f.at.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	issuePayload := func(n int64) []byte {
		return []byte(fmt.Sprintf(`{"repository":{"owner":{"login":"parametron-io"},"name":"engine"},"issue":{"number":%d,"state":"untrusted"}}`, n))
	}
	step := func(id string) {
		t.Helper()
		if err := w.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		e, err := f.store.Event(context.Background(), id)
		if err != nil || e.State.Status != storage.Completed || e.Resource == nil {
			t.Fatal(e, err)
		}
	}
	insert("issue", "issues", issuePayload(1))
	step("issue")
	if f.items[1].Fields[config.Status].OptionID != "blocked" || len(f.remote) != 0 {
		t.Fatal(f.items, f.remote)
	}
	f.change(2, "", "CLOSED")
	insert("bug", "issues", issuePayload(2))
	step("bug")
	if f.items[1].Fields[config.Status].OptionID != "ready" || len(f.remote) != 1 || len(f.items[2].Fields) != 0 {
		t.Fatal(f.items, f.remote)
	}
	f.remote["development/issue-1"] = github.Branch{Repository: f.d.Repositories[0], Name: "development/issue-1", SHA: f.head}
	insert("push", "push", []byte(`{"repository":{"owner":{"login":"parametron-io"},"name":"engine"},"ref":"refs/heads/development/issue-1"}`))
	step("push")
	if f.items[1].Fields[config.Status].OptionID != "progress" || f.items[1].Fields[config.StartDate].Date != "2026-10-11" {
		t.Fatal(f.items)
	}
	// A manual field notification is resolved from current content, without a model.
	f.items[1].Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "blocked"}}
	f.items[1].Fields[config.RoadmapOrder] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Number, Number: 0.5}}
	i := f.issues[1].Identity
	f.projectItem = github.CurrentProjectItem{ID: "item", ProjectID: "P", ContentKind: "Issue", Issue: &i}
	insert("project", "projects_v2_item", []byte(`{"action":"edited","projects_v2_item":{"node_id":"item","project_node_id":"P","status":"Done"}}`))
	step("project")
	if f.items[1].Fields[config.Status].OptionID != "progress" || f.items[1].Fields[config.RoadmapOrder].Number != float64(eo.FirstRoadmapBase) {
		t.Fatal(f.items)
	}
	f.change(1, "Blocked-By: #2", "CLOSED")
	insert("close", "issues", issuePayload(1))
	step("close")
	if _, ok := f.items[1].Fields[config.RoadmapOrder]; ok {
		t.Fatal("closed root not cleared")
	}
	if f.items[1].Fields[config.Status].OptionID != "progress" || len(f.remote) != 1 {
		t.Fatal("invented Done or branch deletion", f.items, f.remote)
	}
	f.calls = nil
	insert("later", "issues", issuePayload(1))
	step("later")
	for _, call := range f.calls {
		if strings.HasPrefix(call, "status:") || strings.HasPrefix(call, "set:") || strings.HasPrefix(call, "clear:") || strings.HasPrefix(call, "create:") {
			t.Fatal("converged state repeated mutation", f.calls)
		}
	}
	// Duplicate durable delivery remains completed and introduces no authority.
	insert("later", "issues", issuePayload(1))
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFlowPartialFailureRecomputesChangedTopology(t *testing.T) {
	for _, stage := range []string{"status:item2", "set:item2", "create:development/issue-2"} {
		t.Run(stage, func(t *testing.T) {
			f := newFlowFixture(t)
			f.add(1, "Task", "", "OPEN")
			f.add(2, "Feature", "", "OPEN")
			f.fail = stage
			if err := f.flow.Process(context.Background(), f.event("first", "issues", 1)); err == nil {
				t.Fatal("named failure not injected", stage, f.calls)
			}
			if f.calls[len(f.calls)-1] != stage {
				t.Fatal("did not stop at named failure", f.calls)
			}
			firstSnapshots := len(f.snapshots)
			f.add(3, "Bug", "", "OPEN")
			f.change(2, "Blocked-By: #3", "OPEN")
			f.fail = ""
			f.calls = nil
			f.branchInputs = nil
			if err := f.flow.Process(context.Background(), f.event("retry", "issues", 1)); err != nil {
				t.Fatal(err)
			}
			if len(f.snapshots) != firstSnapshots+2 || f.items[2].Fields[config.Status].OptionID != "blocked" {
				t.Fatal("not fresh desired state", f.items, f.calls)
			}
			for _, call := range f.calls {
				if call == "status:item1" || call == "set:item1" && stage != "status:item2" || call == "create:development/issue-2" {
					t.Fatal("replayed old successful/stale work", f.calls)
				}
			}
			for _, in := range f.branchInputs {
				if in.Context.Root.Number == 2 {
					t.Fatal("replayed stale branch eligibility", in)
				}
			}
		})
	}
}

func TestFlowSemanticFailurePreventsEngineering(t *testing.T) {
	f := newFlowFixture(t)
	f.add(1, "Task", "", "OPEN")
	want := errors.New("bounded enrichment failure")
	f.flow.cfg.Enricher = enrichFunc(func(context.Context, observe.PolicyInput) (semanticflow.PolicyInput, error) {
		return semanticflow.PolicyInput{}, want
	})
	f.flow.cfg.Semantic = semanticFunc(func(context.Context, semanticflow.PolicyInput) error {
		t.Error("failed enrichment reached downstream")
		return nil
	})
	if err := f.flow.Process(context.Background(), f.event("issue", "issues", 1)); !errors.Is(err, want) || len(f.snapshots) != 0 || len(f.branchInputs) != 0 {
		t.Fatal(err, f.calls)
	}
}

func TestFlowCrossTargetBranchConsumedByActivityAndStoreRestart(t *testing.T) {
	f := newFlowFixture(t)
	f.add(1, "Task", "", "OPEN")
	f.add(2, "Feature", "", "OPEN")
	if err := f.flow.Process(context.Background(), f.event("origin", "issues", 1)); err != nil {
		t.Fatal(err)
	}
	if len(f.branchInputs) != 2 || f.origins[1].Resource != f.resource(1) {
		t.Fatal(f.origins)
	}
	f.remote["development/issue-2"] = github.Branch{Repository: f.d.Repositories[0], Name: "development/issue-2", SHA: f.head}
	if err := f.flow.Process(context.Background(), f.event("push-target", "push", 2)); err != nil {
		t.Fatal(err)
	}
	a, err := ea.New(f.flow.cfg.Activity)
	if err != nil {
		t.Fatal(err)
	}
	want, err := a.ActivityState(context.Background(), f.resource(2))
	if err != nil || !want.Completed || want.DeliveryID != "push-target" {
		t.Fatal(want, err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.branch, err = eb.New(eb.Config{Repositories: f.d.Repositories, Branches: f, Store: f.store, Clock: func() time.Time { return f.at }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.flow.cfg
	cfg.Activity.Store = f.store
	cfg.Activity.Provenance = f.branch
	a, err = ea.New(cfg.Activity)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Reconciler, err = er.New(er.Config{Project: f.d.Engineering, Projects: f, Activity: a, Fields: f, Numbers: f})
	if err != nil {
		t.Fatal(err)
	}
	f.flow, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := f.flow.Process(context.Background(), f.event("restart", "issues", 1)); err != nil {
		t.Fatal(err)
	}
	again, err := a.ActivityState(context.Background(), f.resource(2))
	if err != nil || again != want {
		t.Fatal(again, want, err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "status:") || strings.HasPrefix(call, "set:") || strings.HasPrefix(call, "date:") || strings.HasPrefix(call, "create:") {
			t.Fatal("restart repeated one-shot/converged writes", f.calls)
		}
	}
}
