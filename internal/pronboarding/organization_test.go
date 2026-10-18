package pronboarding

import (
	"errors"
	"testing"
)

func TestConfiguredOrganizationConstructor(t *testing.T) {
	for _, org := range []string{"parametron-io", "prm-workflow", "PRM-Workflow"} {
		for _, tc := range []string{"valid", "foreign owner", "invalid owner", "duplicate name", "duplicate id", "invalid id", "missing dependency", "invalid organization"} {
			t.Run(org+"/"+tc, func(t *testing.T) {
				f := auditSetup(t)
				c := f.cfg
				c.Deployment.Organization.Login = org
				for i := range c.Deployment.Repositories {
					c.Deployment.Repositories[i].Owner = org
				}
				switch tc {
				case "foreign owner":
					c.Deployment.Repositories[0].Owner = "other-org"
				case "invalid owner":
					c.Deployment.Repositories[0].Owner = "bad/owner"
				case "duplicate name":
					r := c.Deployment.Repositories[0]
					r.ID = "OTHER"
					c.Deployment.Repositories = append(c.Deployment.Repositories, r)
				case "duplicate id":
					r := c.Deployment.Repositories[0]
					r.Name = "other"
					c.Deployment.Repositories = append(c.Deployment.Repositories, r)
				case "invalid id":
					c.Deployment.Repositories[0].ID = "bad id"
				case "missing dependency":
					c.Reader = nil
				case "invalid organization":
					c.Deployment.Organization.Login = "bad/owner"
				}
				_, err := New(c)
				if tc == "valid" {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrInvalid) {
					t.Fatalf("expected invalid: %v", err)
				}
			})
		}
	}
}
