package prdevelopment

import (
	"errors"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"reflect"
	"strings"
	"testing"
	"time"
)

func developmentInput(t *testing.T) Input {
	t.Helper()
	repo := github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}
	target := github.Identity{ID: "I", Repository: repo, Number: 48}
	pr := github.Identity{ID: "PR", Repository: repo, Number: 80}
	dev := github.Actor{ID: "U", Login: "dev", Type: "User"}
	name, err := eb.Name(resource(target, "issue"))
	if err != nil {
		t.Fatal(err)
	}
	b := prbinding.Evidence{PR: pr, Target: target, Kind: sp.Task, Developer: dev, Branch: github.Branch{Repository: repo, Name: name, SHA: strings.Repeat("a", 40)}, Policy: "active_org_member_repository_write_assignee_v1"}
	return Input{Binding: prbinding.Decision{Outcome: prbinding.Accepted, Evidence: b}, Onboarding: pronboarding.Completion{Completed: true, Resource: resource(pr, "pull_request"), Binding: b, Project: "ENG", DeliveryID: "delivery", CompletedAt: time.Unix(100, 0)}, Project: "ENG", PR: github.PullRequest{Identity: pr, State: "OPEN", Body: "Target: #48", Author: &dev, HeadRepository: repo, HeadRef: name, HeadSHA: b.Branch.SHA}, Target: github.Issue{Identity: target, State: "OPEN"}, References: github.DevelopmentReferences{PR: pr, Complete: true}}
}
func TestEvaluateCanonicalDevelopment(t *testing.T) {
	base := developmentInput(t)
	target := base.Target.Identity
	a := target
	a.ID = "A"
	a.Number = 49
	b := target
	b.ID = "B"
	b.Number = 50
	for _, tc := range []struct {
		name            string
		manual, keyword []github.Identity
		want            []Action
		bad             bool
	}{
		{name: "missing", want: []Action{{Add: true, Target: target}}},
		{name: "manual", manual: []github.Identity{target}},
		{name: "keyword", keyword: []github.Identity{target}},
		{name: "overlap", manual: []github.Identity{target}, keyword: []github.Identity{target}},
		{name: "contradiction", manual: []github.Identity{a}, want: []Action{{Target: a}, {Add: true, Target: target}}},
		{name: "ordered", manual: []github.Identity{b, a}, want: []Action{{Target: a}, {Target: b}, {Add: true, Target: target}}},
		{name: "reordered", manual: []github.Identity{a, b}, want: []Action{{Target: a}, {Target: b}, {Add: true, Target: target}}},
		{name: "canonical with drift", manual: []github.Identity{b, target, a}, want: []Action{{Target: a}, {Target: b}}},
		{name: "duplicate", manual: []github.Identity{a, a}, bad: true},
		{name: "contradictory keyword", keyword: []github.Identity{a}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.References.Manual = tc.manual
			in.References.Keyword = tc.keyword
			got, err := Evaluate(in)
			if tc.bad {
				if err == nil || len(got) != 0 {
					t.Fatalf("actions=%v err=%v", got, err)
				}
				return
			}
			if err != nil || len(got) != len(tc.want) || len(got) > 0 && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%v err=%v want=%v", got, err, tc.want)
			}
		})
	}
}
func TestEvaluateDevelopmentAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Input)
	}{
		{"rejected", func(i *Input) { i.Binding.Outcome = prbinding.Rejected }},
		{"reason", func(i *Input) { i.Binding.Reason = prbinding.Changed }},
		{"unsupported type", func(i *Input) { i.Binding.Evidence.Kind = sp.Bug }},
		{"missing completion", func(i *Input) { i.Onboarding.Completed = false }},
		{"wrong project", func(i *Input) { i.Onboarding.Project = "OTHER" }},
		{"wrong completion identity", func(i *Input) { i.Onboarding.Resource.NodeID = "OTHER" }},
		{"wrong body", func(i *Input) { i.PR.Body = "Target: #49" }},
		{"refs only", func(i *Input) { i.PR.Body = "Refs: #48" }},
		{"missing target", func(i *Input) { i.PR.Body = "" }},
		{"malformed directives", func(i *Input) { i.PR.Body = "Target: #48\nTarget: #49" }},
		{"wrong author", func(i *Input) { a := *i.PR.Author; a.ID = "OTHER"; i.PR.Author = &a }},
		{"wrong head repo", func(i *Input) { i.PR.HeadRepository.ID = "OTHER" }},
		{"wrong ref", func(i *Input) { i.PR.HeadRef = "other" }},
		{"wrong sha", func(i *Input) { i.PR.HeadSHA = strings.Repeat("b", 40) }},
		{"wrong target identity", func(i *Input) { i.Target.Identity.ID = "OTHER" }},
		{"invalid pr", func(i *Input) { i.Binding.Evidence.PR.Number = 0 }},
		{"incomplete refs", func(i *Input) { i.References.Complete = false }},
		{"closed target", func(i *Input) { i.Target.State = "CLOSED" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := developmentInput(t)
			tc.change(&in)
			actions, err := Evaluate(in)
			if err == nil || len(actions) > 0 {
				t.Fatalf("actions=%v err=%v", actions, err)
			}
		})
	}
}
func TestEvaluateHistoricalAndCrossRepository(t *testing.T) {
	for _, state := range []string{"CLOSED", "MERGED"} {
		t.Run(state, func(t *testing.T) {
			in := developmentInput(t)
			in.PR.State = state
			in.Target.State = "CLOSED"
			in.References.Manual = []github.Identity{in.Target.Identity}
			a, err := Evaluate(in)
			if err != nil || len(a) != 0 {
				t.Fatalf("%v %v", a, err)
			}
		})
	}
	in := developmentInput(t)
	in.Onboarding.Binding.Branch.SHA = strings.Repeat("b", 40)
	if _, err := Evaluate(in); err != nil {
		t.Fatal(err)
	}
	in = developmentInput(t)
	repo := github.Repository{ID: "R2", Owner: "parametron-io", Name: "other"}
	in.Target.Identity.Repository = repo
	in.Binding.Evidence.Target = in.Target.Identity
	in.Binding.Evidence.Branch.Repository = repo
	in.PR.HeadRepository = repo
	in.PR.Body = "Target: parametron-io/other#48"
	in.Onboarding.Binding = in.Binding.Evidence
	a, err := Evaluate(in)
	if err != nil || !reflect.DeepEqual(a, []Action{{Add: true, Target: in.Target.Identity}}) {
		t.Fatalf("cross repository: %v %v", a, err)
	}
	in = developmentInput(t)
	conflict := in.Target.Identity
	conflict.ID = "OTHER"
	in.References.Manual = []github.Identity{conflict}
	if _, err := Evaluate(in); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("identity conflict: %v", err)
	}
}

