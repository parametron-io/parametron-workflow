// Package engineeringpolicy computes pure pre-development authorization from
// current declared facts. It neither observes nor mutates GitHub.
package engineeringpolicy

import (
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid    = errors.New("engineering policy: invalid context")
	ErrPhaseOrder = errors.New("engineering policy: invalid Phase order")
	ErrParent     = errors.New("engineering policy: invalid parent")
	ErrLifecycle  = errors.New("engineering policy: invalid lifecycle graph")
)

// Error contains only a stable local category, never Issue content.
type Error struct{ Kind error }

func (e *Error) Error() string { return e.Kind.Error() }
func (e *Error) Unwrap() error { return e.Kind }
func fail(kind error) error    { return &Error{Kind: kind} }

type Status string

const (
	Backlog Status = "Backlog"
	Blocked Status = "Blocked"
	Ready   Status = "Ready"
)

// PhaseOrder contains every Context Phase exactly once, including closed ones.
// Its producer owns roadmap ordering; Evaluate only validates and consumes it.
type Input struct {
	Context    ec.Context
	PhaseOrder []storage.Resource
}

// Rank has real accepted metadata only. Unavailable metadata stays zero-valued.
// It is attached only to parentless Ready Task/Feature decisions.
type Rank struct {
	Available bool
	Priority  sp.Priority
	Effort    sp.Effort
}
type Decision struct {
	Resource          storage.Resource
	Status            Status
	StatusOwned       bool
	HasActiveBlocker  bool
	ParentPhase       *storage.Resource
	PhaseActive       bool
	CreateBranch      bool
	ExecutionEligible bool
	ParentlessRank    *Rank
}
type Plan struct {
	Decisions []Decision
	// ParentlessReady is relative readiness order, not Project position/admission.
	ParentlessReady []storage.Resource
}

// Validate checks the shared Context/PhaseOrder contract without computing
// lifecycle decisions. Roadmap producers can reuse this boundary independently
// of a lifecycle Plan.
func Validate(in Input) error {
	_, err := validate(in)
	return err
}

func identity(r storage.Resource) storage.Resource { r.NodeID = ""; return r }
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
func validResource(r storage.Resource) bool {
	return r.Owner == "parametron-io" && validName(r.Repository) && r.Kind == "issue" && r.Number > 0 && int64(int(r.Number)) == r.Number && strings.TrimSpace(r.NodeID) != "" && r.NodeID == strings.TrimSpace(r.NodeID)
}

type graph struct {
	nodes   map[storage.Resource]ec.IssueNode
	parents map[storage.Resource]storage.Resource
	blocked map[storage.Resource]bool
}

// Evaluate returns no partial Plan on failure. The input and its nested values
// are never modified or retained in returned pointers.
func Evaluate(in Input) (Plan, error) {
	g, err := validate(in)
	if err != nil {
		return Plan{}, err
	}
	active := map[storage.Resource]bool{}
	selected := map[[2]string]bool{}
	for _, r := range in.PhaseOrder {
		n := g.nodes[r]
		repo := [2]string{r.Owner, r.Repository}
		if n.State == ec.Open && !g.blocked[r] && !selected[repo] {
			active[r] = true
			selected[repo] = true
		}
	}
	out := Plan{Decisions: []Decision{}, ParentlessReady: []storage.Resource{}}
	ranks := map[storage.Resource]Rank{}
	for r, n := range g.nodes {
		if n.State == ec.Closed || n.Type == sp.Bug {
			continue
		}
		d := Decision{Resource: r, Status: Backlog, StatusOwned: n.Intent.Effective().SetStatus, HasActiveBlocker: g.blocked[r], PhaseActive: active[r], CreateBranch: n.Type != sp.Phase}
		if n.Intent.CreateBranch.Explicit {
			d.CreateBranch = n.Intent.CreateBranch.Value
		}
		if n.Type == sp.Phase {
			if g.blocked[r] {
				d.Status = Blocked
			} else if active[r] {
				d.Status = Ready
			}
		} else if parent, ok := g.parents[r]; ok {
			p := parent
			d.ParentPhase = &p
			d.PhaseActive = active[parent]
			if active[parent] {
				d.Status = Ready
			}
		} else {
			d.Status = Ready
			if g.blocked[r] {
				d.Status = Blocked
			}
			if d.Status == Ready {
				rank := Rank{}
				if c := n.AcceptedClassification; c != nil {
					rank = Rank{true, c.Priority, c.Effort}
				}
				d.ParentlessRank = &rank
				ranks[r] = rank
				out.ParentlessReady = append(out.ParentlessReady, r)
			}
		}
		d.ExecutionEligible = d.Status == Ready && !d.HasActiveBlocker && d.CreateBranch
		out.Decisions = append(out.Decisions, d)
	}
	sort.Slice(out.Decisions, func(i, j int) bool { return less(out.Decisions[i].Resource, out.Decisions[j].Resource) })
	sort.Slice(out.ParentlessReady, func(i, j int) bool {
		a, b := out.ParentlessReady[i], out.ParentlessReady[j]
		x, y := ranks[a], ranks[b]
		if x.Available != y.Available {
			return x.Available
		}
		if x.Available {
			if x.Priority != y.Priority {
				return slices.Index(sp.Priorities(), x.Priority) < slices.Index(sp.Priorities(), y.Priority)
			}
			if x.Effort != y.Effort {
				return slices.Index(sp.Efforts(), x.Effort) < slices.Index(sp.Efforts(), y.Effort)
			}
		}
		return less(a, b)
	})
	return out, nil
}

