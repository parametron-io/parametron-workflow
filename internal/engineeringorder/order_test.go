package engineeringorder_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
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
func parent(c *ec.Context, child, p int) {
	a, b := &c.Nodes[child], c.Nodes[p].Resource
	a.Intent.Parent = &intent.IssueRef{Repository: b.Repository, Number: b.Number}
	c.Parents = append(c.Parents, ec.ParentEdge{Child: a.Resource, Parent: b})
}
func dep(c *ec.Context, a, b int) {
	c.Dependencies = append(c.Dependencies, ec.DependencyEdge{Blocker: c.Nodes[a].Resource, Blocked: c.Nodes[b].Resource})
}
func build(t *testing.T, c ec.Context) []storage.Resource {
	t.Helper()
	p, err := eo.BuildRoadmapOrder(c)
	if err != nil {
		t.Fatal(err)
	}
	return p.PhaseOrder
}
func lifecycle(t *testing.T, c ec.Context, p []storage.Resource) ep.Plan {
	t.Helper()
	l, err := ep.Evaluate(ep.Input{Context: c, PhaseOrder: p})
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func input(t *testing.T, c ec.Context) eo.Input {
	t.Helper()
	p, err := eo.BuildRoadmapOrder(c)
	if err != nil {
		t.Fatal(err)
	}
	return eo.Input{Context: c, Order: p, Lifecycle: lifecycle(t, c, p.PhaseOrder)}
}
func evaluate(t *testing.T, c ec.Context) eo.Plan {
	t.Helper()
	out, err := eo.Evaluate(input(t, c))
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
func assigned(p eo.Plan) []storage.Resource {
	out := []storage.Resource{}
	for _, v := range p.Assignments {
		out = append(out, v.Resource)
	}
	return out
}
func values(p eo.Plan) []int {
	out := []int{}
	for _, v := range p.Assignments {
		out = append(out, v.RoadmapOrder)
	}
	return out
}
func TestPhaseOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*ec.Context)
		want  []int
	}{
		{"one", func(c *ec.Context) { c.Nodes = c.Nodes[:1] }, []int{0}},
		{"simple dependency overrides identity", func(c *ec.Context) { dep(c, 1, 0) }, []int{1, 0, 2, 3}},
		{"chain", func(c *ec.Context) { dep(c, 3, 2); dep(c, 2, 1); dep(c, 1, 0) }, []int{3, 2, 1, 0}},
		{"diamond", func(c *ec.Context) { dep(c, 3, 2); dep(c, 3, 1); dep(c, 2, 0); dep(c, 1, 0) }, []int{3, 1, 2, 0}},
		{"canonical fallback", func(*ec.Context) {}, []int{0, 1, 2, 3}},
		{"cross repository", func(c *ec.Context) { c.Nodes[1] = node("freecad", 20, sp.Phase); dep(c, 1, 0) }, []int{2, 3, 1, 0}},
		{"closed topology", func(c *ec.Context) { c.Nodes[1].State = ec.Closed; dep(c, 1, 0) }, []int{1, 0, 2, 3}},
		{"parent not topology", func(c *ec.Context) { parent(c, 0, 1) }, []int{0, 1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := contextOf(node("engine", 10, sp.Phase), node("engine", 20, sp.Phase), node("engine", 30, sp.Phase), node("engine", 40, sp.Phase))
			tc.setup(&c)
			got := build(t, c)
			if !reflect.DeepEqual(got, resources(c, tc.want...)) {
				t.Fatalf("got %v want %v", got, resources(c, tc.want...))
			}
			before := append([]storage.Resource{}, got...)
			l := lifecycle(t, c, got)
			if !reflect.DeepEqual(got, before) {
				t.Fatal("#35 altered PhaseOrder")
			}
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
func TestLongChain(t *testing.T) {
	nodes := make([]ec.IssueNode, 1000)
	for i := range nodes {
		nodes[i] = node("engine", int64(i+1), sp.Phase)
	}
	c := contextOf(nodes...)
	for i := 1; i < len(nodes); i++ {
		dep(&c, i, i-1)
	}
	order := build(t, c)
	for i, r := range order {
		if r != nodes[len(nodes)-1-i].Resource {
			t.Fatal("long topology")
		}
	}
	// Full topology capacity is unbounded by numeric retained-segment capacity.
	for i := range c.Nodes {
		c.Nodes[i].State = ec.Closed
	}
	p := evaluate(t, c)
	if len(p.PhaseOrder) != 1000 || len(p.Assignments) != 0 || len(p.Clear) != 1000 {
		t.Fatal("completed topology lost")
	}
}
func TestNoPhases(t *testing.T) {
	c := contextOf(node("engine", 1, sp.Task), node("engine", 2, sp.Bug))
	p := evaluate(t, c)
	if p.PhaseOrder == nil || len(p.PhaseOrder) != 0 || !reflect.DeepEqual(p.RoadmapRoots, resources(c, 0)) || !reflect.DeepEqual(values(p), []int{10000}) || len(p.Clear) != 0 {
		t.Fatal(p)
	}
}
func TestPolicyConsumesSuppliedOrder(t *testing.T) {
	c := contextOf(node("engine", 1, sp.Phase), node("engine", 2, sp.Phase))
	phaseOrder := resources(c, 1, 0)
	l := lifecycle(t, c, phaseOrder)
	if !slices.ContainsFunc(l.Decisions, func(d ep.Decision) bool { return d.Resource == c.Nodes[1].Resource && d.PhaseActive }) {
		t.Fatal("#35 reordered")
	}
	p, err := eo.Evaluate(eo.Input{Context: c, Order: eo.Order{RoadmapRoots: append([]storage.Resource{}, phaseOrder...), PhaseOrder: phaseOrder}, Lifecycle: l})
	if err != nil || !reflect.DeepEqual(p.PhaseOrder, phaseOrder) || !reflect.DeepEqual(assigned(p), phaseOrder) {
		t.Fatal("stage 2 recomputed PhaseOrder", p, err)
	}
}
func integration() ec.Context {
	c := contextOf(node("parametron-engine", 45, sp.Phase), node("parametron-engine", 48, sp.Task), node("parametron-engine", 49, sp.Task), node("parametron-engine", 60, sp.Phase), node("parametron-engine", 61, sp.Task), node("parametron-freecad", 70, sp.Phase), node("parametron-freecad", 71, sp.Feature), node("parametron-engine", 80, sp.Task), node("parametron-engine", 90, sp.Bug), node("parametron-freecad", 72, sp.Feature), node("parametron-engine", 40, sp.Phase), node("parametron-engine", 41, sp.Task), node("parametron-engine", 47, sp.Task))
	parent(&c, 1, 0)
	parent(&c, 2, 0)
	parent(&c, 4, 3)
	parent(&c, 6, 5)
	parent(&c, 9, 0)
	parent(&c, 8, 0)
	parent(&c, 11, 10)
	parent(&c, 12, 0)
	dep(&c, 0, 5)
	dep(&c, 1, 2)
	dep(&c, 10, 0)
	c.Nodes[2].Intent.Explicit.SetPosition = intent.Boolean{Explicit: true, Value: false}
	c.Nodes[9].TypeSource = ec.ManualNative
	c.Nodes[9].AcceptedClassification = nil
	c.Nodes[9].Intent.Explicit.Automation = intent.Boolean{Explicit: true, Value: false}
	c.Nodes[10].State = ec.Closed
	c.Nodes[11].State = ec.Closed
	c.Nodes[12].State = ec.Closed
	return c
}
func TestPureIntegrationAndAnchor(t *testing.T) {
	c := integration()
	in := input(t, c)
	p, err := eo.Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.PhaseOrder, resources(c, 10, 0, 3, 5)) {
		t.Fatal("full topology", p.PhaseOrder)
	}
	if !reflect.DeepEqual(p.RoadmapRoots, resources(c, 10, 0, 3, 7, 5)) || !reflect.DeepEqual(assigned(p), resources(c, 0, 1, 2, 9, 3, 4, 7, 5, 6)) || !reflect.DeepEqual(values(p), []int{10000, 10010, 10020, 10030, 11000, 11010, 12000, 13000, 13010}) {
		t.Fatal("numeric grouping", p)
	}
	if !reflect.DeepEqual(p.Clear, resources(c, 10, 11, 12)) {
		t.Fatal("clear", p.Clear)
	}
	for _, a := range p.Assignments {
		if a.AnchorPhase != nil && !slices.Contains(c.Parents, ec.ParentEdge{Child: a.Resource, Parent: *a.AnchorPhase}) {
			t.Fatal("authoritative anchor lost")
		}
	}
	d := in.Lifecycle.Decisions[slices.IndexFunc(in.Lifecycle.Decisions, func(d ep.Decision) bool { return d.Resource == c.Nodes[2].Resource })]
	if d.Status != ep.Ready || !d.HasActiveBlocker || d.ExecutionEligible {
		t.Fatal("#35 blocked child eligibility")
	}
	if !slices.Contains(in.Lifecycle.ParentlessReady, c.Nodes[7].Resource) || !slices.Contains(assigned(p), c.Nodes[7].Resource) || slices.Contains(p.Clear, c.Nodes[7].Resource) || slices.Contains(p.PhaseOrder, c.Nodes[7].Resource) {
		t.Fatal("parentless ownership")
	}
	// Simulate only Status/view filtering, with no In Progress transition policy.
	remaining := []eo.Assignment{}
	for _, a := range p.Assignments {
		if a.Resource != c.Nodes[0].Resource && a.Resource != c.Nodes[1].Resource {
			remaining = append(remaining, a)
		}
	}
	if remaining[0].Resource != c.Nodes[2].Resource || remaining[0].RoadmapOrder != 10020 || remaining[2].RoadmapOrder != 11000 {
		t.Fatal("inherited numeric anchor", remaining)
	}
	assertNamespace(t, p)
	// A completed earlier segment has already compacted Phase 45 from 11000.
	c.Nodes[10].State = ec.Open
	c.Nodes[11].State = ec.Open
	before := evaluate(t, c)
	if before.Assignments[2].Resource != c.Nodes[0].Resource || before.Assignments[2].RoadmapOrder != 11000 {
		t.Fatal("reopened segment projection")
	}
}
func assertNamespace(t *testing.T, p eo.Plan) {
	t.Helper()
	seen := map[int]bool{}
	for _, a := range p.Assignments {
		if a.RoadmapOrder < 10000 || a.RoadmapOrder > 99999 || a.RoadmapOrder%10 != 0 || seen[a.RoadmapOrder] {
			t.Fatal("namespace", a)
		}
		seen[a.RoadmapOrder] = true
		for offset := 1; offset <= eo.MaxCompanionsPerIssue; offset++ {
			if slices.ContainsFunc(p.Assignments, func(b eo.Assignment) bool { return b.RoadmapOrder == a.RoadmapOrder+offset }) {
				t.Fatal("reserved companion slot used")
			}
		}
	}
}
func TestCompaction(t *testing.T) {
	c := contextOf(node("engine", 1, sp.Phase), node("engine", 2, sp.Phase), node("engine", 3, sp.Phase), node("engine", 4, sp.Task), node("engine", 5, sp.Task), node("engine", 6, sp.Feature))
	parent(&c, 3, 0)
	parent(&c, 4, 0)
	parent(&c, 5, 1)
	if p := evaluate(t, c); !reflect.DeepEqual(values(p), []int{10000, 10010, 10020, 11000, 11010, 12000}) {
		t.Fatal(p)
	}
	c.Nodes[3].State = ec.Closed
	p := evaluate(t, c)
	if !reflect.DeepEqual(values(p), []int{10000, 10010, 11000, 11010, 12000}) || !reflect.DeepEqual(p.Clear, resources(c, 3)) {
		t.Fatal("closed child compaction", p)
	}
	c.Nodes[0].State = ec.Closed
	p = evaluate(t, c)
	if !reflect.DeepEqual(values(p), []int{10010, 11000, 11010, 12000}) || p.Assignments[0].Resource != c.Nodes[4].Resource || *p.Assignments[0].AnchorPhase != c.Nodes[0].Resource || !slices.Contains(p.Clear, c.Nodes[0].Resource) {
		t.Fatal("closed parent retained segment", p)
	}
	c.Nodes[4].State = ec.Closed
	p = evaluate(t, c)
	if !reflect.DeepEqual(values(p), []int{10000, 10010, 11000}) || !reflect.DeepEqual(p.Clear, resources(c, 0, 3, 4)) || !reflect.DeepEqual(p.PhaseOrder, resources(c, 0, 1, 2)) {
		t.Fatal("completed segment compaction", p)
	}
	c.Nodes[1].State = ec.Closed
	c.Nodes[5].State = ec.Closed
	p = evaluate(t, c)
	if !reflect.DeepEqual(values(p), []int{10000}) || p.Assignments[0].Resource != c.Nodes[2].Resource {
		t.Fatal("second segment compaction", p)
	}
}
func TestCapacity(t *testing.T) {
	if eo.FirstRoadmapBase != 10000 || eo.RoadmapStride != 1000 || eo.DirectChildStride != 10 || eo.MaxRoadmapSegments != 90 || eo.MaxDirectChildren != 99 || eo.MaxCompanionsPerIssue != 9 {
		t.Fatal("v1 constants")
	}
	for _, phases := range []int{90, 91} {
		nodes := []ec.IssueNode{}
		for i := 0; i < phases; i++ {
			nodes = append(nodes, node("engine", int64(i+1), sp.Phase))
		}
		c := contextOf(nodes...)
		in := input(t, c)
		p, err := eo.Evaluate(in)
		if phases == 91 {
			if !errors.Is(err, eo.ErrRoadmapCapacity) || !reflect.DeepEqual(p, eo.Plan{}) {
				t.Fatal("capacity failure returned plan", p, err)
			}
		} else {
			if err != nil || p.Assignments[89].RoadmapOrder != 99000 {
				t.Fatal(p, err)
			}
			assertNamespace(t, p)
		}
	}
	for _, children := range []int{99, 100} {
		c := contextOf(node("engine", 1, sp.Phase))
		for i := 0; i < children; i++ {
			c.Nodes = append(c.Nodes, node("engine", int64(i+2), sp.Task))
			parent(&c, i+1, 0)
		}
		in := input(t, c)
		p, err := eo.Evaluate(in)
		if children == 100 {
			if !errors.Is(err, eo.ErrRoadmapCapacity) || !reflect.DeepEqual(p, eo.Plan{}) {
				t.Fatal("child capacity", p, err)
			}
		} else {
			if err != nil || len(p.Assignments) != 100 || p.Assignments[99].RoadmapOrder != 10990 {
				t.Fatal(p, err)
			}
			assertNamespace(t, p)
		}
		// Closed children do not consume active child capacity.
		c.Nodes[1].State = ec.Closed
		p = evaluate(t, c)
		assertNamespace(t, p)
	}
	// Highest segment plus its last direct child remains five-digit.
	c := contextOf(node("engine", 1, sp.Phase))
	for i := 1; i < 90; i++ {
		c.Nodes = append(c.Nodes, node("engine", int64(i+1), sp.Phase))
	}
	for i := 0; i < 99; i++ {
		c.Nodes = append(c.Nodes, node("engine", int64(i+100), sp.Task))
		parent(&c, 90+i, 89)
	}
	p := evaluate(t, c)
	if p.Assignments[len(p.Assignments)-1].RoadmapOrder != 99990 {
		t.Fatal("upper namespace boundary")
	}
	assertNamespace(t, p)
}
func TestParentlessAndNonPhaseDependencies(t *testing.T) {
	c := integration()
	baseline := evaluate(t, c)
	c.Dependencies = c.Dependencies[:1]
	dep(&c, 8, 0)
	if p := evaluate(t, c); !reflect.DeepEqual(p, baseline) {
		t.Fatal("parentless/child/Bug dependencies affected numeric order", p)
	}
}
func TestSetPositionAndAutomationHaveNoAuthority(t *testing.T) {
	c := integration()
	baseline := evaluate(t, c)
	l := lifecycle(t, c, baseline.PhaseOrder)
	for _, b := range []intent.Boolean{{}, {Explicit: true, Value: true}, {Explicit: true, Value: false}} {
		for i := range c.Nodes {
			c.Nodes[i].Intent.Explicit.SetPosition = b
		}
		if p := evaluate(t, c); !reflect.DeepEqual(p, baseline) {
			t.Fatal("Set-Position altered roadmap")
		}
		if got := lifecycle(t, c, baseline.PhaseOrder); !reflect.DeepEqual(got, l) {
			t.Fatal("Set-Position altered lifecycle")
		}
	}
	for i := range c.Nodes {
		c.Nodes[i].Intent.Explicit.Automation = intent.Boolean{Explicit: true, Value: false}
	}
	if p := evaluate(t, c); !reflect.DeepEqual(p, baseline) {
		t.Fatal("Automation altered roadmap")
	}
	data, _ := json.Marshal(baseline)
	if strings.Contains(string(data), "PositionOwned") {
		t.Fatal("obsolete ownership output")
	}
}
func TestDeterminismAndOwnership(t *testing.T) {
	c := integration()
	in := input(t, c)
	before, _ := json.Marshal(in)
	p, err := eo.Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("input mutated")
	}
	original, _ := json.Marshal(p)
	random := rand.New(rand.NewSource(36))
	for i := 0; i < 20; i++ {
		random.Shuffle(len(c.Nodes), func(i, j int) { c.Nodes[i], c.Nodes[j] = c.Nodes[j], c.Nodes[i] })
		random.Shuffle(len(c.Parents), func(i, j int) { c.Parents[i], c.Parents[j] = c.Parents[j], c.Parents[i] })
		random.Shuffle(len(c.Dependencies), func(i, j int) { c.Dependencies[i], c.Dependencies[j] = c.Dependencies[j], c.Dependencies[i] })
		got, _ := json.Marshal(evaluate(t, c))
		if string(got) != string(original) {
			t.Fatal("nondeterministic output")
		}
	}
	// Shuffling above reorders Context slices; snapshot that new input before
	// mutating any returned slice or pointer.
	unchanged, _ := json.Marshal(in)
	p.PhaseOrder[0].NodeID = "changed"
	p.RoadmapRoots[0].NodeID = "changed"
	p.Assignments[0].Resource.NodeID = "changed"
	p.Assignments[1].AnchorPhase.NodeID = "changed"
	p.Clear[0].NodeID = "changed"
	if p.Assignments[2].AnchorPhase.NodeID == "changed" {
		t.Fatal("shared anchor pointer")
	}
	second := build(t, c)
	second[0].NodeID = "changed"
	now, _ := json.Marshal(in)
	if string(unchanged) != string(now) {
		t.Fatal("output aliases input")
	}
	if got, _ := json.Marshal(evaluate(t, c)); string(got) != string(original) {
		t.Fatal("output mutation reached Context")
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
		{"duplicate node ID", eo.ErrInvalid, func(in *eo.Input) { in.Context.Nodes[1].Resource.NodeID = in.Context.Nodes[0].Resource.NodeID }},
		{"missing phase", eo.ErrPhaseOrder, func(in *eo.Input) { in.Order.PhaseOrder = in.Order.PhaseOrder[1:] }},
		{"duplicate phase", eo.ErrPhaseOrder, func(in *eo.Input) { in.Order.PhaseOrder = append(in.Order.PhaseOrder, in.Order.PhaseOrder[0]) }},
		{"wrong phase evidence", eo.ErrPhaseOrder, func(in *eo.Input) { in.Order.PhaseOrder[0].NodeID = "wrong" }},
		{"nonphase", eo.ErrPhaseOrder, func(in *eo.Input) { in.Order.PhaseOrder[0] = in.Context.Nodes[1].Resource }},
		{"topology", eo.ErrPhaseOrder, func(in *eo.Input) {
			in.Order.PhaseOrder[0], in.Order.PhaseOrder[3] = in.Order.PhaseOrder[3], in.Order.PhaseOrder[0]
		}},
		{"parent endpoint", eo.ErrInvalid, func(in *eo.Input) { in.Context.Parents[0].Parent.NodeID = "wrong" }},
		{"nested parent rejected", eo.ErrInvalid, func(in *eo.Input) {
			in.Context.Parents[0].Parent = in.Context.Nodes[2].Resource
			in.Context.Nodes[1].Intent.Parent = &intent.IssueRef{Repository: in.Context.Nodes[2].Resource.Repository, Number: in.Context.Nodes[2].Resource.Number}
		}},
		{"lifecycle unknown", eo.ErrRoadmap, func(in *eo.Input) { in.Lifecycle.Decisions[0].Resource.Number = 1000 }},
		{"lifecycle evidence", eo.ErrRoadmap, func(in *eo.Input) { in.Lifecycle.Decisions[0].Resource.NodeID = "wrong" }},
		{"duplicate decision", eo.ErrRoadmap, func(in *eo.Input) { in.Lifecycle.Decisions = append(in.Lifecycle.Decisions, in.Lifecycle.Decisions[0]) }},
		{"missing decision", eo.ErrRoadmap, func(in *eo.Input) { in.Lifecycle.Decisions = in.Lifecycle.Decisions[1:] }},
		{"Bug decision", eo.ErrRoadmap, func(in *eo.Input) {
			in.Lifecycle.Decisions = append(in.Lifecycle.Decisions, ep.Decision{Resource: in.Context.Nodes[8].Resource})
		}},
		{"lifecycle parent", eo.ErrRoadmap, func(in *eo.Input) {
			for i := range in.Lifecycle.Decisions {
				if in.Lifecycle.Decisions[i].ParentPhase != nil {
					in.Lifecycle.Decisions[i].ParentPhase = nil
					break
				}
			}
		}},
		{"unsupported status", eo.ErrRoadmap, func(in *eo.Input) { in.Lifecycle.Decisions[0].Status = "invented" }},
		{"parentless duplicate", eo.ErrRoadmap, func(in *eo.Input) {
			in.Lifecycle.ParentlessReady = append(in.Lifecycle.ParentlessReady, in.Lifecycle.ParentlessReady[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input(t, integration())
			tc.edit(&in)
			p, err := eo.Evaluate(in)
			if !errors.Is(err, tc.kind) || !reflect.DeepEqual(p, eo.Plan{}) {
				t.Fatalf("got %v %v want %v", p, err, tc.kind)
			}
		})
	}
}
func TestBuildRejectsMalformed(t *testing.T) {
	for _, mode := range []string{"cycle", "root", "duplicate node", "dependency endpoint"} {
		t.Run(mode, func(t *testing.T) {
			c := integration()
			switch mode {
			case "cycle":
				dep(&c, 5, 0)
			case "root":
				c.Root.NodeID = "wrong"
			case "duplicate node":
				c.Nodes = append(c.Nodes, c.Nodes[0])
			case "dependency endpoint":
				c.Dependencies[0].Blocked.NodeID = "wrong"
			}
			p, err := eo.BuildRoadmapOrder(c)
			if err == nil || !reflect.DeepEqual(p, eo.Order{}) {
				t.Fatal("accepted malformed Context", p, err)
			}
		})
	}
}

func TestClosedStandaloneWorkClearedAndBugExcluded(t *testing.T) {
	c := integration()
	c.Nodes[7].State = ec.Closed
	c.Nodes[8].State = ec.Closed // Bug is outside Engineering derived-field ownership.
	c.Nodes = append(c.Nodes, node("parametron-engine", 81, sp.Feature))
	c.Nodes[len(c.Nodes)-1].State = ec.Closed
	p := evaluate(t, c)
	for _, index := range []int{7, len(c.Nodes) - 1} {
		r := c.Nodes[index].Resource
		if slices.Contains(assigned(p), r) || !slices.Contains(p.Clear, r) {
			t.Fatal("closed root not cleared", r)
		}
	}
	if slices.Contains(p.Clear, c.Nodes[8].Resource) || slices.Contains(assigned(p), c.Nodes[8].Resource) {
		t.Fatal("Bug field claimed")
	}
}

func mixedRoots() ec.Context {
	return contextOf(node("engine", 1, sp.Phase), node("engine", 2, sp.Task), node("engine", 3, sp.Phase), node("engine", 4, sp.Feature))
}
func TestMixedRoadmapRoots(t *testing.T) {
	c := mixedRoots()
	p := evaluate(t, c)
	if !reflect.DeepEqual(p.RoadmapRoots, resources(c, 0, 1, 2, 3)) || !reflect.DeepEqual(p.PhaseOrder, resources(c, 0, 2)) || !reflect.DeepEqual(values(p), []int{10000, 11000, 12000, 13000}) {
		t.Fatal("mixed root projection", p)
	}
	for _, a := range p.Assignments {
		if a.AnchorPhase != nil {
			t.Fatal("root has Phase anchor", a)
		}
	}
	// Direct #35 handoff excludes standalone Task and Feature from PhaseOrder.
	in := input(t, c)
	if !reflect.DeepEqual(in.Order.PhaseOrder, resources(c, 0, 2)) {
		t.Fatal("standalone roots leaked into PhaseOrder")
	}
	if !slices.ContainsFunc(in.Lifecycle.Decisions, func(d ep.Decision) bool { return d.Resource == c.Nodes[0].Resource && d.PhaseActive }) {
		t.Fatal("Phase-only selection changed")
	}
	assertNamespace(t, p)
}
func TestTopLevelDependencies(t *testing.T) {
	for _, tc := range []struct {
		name         string
		edges        [][2]int
		want, phases []int
	}{
		{"Phase to Task", [][2]int{{2, 1}}, []int{0, 2, 1, 3}, []int{0, 2}},
		{"Task to Phase", [][2]int{{1, 0}}, []int{1, 0, 2, 3}, []int{0, 2}},
		{"Task to Feature", [][2]int{{1, 3}}, []int{0, 1, 2, 3}, []int{0, 2}},
		{"Feature before Task overrides identity", [][2]int{{3, 1}}, []int{0, 2, 3, 1}, []int{0, 2}},
		{"transitive Phase projection through Task", [][2]int{{2, 1}, {1, 0}}, []int{2, 1, 0, 3}, []int{2, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mixedRoots()
			for _, e := range tc.edges {
				dep(&c, e[0], e[1])
			}
			p := evaluate(t, c)
			if !reflect.DeepEqual(p.RoadmapRoots, resources(c, tc.want...)) || !reflect.DeepEqual(p.PhaseOrder, resources(c, tc.phases...)) || !reflect.DeepEqual(assigned(p), p.RoadmapRoots) {
				t.Fatal("root topology or Phase projection", p)
			}
			if !reflect.DeepEqual(values(p), []int{10000, 11000, 12000, 13000}) {
				t.Fatal("dependency numeric slots", p)
			}
		})
	}
}
func TestCrossLevelDependenciesRejected(t *testing.T) {
	// Two Phase-owned children plus standalone Task/Feature roots.
	for _, edge := range [][2]int{{0, 5}, {5, 0}, {1, 2}, {2, 1}, {3, 1}, {1, 3}} {
		c := mixedRoots()
		c.Nodes = append(c.Nodes, node("engine", 5, sp.Task), node("engine", 6, sp.Feature))
		parent(&c, 4, 0)
		parent(&c, 5, 2)
		// Cases referencing index 1 treat it as a Phase-owned Task in this fixture.
		parent(&c, 1, 0)
		// Index 2 is the unrelated Phase; index 3 a standalone Feature.
		dep(&c, edge[0], edge[1])
		o, err := eo.BuildRoadmapOrder(c)
		if !errors.Is(err, eo.ErrRoadmapHierarchy) || !reflect.DeepEqual(o, eo.Order{}) {
			t.Fatal("cross-level dependency accepted/promoted", edge, o, err)
		}
		// Stage 2 must reject newly introduced cross-level intent too, even with a
		// previously valid topology/lifecycle snapshot.
		c.Dependencies = nil
		in := input(t, c)
		dep(&in.Context, edge[0], edge[1])
		p, err := eo.Evaluate(in)
		if !errors.Is(err, eo.ErrRoadmapHierarchy) || !reflect.DeepEqual(p, eo.Plan{}) {
			t.Fatal("projection accepted cross-level drift", p, err)
		}
	}
}
func TestChildDependenciesRemainLifecycleFacts(t *testing.T) {
	c := mixedRoots()
	c.Nodes = append(c.Nodes, node("engine", 5, sp.Task), node("engine", 6, sp.Feature), node("engine", 7, sp.Task))
	parent(&c, 4, 0)
	parent(&c, 5, 2)
	parent(&c, 6, 0)
	baseline := evaluate(t, c)
	dep(&c, 4, 5) // Cross-Phase child execution dependency, not promoted to Phase A -> B.
	dep(&c, 6, 4) // Opposes canonical sibling order; numeric values do not change.
	if p := evaluate(t, c); !reflect.DeepEqual(p, baseline) {
		t.Fatal("child edges reordered roots/siblings", p)
	}
	l := input(t, c).Lifecycle
	if !slices.ContainsFunc(l.Decisions, func(d ep.Decision) bool {
		return d.Resource == c.Nodes[4].Resource && d.HasActiveBlocker && !d.ExecutionEligible
	}) {
		t.Fatal("child blocker eligibility not retained")
	}
	// A Phase and its own child are in the same segment, so this remains an
	// execution fact rather than a cross-level relationship to another objective.
	dep(&c, 0, 6)
	if p := evaluate(t, c); !reflect.DeepEqual(p, baseline) {
		t.Fatal("same-Phase dependency affected topology")
	}
}
func TestStandaloneCloseCompactsAndParentAssignmentRegroups(t *testing.T) {
	c := mixedRoots()
	c.Nodes[1].State = ec.Closed
	p := evaluate(t, c)
	if !reflect.DeepEqual(p.RoadmapRoots, resources(c, 0, 1, 2, 3)) || !reflect.DeepEqual(assigned(p), resources(c, 0, 2, 3)) || !reflect.DeepEqual(values(p), []int{10000, 11000, 12000}) || !reflect.DeepEqual(p.Clear, resources(c, 1)) {
		t.Fatal("closed standalone compaction", p)
	}
	c.Nodes[1].State = ec.Open
	parent(&c, 1, 0)
	p = evaluate(t, c)
	if !reflect.DeepEqual(p.RoadmapRoots, resources(c, 0, 2, 3)) || !reflect.DeepEqual(values(p), []int{10000, 10010, 11000, 12000}) || p.Assignments[1].Resource != c.Nodes[1].Resource || p.Assignments[1].AnchorPhase == nil || *p.Assignments[1].AnchorPhase != c.Nodes[0].Resource {
		t.Fatal("root to Phase member regrouping", p)
	}
}
func TestMixedRootCapacity(t *testing.T) {
	for _, count := range []int{90, 91} {
		nodes := []ec.IssueNode{}
		types := []sp.IssueType{sp.Phase, sp.Task, sp.Feature}
		for i := 0; i < count; i++ {
			nodes = append(nodes, node("engine", int64(i+1), types[i%3]))
		}
		c := contextOf(nodes...)
		in := input(t, c)
		p, err := eo.Evaluate(in)
		if count == 91 {
			if !errors.Is(err, eo.ErrRoadmapCapacity) || !reflect.DeepEqual(p, eo.Plan{}) {
				t.Fatal("mixed capacity failed open", p, err)
			}
		} else {
			if err != nil || len(p.PhaseOrder) != 30 || len(p.RoadmapRoots) != 90 || len(p.Assignments) != 90 || p.Assignments[89].RoadmapOrder != 99000 {
				t.Fatal("mixed capacity", p, err)
			}
			assertNamespace(t, p)
		}
		// Completing a standalone root releases one retained segment, but its
		// dependency topology evidence is kept in RoadmapRoots.
		c.Nodes[1].State = ec.Closed
		p = evaluate(t, c)
		if len(p.RoadmapRoots) != count || len(p.Assignments) != count-1 || !slices.Contains(p.Clear, c.Nodes[1].Resource) {
			t.Fatal("capacity after standalone completion", p)
		}
	}
}
func TestOrderValidationAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind error
		edit func(*eo.Input)
	}{
		{"missing standalone root", eo.ErrRoadmap, func(in *eo.Input) {
			in.Order.RoadmapRoots = append(in.Order.RoadmapRoots[:1], in.Order.RoadmapRoots[2:]...)
		}},
		{"duplicate root", eo.ErrRoadmap, func(in *eo.Input) { in.Order.RoadmapRoots = append(in.Order.RoadmapRoots, in.Order.RoadmapRoots[1]) }},
		{"root evidence", eo.ErrRoadmap, func(in *eo.Input) { in.Order.RoadmapRoots[1].NodeID = "wrong" }},
		{"Phase projection contradicts roots", eo.ErrPhaseOrder, func(in *eo.Input) {
			in.Order.PhaseOrder[0], in.Order.PhaseOrder[1] = in.Order.PhaseOrder[1], in.Order.PhaseOrder[0]
		}},
		{"Task in Phase projection", eo.ErrPhaseOrder, func(in *eo.Input) { in.Order.PhaseOrder = append(in.Order.PhaseOrder, in.Order.RoadmapRoots[1]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input(t, mixedRoots())
			tc.edit(&in)
			p, err := eo.Evaluate(in)
			if !errors.Is(err, tc.kind) || !reflect.DeepEqual(p, eo.Plan{}) {
				t.Fatal("malformed root sequence accepted", p, err)
			}
		})
	}
	c := mixedRoots()
	dep(&c, 2, 1)
	in := input(t, c)
	in.Order.RoadmapRoots[1], in.Order.RoadmapRoots[2] = in.Order.RoadmapRoots[2], in.Order.RoadmapRoots[1]
	p, err := eo.Evaluate(in)
	if !errors.Is(err, eo.ErrRoadmap) || !reflect.DeepEqual(p, eo.Plan{}) {
		t.Fatal("root dependency violated", p, err)
	}
	c = mixedRoots()
	o, err := eo.BuildRoadmapOrder(c)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(c)
	o.RoadmapRoots[0].NodeID = "changed"
	if o.PhaseOrder[0].NodeID == "changed" {
		t.Fatal("Phase projection aliases roots")
	}
	o.PhaseOrder[0].NodeID = "changed"
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Fatal("build output aliases Context")
	}
}
func TestMixedTopologyShuffledDeterminism(t *testing.T) {
	c := mixedRoots()
	dep(&c, 2, 1)
	dep(&c, 1, 0)
	dep(&c, 1, 3)
	c.Nodes = append(c.Nodes, node("freecad", 8, sp.Task), node("engine", 9, sp.Feature))
	parent(&c, 4, 0)
	parent(&c, 5, 2)
	dep(&c, 4, 5)
	original, _ := json.Marshal(evaluate(t, c))
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 20; i++ {
		rng.Shuffle(len(c.Nodes), func(i, j int) { c.Nodes[i], c.Nodes[j] = c.Nodes[j], c.Nodes[i] })
		rng.Shuffle(len(c.Parents), func(i, j int) { c.Parents[i], c.Parents[j] = c.Parents[j], c.Parents[i] })
		rng.Shuffle(len(c.Dependencies), func(i, j int) { c.Dependencies[i], c.Dependencies[j] = c.Dependencies[j], c.Dependencies[i] })
		got, _ := json.Marshal(evaluate(t, c))
		if string(got) != string(original) {
			t.Fatal("mixed topology nondeterministic")
		}
	}
}

func TestStandaloneStatusAndMetadataAreNotOrderingAuthority(t *testing.T) {
	c := mixedRoots()
	c.Nodes = append(c.Nodes, node("engine", 5, sp.Bug))
	baseline := evaluate(t, c)
	dep(&c, 4, 1) // Bug blocker changes Task X from Ready to Blocked, never root topology.
	c.Nodes[1].AcceptedClassification.Priority = sp.Critical
	c.Nodes[1].AcceptedClassification.Effort = sp.XS
	p := evaluate(t, c)
	if !reflect.DeepEqual(p, baseline) {
		t.Fatal("Status/metadata reordered roadmap", p)
	}
	in := input(t, c)
	if !slices.ContainsFunc(in.Lifecycle.Decisions, func(d ep.Decision) bool { return d.Resource == c.Nodes[1].Resource && d.Status == ep.Blocked }) {
		t.Fatal("#35 lifecycle did not remain authoritative")
	}
}
