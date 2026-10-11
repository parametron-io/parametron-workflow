package engineeringreconcile

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type noAcceptance struct{}

func (noAcceptance) AcceptedIssue(context.Context, storage.Resource) (sp.AcceptedIssue, bool, error) {
	return sp.AcceptedIssue{}, false, nil
}

type reconcileFixture struct {
	t        *testing.T
	project  config.ResolvedProject
	repo     github.Repository
	issues   map[int64]github.Issue
	items    map[int64]observe.ProjectItem
	activity map[int64]ea.State
	calls    []string
	reads    []int64
	fail     string
	readEdit func(int64, *observe.ObservedState)
	reverse  bool
}

func newReconcileFixture(t *testing.T) *reconcileFixture {
	t.Helper()
	return &reconcileFixture{t: t, repo: github.Repository{ID: "R", Owner: "parametron-io", Name: "engine"}, project: config.ResolvedProject{ID: "P", Fields: map[config.FieldRole]string{config.Status: "status", config.RoadmapOrder: "roadmap"}, StatusOptions: map[config.StatusRole]string{config.Backlog: "backlog", config.Blocked: "blocked", config.Ready: "ready", config.InProgress: "progress"}}, issues: map[int64]github.Issue{}, items: map[int64]observe.ProjectItem{}, activity: map[int64]ea.State{}}
}
func (f *reconcileFixture) resource(n int64) storage.Resource {
	return storage.Resource{Owner: f.repo.Owner, Repository: f.repo.Name, Kind: "issue", Number: n, NodeID: fmt.Sprintf("I%d", n)}
}
func (f *reconcileFixture) add(n int64, typ, body, state string) {
	r := f.resource(n)
	f.issues[n] = github.Issue{Identity: github.Identity{ID: r.NodeID, Repository: f.repo, Number: int(n)}, Body: "Classification: false\n" + body, State: state, Type: &github.IssueType{ID: "type-" + typ, Name: typ}}
	f.items[n] = observe.ProjectItem{ID: fmt.Sprintf("item%d", n), Fields: map[config.FieldRole]observe.FieldValue{}}
}
func (f *reconcileFixture) ReadPrimary(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	i, ok := f.issues[r.Number]
	if !ok {
		return observe.ObservedState{Resource: r, Presence: observe.Missing}, nil
	}
	return observe.ObservedState{Resource: f.resource(r.Number), Presence: observe.Present, Issue: &i}, ctx.Err()
}
func (f *reconcileFixture) ReadCurrent(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	f.reads = append(f.reads, r.Number)
	o, err := f.ReadPrimary(ctx, r)
	i := f.items[r.Number]
	i.Fields = cloneFields(i.Fields)
	o.Projects = []observe.ProjectState{{Profile: config.Engineering, ID: "P", Items: []observe.ProjectItem{i}}}
	if f.readEdit != nil {
		f.readEdit(r.Number, &o)
	}
	return o, err
}
func cloneFields(in map[config.FieldRole]observe.FieldValue) map[config.FieldRole]observe.FieldValue {
	out := map[config.FieldRole]observe.FieldValue{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func (f *reconcileFixture) ListIssues(ctx context.Context, repo github.Repository) ([]github.ListedIssue, error) {
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
func (f *reconcileFixture) snapshot() ec.Snapshot {
	f.t.Helper()
	d := config.ResolvedConfig{Organization: config.Organization{Login: "parametron-io"}, Repositories: []config.Repository{f.repo}, IssueTypes: map[string]string{"Phase": "type-Phase", "Task": "type-Task", "Feature": "type-Feature", "Bug": "type-Bug"}}
	r, err := ec.New(ec.Config{Deployment: d, Primary: f, Lister: f, Accepted: noAcceptance{}})
	if err != nil {
		f.t.Fatal(err)
	}
	s, err := r.ResolveSnapshot(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *reconcileFixture) ActivityState(ctx context.Context, r storage.Resource) (ea.State, error) {
	return f.activity[r.Number], ctx.Err()
}
func (f *reconcileFixture) effect(label, item string, update func(*observe.ProjectItem)) error {
	f.calls = append(f.calls, label)
	if f.fail == label {
		return &github.Error{Category: github.Transient}
	}
	for n, i := range f.items {
		if i.ID == item {
			update(&i)
			f.items[n] = i
			return nil
		}
	}
	f.t.Fatal("wrong item", item)
	return nil
}
func (f *reconcileFixture) SetProjectOption(ctx context.Context, p, item, field, option string) error {
	if p != "P" || field != "status" {
		f.t.Fatal(p, field)
	}
	return f.effect("status:"+item+":"+option, item, func(i *observe.ProjectItem) {
		i.Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: option}}
	})
}
func (f *reconcileFixture) SetProjectDate(context.Context, string, string, string, string) error {
	f.t.Fatal("unexpected Start date write")
	return nil
}
func (f *reconcileFixture) SetProjectNumber(ctx context.Context, p, item, field string, v float64) error {
	if p != "P" || field != "roadmap" {
		f.t.Fatal(p, field)
	}
	return f.effect(fmt.Sprintf("set:%s:%g", item, v), item, func(i *observe.ProjectItem) {
		i.Fields[config.RoadmapOrder] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Number, Number: v}}
	})
}
func (f *reconcileFixture) ClearProjectNumber(ctx context.Context, p, item, field string) error {
	if p != "P" || field != "roadmap" {
		f.t.Fatal(p, field)
	}
	return f.effect("clear:"+item, item, func(i *observe.ProjectItem) { delete(i.Fields, config.RoadmapOrder) })
}
func (f *reconcileFixture) run(s ec.Snapshot, n int64) error {
	r, err := New(Config{Project: f.project, Projects: f, Activity: f, Fields: f, Numbers: f})
	if err != nil {
		f.t.Fatal(err)
	}
	return r.Reconcile(context.Background(), s, f.resource(n))
}
func (f *reconcileFixture) field(n int64, role config.FieldRole, v github.FieldValue) {
	i := f.items[n]
	i.Fields[role] = observe.FieldValue{FieldValue: v}
	f.items[n] = i
}

