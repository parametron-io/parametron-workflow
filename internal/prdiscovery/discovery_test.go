package prdiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type bindingFunc func(context.Context, storage.Resource) (prbinding.Decision, error)

func (f bindingFunc) Evaluate(c context.Context, r storage.Resource) (prbinding.Decision, error) {
	return f(c, r)
}

type onboardingFunc func(context.Context, storage.Resource) (pronboarding.Completion, error)

func (f onboardingFunc) Completed(c context.Context, r storage.Resource) (pronboarding.Completion, error) {
	return f(c, r)
}

type readerFunc func(context.Context, storage.Resource) (observe.ObservedState, error)

func (f readerFunc) ReadCurrent(c context.Context, r storage.Resource) (observe.ObservedState, error) {
	return f(c, r)
}

// Every dependency exposes only reads. There is no Store, Mutator, model, or
// lifecycle dependency to accidentally authorize through this fixture.
type discoveryFixture struct {
	cfg          Config
	prs          []github.ListedPullRequest
	observations map[string]observe.ObservedState
	decisions    map[string]prbinding.Decision
	completions  map[string]pronboarding.Completion
	calls        []string
	phase        int
	hook         func(string, int, storage.Resource)
	failure      func(string) error
}

func fixture() *discoveryFixture {
	repo := github.Repository{ID: "REPO", Owner: "parametron-io", Name: "engine"}
	pr := github.Identity{ID: "PR9", Repository: repo, Number: 9}
	target := github.Identity{ID: "I46", Repository: repo, Number: 46}
	developer := github.Actor{ID: "U", Login: "developer", Type: "User"}
	e := prbinding.Evidence{PR: pr, Target: target, Kind: "Task", Branch: github.Branch{Repository: repo, Name: "development/issue-46", SHA: strings.Repeat("a", 40)}, Developer: developer, Policy: "active_org_member_repository_write_assignee_v1"}
	projects := func() []observe.ProjectState {
		return []observe.ProjectState{{Profile: config.Engineering, ID: "ENG"}, {Profile: config.BugTracker, ID: "BUG"}}
	}
	f := &discoveryFixture{prs: []github.ListedPullRequest{{Identity: pr, State: "OPEN"}}, observations: map[string]observe.ObservedState{}, decisions: map[string]prbinding.Decision{}, completions: map[string]pronboarding.Completion{}}
	f.observations["PR9"] = observe.ObservedState{Resource: resource(pr, "pull_request"), Presence: observe.Present, PullRequest: &github.PullRequest{Identity: pr, Body: "Target: #46", State: "OPEN", HeadRepository: repo, HeadRef: e.Branch.Name, HeadSHA: e.Branch.SHA, Author: &developer, Labels: []string{"z", "a"}}, Projects: projects()}
	f.observations["I46"] = observe.ObservedState{Resource: resource(target, "issue"), Presence: observe.Present, Issue: &github.Issue{Identity: target, State: "OPEN", Body: "Classify: false", Labels: []string{"z", "a"}, Assignees: []github.Actor{developer}}, Projects: projects()}
	f.decisions["PR9"] = prbinding.Decision{Outcome: prbinding.Accepted, Evidence: e}
	record := func(stage string, r storage.Resource) error {
		f.calls = append(f.calls, stage)
		if stage == "list" {
			f.phase++
		}
		if f.hook != nil {
			f.hook(stage, f.phase, r)
		}
		if f.failure != nil {
			return f.failure(stage)
		}
		return nil
	}
	f.cfg = Config{Repositories: []github.Repository{repo}, Project: "ENG", BugProject: "BUG"}
	f.cfg.Lister = &github.PullRequestListerFake{ListPullRequestsFunc: func(ctx context.Context, r github.Repository) ([]github.ListedPullRequest, error) {
		if err := record("list", storage.Resource{}); err != nil {
			return nil, err
		}
		out := []github.ListedPullRequest{}
		for _, p := range f.prs {
			if p.Repository == r {
				out = append(out, p)
			}
		}
		return out, nil
	}}
	f.cfg.Binding = bindingFunc(func(ctx context.Context, r storage.Resource) (prbinding.Decision, error) {
		if err := record("binding", r); err != nil {
			return prbinding.Decision{}, err
		}
		return f.decisions[r.NodeID], nil
	})
	f.cfg.Onboarding = onboardingFunc(func(ctx context.Context, r storage.Resource) (pronboarding.Completion, error) {
		if err := record("onboarding", r); err != nil {
			return pronboarding.Completion{}, err
		}
		return f.completions[r.NodeID], nil
	})
	f.cfg.Reader = readerFunc(func(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
		if err := record("read", r); err != nil {
			return observe.ObservedState{}, err
		}
		if r.NodeID != "" {
			return f.observations[r.NodeID], nil
		}
		for _, o := range f.observations {
			if o.Resource.Owner == r.Owner && o.Resource.Repository == r.Repository && o.Resource.Number == r.Number && o.Resource.Kind == r.Kind {
				return o, nil
			}
		}
		return observe.ObservedState{Presence: observe.Missing}, nil
	})
	return f
}
func (f *discoveryFixture) discover(t *testing.T) (Result, error) {
	t.Helper()
	c, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c.Discover(context.Background())
}
func mustComplete(t *testing.T, f *discoveryFixture) Result {
	t.Helper()
	got, err := f.discover(t)
	if err != nil || got.Completeness != Complete || got.Freshness != StableReads {
		t.Fatalf("baseline: %+v %v", got, err)
	}
	return got
}
func zeroFailure(t *testing.T, got Result, err, want error) {
	t.Helper()
	if !errors.Is(err, want) || !reflect.DeepEqual(got, Result{}) {
		t.Fatalf("want zero result and %v; got %+v %v", want, got, err)
	}
}
func (f *discoveryFixture) completion() pronboarding.Completion {
	d := f.decisions["PR9"]
	return pronboarding.Completion{Completed: true, Resource: f.observations["PR9"].Resource, Binding: d.Evidence, Project: "ENG", DeliveryID: "delivery", CompletedAt: time.Unix(1700000000, 0).UTC()}
}
func (f *discoveryFixture) secondPR() {
	p := f.prs[0]
	p.ID = "PR10"
	p.Number = 10
	f.prs = append(f.prs, p)
	original := f.observations["PR9"]
	raw, _ := json.Marshal(original)
	var o observe.ObservedState
	_ = json.Unmarshal(raw, &o)
	o.Resource = resource(p.Identity, "pull_request")
	o.PullRequest.Identity = p.Identity
	f.observations[p.ID] = o
	d := f.decisions["PR9"]
	d.Evidence.PR = p.Identity
	f.decisions[p.ID] = d
}
func TestDiscoveryCompleteAndReadPattern(t *testing.T) {
	f := fixture()
	got := mustComplete(t, f)
	if len(got.Entries) != 1 || got.Entries[0].Binding != f.decisions["PR9"] || got.Entries[0].Target.Resource.NodeID != "I46" || got.Entries[0].Onboarding.Completed || len(got.Entries[0].PR.Projects[0].Items) != 0 || len(got.Entries[0].PR.PullRequest.ClosingIssues) != 0 {
		t.Fatal(got)
	}
	want := []string{"list", "read", "binding", "read", "onboarding", "list", "read", "binding", "read", "onboarding", "list"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatal(f.calls)
	}
	f = fixture()
	f.prs = nil
	got = mustComplete(t, f)
	if len(got.Entries) != 0 || !reflect.DeepEqual(f.calls, []string{"list", "list", "list"}) {
		t.Fatal(got, f.calls)
	}
}
func TestDiscoverySharedTargetAndTerminalVisibility(t *testing.T) {
	for _, state := range []string{"OPEN", "CLOSED", "MERGED"} {
		t.Run(state, func(t *testing.T) {
			f := fixture()
			f.secondPR()
			f.prs[0].State = state
			f.observations["PR9"].PullRequest.State = state
			f.completions["PR9"] = f.completion()
			if state != "OPEN" {
				f.decisions["PR9"] = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.IneligibleTarget}
			}
			// Unrelated native links remain observation, never Target selection.
			unrelated := github.Identity{ID: "I77", Repository: f.cfg.Repositories[0], Number: 77}
			f.observations["PR9"].PullRequest.ClosingIssues = []github.Identity{unrelated}
			f.observations["I46"].Issue.LinkedPullRequests = []github.Identity{f.prs[1].Identity}
			slices.Reverse(f.prs)
			got := mustComplete(t, f)
			if len(got.Entries) != 2 || got.Entries[0].Resource.Number != 9 || got.Entries[1].Resource.Number != 10 || got.Entries[0].Target.Resource.NodeID != "I46" {
				t.Fatal(got)
			}
		})
	}
}
func TestDiscoveryCrossRepositoryTarget(t *testing.T) {
	f := fixture()
	repo := github.Repository{ID: "ADAPTER", Owner: "parametron-io", Name: "adapter"}
	f.cfg.Repositories = append(f.cfg.Repositories, repo)
	o := f.observations["I46"]
	o.Resource.Repository = "adapter"
	o.Issue.Repository = repo
	f.observations["I46"] = o
	f.observations["PR9"].PullRequest.Body = "Target: parametron-io/adapter#46"
	f.observations["PR9"].PullRequest.HeadRepository = repo
	d := f.decisions["PR9"]
	d.Evidence.Target.Repository = repo
	d.Evidence.Branch.Repository = repo
	f.decisions["PR9"] = d
	got := mustComplete(t, f)
	if got.Entries[0].Target.Resource.Repository != "adapter" {
		t.Fatal(got)
	}
}
func TestDiscoveryConstructor(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"empty", func(c *Config) { c.Repositories = nil }},
		{"65 repos", func(c *Config) {
			for i := 1; i <= 64; i++ {
				c.Repositories = append(c.Repositories, github.Repository{ID: fmt.Sprint("R", i), Owner: "parametron-io", Name: fmt.Sprint("repo", i)})
			}
		}},
		{"duplicate", func(c *Config) { c.Repositories = append(c.Repositories, c.Repositories[0]) }},
		{"case alias", func(c *Config) {
			r := c.Repositories[0]
			r.Name = "ENGINE"
			r.ID = "OTHER"
			c.Repositories = append(c.Repositories, r)
		}},
		{"shared node", func(c *Config) { r := c.Repositories[0]; r.Name = "other"; c.Repositories = append(c.Repositories, r) }},
		{"invalid repo", func(c *Config) { c.Repositories[0].ID = "" }},
		{"outside deployment", func(c *Config) {
			r := c.Repositories[0]
			r.Owner = "other"
			r.ID = "FOREIGN"
			c.Repositories = append(c.Repositories, r)
		}},
		{"project empty", func(c *Config) { c.Project = "" }},
		{"bug empty", func(c *Config) { c.BugProject = "" }},
		{"duplicate projects", func(c *Config) { c.BugProject = c.Project }},
		{"repository project collision", func(c *Config) { c.Project = c.Repositories[0].ID }},
		{"nil lister", func(c *Config) { c.Lister = nil }},
		{"typed nil lister", func(c *Config) { var x *github.PullRequestListerFake; c.Lister = x }},
		{"nil binding", func(c *Config) { c.Binding = nil }},
		{"typed nil binding", func(c *Config) { var x bindingFunc; c.Binding = x }},
		{"nil onboarding", func(c *Config) { c.Onboarding = nil }},
		{"typed nil onboarding", func(c *Config) { var x onboardingFunc; c.Onboarding = x }},
		{"nil reader", func(c *Config) { c.Reader = nil }},
		{"typed nil reader", func(c *Config) { var x readerFunc; c.Reader = x }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture()
			mustComplete(t, f)
			tc.change(&f.cfg)
			c, err := New(f.cfg)
			var typed *Error
			if c != nil || !errors.Is(err, ErrInvalid) || !errors.As(err, &typed) {
				t.Fatal(c, err)
			}
		})
	}
	f := fixture()
	for i := 1; i < 64; i++ {
		f.cfg.Repositories = append(f.cfg.Repositories, github.Repository{ID: fmt.Sprint("R", i), Owner: "parametron-io", Name: fmt.Sprint("repo", i)})
	}
	slices.Reverse(f.cfg.Repositories)
	before := slices.Clone(f.cfg.Repositories)
	mustComplete(t, f)
	if !reflect.DeepEqual(before, f.cfg.Repositories) {
		t.Fatal("constructor mutated input")
	}
}
func TestDiscoveryBindingValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*prbinding.Decision)
		want   error
	}{
		{"wrong PR", func(d *prbinding.Decision) { d.Evidence.PR.ID = "other" }, ErrInvalid},
		{"changed SHA", func(d *prbinding.Decision) { d.Evidence.Branch.SHA = strings.Repeat("b", 40) }, ErrChanged},
		{"changed ref", func(d *prbinding.Decision) { d.Evidence.Branch.Name = "development/other" }, ErrChanged},
		{"changed developer", func(d *prbinding.Decision) { d.Evidence.Developer.ID = "OTHER" }, ErrChanged},
		{"changed Target", func(d *prbinding.Decision) { d.Evidence.Target.ID = "OTHER"; d.Evidence.Target.Number = 47 }, ErrChanged},
		{"bad kind", func(d *prbinding.Decision) { d.Evidence.Kind = "Bug" }, ErrInvalid},
		{"bad policy", func(d *prbinding.Decision) { d.Evidence.Policy = "observed" }, ErrInvalid},
		{"bad branch", func(d *prbinding.Decision) { d.Evidence.Branch.Name = "bad..branch" }, ErrInvalid},
		{"bad developer", func(d *prbinding.Decision) { d.Evidence.Developer.Type = "Bot" }, ErrInvalid},
		{"accepted reason", func(d *prbinding.Decision) { d.Reason = prbinding.HeadMismatch }, ErrInvalid},
		{"unknown reason", func(d *prbinding.Decision) { *d = prbinding.Decision{Outcome: prbinding.Rejected, Reason: "unknown"} }, ErrInvalid},
		{"contradictory rejection", func(d *prbinding.Decision) { d.Outcome = prbinding.Rejected; d.Reason = prbinding.HeadMismatch }, ErrInvalid},
		{"changed", func(d *prbinding.Decision) {
			*d = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.Changed}
		}, ErrChanged},
		{"missing PR", func(d *prbinding.Decision) {
			*d = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingPR}
		}, ErrChanged},
		{"unresolved", func(d *prbinding.Decision) {
			*d = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.UnresolvedTarget}
		}, ErrIncomplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture()
			mustComplete(t, f)
			d := f.decisions["PR9"]
			tc.change(&d)
			f.decisions["PR9"] = d
			got, err := f.discover(t)
			zeroFailure(t, got, err, tc.want)
		})
	}
	for _, reason := range []prbinding.Reason{prbinding.InvalidTarget, prbinding.MissingTarget, prbinding.OutsideDeployment, prbinding.UnsupportedTarget, prbinding.IneligibleTarget, prbinding.MissingProvenance, prbinding.InvalidProvenance, prbinding.HeadMismatch, prbinding.MissingHead, prbinding.InvalidDeveloper, prbinding.InactiveMembership, prbinding.InsufficientPermission, prbinding.AssigneeMismatch} {
		t.Run(string(reason), func(t *testing.T) {
			f := fixture()
			f.decisions["PR9"] = prbinding.Decision{Outcome: prbinding.Rejected, Reason: reason}
			got := mustComplete(t, f)
			if got.Entries[0].Binding.Reason != reason {
				t.Fatal(got)
			}
		})
	}
}
func TestDiscoveryOnboardingValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*pronboarding.Completion)
	}{
		{"wrong PR", func(c *pronboarding.Completion) { c.Resource.NodeID = "OTHER" }},
		{"wrong binding PR", func(c *pronboarding.Completion) { c.Binding.PR.ID = "OTHER" }},
		{"wrong Project", func(c *pronboarding.Completion) { c.Project = "OTHER" }},
		{"missing delivery", func(c *pronboarding.Completion) { c.DeliveryID = "" }},
		{"missing timestamp", func(c *pronboarding.Completion) { c.CompletedAt = time.Time{} }},
		{"contradictory absence", func(c *pronboarding.Completion) { c.Completed = false }},
		{"invalid historical evidence", func(c *pronboarding.Completion) { c.Binding.Policy = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture()
			f.completions["PR9"] = f.completion()
			mustComplete(t, f)
			c := f.completions["PR9"]
			tc.change(&c)
			f.completions["PR9"] = c
			got, err := f.discover(t)
			zeroFailure(t, got, err, ErrInvalid)
		})
	}
	f := fixture()
	f.completions["PR9"] = f.completion()
	f.decisions["PR9"] = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.InactiveMembership}
	got := mustComplete(t, f)
	if got.Entries[0].Binding.Outcome != prbinding.Rejected || !got.Entries[0].Onboarding.Completed {
		t.Fatal(got)
	}
}
func TestDiscoveryBetweenPassChanges(t *testing.T) {
	cases := []struct {
		name   string
		change func(*discoveryFixture)
	}{
		{"body", func(f *discoveryFixture) { f.observations["PR9"].PullRequest.Body += "\nchanged" }},
		{"Target directive", func(f *discoveryFixture) {
			f.observations["PR9"].PullRequest.Body = ""
			f.decisions["PR9"] = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingTarget}
		}},
		{"head repo", func(f *discoveryFixture) { f.observations["PR9"].PullRequest.HeadRepository.ID = "OTHER" }},
		{"head ref", func(f *discoveryFixture) { f.observations["PR9"].PullRequest.HeadRef = "other" }},
		{"head SHA", func(f *discoveryFixture) { f.observations["PR9"].PullRequest.HeadSHA = strings.Repeat("b", 40) }},
		{"author", func(f *discoveryFixture) {
			f.observations["PR9"].PullRequest.Author = &github.Actor{ID: "OTHER", Login: "other", Type: "User"}
		}},
		{"binding", func(f *discoveryFixture) {
			f.decisions["PR9"] = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingProvenance}
		}},
		{"onboarding appears", func(f *discoveryFixture) { f.completions["PR9"] = f.completion() }},
		{"target state", func(f *discoveryFixture) { f.observations["I46"].Issue.State = "CLOSED" }},
		{"target body", func(f *discoveryFixture) { f.observations["I46"].Issue.Body += "\nchanged" }},
		{"target parent", func(f *discoveryFixture) {
			f.observations["I46"].Issue.Parent = &github.Identity{ID: "PARENT", Repository: f.cfg.Repositories[0], Number: 2}
		}},
		{"target relation", func(f *discoveryFixture) {
			f.observations["I46"].Issue.BlockedBy = []github.Identity{{ID: "BLOCKER", Repository: f.cfg.Repositories[0], Number: 3}}
		}},
		{"PR membership", func(f *discoveryFixture) {
			o := f.observations["PR9"]
			o.Projects[0].Items = []observe.ProjectItem{{ID: "ITEM"}}
		}},
		{"Target membership", func(f *discoveryFixture) {
			o := f.observations["I46"]
			o.Projects[0].Items = []observe.ProjectItem{{ID: "ITEM"}}
		}},
		{"lifecycle", func(f *discoveryFixture) {
			f.prs[0].State = "CLOSED"
			f.observations["PR9"].PullRequest.State = "CLOSED"
		}},
		{"removed", func(f *discoveryFixture) { f.prs = nil }},
		{"added", func(f *discoveryFixture) { f.secondPR() }},
		{"replaced identity", func(f *discoveryFixture) {
			f.prs[0].ID = "REPLACED"
			o := f.observations["PR9"]
			o.Resource.NodeID = "REPLACED"
			o.PullRequest.ID = "REPLACED"
			f.observations["REPLACED"] = o
			d := f.decisions["PR9"]
			d.Evidence.PR.ID = "REPLACED"
			f.decisions["REPLACED"] = d
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture()
			mustComplete(t, f)
			f.phase = 0
			f.calls = nil
			f.hook = func(stage string, phase int, r storage.Resource) {
				if stage == "list" && phase == 2 {
					tc.change(f)
				}
			}
			got, err := f.discover(t)
			zeroFailure(t, got, err, ErrChanged)
		})
	}
	for _, name := range []string{"completion disappears", "completion changes", "item identity", "field"} {
		t.Run(name, func(t *testing.T) {
			f := fixture()
			f.completions["PR9"] = f.completion()
			o := f.observations["PR9"]
			o.Projects[0].Items = []observe.ProjectItem{{ID: "ITEM", Fields: map[config.FieldRole]observe.FieldValue{config.RoadmapOrder: {FieldValue: github.FieldValue{Kind: config.Number, Number: 10001}}}}}
			mustComplete(t, f)
			f.phase = 0
			f.hook = func(stage string, phase int, r storage.Resource) {
				if stage != "list" || phase != 2 {
					return
				}
				switch name {
				case "completion disappears":
					delete(f.completions, "PR9")
				case "completion changes":
					c := f.completions["PR9"]
					c.DeliveryID = "new"
					f.completions["PR9"] = c
				case "item identity":
					f.observations["PR9"].Projects[0].Items[0].ID = "NEWITEM"
				case "field":
					f.observations["PR9"].Projects[0].Items[0].Fields[config.RoadmapOrder] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Number, Number: 10002}}
				}
			}
			got, err := f.discover(t)
			zeroFailure(t, got, err, ErrChanged)
		})
	}
}
func TestDiscoverySharedTargetInconsistency(t *testing.T) {
	f := fixture()
	f.secondPR()
	mustComplete(t, f)
	f.phase = 0
	reads := 0
	f.hook = func(stage string, phase int, r storage.Resource) {
		if stage == "read" && r.Kind == "issue" {
			reads++
			if reads == 2 {
				f.observations["I46"].Issue.Body += "\nchanged"
			}
		}
	}
	got, err := f.discover(t)
	zeroFailure(t, got, err, ErrChanged)
	if f.phase != 1 {
		t.Fatal("contradiction escaped first pass", f.phase)
	}
}
func TestDiscoveryFinalBarrier(t *testing.T) {
	for _, name := range []string{"added", "removed", "lifecycle", "identity", "reordered"} {
		t.Run(name, func(t *testing.T) {
			f := fixture()
			f.secondPR()
			mustComplete(t, f)
			f.phase = 0
			f.hook = func(stage string, phase int, r storage.Resource) {
				if stage != "list" || phase != 3 {
					return
				}
				switch name {
				case "added":
					p := f.prs[0]
					p.ID = "PR11"
					p.Number = 11
					f.prs = append(f.prs, p)
				case "removed":
					f.prs = f.prs[:1]
				case "lifecycle":
					f.prs[0].State = "MERGED"
				case "identity":
					f.prs[0].ID = "REPLACED"
				case "reordered":
					slices.Reverse(f.prs)
				}
			}
			got, err := f.discover(t)
			if name == "reordered" {
				if err != nil || len(got.Entries) != 2 {
					t.Fatal(got, err)
				}
			} else {
				zeroFailure(t, got, err, ErrChanged)
			}
		})
	}
}