func validate(in Input) (graph, error) {
	g := graph{map[storage.Resource]ec.IssueNode{}, map[storage.Resource]storage.Resource{}, map[storage.Resource]bool{}}
	ids := map[storage.Resource]bool{}
	nodeIDs := map[string]bool{}
	for _, n := range in.Context.Nodes {
		r := n.Resource
		if !validResource(r) || ids[identity(r)] || nodeIDs[r.NodeID] || !slices.Contains(sp.Types(), n.Type) || (n.State != ec.Open && n.State != ec.Closed) {
			return g, fail(ErrInvalid)
		}
		switch n.TypeSource {
		case ec.AcceptedSemantic:
			c := n.AcceptedClassification
			if c == nil || c.Type != n.Type {
				return g, fail(ErrInvalid)
			}
			data, err := json.Marshal(c)
			if err != nil {
				return g, fail(ErrInvalid)
			}
			if _, err := sp.DecodeIssueClassification(data); err != nil {
				return g, fail(ErrInvalid)
			}
		case ec.ManualNative:
			if n.AcceptedClassification != nil || n.Intent.Effective().Classification {
				return g, fail(ErrInvalid)
			}
		default:
			return g, fail(ErrInvalid)
		}
		ids[identity(r)], nodeIDs[r.NodeID] = true, true
		g.nodes[r] = n
	}
	if _, ok := g.nodes[in.Context.Root]; !ok {
		return g, fail(ErrInvalid)
	}
	parentPairs := map[[2]storage.Resource]bool{}
	for _, e := range in.Context.Parents {
		child, cok := g.nodes[e.Child]
		parent, pok := g.nodes[e.Parent]
		pair := [2]storage.Resource{e.Child, e.Parent}
		_, already := g.parents[e.Child]
		if !cok || !pok || e.Child == e.Parent || already {
			return g, fail(ErrParent)
		}
		ref := child.Intent.Parent
		if ref == nil || ref.Repository != e.Parent.Repository || ref.Number != e.Parent.Number || e.Child.Owner != e.Parent.Owner {
			return g, fail(ErrParent)
		}
		if child.State == ec.Open && child.Type != sp.Bug && parent.Type != sp.Phase {
			return g, fail(ErrParent)
		}
		g.parents[e.Child] = e.Parent
		parentPairs[pair] = true
	}
	for r, n := range g.nodes {
		if n.Intent.Parent != nil {
			if _, ok := g.parents[r]; !ok {
				return g, fail(ErrParent)
			}
		}
	}
	deps := map[[2]storage.Resource]bool{}
	for _, e := range in.Context.Dependencies {
		blocker, bok := g.nodes[e.Blocker]
		_, tok := g.nodes[e.Blocked]
		pair := [2]storage.Resource{e.Blocker, e.Blocked}
		if !bok || !tok || e.Blocker == e.Blocked || deps[pair] {
			return g, fail(ErrLifecycle)
		}
		deps[pair] = true
		if blocker.State == ec.Open {
			g.blocked[e.Blocked] = true
		}
	}
	if cyclic(g.nodes, parentPairs) || cyclic(g.nodes, deps) {
		return g, fail(ErrLifecycle)
	}
	positions := map[storage.Resource]int{}
	for i, r := range in.PhaseOrder {
		n, ok := g.nodes[r]
		_, dup := positions[r]
		if !ok || n.Type != sp.Phase || dup {
			return g, fail(ErrPhaseOrder)
		}
		positions[r] = i
	}
	for r, n := range g.nodes {
		if n.Type == sp.Phase {
			if _, ok := positions[r]; !ok {
				return g, fail(ErrPhaseOrder)
			}
		}
	}
	for pair := range deps {
		if g.nodes[pair[0]].Type == sp.Phase && g.nodes[pair[1]].Type == sp.Phase && positions[pair[0]] >= positions[pair[1]] {
			return g, fail(ErrPhaseOrder)
		}
	}
	return g, nil
}

// Iterative cycle rejection protects manually constructed contexts too.
func cyclic(nodes map[storage.Resource]ec.IssueNode, edges map[[2]storage.Resource]bool) bool {
	degree := map[storage.Resource]int{}
	next := map[storage.Resource][]storage.Resource{}
	for r := range nodes {
		degree[r] = 0
	}
	for p := range edges {
		degree[p[1]]++
		next[p[0]] = append(next[p[0]], p[1])
	}
	queue := []storage.Resource{}
	for r, d := range degree {
		if d == 0 {
			queue = append(queue, r)
		}
	}
	visited := 0
	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]
		visited++
		for _, target := range next[r] {
			degree[target]--
			if degree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	return visited != len(nodes)
}