func TestReconcileCanonicalStatusAndActivityOverlay(t *testing.T) {
	for _, tc := range []struct {
		name, body, typ, want      string
		accepted, completed, owned bool
	}{
		{"Ready", "", "Task", "ready", false, false, true},
		{"Blocked", "Blocked-By: #2", "Task", "blocked", false, false, true},
		{"Backlog", "Parent: #3", "Task", "backlog", false, false, true},
		{"unfinished", "Blocked-By: #2", "Task", "blocked", true, false, true},
		{"completed Blocked", "Blocked-By: #2", "Task", "progress", true, true, true},
		{"completed inactive parent", "Parent: #3", "Task", "progress", true, true, true},
		{"completed disabled branch", "Create-Branch: false", "Feature", "progress", true, true, true},
		{"Phase itself completed", "", "Phase", "progress", true, true, true},
		{"unowned", "Set-Status: false", "Task", "", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			f.add(1, tc.typ, tc.body, "OPEN")
			f.add(2, "Bug", "", "OPEN")
			f.add(3, "Phase", "", "OPEN")
			f.add(0+4, "Phase", "", "OPEN")
			// Phase #3 becomes inactive by depending on Phase #4.
			if tc.want == "backlog" || tc.name == "completed inactive parent" {
				i := f.issues[3]
				i.Body += "\nBlocked-By: #4"
				f.issues[3] = i
			}
			f.field(1, config.Status, github.FieldValue{Kind: config.SingleSelect, OptionID: "progress"})
			f.activity[1] = ea.State{Accepted: tc.accepted, Completed: tc.completed}
			if err := f.run(f.snapshot(), 1); err != nil {
				t.Fatal(err)
			}
			got := f.items[1].Fields[config.Status].OptionID
			want := tc.want
			if !tc.owned {
				want = "progress"
			}
			if got != want {
				t.Fatal(got, want, f.calls)
			}
			if _, ok := f.items[2].Fields[config.Status]; ok {
				t.Fatal("Bug Status mutated")
			}
			f.calls = nil
			f.reads = nil
			if err := f.run(f.snapshot(), 1); err != nil || len(f.calls) != 0 {
				t.Fatal("converged state wrote again", err, f.calls)
			}
		})
	}
}

