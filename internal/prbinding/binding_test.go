package prbinding

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var auditOrg = config.Organization{ID: "O", Login: "parametron-io"}
var auditRepo = github.Repository{ID: "R", Owner: "parametron-io", Name: "engine"}
var auditUser = github.Actor{ID: "U", Login: "developer", Type: "User"}

func TestDeveloperEligibility(t *testing.T) {
	type facts struct {
		author, user github.Actor
		org          config.Organization
		repo         github.Repository
		membership   github.Membership
		permission   github.Permission
		assignees    []github.Actor
	}
	base := func() facts {
		return facts{author: auditUser, user: auditUser, org: auditOrg, repo: auditRepo, membership: github.Membership{Organization: auditOrg, User: auditUser, State: "active"}, permission: github.Permission{Repository: auditRepo, User: auditUser, Base: "write"}}
	}
	cases := []struct {
		name string
		edit func(*facts)
		want Reason
	}{
		{"write without assignment", func(*facts) {}, ""},
		{"admin", func(f *facts) { f.permission.Base = "admin" }, ""},
		{"manual match", func(f *facts) { f.assignees = []github.Actor{auditUser} }, ""},
		{"multiple manual assignees", func(f *facts) { f.assignees = []github.Actor{{ID: "other", Login: "other", Type: "User"}, auditUser} }, ""},
		{"canonical case", func(f *facts) {
			f.author.Login = "DEVELOPER"
			f.assignees = []github.Actor{{ID: "U", Login: "Developer", Type: "User"}}
		}, ""},
		{"pending with assignee", func(f *facts) { f.membership.State = "pending"; f.assignees = []github.Actor{auditUser} }, InactiveMembership},
		{"outside collaborator", func(f *facts) { f.membership = github.Membership{} }, InvalidDeveloper},
		{"wrong organization", func(f *facts) { f.membership.Organization.ID = "other" }, InvalidDeveloper},
		{"wrong repository", func(f *facts) { f.permission.Repository.ID = "other" }, InvalidDeveloper},
		{"same login different stable ID", func(f *facts) { f.author.ID = "other" }, InvalidDeveloper},
		{"same ID different login", func(f *facts) { f.author.Login = "other" }, InvalidDeveloper},
		{"membership subject", func(f *facts) { f.membership.User.ID = "other" }, InvalidDeveloper},
		{"permission subject", func(f *facts) { f.permission.User.Login = "other" }, InvalidDeveloper},
		{"missing identity", func(f *facts) { f.user.ID = "" }, InvalidDeveloper},
		{"malformed login", func(f *facts) { f.author.Login = "bad/login" }, InvalidDeveloper},
		{"bot", func(f *facts) { f.author.Type = "Bot" }, InvalidDeveloper},
		{"organization actor", func(f *facts) { f.author.Type = "Organization" }, InvalidDeveloper},
		{"unsupported actor", func(f *facts) { f.author.Type = "Mannequin" }, InvalidDeveloper},
		{"other assignee", func(f *facts) { f.assignees = []github.Actor{{ID: "other", Login: "other", Type: "User"}} }, AssigneeMismatch},
		{"unverifiable assignee alongside match", func(f *facts) { f.assignees = []github.Actor{auditUser, {Login: "other", Type: "User"}} }, InvalidDeveloper},
	}
	for _, p := range []string{"read", "triage", "none", "", "maintain", "push", "custom", "WRITE"} {
		p := p
		cases = append(cases, struct {
			name string
			edit func(*facts)
			want Reason
		}{"permission " + p, func(f *facts) { f.permission.Base = p; f.assignees = []github.Actor{auditUser} }, InsufficientPermission})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			tc.edit(&f)
			before := append([]github.Actor(nil), f.assignees...)
			got := DeveloperEligibility(f.author, f.user, f.org, f.repo, f.membership, f.permission, f.assignees)
			if got != tc.want || !reflect.DeepEqual(before, f.assignees) {
				t.Fatalf("reason=%q want=%q; assignees=%v", got, tc.want, f.assignees)
			}
		})
	}
}

