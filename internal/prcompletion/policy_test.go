package prcompletion

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
)

func completionInput() Input {
	repo := github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}
	pr := github.Identity{ID: "PR", Repository: repo, Number: 51}
	target := github.Identity{ID: "I", Repository: repo, Number: 50}
	dev := github.Actor{ID: "U", Login: "dev", Type: "User"}
	at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	b := prbinding.Evidence{PR: pr, Target: target, Kind: sp.Task, Developer: dev, Policy: "active_org_member_repository_write_assignee_v1", Branch: github.Branch{Repository: repo, Name: "development/issue-50", SHA: strings.Repeat("a", 40)}}
	return Input{Onboarding: pronboarding.Completion{Completed: true, Resource: resource(pr, "pull_request"), Binding: b, Project: "ENG", DeliveryID: "onboard", CompletedAt: at}, PR: github.PullRequest{Identity: pr, State: "MERGED", Body: "Target: #50", Author: &dev, HeadRepository: repo, HeadRef: b.Branch.Name, HeadSHA: b.Branch.SHA}, Target: github.Issue{Identity: target, State: "OPEN"}, Node: ec.IssueNode{Resource: resource(target, "issue"), Type: sp.Task, State: ec.Open}, Native: github.NativeLifecycle{Resource: pr, Kind: "pull_request", State: "MERGED", Transitions: []github.NativeTransition{{ID: "MERGE", Kind: "MergedEvent", Resource: pr, CreatedAt: at.Add(time.Hour)}}}}
}

func TestEvaluateCompletionAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Input)
		want   Decision
		err    error
	}{
		{"merged", func(*Input) {}, Decision{Outcome: Merged, PRStatus: config.Done, CloseTarget: true}, nil},
		{"closed without merge", func(i *Input) {
			i.PR.State = "CLOSED"
			i.Native.State = "CLOSED"
			i.Native.Transitions[0].Kind = "ClosedEvent"
		}, Decision{Outcome: Abandoned, PRStatus: config.Done, TargetStatus: config.InProgress}, nil},
		{"open approval cannot complete", func(i *Input) { i.PR.State = "OPEN" }, Decision{Outcome: Open}, nil},
		{"phase merge", func(i *Input) {
			i.Node.Type = sp.Phase
			i.Onboarding.Binding.Kind = sp.Phase
			i.Node.Intent.CreateBranch = intent.Boolean{Value: true, Explicit: true}
		}, Decision{Outcome: Merged, PRStatus: config.Done}, nil},
		{"feature merge", func(i *Input) { i.Node.Type = sp.Feature; i.Onboarding.Binding.Kind = sp.Feature }, Decision{Outcome: Merged, PRStatus: config.Done, CloseTarget: true}, nil},
		{"phase without direct authority", func(i *Input) { i.Node.Type = sp.Phase; i.Onboarding.Binding.Kind = sp.Phase }, Decision{}, ErrRejected},
		{"unsupported state", func(i *Input) { i.PR.State = "APPROVED" }, Decision{}, ErrRejected},
		{"missing completion", func(i *Input) { i.Onboarding.Completed = false }, Decision{}, ErrRejected},
		{"missing delivery", func(i *Input) { i.Onboarding.DeliveryID = "" }, Decision{}, ErrRejected},
		{"missing timestamp", func(i *Input) { i.Onboarding.CompletedAt = time.Time{} }, Decision{}, ErrRejected},
		{"changed target", func(i *Input) { i.Target.ID = "OTHER" }, Decision{}, ErrRejected},
		{"changed PR", func(i *Input) { i.PR.Number++ }, Decision{}, ErrRejected},
		{"changed target directive", func(i *Input) { i.PR.Body = "Target: #49" }, Decision{}, ErrRejected},
		{"conflicting directive", func(i *Input) { i.PR.Body = "Target: #50\nTarget: #49" }, Decision{}, ErrRejected},
		{"changed author", func(i *Input) { d := *i.PR.Author; d.ID = "OTHER"; i.PR.Author = &d }, Decision{}, ErrChanged},
		{"changed branch", func(i *Input) { i.PR.HeadRef = "main" }, Decision{}, ErrChanged},
		{"merge before onboarding", func(i *Input) { i.Native.Transitions[0].CreatedAt = i.Onboarding.CompletedAt }, Decision{}, ErrRejected},
		{"stale merge identity", func(i *Input) { i.Native.Resource.ID = "OTHER" }, Decision{}, ErrChanged},
		{"duplicate merge evidence", func(i *Input) { i.Native.Transitions = append(i.Native.Transitions, i.Native.Transitions[0]) }, Decision{}, ErrRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := completionInput()
			tc.change(&in)
			got, err := Evaluate(in)
			if !errors.Is(err, tc.err) {
				t.Fatalf("error %v want %v", err, tc.err)
			}
			if err == nil {
				want := tc.want
				if want.Outcome == Merged {
					want.Merge = in.Native.Transitions[0]
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("got %+v want %+v", got, want)
				}
				again, e := Evaluate(in)
				if e != nil || !reflect.DeepEqual(got, again) {
					t.Fatal("nondeterministic duplicate", again, e)
				}
			}
		})
	}
}