func TestEvaluateTargetRepositoryConflict(t *testing.T) {
	in := developmentInput(t)
	// Cross-repository binding: observed Issue has a different number, but claims
	// the authoritative Target's repository name with a conflicting node ID.
	repo := github.Repository{ID: "R2", Owner: "parametron-io", Name: "other"}
	in.Target.Identity.Repository = repo
	in.Binding.Evidence.Target = in.Target.Identity
	in.Binding.Evidence.Branch.Repository = repo
	in.PR.HeadRepository = repo
	in.PR.Body = "Target: parametron-io/other#48"
	in.Onboarding.Binding = in.Binding.Evidence
	drift := in.Target.Identity
	drift.Number = 49
	drift.ID = "DRIFT"
	drift.Repository.ID = "WRONG"
	in.References.Manual = []github.Identity{drift}
	actions, err := Evaluate(in)
	if err == nil || len(actions) > 0 {
		t.Fatalf("unverifiable target repository: actions=%v err=%v", actions, err)
	}
}

func TestEvaluateRepositoryQualifiedNumbers(t *testing.T) {
	in := developmentInput(t)
	other := in.Target.Identity
	other.Repository = github.Repository{ID: "R2", Owner: "parametron-io", Name: "other"}
	other.ID = "OTHER"
	in.References.Manual = []github.Identity{other}
	got, err := Evaluate(in)
	want := []Action{{Target: other}, {Add: true, Target: in.Target.Identity}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v %v", got, err)
	}
}