func TestConstructorAndInput(t *testing.T) {
	f := newBindingFixture(t)
	good := f.config()
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	var nilFixture *bindingFixture
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"primary absent", func(c *Config) { c.Primary = nil }}, {"primary typed nil", func(c *Config) { c.Primary = nilFixture }},
		{"context absent", func(c *Config) { c.Context = nil }}, {"context typed nil", func(c *Config) { c.Context = nilFixture }},
		{"completion absent", func(c *Config) { c.Completion = nil }}, {"completion typed nil", func(c *Config) { c.Completion = nilFixture }},
		{"branches absent", func(c *Config) { c.Branches = nil }}, {"branches typed nil", func(c *Config) { c.Branches = nilFixture }},
		{"developers absent", func(c *Config) { c.Developers = nil }}, {"developers typed nil", func(c *Config) { c.Developers = nilFixture }},
		{"no repositories", func(c *Config) { c.Deployment.Repositories = nil }},
		{"duplicate name", func(c *Config) {
			r := auditRepo
			r.ID = "other"
			r.Name = "ENGINE"
			c.Deployment.Repositories = append(c.Deployment.Repositories, r)
		}},
		{"duplicate ID", func(c *Config) {
			r := auditRepo
			r.Name = "other"
			c.Deployment.Repositories = append(c.Deployment.Repositories, r)
		}},
		{"invalid repo", func(c *Config) {
			c.Deployment.Repositories = []config.Repository{{ID: "R", Owner: auditRepo.Owner, Name: "bad/name"}}
		}},
		{"foreign owner", func(c *Config) {
			c.Deployment.Repositories = []config.Repository{{ID: "R", Owner: "other", Name: "engine"}}
		}},
		{"blank organization ID", func(c *Config) { c.Deployment.Organization.ID = " " }},
		{"foreign organization", func(c *Config) { c.Deployment.Organization.Login = "other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := f.config()
			tc.edit(&c)
			v, err := New(c)
			if !errors.Is(err, ErrInvalid) || v != nil {
				t.Fatalf("%v %v", v, err)
			}
		})
	}
	c := f.config()
	v, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Deployment.Repositories[0].ID = "caller mutation"
	if v.cfg.Deployment.Repositories[0] != auditRepo {
		t.Fatal("configuration aliases caller")
	}
	for _, r := range []storage.Resource{{Owner: auditRepo.Owner, Repository: auditRepo.Name, Kind: "issue", Number: 9}, {Owner: auditRepo.Owner, Repository: auditRepo.Name, Kind: "pull_request", Number: 0}, {Owner: auditRepo.Owner, Repository: auditRepo.Name, Kind: "pull_request", Number: -1}, {Owner: "other", Repository: auditRepo.Name, Kind: "pull_request", Number: 9}, {Owner: auditRepo.Owner, Repository: "outside", Kind: "pull_request", Number: 9}} {
		d, err := v.Evaluate(context.Background(), r)
		if !errors.Is(err, ErrInvalid) || d != (Decision{}) {
			t.Fatal(d, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("invalid input performed reads", f.calls)
	}
}

func requireRejected(t *testing.T, d Decision, err error, want Reason) {
	t.Helper()
	if err != nil || d.Outcome != Rejected || d.Reason != want || d.Evidence != (Evidence{}) {
		t.Fatalf("decision=%+v error=%v want rejection=%s", d, err, want)
	}
}
func requireError(t *testing.T, d Decision, err, want error) {
	t.Helper()
	if !errors.Is(err, want) || d != (Decision{}) {
		t.Fatalf("decision=%+v error=%v want=%v", d, err, want)
	}
}

const auditSHA = "0123456789abcdef0123456789abcdef01234567"

// A nil named map can implement a dependency interface just as a nil pointer can.
type nilSnapshotReader map[string]int

func (nilSnapshotReader) ResolveSnapshot(context.Context) (ec.Snapshot, error) {
	return ec.Snapshot{}, nil
}
func TestConstructorRejectsMalformedAuthorizationContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"typed nil map dependency", func(c *Config) { var dependency nilSnapshotReader; c.Context = dependency }},
		{"organization ID has surrounding whitespace", func(c *Config) { c.Deployment.Organization.ID = " O " }},
		{"organization ID has embedded whitespace", func(c *Config) { c.Deployment.Organization.ID = "O other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBindingFixture(t)
			c := f.config()
			tc.edit(&c)
			v, err := New(c)
			if v != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("malformed authorization context accepted: validator=%v error=%v", v, err)
			}
		})
	}
}
