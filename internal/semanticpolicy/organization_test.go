package semanticpolicy

import (
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/intent"
)

func TestPlanningOrganization(t *testing.T) {
	for _, org := range []string{"parametron-io", "prm-workflow", "PRM-Workflow"} {
		p, err := NewPlanningContext(Repository{org, "Workflow_Repo_1"}, "title", "body", classification(), intent.Intent{}, nil)
		if err != nil || p.Repository.Owner != strings.ToLower(org) {
			t.Fatalf("%+v %v", p, err)
		}
		first, err := p.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		second, err := p.Serialize()
		if err != nil || first != second {
			t.Fatalf("unstable serialization: %v", err)
		}
	}
}
