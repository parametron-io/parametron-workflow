// Package engineeringorder computes roadmap root topology and numeric Roadmap Order.
// It performs no observation, lifecycle transitions, or mutations.
package engineeringorder

import (
	"errors"
	"sort"

	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid          = errors.New("engineering order: invalid context")
	ErrPhaseOrder       = errors.New("engineering order: invalid Phase order")
	ErrRoadmap          = errors.New("engineering order: invalid roadmap input")
	ErrRoadmapHierarchy = errors.New("engineering order: cross-level roadmap dependency")
	ErrRoadmapCapacity  = errors.New("engineering order: roadmap capacity exceeded")
)

type Error struct{ Kind error }

func (e *Error) Error() string { return e.Kind.Error() }
func (e *Error) Unwrap() error { return e.Kind }
func fail(kind error) error    { return &Error{kind} }

// The fixed v1 namespace reserves offsets +1..+9 after every primary roadmap
// Issue slot for generic companions. This policy assigns no companion slots.
const (
	FirstRoadmapBase      = 10000
	RoadmapStride         = 1000
	DirectChildStride     = 10
	MaxRoadmapSegments    = 90
	MaxDirectChildren     = 99
	MaxCompanionsPerIssue = 9
)

type Input struct {
	Context   ec.Context
	Lifecycle ep.Plan
	Order     Order
}

// Order separates complete root topology from its Phase-only lifecycle projection.
// Both sequences include CLOSED work as dependency evidence; numeric retention
// is applied later. PhaseOrder is always a subsequence of RoadmapRoots.
type Order struct {
	RoadmapRoots []storage.Resource
	PhaseOrder   []storage.Resource
}
type Assignment struct {
	Resource     storage.Resource
	RoadmapOrder int
	AnchorPhase  *storage.Resource
}
type Plan struct {
	Order
	// Assignments are current roadmap work, ordered by ascending numeric value.
	Assignments []Assignment
	// Clear is canonical unset for CLOSED roots and direct Task/Feature children.
	// Bugs remain outside this policy's ownership.
	Clear []storage.Resource
}

func less(a, b storage.Resource) bool {
	if a.Owner != b.Owner {
		return a.Owner < b.Owner
	}
	if a.Repository != b.Repository {
		return a.Repository < b.Repository
	}
	if a.Number != b.Number {
		return a.Number < b.Number
	}
	return a.NodeID < b.NodeID
}
func check(c ec.Context, phases []storage.Resource) error {
	if err := ep.Validate(ep.Input{Context: c, PhaseOrder: phases}); err != nil {
		if errors.Is(err, ep.ErrPhaseOrder) {
			return fail(ErrPhaseOrder)
		}
		return fail(ErrInvalid)
	}
	return nil
}

type rootGraph struct {
	roots  map[storage.Resource]ec.IssueNode
	degree map[storage.Resource]int
	next   map[storage.Resource][]storage.Resource
}

