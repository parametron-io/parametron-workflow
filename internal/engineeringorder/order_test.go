package engineeringorder_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"

	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func node(repo string, n int64, typ sp.IssueType) ec.IssueNode {
	return ec.IssueNode{Resource: storage.Resource{Owner: "parametron-io", Repository: repo, Kind: "issue", Number: n, NodeID: fmt.Sprintf("%s-%d", repo, n)}, State: ec.Open, Type: typ, TypeSource: ec.AcceptedSemantic, AcceptedClassification: &sp.IssueClassification{Type: typ, Labels: []sp.Label{}, Priority: sp.Medium, Effort: sp.M}}
}
func contextOf(nodes ...ec.IssueNode) ec.Context {
	return ec.Context{Root: nodes[0].Resource, Nodes: nodes}
}
func current(c ec.Context, indices ...int) eo.CurrentOrder {
	out := eo.CurrentOrder{Items: []eo.CurrentItem{}}
	for i, index := range indices {
		r := c.Nodes[index].Resource
		out.Items = append(out.Items, eo.CurrentItem{ProjectItemID: fmt.Sprint("item-", i), Resource: &r})
	}
	return out
}
func parent(c *ec.Context, child, p int) {
	a, b := &c.Nodes[child], c.Nodes[p].Resource
	a.Intent.Parent = &intent.IssueRef{Repository: b.Repository, Number: b.Number}
	c.Parents = append(c.Parents, ec.ParentEdge{Child: a.Resource, Parent: b})
}
func dep(c *ec.Context, a, b int) {
	c.Dependencies = append(c.Dependencies, ec.DependencyEdge{Blocker: c.Nodes[a].Resource, Blocked: c.Nodes[b].Resource})
}
func build(t *testing.T, c ec.Context, o eo.CurrentOrder) []storage.Resource {
	t.Helper()
	p, err := eo.BuildPhaseOrder(c, o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func lifecycle(t *testing.T, c ec.Context, p []storage.Resource) ep.Plan {
	t.Helper()
	l, err := ep.Evaluate(ep.Input{Context: c, PhaseOrder: p})
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func evaluate(t *testing.T, c ec.Context, o eo.CurrentOrder) eo.Plan {
	t.Helper()
	p := build(t, c, o)
	out, err := eo.Evaluate(eo.Input{Context: c, CurrentOrder: o, PhaseOrder: p, Lifecycle: lifecycle(t, c, p)})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func resources(c ec.Context, indices ...int) []storage.Resource {
	out := []storage.Resource{}
	for _, i := range indices {
		out = append(out, c.Nodes[i].Resource)
	}
	return out
}
func placed(p eo.Plan) []storage.Resource {
	out := []storage.Resource{}
	for _, v := range p.DesiredOrder {
		out = append(out, v.Resource)
	}
	return out
}
func TestPhaseOrder(t *testing.T) {
	for _, tc := range []struct {
		name         string
		setup        func(*ec.Context)
		manual, want []int
	}{
		{"one", func(c *ec.Context) { c.Nodes = c.Nodes[:1] }, nil, []int{0}},
		{"simple", func(c *ec.Context) { dep(c, 0, 1) }, []int{1, 0, 2, 3}, []int{0, 1, 2, 3}},
		{"chain", func(c *ec.Context) { dep(c, 0, 1); dep(c, 1, 2); dep(c, 2, 3) }, []int{3, 2, 1, 0}, []int{0, 1, 2, 3}},
		{"diamond", func(c *ec.Context) { dep(c, 0, 1); dep(c, 0, 2); dep(c, 1, 3); dep(c, 2, 3) }, []int{3, 2, 1, 0}, []int{0, 2, 1, 3}},
		{"manual unrelated", func(*ec.Context) {}, []int{3, 1, 2, 0}, []int{3, 1, 2, 0}},
		{"manual conflict", func(c *ec.Context) { dep(c, 0, 2) }, []int{2, 1, 0, 3}, []int{1, 0, 2, 3}},
		{"ranked first", func(*ec.Context) {}, []int{2}, []int{2, 0, 1, 3}},
		{"fallback", func(*ec.Context) {}, nil, []int{0, 1, 2, 3}},
		{"cross repository", func(c *ec.Context) { c.Nodes[1] = node("freecad", 2, sp.Phase); dep(c, 1, 0) }, []int{0, 1, 2, 3}, []int{1, 0, 2, 3}},
		{"closed topology", func(c *ec.Context) { c.Nodes[0].State = ec.Closed; dep(c, 0, 2) }, []int{2, 1, 0, 3}, []int{1, 0, 2, 3}},
		{"parent not topology", func(c *ec.Context) { parent(c, 0, 1) }, []int{0, 1, 2, 3}, []int{0, 1, 2, 3}},
		{"archived rank ignored", func(*ec.Context) {}, nil, []int{0, 1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := contextOf(node("engine", 10, sp.Phase), node("engine", 20, sp.Phase), node("engine", 30, sp.Phase), node("engine", 40, sp.Phase))
			tc.setup(&c)
			o := current(c, tc.manual...)
			if tc.name == "archived rank ignored" {
				o = current(c, 3)
				o.Items[0].Archived = true
			}
			got := build(t, c, o)
			if !reflect.DeepEqual(got, resources(c, tc.want...)) {
				t.Fatalf("%v want %v", got, resources(c, tc.want...))
			}
			// Direct #35 consumption preserves exact evidence and supplied activation order.
			l := lifecycle(t, c, got)
			for _, r := range got {
				if c.Nodes[slices.IndexFunc(c.Nodes, func(n ec.IssueNode) bool { return n.Resource == r })].State == ec.Closed {
					continue
				}
				if !slices.ContainsFunc(l.Decisions, func(d ep.Decision) bool { return d.Resource == r }) {
					t.Fatal("identity lost")
				}
			}
		})
	}
}
func TestPolicyDoesNotReorderPhaseOrder(t *testing.T) {
	c := contextOf(node("engine", 1, sp.Phase), node("engine", 2, sp.Phase))
	p := build(t, c, current(c, 1, 0))
	l := lifecycle(t, c, p)
	if !slices.ContainsFunc(l.Decisions, func(d ep.Decision) bool { return d.Resource == c.Nodes[1].Resource && d.PhaseActive }) {
		t.Fatal("#35 ignored supplied order")
	}
}
func integration() (ec.Context, eo.CurrentOrder) {
	c := contextOf(node("parametron-engine", 45, sp.Phase), node("parametron-engine", 48, sp.Task), node("parametron-engine", 49, sp.Task), node("parametron-engine", 60, sp.Phase), node("parametron-engine", 61, sp.Task), node("parametron-freecad", 70, sp.Phase), node("parametron-freecad", 71, sp.Feature), node("parametron-engine", 80, sp.Task), node("parametron-engine", 90, sp.Bug), node("parametron-freecad", 72, sp.Feature))
	parent(&c, 1, 0)
	parent(&c, 2, 0)
	parent(&c, 4, 3)
	parent(&c, 6, 5)
	parent(&c, 9, 0)
	parent(&c, 8, 0)
	dep(&c, 0, 5)
	dep(&c, 1, 2) // Cross-repo Phase topology; children keep manual order.
	c.Nodes[2].Intent.Explicit.SetPosition = intent.Boolean{Explicit: true, Value: false}
	c.Nodes[9].TypeSource = ec.ManualNative
	c.Nodes[9].AcceptedClassification = nil
	c.Nodes[9].Intent.Explicit.Automation = intent.Boolean{Explicit: true, Value: false}
	// First Phase, its second child, later Phase interleaving, then first child.
	// Child 72 has no manual position. Opaque landmarks are preserved input slots.
	o := current(c, 0, 2, 3, 4, 1, 5, 6, 7, 8)
	o.Items = append(o.Items, eo.CurrentItem{ProjectItemID: "draft"})
	return c, o
}
func TestPureIntegrationAndAnchor(t *testing.T) {
	c, o := integration()
	p := evaluate(t, c, o)
	if !reflect.DeepEqual(p.PhaseOrder, resources(c, 0, 3, 5)) {
		t.Fatal(p.PhaseOrder)
	}
	want := resources(c, 0, 2, 1, 9, 3, 4, 5, 6)
	if !reflect.DeepEqual(placed(p), want) {
		t.Fatalf("segments %v", placed(p))
	}
	for _, v := range p.DesiredOrder {
		if v.Resource == c.Nodes[2].Resource && v.PositionOwned {
			t.Fatal("unowned")
		}
		if v.Resource == c.Nodes[9].Resource && !v.PositionOwned {
			t.Fatal("Automation disabled position")
		}
		if v.AnchorPhase != nil {
			if !slices.Contains(c.Parents, ec.ParentEdge{Child: v.Resource, Parent: *v.AnchorPhase}) {
				t.Fatal("lost authoritative anchor")
			}
		}
	}
	l := lifecycle(t, c, p.PhaseOrder)
	if !slices.Contains(l.ParentlessReady, c.Nodes[7].Resource) {
		t.Fatal("missing parentless Ready")
	}
	// Simulate view filtering only, not In Progress lifecycle policy.
	remaining := []storage.Resource{}
	for _, v := range p.DesiredOrder {
		if v.Resource != c.Nodes[0].Resource && v.Resource != c.Nodes[1].Resource {
			remaining = append(remaining, v.Resource)
		}
	}
	if !reflect.DeepEqual(remaining, resources(c, 2, 9, 3, 4, 5, 6)) {
		t.Fatal("Ready children lost inherited slot", remaining)
	}
	original, _ := json.Marshal(p)
	random := rand.New(rand.NewSource(36))
	for i := 0; i < 20; i++ {
		random.Shuffle(len(c.Nodes), func(i, j int) { c.Nodes[i], c.Nodes[j] = c.Nodes[j], c.Nodes[i] })
		random.Shuffle(len(c.Parents), func(i, j int) { c.Parents[i], c.Parents[j] = c.Parents[j], c.Parents[i] })
		random.Shuffle(len(c.Dependencies), func(i, j int) { c.Dependencies[i], c.Dependencies[j] = c.Dependencies[j], c.Dependencies[i] })
		got, _ := json.Marshal(evaluate(t, c, o))
		if string(got) != string(original) {
			t.Fatal("nondeterministic output")
		}
	}
}
func TestSiblingFallbackAndClosed(t *testing.T) {
	c, o := integration()
	o = eo.CurrentOrder{}
	p := evaluate(t, c, o)
	if !reflect.DeepEqual(placed(p), resources(c, 0, 1, 2, 9, 3, 4, 5, 6)) {
		t.Fatal("canonical siblings")
	}
	c.Nodes[0].State = ec.Closed
	c.Nodes[1].State = ec.Closed
	p = evaluate(t, c, o)
	if !slices.Contains(p.PhaseOrder, c.Nodes[0].Resource) || slices.Contains(placed(p), c.Nodes[0].Resource) || slices.Contains(placed(p), c.Nodes[1].Resource) {
		t.Fatal("closed topology/action distinction")
	}
	if p.DesiredOrder[0].AnchorPhase == nil || *p.DesiredOrder[0].AnchorPhase != c.Nodes[0].Resource {
		t.Fatal("closed Phase children lost anchor")
	}
}
func TestSetPositionOwnershipOnly(t *testing.T) {
	c, o := integration()
	baseline := evaluate(t, c, o)
	baseLifecycle := lifecycle(t, c, baseline.PhaseOrder)
	for _, explicit := range []intent.Boolean{{}, {Explicit: true, Value: true}, {Explicit: true, Value: false}} {
		for i := range c.Nodes {
			c.Nodes[i].Intent.Explicit.SetPosition = explicit
		}
		p := evaluate(t, c, o)
		if !reflect.DeepEqual(p.PhaseOrder, baseline.PhaseOrder) || !reflect.DeepEqual(placed(p), placed(baseline)) {
			t.Fatal("ownership changed canonical order")
		}
		if !reflect.DeepEqual(lifecycle(t, c, p.PhaseOrder), baseLifecycle) {
			t.Fatal("ownership changed lifecycle")
		}
		for _, v := range p.DesiredOrder {
			if v.PositionOwned != (!explicit.Explicit || explicit.Value) {
				t.Fatal("ownership default/override")
			}
		}
	}
}
func TestInputOutputOwnership(t *testing.T) {
	c, o := integration()
	phases := build(t, c, o)
	l := lifecycle(t, c, phases)
	in := eo.Input{Context: c, CurrentOrder: o, PhaseOrder: phases, Lifecycle: l}
	before, _ := json.Marshal(in)
	p, err := eo.Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("input mutated")
	}
	p.PhaseOrder[0].NodeID = "changed"
	p.DesiredOrder[0].Resource.NodeID = "changed"
	p.DesiredOrder[1].AnchorPhase.NodeID = "changed"
	after, _ = json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("output aliases input")
	}
	p2 := evaluate(t, c, o)
	if p2.DesiredOrder[2].AnchorPhase.NodeID == "changed" {
		t.Fatal("output pointers shared")
	}
}
func TestMalformedInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind error
		edit func(*eo.Input)
	}{
		{"root", eo.ErrInvalid, func(in *eo.Input) { in.Context.Root.NodeID = "wrong" }},
		{"duplicate node", eo.ErrInvalid, func(in *eo.Input) { in.Context.Nodes = append(in.Context.Nodes, in.Context.Nodes[0]) }},
		{"node evidence", eo.ErrInvalid, func(in *eo.Input) { in.Context.Nodes[1].Resource.NodeID = in.Context.Nodes[0].Resource.NodeID }},
		{"missing phase", eo.ErrPhaseOrder, func(in *eo.Input) { in.PhaseOrder = in.PhaseOrder[1:] }},
		{"duplicate phase", eo.ErrPhaseOrder, func(in *eo.Input) { in.PhaseOrder = append(in.PhaseOrder, in.PhaseOrder[0]) }},
		{"wrong phase evidence", eo.ErrPhaseOrder, func(in *eo.Input) { in.PhaseOrder[0].NodeID = "wrong" }},
		{"nonphase", eo.ErrPhaseOrder, func(in *eo.Input) { in.PhaseOrder[0] = in.Context.Nodes[1].Resource }},
		{"topology", eo.ErrPhaseOrder, func(in *eo.Input) { in.PhaseOrder[0], in.PhaseOrder[2] = in.PhaseOrder[2], in.PhaseOrder[0] }},
		{"duplicate item", eo.ErrProjectOrder, func(in *eo.Input) { in.CurrentOrder.Items = append(in.CurrentOrder.Items, in.CurrentOrder.Items[0]) }},
		{"blank item", eo.ErrProjectOrder, func(in *eo.Input) { in.CurrentOrder.Items[0].ProjectItemID = " " }},
		{"duplicate active phase", eo.ErrProjectOrder, func(in *eo.Input) {
			i := in.CurrentOrder.Items[0]
			i.ProjectItemID = "another"
			in.CurrentOrder.Items = append(in.CurrentOrder.Items, i)
		}},
		{"archived duplicate", eo.ErrProjectOrder, func(in *eo.Input) {
			i := in.CurrentOrder.Items[0]
			i.ProjectItemID = "another"
			i.Archived = true
			in.CurrentOrder.Items = append(in.CurrentOrder.Items, i)
		}},
		{"unknown evidence", eo.ErrProjectOrder, func(in *eo.Input) {
			r := *in.CurrentOrder.Items[0].Resource
			r.NodeID = "wrong"
			in.CurrentOrder.Items[0].Resource = &r
		}},
		{"nodeID stolen", eo.ErrProjectOrder, func(in *eo.Input) {
			r := *in.CurrentOrder.Items[0].Resource
			r.Number = 1000
			in.CurrentOrder.Items[0].Resource = &r
		}},
		{"PR as issue", eo.ErrProjectOrder, func(in *eo.Input) {
			r := *in.CurrentOrder.Items[0].Resource
			r.Kind = "pull_request"
			in.CurrentOrder.Items[0].Resource = &r
		}},
		{"parent endpoint", eo.ErrInvalid, func(in *eo.Input) { in.Context.Parents[0].Parent.NodeID = "wrong" }},
		{"parent not phase", eo.ErrInvalid, func(in *eo.Input) {
			in.Context.Parents[0].Parent = in.Context.Nodes[4].Resource
			in.Context.Nodes[1].Intent.Parent = &intent.IssueRef{Repository: in.Context.Nodes[4].Resource.Repository, Number: in.Context.Nodes[4].Resource.Number}
		}},
		{"lifecycle unknown", eo.ErrPlacement, func(in *eo.Input) { in.Lifecycle.Decisions[0].Resource.Number = 1000 }},
		{"lifecycle wrong evidence", eo.ErrPlacement, func(in *eo.Input) { in.Lifecycle.Decisions[0].Resource.NodeID = "wrong" }},
		{"duplicate decision", eo.ErrPlacement, func(in *eo.Input) { in.Lifecycle.Decisions = append(in.Lifecycle.Decisions, in.Lifecycle.Decisions[0]) }},
		{"missing decision", eo.ErrPlacement, func(in *eo.Input) { in.Lifecycle.Decisions = in.Lifecycle.Decisions[1:] }},
		{"bug decision", eo.ErrPlacement, func(in *eo.Input) {
			in.Lifecycle.Decisions = append(in.Lifecycle.Decisions, ep.Decision{Resource: in.Context.Nodes[8].Resource})
		}},
		{"lifecycle parent", eo.ErrPlacement, func(in *eo.Input) {
			for i := range in.Lifecycle.Decisions {
				if in.Lifecycle.Decisions[i].ParentPhase != nil {
					in.Lifecycle.Decisions[i].ParentPhase = nil
					break
				}
			}
		}},
		{"parentless duplicate", eo.ErrPlacement, func(in *eo.Input) {
			in.Lifecycle.ParentlessReady = append(in.Lifecycle.ParentlessReady, in.Lifecycle.ParentlessReady[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, o := integration()
			p := build(t, c, o)
			in := eo.Input{Context: c, CurrentOrder: o, PhaseOrder: p, Lifecycle: lifecycle(t, c, p)}
			tc.edit(&in)
			got, err := eo.Evaluate(in)
			if !errors.Is(err, tc.kind) || !reflect.DeepEqual(got, eo.Plan{}) {
				t.Fatalf("got %+v %v want %v", got, err, tc.kind)
			}
		})
	}
}
func TestBuildRejectsMalformed(t *testing.T) {
	for _, mode := range []string{"cycle", "invalid root", "duplicate context", "duplicate membership", "bad dependency"} {
		t.Run(mode, func(t *testing.T) {
			c, o := integration()
			switch mode {
			case "cycle":
				dep(&c, 5, 0)
			case "invalid root":
				c.Root.NodeID = "wrong"
			case "duplicate context":
				c.Nodes = append(c.Nodes, c.Nodes[0])
			case "duplicate membership":
				i := o.Items[0]
				i.ProjectItemID = "duplicate"
				o.Items = append(o.Items, i)
			case "bad dependency":
				c.Dependencies[0].Blocked.NodeID = "wrong"
			}
			p, err := eo.BuildPhaseOrder(c, o)
			if err == nil || p != nil {
				t.Fatalf("accepted malformed stage 1 %v %v", p, err)
			}
		})
	}
}

func TestLongChain(t *testing.T) {
	nodes := make([]ec.IssueNode, 1000)
	for i := range nodes {
		nodes[i] = node("engine", int64(i+1), sp.Phase)
	}
	c := contextOf(nodes...)
	for i := 1; i < len(nodes); i++ {
		dep(&c, i-1, i)
	}
	indices := make([]int, len(nodes))
	for i := range indices {
		indices[i] = len(nodes) - 1 - i
	}
	order := build(t, c, current(c, indices...))
	for i, r := range order {
		if r != nodes[i].Resource {
			t.Fatal("long topology")
		}
	}
}
func TestNonPhaseDependenciesAndParentlessDoNotReorder(t *testing.T) {
	c, o := integration()
	baseline := evaluate(t, c, o)
	// Child edge opposes manual sibling order; removing it changes eligibility,
	// never canonical sibling order. Native SubIssues cannot enter Context/API.
	c.Dependencies = c.Dependencies[:1]
	c.Nodes[7].Intent.Blocks = []intent.IssueRef{{Repository: c.Nodes[0].Resource.Repository, Number: c.Nodes[0].Resource.Number}}
	dep(&c, 7, 0) // Task blocker affects #35, not Phase topology.
	got := evaluate(t, c, o)
	if !reflect.DeepEqual(got, baseline) {
		t.Fatal("non-Phase dependency affected order")
	}
	// Parentless and unrelated Issue positions remain input landmarks only.
	unrelated := node("unrelated", 1, sp.Task).Resource
	o.Items = append([]eo.CurrentItem{{ProjectItemID: "unrelated", Resource: &unrelated}}, o.Items...)
	got = evaluate(t, c, o)
	if !reflect.DeepEqual(got, baseline) {
		t.Fatal("unrelated/parentless items moved Phase segments")
	}
}
func TestEvaluateConsumesSuppliedOrder(t *testing.T) {
	c := contextOf(node("engine", 1, sp.Phase), node("engine", 2, sp.Phase))
	o := current(c, 1, 0)
	// Stage 2 validates but does not replace a valid supplied order with a new
	// order selected from a changed manual snapshot.
	phases := resources(c, 0, 1)
	p, err := eo.Evaluate(eo.Input{Context: c, CurrentOrder: o, PhaseOrder: phases, Lifecycle: lifecycle(t, c, phases)})
	if err != nil || !reflect.DeepEqual(p.PhaseOrder, phases) || !reflect.DeepEqual(placed(p), phases) {
		t.Fatal(p, err)
	}
}
func TestStageOneInputAndOutputOwnership(t *testing.T) {
	c, o := integration()
	before, _ := json.Marshal(struct {
		Context ec.Context
		Current eo.CurrentOrder
	}{c, o})
	p := build(t, c, o)
	after, _ := json.Marshal(struct {
		Context ec.Context
		Current eo.CurrentOrder
	}{c, o})
	if string(before) != string(after) {
		t.Fatal("Stage 1 mutated input")
	}
	p[0].NodeID = "changed"
	if c.Nodes[0].Resource.NodeID == "changed" || o.Items[0].Resource.NodeID == "changed" {
		t.Fatal("Stage 1 aliased input")
	}
}

func TestPhaseFallbackAcrossRepositories(t *testing.T) {
	c := contextOf(node("freecad", 1, sp.Phase), node("engine", 100, sp.Phase), node("engine", 9, sp.Phase), node("engine", 2, sp.Task))
	if got := build(t, c, eo.CurrentOrder{}); !reflect.DeepEqual(got, resources(c, 2, 1, 0)) {
		t.Fatal("canonical resource fallback", got)
	}
}
func TestNoPhases(t *testing.T) {
	c := contextOf(node("engine", 1, sp.Task), node("engine", 2, sp.Bug))
	p := evaluate(t, c, current(c, 1, 0))
	if len(p.PhaseOrder) != 0 || len(p.DesiredOrder) != 0 {
		t.Fatal("non-Phase/parentless acquired placement", p)
	}
}
func TestOutputAnchorPointersAreIndependent(t *testing.T) {
	c, o := integration()
	p := evaluate(t, c, o)
	p.DesiredOrder[1].AnchorPhase.NodeID = "modified"
	if p.DesiredOrder[2].AnchorPhase.NodeID == "modified" || p.PhaseOrder[0].NodeID == "modified" {
		t.Fatal("anchor pointers alias")
	}
}
