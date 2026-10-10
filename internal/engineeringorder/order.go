// Package engineeringorder computes roadmap order and canonical Phase segments.
// It performs no observation, lifecycle transitions, or mutations.
package engineeringorder

import (
	"errors"
	"sort"
	"strings"

	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid      = errors.New("engineering order: invalid context")
	ErrProjectOrder = errors.New("engineering order: invalid Project order")
	ErrPhaseOrder   = errors.New("engineering order: invalid Phase order")
	ErrPlacement    = errors.New("engineering order: invalid placement input")
)

type Error struct{ Kind error }

func (e *Error) Error() string { return e.Kind.Error() }
func (e *Error) Unwrap() error { return e.Kind }
func fail(kind error) error    { return &Error{kind} }

// CurrentOrder is one Engineering Project sequence. Resource is nil for PR,
// draft, opaque, or null content; those items retain their sequence slots.
// Non-nil Resource is exact canonical Issue evidence, including unrelated Issues.
type CurrentItem struct {
	ProjectItemID string
	Resource      *storage.Resource
	Archived      bool
}
type CurrentOrder struct{ Items []CurrentItem }
type Input struct {
	Context      ec.Context
	Lifecycle    ep.Plan
	PhaseOrder   []storage.Resource
	CurrentOrder CurrentOrder
}
type Placement struct {
	Resource      storage.Resource
	AnchorPhase   *storage.Resource
	PositionOwned bool
}
type Plan struct {
	PhaseOrder   []storage.Resource
	DesiredOrder []Placement
}

func identity(r storage.Resource) storage.Resource { r.NodeID = ""; return r }

func validName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
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
func ranks(c ec.Context, current CurrentOrder) (map[storage.Resource]int, error) {
	nodes := map[storage.Resource]storage.Resource{}
	nodeIDs := map[string]storage.Resource{}
	for _, n := range c.Nodes {
		nodes[identity(n.Resource)] = n.Resource
		nodeIDs[n.Resource.NodeID] = n.Resource
	}
	out := map[storage.Resource]int{}
	items, contents, ids := map[string]bool{}, map[storage.Resource]bool{}, map[string]bool{}
	for i, item := range current.Items {
		if strings.TrimSpace(item.ProjectItemID) == "" || items[item.ProjectItemID] {
			return nil, fail(ErrProjectOrder)
		}
		items[item.ProjectItemID] = true
		if item.Resource == nil {
			continue
		}
		r := *item.Resource
		key := identity(r)
		if r.Kind != "issue" || r.Number <= 0 || int64(int(r.Number)) != r.Number || strings.TrimSpace(r.NodeID) == "" || r.NodeID != strings.TrimSpace(r.NodeID) || !validName(r.Owner) || !validName(r.Repository) || contents[key] || ids[r.NodeID] {
			return nil, fail(ErrProjectOrder)
		}
		contents[key], ids[r.NodeID] = true, true
		if exact, ok := nodes[key]; ok && exact != r {
			return nil, fail(ErrProjectOrder)
		}
		if exact, ok := nodeIDs[r.NodeID]; ok && exact != r {
			return nil, fail(ErrProjectOrder)
		}
		if !item.Archived {
			out[r] = i
		}
	}
	return out, nil
}
func rankedLess(a, b storage.Resource, rank map[storage.Resource]int) bool {
	x, xok := rank[a]
	y, yok := rank[b]
	if xok != yok {
		return xok
	}
	if xok && x != y {
		return x < y
	}
	return less(a, b)
}

