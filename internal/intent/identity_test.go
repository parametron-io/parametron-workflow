package intent

import "testing"

func TestOrganizationLogin(t *testing.T) {
	for _, s := range []string{"parametron-io", "prm-workflow", "PRM-Workflow"} {
		if !ValidOrganizationLogin(s) {
			t.Fatalf("valid login rejected: %q", s)
		}
	}
	for _, s := range []string{"", " ", "bad/name", "bad_name", ".", "-bad", "bad-", "bad--name", "owner org", "abcdefghijklmnopqrstuvwxyzabcdefghijklmn"} {
		if ValidOrganizationLogin(s) {
			t.Fatalf("invalid login accepted: %q", s)
		}
	}
}
