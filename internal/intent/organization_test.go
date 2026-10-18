package intent

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestConfiguredOrganization(t *testing.T) {
	for _, org := range []string{"parametron-io", "prm-workflow", "PRM-Workflow"} {
		t.Run(org, func(t *testing.T) {
			ctx := Context{Organization: org, Repository: "Workflow_Repo_1"}
			canonical := strings.ToLower(org)
			got, err := Parse("Parent: #10\nBlocked-By: "+canonical+"/Workflow_Repo_2#7", ctx)
			if err != nil || got.Parent == nil || *got.Parent != (IssueRef{"workflow_repo_1", 10}) || !reflect.DeepEqual(got.BlockedBy, []IssueRef{{"workflow_repo_2", 7}}) {
				t.Fatalf("%+v %v", got, err)
			}
			for _, tc := range []struct {
				body string
				want error
			}{
				{"Blocked-By: other-org/repository#7", ErrReference},
				{"Parent: " + canonical + "/workflow_repo_1#10", ErrNoncanonical},
				{"Blocked-By: " + strings.ToUpper(canonical) + "/workflow_repo_2#7", ErrReference},
				{"Blocked-By: " + strings.ToUpper(canonical[:1]) + canonical[1:] + "/workflow_repo_2#7", ErrReference},
				{"Parent: #010", ErrNoncanonical},
			} {
				got, err := Parse(tc.body, ctx)
				if !errors.Is(err, tc.want) || !reflect.DeepEqual(got, Intent{}) {
					t.Fatalf("%+v %v", got, err)
				}
			}
			// Foreign owners remain rejected; repository names retain existing folding.
			_, err = Parse("Blocked-By: OTHER-ORG/repository#7", ctx)
			if !errors.Is(err, ErrReference) {
				t.Fatal(err)
			}
		})
	}
}