func TestDiscoveryContradictoryEvidence(t *testing.T) {
	cases := []struct {
		name   string
		change func(*discoveryFixture)
		want   error
	}{
		{"listed duplicate", func(f *discoveryFixture) { f.prs = append(f.prs, f.prs[0]) }, ErrInvalid},
		{"listed number conflict", func(f *discoveryFixture) { p := f.prs[0]; p.ID = "OTHER"; f.prs = append(f.prs, p) }, ErrInvalid},
		{"listed node conflict", func(f *discoveryFixture) { p := f.prs[0]; p.Number++; f.prs = append(f.prs, p) }, ErrInvalid},
		{"listed wrong repo", func(f *discoveryFixture) {
			f.cfg.Lister = &github.PullRequestListerFake{ListPullRequestsFunc: func(context.Context, github.Repository) ([]github.ListedPullRequest, error) {
				p := f.prs[0]
				p.Repository.ID = "WRONG"
				return []github.ListedPullRequest{p}, nil
			}}
		}, ErrInvalid},
		{"read identity", func(f *discoveryFixture) { f.observations["PR9"].PullRequest.ID = "OTHER" }, ErrInvalid},
		{"resource identity", func(f *discoveryFixture) {
			o := f.observations["PR9"]
			o.Resource.NodeID = "OTHER"
			f.observations["PR9"] = o
		}, ErrInvalid},
		{"wrong kind", func(f *discoveryFixture) {
			o := f.observations["PR9"]
			o.Issue = &github.Issue{}
			f.observations["PR9"] = o
		}, ErrInvalid},
		{"missing PR", func(f *discoveryFixture) {
			o := f.observations["PR9"]
			o.Presence = observe.Missing
			f.observations["PR9"] = o
		}, ErrChanged},
		{"missing Target", func(f *discoveryFixture) {
			o := f.observations["I46"]
			o.Presence = observe.Missing
			f.observations["I46"] = o
		}, ErrChanged},
		{"target PR node reuse", func(f *discoveryFixture) {
			o := f.observations["I46"]
			o.Resource.NodeID = "PR9"
			o.Issue.ID = "PR9"
			f.observations["I46"] = o
		}, ErrInvalid},
		{"Project resource collision", func(f *discoveryFixture) {
			f.cfg.Project = "PR9"
			for k, o := range f.observations {
				o.Projects[0].ID = "PR9"
				f.observations[k] = o
			}
		}, ErrInvalid},
		{"resource repository collision", func(f *discoveryFixture) {
			f.observations["PR9"].PullRequest.ClosingIssues = []github.Identity{{ID: "REPO", Repository: f.cfg.Repositories[0], Number: 77}}
		}, ErrInvalid},
		{"repo aliases", func(f *discoveryFixture) {
			r := f.cfg.Repositories[0]
			r.Name = "other"
			f.observations["PR9"].PullRequest.ClosingIssues = []github.Identity{{ID: "I77", Repository: r, Number: 77}}
		}, ErrInvalid},
		{"repository conflicting IDs", func(f *discoveryFixture) {
			r := f.cfg.Repositories[0]
			r.ID = "OTHER"
			f.observations["PR9"].PullRequest.ClosingIssues = []github.Identity{{ID: "I77", Repository: r, Number: 77}}
		}, ErrInvalid},
		{"duplicate endpoints", func(f *discoveryFixture) {
			i := f.observations["I46"].Issue.Identity
			f.observations["PR9"].PullRequest.ClosingIssues = []github.Identity{i, i}
		}, ErrInvalid},
		{"invalid endpoint", func(f *discoveryFixture) { f.observations["PR9"].PullRequest.ClosingIssues = []github.Identity{{}} }, ErrInvalid},
		{"incomplete Projects", func(f *discoveryFixture) {
			o := f.observations["PR9"]
			o.Projects = o.Projects[:1]
			f.observations["PR9"] = o
		}, ErrIncomplete},
		{"duplicate profile", func(f *discoveryFixture) { o := f.observations["PR9"]; o.Projects[1] = o.Projects[0] }, ErrInvalid},
		{"wrong Project", func(f *discoveryFixture) { f.observations["PR9"].Projects[0].ID = "OTHER" }, ErrInvalid},
		{"duplicate items", func(f *discoveryFixture) {
			f.observations["PR9"].Projects[0].Items = []observe.ProjectItem{{ID: "ITEM"}, {ID: "ITEM"}}
		}, ErrInvalid},
		{"item resource reuse", func(f *discoveryFixture) {
			f.observations["PR9"].Projects[0].Items = []observe.ProjectItem{{ID: "I46"}}
		}, ErrInvalid},
		{"item repo reuse", func(f *discoveryFixture) {
			f.observations["PR9"].Projects[0].Items = []observe.ProjectItem{{ID: "REPO"}}
		}, ErrInvalid},
		{"item Project reuse", func(f *discoveryFixture) {
			f.observations["PR9"].Projects[0].Items = []observe.ProjectItem{{ID: "ENG"}}
		}, ErrInvalid},
		{"item shared across resources", func(f *discoveryFixture) {
			for _, o := range f.observations {
				o.Projects[0].Items = []observe.ProjectItem{{ID: "ITEM"}}
			}
		}, ErrInvalid},
		{"bad field type", func(f *discoveryFixture) {
			f.observations["PR9"].Projects[0].Items = []observe.ProjectItem{{ID: "ITEM", Fields: map[config.FieldRole]observe.FieldValue{config.RoadmapOrder: {FieldValue: github.FieldValue{Kind: config.SingleSelect}}}}}
		}, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture()
			mustComplete(t, f)
			tc.change(f)
			got, err := f.discover(t)
			zeroFailure(t, got, err, tc.want)
		})
	}
}
func TestDiscoveryRegistryCanonicalReferences(t *testing.T) {
	r := fixture().cfg.Repositories[0]
	i := github.Identity{ID: "I", Repository: r, Number: 46}
	v := newIdentities([]github.Repository{r}, "ENG", "BUG")
	if !v.add(i, "issue", false) || !v.add(i, "issue", false) {
		t.Fatal("legitimate repeated Target rejected")
	}
	if v.add(i, "issue", true) || v.add(i, "pull_request", false) {
		t.Fatal("duplicate declaration/kind collision accepted")
	}
	// Same node in another configured repository is never a second PR identity.
	r2 := github.Repository{ID: "R2", Owner: r.Owner, Name: "other"}
	v = newIdentities([]github.Repository{r, r2}, "ENG", "BUG")
	p := github.Identity{ID: "PR", Repository: r, Number: 9}
	if !v.add(p, "pull_request", true) {
		t.Fatal("baseline")
	}
	p.Repository = r2
	if v.add(p, "pull_request", true) {
		t.Fatal("cross-repository node reuse")
	}
}
func TestDiscoveryOperationalFailures(t *testing.T) {
	for _, stage := range []string{"list", "read", "binding", "onboarding"} {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded, github.ErrRequestLimit, github.ErrPullRequestChanged, github.ErrPullRequestIncomplete, github.ErrPullRequestLimit, &github.Error{Category: github.Forbidden}, &github.Error{Category: github.Unauthorized}, &github.Error{Category: github.NotFound}, &github.Error{Category: github.Transient}, &github.Error{Category: github.RateLimited}, &github.Error{Category: github.Malformed}, pronboarding.ErrProvenance} {
			t.Run(stage+"/"+cause.Error(), func(t *testing.T) {
				f := fixture()
				mustComplete(t, f)
				f.calls = nil
				f.failure = func(s string) error {
					if s == stage {
						return cause
					}
					return nil
				}
				got, err := f.discover(t)
				want := cause
				switch cause {
				case github.ErrRequestLimit, github.ErrPullRequestLimit:
					want = ErrLimit
				case github.ErrPullRequestChanged:
					want = ErrChanged
				case github.ErrPullRequestIncomplete:
					want = ErrIncomplete
				}
				zeroFailure(t, got, err, want)
				if f.calls[len(f.calls)-1] != stage {
					t.Fatal("continued after failure", f.calls)
				}
			})
		}
	}
}
func TestDiscoveryCancellation(t *testing.T) {
	for _, stage := range []string{"before", "list", "read", "binding", "onboarding", "final"} {
		t.Run(stage, func(t *testing.T) {
			f := fixture()
			c, err := New(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "before" {
				cancel()
			}
			f.hook = func(s string, phase int, r storage.Resource) {
				if s == stage || stage == "final" && s == "list" && phase == 3 {
					cancel()
				}
			}
			got, err := c.Discover(ctx)
			zeroFailure(t, got, err, context.Canceled)
			if stage == "before" && len(f.calls) != 0 {
				t.Fatal(f.calls)
			}
		})
	}
}
func TestDiscoveryBounds(t *testing.T) {
	f := fixture()
	f.cfg.Lister = &github.PullRequestListerFake{ListPullRequestsFunc: func(context.Context, github.Repository) ([]github.ListedPullRequest, error) {
		return make([]github.ListedPullRequest, MaxPullRequests+1), nil
	}}
	got, err := f.discover(t)
	zeroFailure(t, got, err, ErrLimit)
	if len(f.calls) != 0 {
		t.Fatal("oversize list materialized", f.calls)
	}
}
func TestDiscoveryCanonicalAndDetached(t *testing.T) {
	f := fixture()
	f.secondPR()
	f.observations["I46"].Issue.Assignees = append(f.observations["I46"].Issue.Assignees, github.Actor{ID: "U2", Login: "another", Type: "User"})
	a := github.Identity{ID: "I77", Repository: f.cfg.Repositories[0], Number: 77}
	b := github.Identity{ID: "I78", Repository: f.cfg.Repositories[0], Number: 78}
	f.observations["PR9"].PullRequest.ClosingIssues = []github.Identity{b, a}
	f.observations["I46"].Issue.BlockedBy = []github.Identity{b, a}
	f.observations["PR9"].Projects[0].Items = []observe.ProjectItem{{ID: "ITEM", Fields: map[config.FieldRole]observe.FieldValue{
		config.RoadmapOrder: {FieldValue: github.FieldValue{Kind: config.Number, Number: 10001}},
		config.Status:       {FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "READY"}, StatusRole: config.Ready},
	}}}
	before, _ := json.Marshal(f.observations)
	first := mustComplete(t, f)
	after, _ := json.Marshal(f.observations)
	if string(before) != string(after) {
		t.Fatal("reader mutated")
	}
	slices.Reverse(f.prs)
	for _, o := range f.observations {
		slices.Reverse(o.Projects)
		if o.PullRequest != nil {
			slices.Reverse(o.PullRequest.Labels)
			slices.Reverse(o.PullRequest.ClosingIssues)
		}
		if o.Issue != nil {
			slices.Reverse(o.Issue.Labels)
			slices.Reverse(o.Issue.Assignees)
			slices.Reverse(o.Issue.BlockedBy)
		}
	}
	second := mustComplete(t, f)
	x, _ := json.Marshal(first)
	y, _ := json.Marshal(second)
	if !reflect.DeepEqual(first, second) || string(x) != string(y) {
		t.Fatal("noncanonical equivalent evidence")
	}
	f.observations["PR9"].PullRequest.Labels[0] = "changed"
	f.observations["PR9"].PullRequest.Author.ID = "changed"
	for _, p := range f.observations["PR9"].Projects {
		if p.Profile == config.Engineering {
			p.Items[0].Fields[config.RoadmapOrder] = observe.FieldValue{}
		}
	}
	if first.Entries[0].PR.PullRequest.Labels[0] != "a" || first.Entries[0].PR.PullRequest.Author.ID != "U" {
		t.Fatal("result aliases reader")
	}
	for _, p := range first.Entries[0].PR.Projects {
		if p.Profile == config.Engineering && p.Items[0].Fields[config.RoadmapOrder].Number != 10001 {
			t.Fatal("result aliases reader map")
		}
	}
}
func TestDiscoveryConcurrentIndependentInvocations(t *testing.T) {
	f := fixture()
	baseline := mustComplete(t, f)
	// Immutable dependencies deliberately avoid the sequenced fixture's counters.
	f.cfg.Lister = &github.PullRequestListerFake{ListPullRequestsFunc: func(context.Context, github.Repository) ([]github.ListedPullRequest, error) { return f.prs, nil }}
	f.cfg.Reader = readerFunc(func(_ context.Context, r storage.Resource) (observe.ObservedState, error) {
		if r.Kind == "issue" {
			return f.observations["I46"], nil
		}
		return f.observations[r.NodeID], nil
	})
	f.cfg.Binding = bindingFunc(func(_ context.Context, r storage.Resource) (prbinding.Decision, error) {
		return f.decisions[r.NodeID], nil
	})
	f.cfg.Onboarding = onboardingFunc(func(context.Context, storage.Resource) (pronboarding.Completion, error) {
		return pronboarding.Completion{}, nil
	})
	c, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.Discover(context.Background())
			if err != nil || !reflect.DeepEqual(got, baseline) {
				t.Errorf("independent result: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestDiscoveryOuterTransportBudget(t *testing.T) {
	var requests, tokens atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "POST" {
			t.Error("unexpected method", r.Method)
		}
		var q struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		if strings.Contains(q.Query, "mutation") {
			t.Error("unexpected mutation")
		}
		repo := map[string]any{"id": "REPO", "name": "engine", "owner": map[string]any{"login": "parametron-io"}}
		data := map[string]any{"repository": repo}
		if strings.Contains(q.Query, "result:pullRequests") {
			repo["__typename"] = "Repository"
			repo["result"] = map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
			data = map[string]any{"node": repo}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer s.Close()
	tr, err := github.NewTransport(s.URL, s.Client(), github.TokenFunc(func(context.Context) (string, error) { tokens.Add(1); return "token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture()
	f.cfg.Lister = tr
	c, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Discover(github.WithRequestLimit(context.Background(), 5))
	zeroFailure(t, got, err, ErrLimit)
	if requests.Load() != 5 || tokens.Load() != 5 || len(f.calls) != 0 {
		t.Fatal(requests.Load(), tokens.Load(), f.calls)
	}
	// A complete empty invocation is nine actual transport requests: three
	// repository enumerations, each with initial identity, page, final identity.
	got, err = c.Discover(github.WithRequestLimit(context.Background(), 9))
	if err != nil || got.Completeness != Complete || requests.Load() != 14 || tokens.Load() != 14 {
		t.Fatal(got, err, requests.Load())
	}
}
func TestDiscoveryExactPRLimit(t *testing.T) {
	f := fixture()
	repo := f.cfg.Repositories[0]
	listed := make([]github.ListedPullRequest, MaxPullRequests)
	for n := range listed {
		listed[n] = github.ListedPullRequest{Identity: github.Identity{ID: fmt.Sprint("P", n+1), Repository: repo, Number: n + 1}, State: "OPEN"}
	}
	f.cfg.Lister = &github.PullRequestListerFake{ListPullRequestsFunc: func(context.Context, github.Repository) ([]github.ListedPullRequest, error) { return listed, nil }}
	f.cfg.Reader = readerFunc(func(_ context.Context, r storage.Resource) (observe.ObservedState, error) {
		return observe.ObservedState{Resource: r, Presence: observe.Present, PullRequest: &github.PullRequest{Identity: github.Identity{ID: r.NodeID, Repository: repo, Number: int(r.Number)}, State: "OPEN"}, Projects: []observe.ProjectState{{Profile: config.Engineering, ID: "ENG"}, {Profile: config.BugTracker, ID: "BUG"}}}, nil
	})
	f.cfg.Binding = bindingFunc(func(context.Context, storage.Resource) (prbinding.Decision, error) {
		return prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingTarget}, nil
	})
	f.cfg.Onboarding = onboardingFunc(func(context.Context, storage.Resource) (pronboarding.Completion, error) {
		return pronboarding.Completion{}, nil
	})
	got := mustComplete(t, f)
	if len(got.Entries) != MaxPullRequests {
		t.Fatal(len(got.Entries))
	}
}
func TestDiscoveryEvidenceSizeBound(t *testing.T) {
	f := fixture()
	mustComplete(t, f)
	f.observations["PR9"].PullRequest.Title = strings.Repeat("x", MaxEvidenceBytes)
	got, err := f.discover(t)
	zeroFailure(t, got, err, ErrLimit)
}
func TestDiscoveryBindingEvidenceChangesBetweenPasses(t *testing.T) {
	f := fixture()
	mustComplete(t, f)
	f.phase = 0
	f.hook = func(stage string, phase int, r storage.Resource) {
		if stage == "list" && phase == 2 {
			d := f.decisions["PR9"]
			d.Evidence.Branch.SHA = strings.Repeat("b", 40)
			f.decisions["PR9"] = d
			f.observations["PR9"].PullRequest.HeadSHA = d.Evidence.Branch.SHA
		}
	}
	got, err := f.discover(t)
	zeroFailure(t, got, err, ErrChanged)
}

func TestDiscoveryAggregateEvidenceSizeBound(t *testing.T) {
	f := fixture()
	payload := strings.Repeat("x", 17<<20)
	f.observations["PR9"].PullRequest.Title = payload
	f.observations["I46"].Issue.Title = payload
	// Each resource and this entire single-PR pass are below 64 MiB.
	mustComplete(t, f)
	f.secondPR()
	// Repeated, coherent Target references remain valid but still count toward
	// the encoded per-pass evidence bound; the second Entry must not escape.
	got, err := f.discover(t)
	zeroFailure(t, got, err, ErrLimit)
}
