package workflowfeedback

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	"github.com/parametron-io/parametron-workflow/internal/engineeringterminal"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/prcompanion"
	"github.com/parametron-io/parametron-workflow/internal/prdevelopment"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
)

func feedbackResource(kind string) Resource {
	return Resource{Identity: github.Identity{ID: "PR", Number: 80, Repository: github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}}, Kind: kind}
}
func TestBindingAuthority(t *testing.T) {
	r := feedbackResource("pull_request")
	groups := map[Category][]prbinding.Reason{
		BindingTarget:    {prbinding.InvalidTarget, prbinding.MissingTarget, prbinding.OutsideDeployment, prbinding.UnsupportedTarget, prbinding.IneligibleTarget},
		BindingBranch:    {prbinding.MissingProvenance, prbinding.InvalidProvenance, prbinding.HeadMismatch, prbinding.MissingHead},
		BindingDeveloper: {prbinding.InvalidDeveloper, prbinding.InactiveMembership, prbinding.InsufficientPermission, prbinding.AssigneeMismatch},
	}
	for category, reasons := range groups {
		for _, reason := range reasons {
			t.Run(string(reason), func(t *testing.T) {
				d := prbinding.Decision{Outcome: prbinding.Rejected, Reason: reason}
				v, e := BindingRejection(r, d)
				if e != nil || v.category != category || v.resource != r {
					t.Fatal(v, e)
				}
				d.Outcome = prbinding.Accepted
				if _, e = BindingRejection(r, d); !errors.Is(e, ErrInvalid) {
					t.Fatal(e)
				}
				d.Outcome = prbinding.Rejected
				d.Evidence.PR = r.Identity
				if _, e = BindingRejection(r, d); !errors.Is(e, ErrInvalid) {
					t.Fatal(e)
				}
				bad := r
				bad.Kind = "issue"
				if _, e = BindingRejection(bad, prbinding.Decision{Outcome: prbinding.Rejected, Reason: reason}); !errors.Is(e, ErrInvalid) {
					t.Fatal(e)
				}
			})
		}
	}
	for _, reason := range []prbinding.Reason{prbinding.Changed, prbinding.UnresolvedTarget, prbinding.MissingPR, "", "transport failure secret"} {
		_, e := BindingRejection(r, prbinding.Decision{Outcome: prbinding.Rejected, Reason: reason})
		want := ErrStale
		if reason == "" || reason == "transport failure secret" {
			want = ErrInvalid
		}
		if !errors.Is(e, want) {
			t.Fatal(reason, e)
		}
	}
}
func capacityInput(n int) prcompanion.Input {
	r := feedbackResource("pull_request")
	target := r.Identity
	target.ID = "I"
	target.Number = 48
	in := prcompanion.Input{Primary: eo.Plan{Assignments: []eo.Assignment{{Resource: res(target, "issue"), RoadmapOrder: 10000}}}}
	for j := 0; j < n; j++ {
		p := r.Identity
		p.ID = fmt.Sprintf("P%d", j)
		p.Number = 80 + j
		in.PullRequests = append(in.PullRequests, prcompanion.PullRequest{Resource: res(p, "pull_request"), Identity: p, State: prcompanion.Open, Binding: prbinding.Decision{Outcome: prbinding.Accepted, Evidence: prbinding.Evidence{PR: p, Target: target, Kind: sp.Task, Developer: github.Actor{ID: "U", Login: "dev", Type: "User"}, Branch: github.Branch{Repository: target.Repository, Name: "development/issue-48", SHA: strings.Repeat("a", 40)}, Policy: "active_org_member_repository_write_assignee_v1"}}})
	}
	return in
}
func TestCapacityUsesExactProducerAndTarget(t *testing.T) {
	for _, n := range []int{9, 10} {
		in := capacityInput(n)
		r := Resource{Identity: in.PullRequests[0].Identity, Kind: "pull_request"}
		_, pe := prcompanion.Evaluate(in)
		v, e := CapacityRejection(r, in)
		if n == 9 {
			if pe != nil || !errors.Is(e, ErrInvalid) {
				t.Fatal(pe, e)
			}
		} else if !errors.Is(pe, prcompanion.ErrCapacity) || e != nil || v.detail != "10" || v.related != in.PullRequests[0].Binding.Evidence.Target {
			t.Fatal(v, pe, e)
		}
	}
	for _, variant := range []string{"missing", "closed", "wrong resource", "malformed", "other target", "cross repository"} {
		t.Run(variant, func(t *testing.T) {
			in := capacityInput(10)
			r := Resource{Identity: in.PullRequests[0].Identity, Kind: "pull_request"}
			switch variant {
			case "missing":
				r.Identity.ID = "absent"
			case "closed":
				in.PullRequests[0].State = prcompanion.Closed
			case "wrong resource":
				r.Kind = "issue"
			case "malformed":
				in.PullRequests[0].Binding.Evidence.Policy = "bad"
			case "other target":
				other := in.PullRequests[0].Binding.Evidence.Target
				other.ID = "OTHER"
				other.Number = 49
				in.Primary.Assignments = append(in.Primary.Assignments, eo.Assignment{Resource: res(other, "issue"), RoadmapOrder: 11000})
				p := in.PullRequests[0]
				p.Identity.ID = "EXTRA"
				p.Identity.Number = 100
				p.Resource = res(p.Identity, "pull_request")
				p.Binding.Evidence.PR = p.Identity
				p.Binding.Evidence.Target = other
				in.PullRequests = append(in.PullRequests, p)
				r.Identity = p.Identity
			case "cross repository":
				r.Identity.Repository.ID = "OTHER"
			}
			if _, e := CapacityRejection(r, in); e == nil {
				t.Fatal("invalid capacity authority accepted")
			}
		})
	}
}
func TestClosureAndLifecycleAuthority(t *testing.T) {
	r := feedbackResource("issue")
	d := engineeringterminal.Rejection{Resource: res(r.Identity, r.Kind), Reason: engineeringterminal.OpenChildren}
	if _, e := ClosureRejection(r, d); e != nil {
		t.Fatal(e)
	}
	for _, variant := range []string{"wrong identity", "wrong kind", "missing reason", "unknown reason"} {
		bad := d
		rr := r
		switch variant {
		case "wrong identity":
			bad.Resource.Number++
		case "wrong kind":
			rr.Kind = "pull_request"
		case "missing reason":
			bad.Reason = ""
		case "unknown reason":
			bad.Reason = "Done"
		}
		if _, e := ClosureRejection(rr, bad); !errors.Is(e, ErrInvalid) {
			t.Fatal(variant, e)
		}
	}
	for _, reason := range []LifecycleReason{ActiveBlocker, InactiveParent, MissingExecutionAuthorization, "", "Done", "execution failure"} {
		v, e := LifecycleRejection(ManualRejection{r, reason})
		if reason == ActiveBlocker || reason == InactiveParent || reason == MissingExecutionAuthorization {
			if e != nil || v.category != Lifecycle {
				t.Fatal(v, e)
			}
		} else if !errors.Is(e, ErrInvalid) {
			t.Fatal(e)
		}
	}
	r.Kind = "pull_request"
	if _, e := LifecycleRejection(ManualRejection{r, ActiveBlocker}); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
}
func TestCanonicalRenderingPermutationAndIsolation(t *testing.T) {
	r := feedbackResource("pull_request")
	a, _ := BindingRejection(r, prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingTarget})
	b, _ := BindingRejection(r, prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.HeadMismatch})
	c, _ := BindingRejection(r, prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.InvalidTarget})
	expected := Marker + "\n### Workflow feedback\n\n" +
		"- PR binding was rejected (invalid_target_directive). The Target must identify an eligible Engineering Task, Feature, or explicitly branch-enabled Phase. Correct the `Target:` directive and satisfy the accepted development branch and author contract.\n" +
		"- PR binding was rejected (missing_target_directive). The Target must identify an eligible Engineering Task, Feature, or explicitly branch-enabled Phase. Correct the `Target:` directive and satisfy the accepted development branch and author contract.\n" +
		"- PR branch binding was rejected (head_mismatch). The head must match the current controller-authorized development branch with verified creation provenance. Use the accepted Target's authorized branch and satisfy the branch and author contract.\n"
	rng := rand.New(rand.NewSource(42))
	for j := 0; j < 100; j++ {
		vs := []Violation{a, b, c, a}
		rng.Shuffle(len(vs), func(i, j int) { vs[i], vs[j] = vs[j], vs[i] })
		before := append([]Violation(nil), vs...)
		got, e := Render(Current{r, true, vs})
		if e != nil || got.Body != expected || !got.Active || !reflect.DeepEqual(vs, before) {
			t.Fatal(got, e)
		}
		if strings.Count(got.Body, Marker) != 1 {
			t.Fatal(got)
		}
	}
	for _, c := range []Current{{r, false, nil}, {Resource{}, true, nil}, {r, true, make([]Violation, 257)}, {r, true, []Violation{{resource: r, category: 99}}}} {
		if _, e := Render(c); !errors.Is(e, ErrInvalid) {
			t.Fatal(c, e)
		}
	}
	other := a
	other.resource.Identity.Number++
	if _, e := Render(Current{r, true, []Violation{other}}); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	vs := make([]Violation, 256)
	for i := range vs {
		vs[i] = a
	}
	if _, e := Render(Current{r, true, vs}); e != nil {
		t.Fatal(e)
	}
	resolved, e := Render(Current{Resource: r, Complete: true})
	if e != nil || resolved.Active || !strings.Contains(resolved.Body, "No current rejection") {
		t.Fatal(resolved, e)
	}
}
func TestRenderRejectsMalformedInternalValues(t *testing.T) {
	r := feedbackResource("pull_request")
	for _, v := range []Violation{
		{resource: r, category: BindingTarget, detail: "<script>secret</script>"},
		{resource: r, category: BindingBranch, detail: string(prbinding.MissingTarget)},
		{resource: r, category: CompanionCapacity, detail: "10"},
		{resource: r, category: PhaseClosure},
		{resource: r, category: Development},
	} {
		if _, e := Render(Current{r, true, []Violation{v}}); !errors.Is(e, ErrInvalid) {
			t.Errorf("malformed value rendered: %+v error=%v", v, e)
		}
	}
}