// rootGraphOf classifies authoritative membership without inventing promoted
// dependency edges. Child-to-child and same-Phase root/child dependencies remain
// lifecycle facts. A root cannot depend on an unrelated Phase-owned child, or
// block that child, as a cross-level roadmap relationship.
func rootGraphOf(c ec.Context) (rootGraph, error) {
	g := rootGraph{map[storage.Resource]ec.IssueNode{}, map[storage.Resource]int{}, map[storage.Resource][]storage.Resource{}}
	nodes := map[storage.Resource]ec.IssueNode{}
	parents := map[storage.Resource]storage.Resource{}
	owned := map[storage.Resource]storage.Resource{}
	for _, n := range c.Nodes {
		nodes[n.Resource] = n
	}
	for _, e := range c.Parents {
		parents[e.Child] = e.Parent
		n := nodes[e.Child]
		if (n.Type == sp.Task || n.Type == sp.Feature) && nodes[e.Parent].Type == sp.Phase {
			owned[e.Child] = e.Parent
		}
	}
	for _, n := range c.Nodes {
		_, hasParent := parents[n.Resource]
		if n.Type == sp.Phase || (n.Type == sp.Task || n.Type == sp.Feature) && !hasParent {
			g.roots[n.Resource] = n
			g.degree[n.Resource] = 0
		}
	}
	for _, e := range c.Dependencies {
		_, blockerRoot := g.roots[e.Blocker]
		_, blockedRoot := g.roots[e.Blocked]
		if p, child := owned[e.Blocker]; child && blockedRoot && p != e.Blocked {
			return g, fail(ErrRoadmapHierarchy)
		}
		if p, child := owned[e.Blocked]; child && blockerRoot && p != e.Blocker {
			return g, fail(ErrRoadmapHierarchy)
		}
		if blockerRoot && blockedRoot {
			g.degree[e.Blocked]++
			g.next[e.Blocker] = append(g.next[e.Blocker], e.Blocked)
		}
	}
	return g, nil
}

