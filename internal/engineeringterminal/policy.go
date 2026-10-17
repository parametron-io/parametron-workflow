// Package engineeringterminal owns only native terminal consistency and the
// mechanical, direct-child Phase closure gate. It never initiates Phase closure.
package engineeringterminal

import (
	"errors"
	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var ErrInvalid = errors.New("engineering terminal: invalid graph or state")

type Reason string

const (
	OpenChildren        Reason = "open_direct_children"
	UnauthorizedClosure Reason = "unauthorized_native_closure"
	UnauthorizedDone    Reason = "unauthorized_project_done"
)

type Rejection struct {
	Resource storage.Resource
	Reason   Reason
}
type Decision struct {
	Reopen    bool
	Status    config.StatusRole
	Rejection *Rejection
}

// Evaluate expects a complete, independently validated Engineering graph.
func Evaluate(graph ec.Context, target storage.Resource) (Decision, error) {
	nodes := map[storage.Resource]ec.IssueNode{}
	for _, n := range graph.Nodes {
		if _, ok := nodes[n.Resource]; ok {
			return Decision{}, ErrInvalid
		}
		nodes[n.Resource] = n
	}
	n, ok := nodes[target]
	if !ok || n.Type != sp.Phase && n.Type != sp.Task && n.Type != sp.Feature || n.State != ec.Open && n.State != ec.Closed {
		return Decision{}, ErrInvalid
	}
	if n.State == ec.Open {
		return Decision{}, nil
	}
	if n.Type == sp.Phase {
		children := map[storage.Resource]bool{}
		open := false
		for _, e := range graph.Parents {
			if e.Parent != target {
				continue
			}
			child, ok := nodes[e.Child]
			if !ok || e.Child == target || children[e.Child] || child.State != ec.Open && child.State != ec.Closed {
				return Decision{}, ErrInvalid
			}
			children[e.Child] = true
			if child.State == ec.Open {
				open = true
			}
		}
		if open {
			return Decision{Reopen: true, Rejection: &Rejection{Resource: target, Reason: OpenChildren}}, nil
		}
		return Decision{Status: config.Done}, nil
	}
	// CLOSED is an observation. The controller may replace this denial only
	// after independent #50 merge or single-use Force-Close authorization.
	return Decision{Reopen: true, Rejection: &Rejection{Resource: target, Reason: UnauthorizedClosure}}, nil
}