func TestReconcileCurrentDirectChildPhaseOverlay(t *testing.T) {
	for _, tc := range []struct {
		name, typ, state, parentBody string
		completed                    bool
		want                         string
	}{
		{"Task", "Task", "OPEN", "", true, "progress"},
		{"Feature", "Feature", "OPEN", "", true, "progress"},
		{"closed child", "Task", "CLOSED", "", true, "progress"},
		{"Bug", "Bug", "OPEN", "", true, "ready"},
		{"accepted unfinished", "Task", "OPEN", "", false, "ready"},
		{"parent unowned", "Task", "OPEN", "Set-Status: false", true, "manual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			f.add(1, "Phase", tc.parentBody, "OPEN")
			f.add(2, tc.typ, "Parent: #1", tc.state)
			f.field(1, config.Status, github.FieldValue{Kind: config.SingleSelect, OptionID: "manual"})
			f.activity[2] = ea.State{Accepted: true, Completed: tc.completed}
			if err := f.run(f.snapshot(), 1); err != nil {
				t.Fatal(err)
			}
			if got := f.items[1].Fields[config.Status].OptionID; got != tc.want {
				t.Fatal(got, tc.want, f.calls)
			}
		})
	}
	f := newReconcileFixture(t)
	f.add(1, "Phase", "", "OPEN")
	f.add(2, "Phase", "", "OPEN")
	f.add(3, "Task", "Parent: #1", "CLOSED")
	f.activity[3] = ea.State{Accepted: true, Completed: true}
	if err := f.run(f.snapshot(), 1); err != nil {
		t.Fatal(err)
	}
	i := f.issues[3]
	i.Body = "Classification: false\nParent: #2"
	f.issues[3] = i
	if err := f.run(f.snapshot(), 1); err != nil {
		t.Fatal(err)
	}
	if f.items[1].Fields[config.Status].OptionID != "ready" || f.items[2].Fields[config.Status].OptionID != "progress" {
		t.Fatal(f.items)
	}
}

func TestReconcileExactRoadmapPlanAndOrder(t *testing.T) {
	f := newReconcileFixture(t)
	f.add(1, "Phase", "", "OPEN")
	f.add(2, "Task", "Parent: #1", "CLOSED")
	f.add(3, "Feature", "Parent: #1", "OPEN")
	f.add(4, "Task", "", "OPEN")
	f.add(5, "Feature", "", "CLOSED")
	f.field(2, config.RoadmapOrder, github.FieldValue{Kind: config.Number, Number: 0})
	f.field(5, config.RoadmapOrder, github.FieldValue{Kind: config.Number, Number: 999})
	s := f.snapshot()
	c, err := s.Context(f.resource(1))
	if err != nil {
		t.Fatal(err)
	}
	_, lifecycle, roadmap, err := Compute(c)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{}
	for _, d := range lifecycle.Decisions {
		role := config.StatusRole(strings.ToLower(string(d.Status)))
		want = append(want, "status:"+f.items[d.Resource.Number].ID+":"+f.project.StatusOptions[role])
	}
	for _, r := range roadmap.Clear {
		want = append(want, "clear:"+f.items[r.Number].ID)
	}
	for _, a := range roadmap.Assignments {
		want = append(want, fmt.Sprintf("set:%s:%g", f.items[a.Resource.Number].ID, float64(a.RoadmapOrder)))
	}
	if err := f.run(s, 1); err != nil || !reflect.DeepEqual(f.calls, want) {
		t.Fatal(err, f.calls, want)
	}
	if _, ok := f.items[2].Fields[config.RoadmapOrder]; ok {
		t.Fatal("zero not cleared")
	}
	if _, ok := f.items[5].Fields[config.RoadmapOrder]; ok {
		t.Fatal("closed root not cleared")
	}
	for _, v := range []float64{0, 0.25, -100, 1e30, 1200001} {
		f.field(1, config.RoadmapOrder, github.FieldValue{Kind: config.Number, Number: v})
		f.calls = nil
		if err := f.run(f.snapshot(), 1); err != nil || len(f.calls) != 1 || f.items[1].Fields[config.RoadmapOrder].Number != float64(roadmap.Assignments[0].RoadmapOrder) {
			t.Fatal(v, err, f.calls)
		}
	}
	i := f.issues[3]
	i.State = "CLOSED"
	f.issues[3] = i
	if err := f.run(f.snapshot(), 1); err != nil {
		t.Fatal(err)
	}
	if f.items[4].Fields[config.RoadmapOrder].Number != float64(eo.FirstRoadmapBase+eo.RoadmapStride) {
		t.Fatal("root compaction", f.items)
	}
}

