package workflowfeedback

import (
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/prdevelopment"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"strings"
	"testing"
	"time"
)

func developmentFixture(t *testing.T) prdevelopment.Input {
	t.Helper()
	r := feedbackResource("pull_request")
	target := r.Identity
	target.ID = "I"
	target.Number = 48
	dev := github.Actor{ID: "U", Login: "dev", Type: "User"}
	name, e := eb.Name(res(target, "issue"))
	if e != nil {
		t.Fatal(e)
	}
	b := prbinding.Evidence{PR: r.Identity, Target: target, Kind: sp.Task, Developer: dev, Branch: github.Branch{Repository: target.Repository, Name: name, SHA: strings.Repeat("a", 40)}, Policy: "active_org_member_repository_write_assignee_v1"}
	return prdevelopment.Input{Binding: prbinding.Decision{Outcome: prbinding.Accepted, Evidence: b}, Onboarding: pronboarding.Completion{Completed: true, Resource: res(r.Identity, r.Kind), Binding: b, Project: "ENG", DeliveryID: "delivery", CompletedAt: time.Unix(100, 0)}, Project: "ENG", PR: github.PullRequest{Identity: r.Identity, State: "OPEN", Body: "Target: #48", Author: &dev, HeadRepository: target.Repository, HeadRef: name, HeadSHA: b.Branch.SHA}, Target: github.Issue{Identity: target, State: "OPEN"}, References: github.DevelopmentReferences{PR: r.Identity, Complete: true}}
}
func TestDevelopmentCorrectionRequiresValidatedRemoval(t *testing.T) {
	for _, variant := range []string{"one", "several", "missing link", "rejected", "changed", "onboarding", "incomplete", "keyword", "ambiguous", "unrelated action", "wrong resource", "wrong target"} {
		t.Run(variant, func(t *testing.T) {
			in := developmentFixture(t)
			r := feedbackResource("pull_request")
			other := in.Target.Identity
			other.ID = "OTHER"
			other.Number = 49
			in.References.Manual = []github.Identity{other}
			a := prdevelopment.Action{Target: other}
			valid := variant == "one" || variant == "several"
			switch variant {
			case "several":
				second := other
				second.ID = "SECOND"
				second.Number = 50
				in.References.Manual = append(in.References.Manual, second)
			case "missing link":
				in.References.Manual = nil
				a = prdevelopment.Action{Add: true, Target: in.Target.Identity}
			case "rejected":
				in.Binding = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingTarget}
			case "changed":
				in.Binding.Reason = prbinding.Changed
			case "onboarding":
				in.Onboarding.Completed = false
			case "incomplete":
				in.References.Complete = false
			case "keyword":
				in.References.Manual = nil
				in.References.Keyword = []github.Identity{other}
			case "ambiguous":
				in.References.Manual = append(in.References.Manual, other)
			case "unrelated action":
				a.Target.Number = 99
				a.Target.ID = "ABSENT"
			case "wrong resource":
				r.Identity.Number++
			case "wrong target":
				in.Target.Identity.ID = "BAD"
			}
			v, e := DevelopmentCorrection(r, in, a)
			if valid {
				actions, pe := prdevelopment.Evaluate(in)
				if pe != nil || len(actions) < 2 || e != nil || v.related != in.Target.Identity {
					t.Fatal(actions, pe, v, e)
				}
			} else if e == nil {
				t.Fatal("non-authoritative correction accepted", v)
			}
		})
	}
}