// BuildRoadmapOrder traverses all Phase and parentless Task/Feature roots once.
// PhaseOrder is filtered from that same result, never separately topologically
// sorted. No lifecycle Plan, Status, or Project input is required.
func BuildRoadmapOrder(c ec.Context) (Order, error) {
	g, err := rootGraphOf(c)
	if err != nil {
		return Order{}, err
	}
	queue := []storage.Resource{}
	for r, d := range g.degree {
		if d == 0 {
			queue = append(queue, r)
		}
	}
	out := Order{RoadmapRoots: make([]storage.Resource, 0, len(g.roots)), PhaseOrder: []storage.Resource{}}
	for len(queue) > 0 {
		sort.Slice(queue, func(i, j int) bool { return less(queue[i], queue[j]) })
		r := queue[0]
		queue = queue[1:]
		out.RoadmapRoots = append(out.RoadmapRoots, r)
		if g.roots[r].Type == sp.Phase {
			out.PhaseOrder = append(out.PhaseOrder, r)
		}
		for _, target := range g.next[r] {
			g.degree[target]--
			if g.degree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	if len(out.RoadmapRoots) != len(g.roots) {
		return Order{}, fail(ErrRoadmap)
	}
	if err := check(c, out.PhaseOrder); err != nil {
		return Order{}, err
	}
	return out, nil
}

func validateOrder(c ec.Context, order Order) error {
	if err := check(c, order.PhaseOrder); err != nil {
		return err
	}
	g, err := rootGraphOf(c)
	if err != nil {
		return err
	}
	positions := map[storage.Resource]int{}
	phases := []storage.Resource{}
	for i, r := range order.RoadmapRoots {
		n, ok := g.roots[r]
		_, dup := positions[r]
		if !ok || dup {
			return fail(ErrRoadmap)
		}
		positions[r] = i
		if n.Type == sp.Phase {
			phases = append(phases, r)
		}
	}
	if len(positions) != len(g.roots) {
		return fail(ErrRoadmap)
	}
	if len(phases) != len(order.PhaseOrder) {
		return fail(ErrPhaseOrder)
	}
	for i, r := range phases {
		if r != order.PhaseOrder[i] {
			return fail(ErrPhaseOrder)
		}
	}
	for blocker, targets := range g.next {
		for _, blocked := range targets {
			if positions[blocker] >= positions[blocked] {
				return fail(ErrRoadmap)
			}
		}
	}
	return nil
}

// Evaluate validates and consumes supplied root topology and Phase projection unchanged. Retained
// segments are compacted from current OPEN work, independent of workflow Status,
// Set-Position, Automation, and historical Project order. Failures return no Plan.
func Evaluate(in Input) (Plan, error) {
	if err := validateOrder(in.Context, in.Order); err != nil {
		return Plan{}, err
	}
	nodes := map[storage.Resource]ec.IssueNode{}
	parents := map[storage.Resource]storage.Resource{}
	children := map[storage.Resource][]storage.Resource{}
	for _, n := range in.Context.Nodes {
		nodes[n.Resource] = n
	}
	for _, e := range in.Context.Parents {
		n := nodes[e.Child]
		if (n.Type == sp.Task || n.Type == sp.Feature) && nodes[e.Parent].Type == sp.Phase {
			children[e.Parent] = append(children[e.Parent], e.Child)
			if n.State == ec.Open {
				parents[e.Child] = e.Parent
			}
		}
	}
	decisions := map[storage.Resource]ep.Decision{}
	for _, d := range in.Lifecycle.Decisions {
		n, ok := nodes[d.Resource]
		_, dup := decisions[d.Resource]
		if !ok || dup || n.State != ec.Open || n.Type == sp.Bug {
			return Plan{}, fail(ErrRoadmap)
		}
		if d.Status != ep.Backlog && d.Status != ep.Blocked && d.Status != ep.Ready {
			return Plan{}, fail(ErrRoadmap)
		}
		if p, ok := parents[d.Resource]; ok {
			if d.ParentPhase == nil || *d.ParentPhase != p {
				return Plan{}, fail(ErrRoadmap)
			}
		} else if d.ParentPhase != nil {
			return Plan{}, fail(ErrRoadmap)
		}
		decisions[d.Resource] = d
	}
	for r, n := range nodes {
		if n.State == ec.Open && n.Type != sp.Bug {
			if _, ok := decisions[r]; !ok {
				return Plan{}, fail(ErrRoadmap)
			}
		}
	}
	seen := map[storage.Resource]bool{}
	for _, r := range in.Lifecycle.ParentlessReady {
		d, ok := decisions[r]
		n := nodes[r]
		if !ok || seen[r] || d.Status != ep.Ready || d.ParentPhase != nil || (n.Type != sp.Task && n.Type != sp.Feature) {
			return Plan{}, fail(ErrRoadmap)
		}
		seen[r] = true
	}
	for r, d := range decisions {
		n := nodes[r]
		if (n.Type == sp.Task || n.Type == sp.Feature) && d.ParentPhase == nil && d.Status == ep.Ready && !seen[r] {
			return Plan{}, fail(ErrRoadmap)
		}
	}
	out := Plan{Order: Order{RoadmapRoots: append([]storage.Resource{}, in.Order.RoadmapRoots...), PhaseOrder: append([]storage.Resource{}, in.Order.PhaseOrder...)}, Assignments: []Assignment{}, Clear: []storage.Resource{}}
	retained := 0
	for _, phase := range in.Order.RoadmapRoots {
		n := nodes[phase]
		if n.State == ec.Closed {
			out.Clear = append(out.Clear, phase)
		}
		open := []storage.Resource{}
		for _, r := range children[phase] {
			if nodes[r].State == ec.Open {
				open = append(open, r)
			} else {
				out.Clear = append(out.Clear, r)
			}
		}
		if n.State == ec.Closed && len(open) == 0 {
			continue
		}
		if retained == MaxRoadmapSegments || len(open) > MaxDirectChildren {
			return Plan{}, fail(ErrRoadmapCapacity)
		}
		base := FirstRoadmapBase + retained*RoadmapStride
		retained++
		if n.State == ec.Open {
			out.Assignments = append(out.Assignments, Assignment{Resource: phase, RoadmapOrder: base})
		}
		sort.Slice(open, func(i, j int) bool { return less(open[i], open[j]) })
		for i, r := range open {
			anchor := phase
			out.Assignments = append(out.Assignments, Assignment{Resource: r, RoadmapOrder: base + (i+1)*DirectChildStride, AnchorPhase: &anchor})
		}
	}
	sort.Slice(out.Clear, func(i, j int) bool { return less(out.Clear[i], out.Clear[j]) })
	return out, nil
}