func TestEveryCategoryRendersActionableBoundedContent(t *testing.T) {
	pr := feedbackResource("pull_request")
	issue := feedbackResource("issue")
	var violations []Violation
	for _, reason := range []prbinding.Reason{prbinding.MissingTarget, prbinding.MissingHead, prbinding.InvalidDeveloper} {
		v, e := BindingRejection(pr, prbinding.Decision{Outcome: prbinding.Rejected, Reason: reason})
		if e != nil {
			t.Fatal(e)
		}
		violations = append(violations, v)
	}
	in := capacityInput(10)
	capacity, e := CapacityRejection(Resource{Identity: in.PullRequests[0].Identity, Kind: "pull_request"}, in)
	if e != nil {
		t.Fatal(e)
	}
	violations = append(violations, capacity)
	dev := developmentFixture(t)
	other := dev.Target.Identity
	other.ID = "OTHER"
	other.Number = 49
	dev.References.Manual = []github.Identity{other}
	v, e := DevelopmentCorrection(pr, dev, prdevelopment.Action{Target: other})
	if e != nil {
		t.Fatal(e)
	}
	violations = append(violations, v)
	for _, reason := range []LifecycleReason{ActiveBlocker, InactiveParent, MissingExecutionAuthorization} {
		v, e := LifecycleRejection(ManualRejection{issue, reason})
		if e != nil {
			t.Fatal(e)
		}
		violations = append(violations, v)
	}
	closure, e := ClosureRejection(issue, engineeringterminal.Rejection{Resource: res(issue.Identity, "issue"), Reason: engineeringterminal.OpenChildren})
	if e != nil {
		t.Fatal(e)
	}
	violations = append(violations, closure)
	for _, v := range violations {
		c, e := Render(Current{v.resource, true, []Violation{v}})
		if e != nil || !c.Active || len(c.Body) > 60000 || !strings.HasPrefix(c.Body, Marker+"\n") {
			t.Fatal(c, e)
		}
		if !strings.Contains(c.Body, "rejected") && !strings.Contains(c.Body, "correction") {
			t.Fatal("action missing", c.Body)
		}
		if !strings.Contains(c.Body, "before") && !strings.Contains(c.Body, "Correct") && !strings.Contains(c.Body, "Use") && !strings.Contains(c.Body, "Satisfy") && !strings.Contains(c.Body, "Reduce") {
			t.Fatal("valid next action missing", c.Body)
		}
	}
	// Many distinct valid details exceed the body bound without exceeding input count.
	vs := make([]Violation, 256)
	for i := range vs {
		v := capacity
		v.related.Number = i + 1
		v.related.ID = fmt.Sprintf("I%d", i)
		vs[i] = v
	}
	if _, e := Render(Current{capacity.resource, true, vs}); !errors.Is(e, ErrInvalid) {
		t.Fatal("oversized body accepted", e)
	}
}
func TestCategoryOrderAndMalformedBounds(t *testing.T) {
	r := feedbackResource("pull_request")
	related := r.Identity
	related.ID = "I"
	related.Number = 48
	vs := []Violation{{resource: r, category: Development, related: related}, {resource: r, category: CompanionCapacity, detail: "10", related: related}}
	for _, reason := range []prbinding.Reason{prbinding.InvalidDeveloper, prbinding.MissingHead, prbinding.MissingTarget} {
		v, _ := BindingRejection(r, prbinding.Decision{Outcome: prbinding.Rejected, Reason: reason})
		vs = append(vs, v)
	}
	got, e := canonical(Current{r, true, vs})
	if e != nil {
		t.Fatal(e)
	}
	for i, c := range []Category{BindingTarget, BindingBranch, BindingDeveloper, CompanionCapacity, Development} {
		if got[i].category != c {
			t.Fatal(got)
		}
	}
	for _, n := range []string{"9", "10001", "010", "-1", "<b>10</b>"} {
		v := Violation{resource: r, category: CompanionCapacity, detail: n, related: related}
		if _, e := Render(Current{r, true, []Violation{v}}); !errors.Is(e, ErrInvalid) {
			t.Fatal(n, e)
		}
	}
}

