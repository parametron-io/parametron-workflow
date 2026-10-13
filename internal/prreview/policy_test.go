package prreview

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
)

func policyInput(t *testing.T) Input {
	t.Helper()
	repo := github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}
	dev := github.Actor{ID: "U", Login: "dev", Type: "User"}
	pr := github.Identity{ID: "PR", Repository: repo, Number: 49}
	target := github.Identity{ID: "I", Repository: repo, Number: 48}
	name, err := eb.Name(resource(target, "issue"))
	if err != nil {
		t.Fatal(err)
	}
	b := prbinding.Evidence{PR: pr, Target: target, Kind: sp.Task, Developer: dev, Branch: github.Branch{Repository: repo, Name: name, SHA: strings.Repeat("a", 40)}, Policy: "active_org_member_repository_write_assignee_v1"}
	at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	ready := github.ReviewTransition{ID: "READY1", Kind: "ReadyForReviewEvent", PR: pr, Actor: &dev, CreatedAt: at.Add(time.Minute)}
	return Input{Candidate: Candidate{Kind: "ready", PRID: pr.ID, Number: 49, Actor: dev, At: ready.CreatedAt}, Binding: prbinding.Decision{Outcome: prbinding.Accepted, Evidence: b}, Onboarding: pronboarding.Completion{Completed: true, Resource: resource(pr, "pull_request"), Binding: b, Project: "ENG", DeliveryID: "onboard", CompletedAt: at}, PR: github.PullRequest{Identity: pr, State: "OPEN", Author: &dev, HeadRepository: repo, HeadRef: name, HeadSHA: b.Branch.SHA}, Facts: github.ReviewFacts{PR: pr, Transitions: []github.ReviewTransition{ready}}, PROwned: true, TargetOwned: true}
}
func assertNoEffect(t *testing.T, d Decision) {
	t.Helper()
	if d.PRStatus || d.TargetStatus || d.ConvertDraft || d.Outcome != NoTransition && d.Outcome != "" {
		t.Fatalf("unexpected effects: %+v", d)
	}
}
func TestReadyAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
		want   error
		reason Reason
	}{
		{"valid", func(*Input) {}, nil, ""},
		{"unsupported", func(i *Input) { i.Candidate.Kind = "opened" }, nil, Unsupported},
		{"missing_native_event", func(i *Input) { i.Facts.Transitions = nil }, ErrCorrelation, ""},
		{"equal_times", func(i *Input) {
			x := i.Facts.Transitions[0]
			x.ID = "OTHER"
			i.Facts.Transitions = append(i.Facts.Transitions, x)
		}, ErrCorrelation, ""},
		{"wrong_native_actor", func(i *Input) { a := *i.Facts.Transitions[0].Actor; a.ID = "OTHER"; i.Facts.Transitions[0].Actor = &a }, ErrCorrelation, ""},
		{"wrong_selector_actor", func(i *Input) { i.Candidate.Actor.ID = "OTHER" }, ErrCorrelation, ""},
		{"reviewer_cannot_ready", func(i *Input) {
			a := i.Candidate.Actor
			a.ID = "OTHER"
			i.Candidate.Actor = a
			i.Facts.Transitions[0].Actor = &a
		}, ErrRejected, ""},
		{"before_completion", func(i *Input) { i.Onboarding.CompletedAt = i.Candidate.At }, nil, Stale},
		{"incomplete_onboarding", func(i *Input) { i.Onboarding.Completed = false }, ErrRejected, ""},
		{"rejected_binding", func(i *Input) { i.Binding.Outcome = prbinding.Rejected }, ErrRejected, ""},
		{"wrong_pr", func(i *Input) { i.Candidate.PRID = "OTHER" }, ErrChanged, ""},
		{"wrong_repository", func(i *Input) { i.PR.Repository.ID = "OTHER" }, ErrChanged, ""},
		{"wrong_target", func(i *Input) { i.Onboarding.Binding.Target.ID = "OTHER" }, ErrRejected, ""},
		{"head_drift", func(i *Input) { i.PR.HeadSHA = strings.Repeat("b", 40) }, ErrChanged, ""},
		{"closed", func(i *Input) { i.PR.State = "CLOSED" }, ErrChanged, ""},
		{"draft", func(i *Input) { i.PR.Draft = true }, nil, Stale},
		{"nullable_actor", func(i *Input) { i.Facts.Transitions[0].Actor = nil }, ErrCorrelation, ""},
		{"native_wrong_pr", func(i *Input) { i.Facts.Transitions[0].PR.ID = "OTHER" }, ErrInvalid, ""},
		{"duplicate_native_id", func(i *Input) {
			x := i.Facts.Transitions[0]
			x.CreatedAt = x.CreatedAt.Add(time.Second)
			i.Facts.Transitions = append(i.Facts.Transitions, x)
		}, ErrInvalid, ""},
		{"superseded", func(i *Input) {
			x := i.Facts.Transitions[0]
			x.ID = "DRAFT"
			x.Kind = "ConvertToDraftEvent"
			x.CreatedAt = x.CreatedAt.Add(time.Second)
			i.Facts.Transitions = append(i.Facts.Transitions, x)
		}, nil, Stale},
		{"duplicate", func(i *Input) { i.Current = &Cycle{Ready: i.Facts.Transitions[0]} }, nil, Duplicate},
		{"second_without_rework", func(i *Input) {
			old := i.Facts.Transitions[0]
			old.ID = "OLD"
			old.CreatedAt = old.CreatedAt.Add(-time.Second)
			i.Current = &Cycle{Ready: old}
			i.Facts.Transitions = append([]github.ReviewTransition{old}, i.Facts.Transitions...)
		}, ErrRejected, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := policyInput(t)
			tc.mutate(&i)
			d, err := Evaluate(i)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if tc.name == "valid" {
				if d.Outcome != StartReview || d.Status != config.InReview || !d.PRStatus || !d.TargetStatus || d.ConvertDraft {
					t.Fatal(d)
				}
			} else {
				assertNoEffect(t, d)
				if err == nil && d.Reason != tc.reason {
					t.Fatal(d)
				}
			}
		})
	}
}
func reviewInput(t *testing.T, state string) Input {
	i := policyInput(t)
	i.Current = &Cycle{Ready: i.Facts.Transitions[0]}
	reviewer := github.Actor{ID: "REV", Login: "reviewer", Type: "User"}
	r := github.ReviewSubmission{ID: "REVIEW1", PR: i.PR.Identity, Author: &reviewer, State: state, SubmittedAt: i.Candidate.At.Add(time.Minute)}
	i.Candidate = Candidate{Kind: "review", PRID: i.PR.ID, Number: 49, Actor: reviewer, ReviewID: r.ID, State: state, At: r.SubmittedAt}
	i.Facts.Submissions = []github.ReviewSubmission{r}
	i.Facts.LatestOpinionated = []github.ReviewSubmission{r}
	return i
}
func TestReviewOutcomesAndOwnership(t *testing.T) {
	for _, state := range []string{"COMMENTED", "APPROVED", "CHANGES_REQUESTED"} {
		for _, pr := range []bool{false, true} {
			for _, target := range []bool{false, true} {
				t.Run(state+"/"+map[bool]string{true: "pr", false: "no_pr"}[pr]+"/"+map[bool]string{true: "target", false: "no_target"}[target], func(t *testing.T) {
					i := reviewInput(t, state)
					i.PROwned = pr
					i.TargetOwned = target
					before := reviewInput(t, state)
					before.PROwned = pr
					before.TargetOwned = target
					d, err := Evaluate(i)
					if err != nil {
						t.Fatal(err)
					}
					if state == "CHANGES_REQUESTED" {
						if d.Outcome != EnterRework || !d.ConvertDraft || d.PRStatus != pr || d.TargetStatus != target || d.Status != config.InProgress || d.EffectID != "REVIEW1" {
							t.Fatal(d)
						}
					} else {
						assertNoEffect(t, d)
					}
					if !reflect.DeepEqual(i, before) {
						t.Fatal("policy mutated input")
					}
				})
			}
		}
	}
}
func TestReviewRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
		want   error
	}{
		{"no_cycle", func(i *Input) { i.Current = nil }, nil},
		{"wrong_actor", func(i *Input) { i.Candidate.Actor.ID = "OTHER" }, ErrChanged},
		{"wrong_timestamp", func(i *Input) { i.Candidate.At = i.Candidate.At.Add(time.Second) }, ErrChanged},
		{"wrong_submission", func(i *Input) { i.Candidate.ReviewID = "OTHER" }, nil},
		{"other_pr", func(i *Input) { i.Facts.Submissions[0].PR.ID = "OTHER" }, ErrInvalid},
		{"superseded_opinion", func(i *Input) { i.Facts.LatestOpinionated = nil }, nil},
		{"old_review", func(i *Input) {
			i.Facts.Submissions[0].SubmittedAt = i.Current.Ready.CreatedAt
			i.Facts.LatestOpinionated = i.Facts.Submissions
			i.Candidate.At = i.Current.Ready.CreatedAt
		}, nil},
		{"new_ready", func(i *Input) {
			x := i.Current.Ready
			x.ID = "READY2"
			x.CreatedAt = i.Candidate.At.Add(time.Second)
			i.Facts.Transitions = append(i.Facts.Transitions, x)
		}, nil},
		{"draft", func(i *Input) { i.PR.Draft = true }, nil},
		{"duplicate_completed", func(i *Input) { i.Current.Rework = &i.Facts.Submissions[0]; i.Current.DraftCompleted = true }, nil},
		{"unknown_attempt", func(i *Input) { i.Current.Rework = &i.Facts.Submissions[0] }, ErrAmbiguous},
		{"pending", func(i *Input) {
			i.Facts.Submissions[0].State = "PENDING"
			i.Facts.Submissions[0].SubmittedAt = time.Time{}
			i.Facts.LatestOpinionated = nil
		}, nil},
		{"dismissed", func(i *Input) { i.Facts.Submissions[0].State = "DISMISSED"; i.Facts.LatestOpinionated = nil }, nil},
		{"unsupported", func(i *Input) { i.Facts.Submissions[0].State = "UNKNOWN" }, ErrInvalid},
		{"conflicting_connections", func(i *Input) { i.Facts.LatestOpinionated[0].State = "APPROVED" }, ErrChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := reviewInput(t, "CHANGES_REQUESTED") // Keep independent connection values.
			i.Facts.LatestOpinionated = append([]github.ReviewSubmission(nil), i.Facts.LatestOpinionated...)
			tc.mutate(&i)
			d, err := Evaluate(i)
			if !errors.Is(err, tc.want) {
				t.Fatalf("%v want %v", err, tc.want)
			}
			assertNoEffect(t, d)
		})
	}
}