func TestReconcileRejectsWriteIdentityBeforeAnyMutation(t *testing.T) {
	for _, bad := range []string{"body", "state", "node", "missing membership", "multiple membership", "archived", "wrong Project", "wrong profile", "duplicate item", "Status kind", "Status date", "NaN", "Inf", "number kind"} {
		t.Run(bad, func(t *testing.T) {
			f := newReconcileFixture(t)
			f.add(1, "Task", "", "OPEN")
			f.add(2, "Task", "", "OPEN")
			s := f.snapshot()
			f.readEdit = func(n int64, o *observe.ObservedState) {
				if n != 2 {
					return
				}
				switch bad {
				case "body":
					o.Issue.Body += "\nCreate-Branch: false"
				case "state":
					o.Issue.State = "CLOSED"
				case "node":
					o.Resource.NodeID = "other"
					o.Issue.ID = "other"
				case "missing membership":
					o.Projects = nil
				case "multiple membership":
					o.Projects = append(o.Projects, o.Projects[0])
				case "archived":
					o.Projects[0].Items[0].Archived = true
				case "wrong Project":
					o.Projects[0].ID = "other"
				case "wrong profile":
					o.Projects[0].Profile = config.BugTracker
				case "duplicate item":
					o.Projects[0].Items[0].ID = f.items[1].ID
				case "Status kind":
					o.Projects[0].Items[0].Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Number}}
				case "Status date":
					o.Projects[0].Items[0].Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "ready", Date: "2026-10-11"}}
				case "NaN", "Inf", "number kind":
					v := math.NaN()
					if bad == "Inf" {
						v = math.Inf(1)
					}
					kind := config.Number
					if bad == "number kind" {
						kind = config.Date
						v = 0
					}
					o.Projects[0].Items[0].Fields[config.RoadmapOrder] = observe.FieldValue{FieldValue: github.FieldValue{Kind: kind, Number: v}}
				}
			}
			want := ErrProject
			if bad == "body" || bad == "state" || bad == "node" {
				want = ec.ErrChanged
			}
			if err := f.run(s, 1); !errors.Is(err, want) || len(f.calls) != 0 {
				t.Fatal(err, f.calls)
			}
		})
	}
}

func TestReconcilePartialFailureRetriesFreshDesiredState(t *testing.T) {
	for _, fail := range []string{"status:item3:ready", "clear:item2", fmt.Sprintf("set:item3:%g", float64(eo.FirstRoadmapBase+eo.DirectChildStride))} {
		t.Run(fail, func(t *testing.T) {
			f := newReconcileFixture(t)
			f.add(1, "Phase", "", "OPEN")
			f.add(2, "Task", "Parent: #1", "CLOSED")
			f.add(3, "Task", "Parent: #1", "OPEN")
			f.field(2, config.RoadmapOrder, github.FieldValue{Kind: config.Number, Number: 0})
			f.fail = fail
			if err := f.run(f.snapshot(), 1); err == nil {
				t.Fatal("failure stage not reached", f.calls)
			}
			if f.calls[len(f.calls)-1] != fail {
				t.Fatal("did not stop at first error", f.calls)
			}
			f.fail = ""
			f.calls = nil
			f.reads = nil
			if err := f.run(f.snapshot(), 1); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.reads, []int64{1, 2, 3}) {
				t.Fatal("not fresh Project observations", f.reads)
			}
			f.calls = nil
			if err := f.run(f.snapshot(), 1); err != nil || len(f.calls) != 0 {
				t.Fatal(err, f.calls)
			}
		})
	}
}

