package prdevelopment

import (
	"errors"
	"strings"
	"testing"
)

func TestRepositoryOrganizationConstructor(t *testing.T) {
	for _, org := range []string{"parametron-io", "prm-workflow", "PRM-Workflow"} {
		for _, tc := range []string{"valid", "mixed case owner", "foreign owner", "duplicate name", "duplicate id", "invalid owner", "invalid login", "missing dependency"} {
			t.Run(org+"/"+tc, func(t *testing.T) {
				f, _ := fixtureController(t)
				c := f.config()
				for i := range c.Repositories {
					c.Repositories[i].Owner = org
				}
				r := c.Repositories[0]
				r.ID = "OTHER"
				r.Name = "workflow_repo_2"
				r.Owner = org
				c.Repositories = append(c.Repositories, r)
				switch tc {
				case "mixed case owner":
					c.Repositories[1].Owner = strings.ToUpper(org)
				case "foreign owner":
					c.Repositories[1].Owner = "other-org"
				case "duplicate name":
					c.Repositories[1].Name = strings.ToUpper(c.Repositories[0].Name)
				case "duplicate id":
					c.Repositories[1].ID = c.Repositories[0].ID
				case "invalid login":
					for i := range c.Repositories {
						c.Repositories[i].Owner = "bad_owner"
					}
				case "invalid owner":
					c.Repositories[0].Owner = "bad/owner"
				case "missing dependency":
					c.Reader = nil
				}
				_, err := New(c)
				if tc == "valid" || tc == "mixed case owner" {
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