func TestCapacityIndependentTargetsAndRepositories(t *testing.T) {
	in := capacityInput(10)
	other := capacityInput(10)
	target := other.PullRequests[0].Binding.Evidence.Target
	target.Repository = github.Repository{ID: "OTHER-R", Owner: "parametron-io", Name: "other"}
	target.ID = "OTHER-I"
	other.Primary.Assignments[0].Resource = res(target, "issue")
	other.Primary.Assignments[0].RoadmapOrder = 11000
	for j := range other.PullRequests {
		p := &other.PullRequests[j]
		p.Identity.Repository = target.Repository
		p.Identity.ID = fmt.Sprintf("OTHER-P%d", j)
		p.Resource = res(p.Identity, "pull_request")
		p.Binding.Evidence.PR = p.Identity
		p.Binding.Evidence.Target = target
		p.Binding.Evidence.Branch.Repository = target.Repository
	}
	in.Primary.Assignments = append(in.Primary.Assignments, other.Primary.Assignments...)
	in.PullRequests = append(in.PullRequests, other.PullRequests...)
	if _, e := prcompanion.Evaluate(in); !errors.Is(e, prcompanion.ErrCapacity) {
		t.Fatal(e)
	}
	for _, index := range []int{0, 10} {
		p := in.PullRequests[index]
		v, e := CapacityRejection(Resource{Identity: p.Identity, Kind: "pull_request"}, in)
		if e != nil || v.detail != "10" || v.related != p.Binding.Evidence.Target {
			t.Fatal(v, e)
		}
	}
	// An independent non-full Target cannot borrow the other Target's failure.
	in.PullRequests = in.PullRequests[:11]
	p := in.PullRequests[10]
	if _, e := CapacityRejection(Resource{Identity: p.Identity, Kind: "pull_request"}, in); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
}