func TestReconcileClosedPhaseRetainsOpenChildrenWithoutStatusCorrection(t *testing.T) {
	f := newReconcileFixture(t)
	f.add(1, "Phase", "", "CLOSED")
	f.add(2, "Task", "Parent: #1", "OPEN")
	f.add(3, "Feature", "Parent: #1", "CLOSED")
	f.field(1, config.Status, github.FieldValue{Kind: config.SingleSelect, OptionID: "manual"})
	f.field(1, config.RoadmapOrder, github.FieldValue{Kind: config.Number, Number: 0})
	f.field(3, config.RoadmapOrder, github.FieldValue{Kind: config.Number, Number: 99})
	f.activity[1] = ea.State{Accepted: true, Completed: true}
	f.activity[3] = ea.State{Accepted: true, Completed: true}
	if err := f.run(f.snapshot(), 2); err != nil {
		t.Fatal(err)
	}
	if f.items[1].Fields[config.Status].OptionID != "manual" || f.items[2].Fields[config.Status].OptionID != "backlog" || f.items[2].Fields[config.RoadmapOrder].Number != float64(eo.FirstRoadmapBase+eo.DirectChildStride) {
		t.Fatal(f.items)
	}
	for _, n := range []int64{1, 3} {
		if _, ok := f.items[n].Fields[config.RoadmapOrder]; ok {
			t.Fatal("closed item not cleared", n)
		}
	}
}

func TestReconcileRootAndSiblingCompaction(t *testing.T) {
	f := newReconcileFixture(t)
	f.add(1, "Phase", "", "OPEN")
	f.add(2, "Task", "Parent: #1", "OPEN")
	f.add(3, "Feature", "Parent: #1", "OPEN")
	f.add(4, "Task", "", "OPEN")
	if err := f.run(f.snapshot(), 1); err != nil {
		t.Fatal(err)
	}
	i := f.issues[2]
	i.State = "CLOSED"
	f.issues[2] = i
	if err := f.run(f.snapshot(), 1); err != nil {
		t.Fatal(err)
	}
	if f.items[3].Fields[config.RoadmapOrder].Number != float64(eo.FirstRoadmapBase+eo.DirectChildStride) {
		t.Fatal("sibling did not compact", f.items)
	}
	for _, n := range []int64{1, 3} {
		i := f.issues[n]
		i.State = "CLOSED"
		f.issues[n] = i
	}
	if err := f.run(f.snapshot(), 4); err != nil {
		t.Fatal(err)
	}
	if f.items[4].Fields[config.RoadmapOrder].Number != float64(eo.FirstRoadmapBase) {
		t.Fatal("root did not compact", f.items)
	}
	for _, n := range []int64{1, 2, 3} {
		if _, ok := f.items[n].Fields[config.RoadmapOrder]; ok {
			t.Fatal("closed item retained Roadmap", n)
		}
	}
}

func TestReconcileNestedChildDoesNotStartAncestorPhase(t *testing.T) {
	f := newReconcileFixture(t)
	f.add(1, "Phase", "", "OPEN")
	f.add(2, "Phase", "Parent: #1", "OPEN")
	f.add(3, "Task", "Parent: #2", "CLOSED")
	f.activity[3] = ea.State{Accepted: true, Completed: true}
	if err := f.run(f.snapshot(), 1); err != nil {
		t.Fatal(err)
	}
	if f.items[1].Fields[config.Status].OptionID == "progress" || f.items[2].Fields[config.Status].OptionID != "progress" {
		t.Fatal("non-direct child started ancestor", f.items)
	}
}

func TestReconcilePermutationAndInputIndependence(t *testing.T) {
	var baselineCalls []string
	for _, reverse := range []bool{false, true} {
		f := newReconcileFixture(t)
		f.reverse = reverse
		f.add(1, "Phase", "", "OPEN")
		f.add(2, "Task", "Parent: #1", "OPEN")
		f.add(3, "Feature", "Parent: #1", "CLOSED")
		f.add(4, "Task", "", "OPEN")
		f.field(3, config.RoadmapOrder, github.FieldValue{Kind: config.Number, Number: 0})
		s := f.snapshot()
		in, err := s.Context(f.resource(1))
		if err != nil {
			t.Fatal(err)
		}
		before, err := s.Context(f.resource(1))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := Compute(in); err != nil || !reflect.DeepEqual(in, before) {
			t.Fatal("Compute mutated input", err)
		}
		if err := f.run(s, 1); err != nil {
			t.Fatal(err)
		}
		again, err := s.Context(f.resource(1))
		if err != nil || !reflect.DeepEqual(again, before) {
			t.Fatal("Reconcile mutated Snapshot", err)
		}
		if !reverse {
			baselineCalls = append([]string(nil), f.calls...)
		} else if !reflect.DeepEqual(f.calls, baselineCalls) {
			t.Fatal("enumeration changed write order", f.calls, baselineCalls)
		}
	}
}
