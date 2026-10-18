package engineeringpolicy

import (
	"errors"
	"testing"

	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
)

func TestPolicyOrganization(t *testing.T) {
	for _, org := range []string{"parametron-io", "prm-workflow"} {
		a, b := node("one", 1, sp.Task), node("two", 2, sp.Task)
		a.Resource.Owner, b.Resource.Owner = org, org
		in := input(a, b)
		dependency(&in, 1, 0)
		p, err := Evaluate(in)
		if err != nil || len(p.Decisions) != 2 || !decision(t, p, a.Resource).HasActiveBlocker {
			t.Fatalf("%+v %v", p, err)
		}
		for _, owner := range []string{"other-org", "PRM-WORKFLOW", "", "bad/owner"} {
			bad := input(a, b)
			bad.Context.Nodes[1].Resource.Owner = owner
			if _, err := Evaluate(bad); !errors.Is(err, ErrInvalid) {
				t.Fatalf("%q: %v", owner, err)
			}
		}
	}
}