// BuildPhaseOrder needs no lifecycle Plan. All Phases (OPEN and CLOSED) enter
// stable Kahn traversal; only Phase-to-Phase dependency edges constrain it.
func BuildPhaseOrder(c ec.Context, current CurrentOrder) ([]storage.Resource, error) {
	nodes := map[storage.Resource]bool{}
	degree := map[storage.Resource]int{}
	next := map[storage.Resource][]storage.Resource{}
	for _, n := range c.Nodes {
		if n.Type == sp.Phase {
			nodes[n.Resource] = true
			degree[n.Resource] = 0
		}
	}
	for _, e := range c.Dependencies {
		if nodes[e.Blocker] && nodes[e.Blocked] {
			degree[e.Blocked]++
			next[e.Blocker] = append(next[e.Blocker], e.Blocked)
		}
	}
	rank, err := ranks(c, current)
	if err != nil {
		return nil, err
	}
	queue := []storage.Resource{}
	for r, d := range degree {
		if d == 0 {
			queue = append(queue, r)
		}
	}
	out := make([]storage.Resource, 0, len(nodes))
	for len(queue) > 0 {
		sort.Slice(queue, func(i, j int) bool { return rankedLess(queue[i], queue[j], rank) })
		r := queue[0]
		queue = queue[1:]
		out = append(out, r)
		for _, target := range next[r] {
			degree[target]--
			if degree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	if len(out) != len(nodes) {
		return nil, fail(ErrPhaseOrder)
	}
	if err := check(c, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Evaluate consumes, never repairs/recomputes, the supplied PhaseOrder. OPEN
// Task/Feature children remain in their parent's segment regardless of Status.
// Closed Phases leave an anchor slot in topology but no actionable placement.
// Parentless work stays outside position ownership; no global placement is invented.
func Evaluate(in Input) (Plan, error) {
	if err := check(in.Context, in.PhaseOrder); err != nil {
		return Plan{}, err
	}
	rank, err := ranks(in.Context, in.CurrentOrder)
	if err != nil {
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
		if n.State == ec.Open && (n.Type == sp.Task || n.Type == sp.Feature) {
			if nodes[e.Parent].Type != sp.Phase {
				return Plan{}, fail(ErrPlacement)
			}
			parents[e.Child] = e.Parent
			children[e.Parent] = append(children[e.Parent], e.Child)
		}
	}
	decisions := map[storage.Resource]ep.Decision{}
	for _, d := range in.Lifecycle.Decisions {
		n, ok := nodes[d.Resource]
		_, dup := decisions[d.Resource]
		if !ok || dup || n.State != ec.Open || n.Type == sp.Bug {
			return Plan{}, fail(ErrPlacement)
		}
		if d.Status != ep.Backlog && d.Status != ep.Blocked && d.Status != ep.Ready {
			return Plan{}, fail(ErrPlacement)
		}
		if p, ok := parents[d.Resource]; ok {
			if d.ParentPhase == nil || *d.ParentPhase != p {
				return Plan{}, fail(ErrPlacement)
			}
		} else if d.ParentPhase != nil {
			return Plan{}, fail(ErrPlacement)
		}
		decisions[d.Resource] = d
	}
	for r, n := range nodes {
		if n.State == ec.Open && n.Type != sp.Bug {
			if _, ok := decisions[r]; !ok {
				return Plan{}, fail(ErrPlacement)
			}
		}
	}
	seen := map[storage.Resource]bool{}
	for _, r := range in.Lifecycle.ParentlessReady {
		d, ok := decisions[r]
		n := nodes[r]
		if !ok || seen[r] || d.Status != ep.Ready || d.ParentPhase != nil || (n.Type != sp.Task && n.Type != sp.Feature) {
			return Plan{}, fail(ErrPlacement)
		}
		seen[r] = true
	}
	for r, d := range decisions {
		n := nodes[r]
		if (n.Type == sp.Task || n.Type == sp.Feature) && d.ParentPhase == nil && d.Status == ep.Ready && !seen[r] {
			return Plan{}, fail(ErrPlacement)
		}
	}
	out := Plan{PhaseOrder: append([]storage.Resource{}, in.PhaseOrder...), DesiredOrder: []Placement{}}
	for _, phase := range in.PhaseOrder {
		n := nodes[phase]
		if n.State == ec.Open {
			out.DesiredOrder = append(out.DesiredOrder, Placement{Resource: phase, PositionOwned: n.Intent.Effective().SetPosition})
		}
		group := children[phase]
		sort.Slice(group, func(i, j int) bool { return rankedLess(group[i], group[j], rank) })
		for _, r := range group {
			anchor := phase
			out.DesiredOrder = append(out.DesiredOrder, Placement{Resource: r, AnchorPhase: &anchor, PositionOwned: nodes[r].Intent.Effective().SetPosition})
		}
	}
	return out, nil
}
