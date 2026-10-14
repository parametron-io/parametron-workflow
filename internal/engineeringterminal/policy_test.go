package engineeringterminal

import (
	"errors"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func TestDirectChildClosureGate(t *testing.T) {
	phase := storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: "issue", Number: 50, NodeID: "P"}
	child := phase
	child.Number = 51
	child.NodeID = "C"
	child.Repository = "other"
	nested := phase
	nested.Number = 52
	nested.NodeID = "N"
	for _, tc := range []struct {
		name     string
		state    ec.IssueState
		children []ec.IssueNode
		edges    []ec.ParentEdge
		reopen   bool
		invalid  bool
	}{
		{name: "empty closed phase", state: ec.Closed},
		{name: "all closed cross repository", state: ec.Closed, children: []ec.IssueNode{{Resource: child, State: ec.Closed, Type: sp.Task}}, edges: []ec.ParentEdge{{Child: child, Parent: phase}}},
		{name: "open direct child", state: ec.Closed, children: []ec.IssueNode{{Resource: child, State: ec.Open, Type: sp.Task}}, edges: []ec.ParentEdge{{Child: child, Parent: phase}}, reopen: true},
		{name: "nested open descendant excluded", state: ec.Closed, children: []ec.IssueNode{{Resource: child, State: ec.Closed, Type: sp.Phase}, {Resource: nested, State: ec.Open, Type: sp.Task}}, edges: []ec.ParentEdge{{Child: child, Parent: phase}, {Child: nested, Parent: child}}},
		{name: "reparented child excluded", state: ec.Closed, children: []ec.IssueNode{{Resource: child, State: ec.Open, Type: sp.Task}}, edges: []ec.ParentEdge{{Child: child, Parent: nested}}},
		{name: "missing child fails closed", state: ec.Closed, edges: []ec.ParentEdge{{Child: child, Parent: phase}}, invalid: true},
		{name: "duplicate membership fails closed", state: ec.Closed, children: []ec.IssueNode{{Resource: child, State: ec.Closed}}, edges: []ec.ParentEdge{{Child: child, Parent: phase}, {Child: child, Parent: phase}}, invalid: true},
		{name: "unsupported child state", state: ec.Closed, children: []ec.IssueNode{{Resource: child, State: "UNKNOWN"}}, edges: []ec.ParentEdge{{Child: child, Parent: phase}}, invalid: true},
		{name: "eligible open phase never automatically closes", state: ec.Open, children: []ec.IssueNode{{Resource: child, State: ec.Closed}}, edges: []ec.ParentEdge{{Child: child, Parent: phase}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := ec.Context{Root: phase, Nodes: append([]ec.IssueNode{{Resource: phase, Type: sp.Phase, State: tc.state}}, tc.children...), Parents: tc.edges}
			d, err := Evaluate(g, phase)
			if tc.invalid {
				if !errors.Is(err, ErrInvalid) {
					t.Fatal(d, err)
				}
				return
			}
			if err != nil || d.Reopen != tc.reopen {
				t.Fatal(d, err)
			}
			if tc.reopen {
				if d.Rejection == nil || d.Rejection.Resource != phase || d.Rejection.Reason != OpenChildren || d.Status != "" {
					t.Fatal(d)
				}
			} else if tc.state == ec.Closed && d.Status != config.Done {
				t.Fatal(d)
			} else if tc.state == ec.Open && (d.Status != "" || d.Rejection != nil) {
				t.Fatal("automatic closure", d)
			}
		})
	}
}
