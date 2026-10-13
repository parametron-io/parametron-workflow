package prcompanion_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	"github.com/parametron-io/parametron-workflow/internal/github"
	pb "github.com/parametron-io/parametron-workflow/internal/prbinding"
	pc "github.com/parametron-io/parametron-workflow/internal/prcompanion"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func identity(repo string, n int, node string) github.Identity {
	return github.Identity{ID: node, Number: n, Repository: github.Repository{Owner: "parametron-io", Name: repo, ID: "R-" + strings.ToLower(repo)}}
}
func res(i github.Identity, kind string) storage.Resource {
	return storage.Resource{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Kind: kind, Number: int64(i.Number), NodeID: i.ID}
}
func pr(i, target github.Identity) pc.PullRequest {
	return pc.PullRequest{Resource: res(i, "pull_request"), Identity: i, State: pc.Open, Binding: pb.Decision{Outcome: pb.Accepted, Evidence: pb.Evidence{PR: i, Target: target, Kind: sp.Task, Branch: github.Branch{Repository: target.Repository, Name: fmt.Sprintf("development/issue-%d", target.Number), SHA: strings.Repeat("a", 40)}, Developer: github.Actor{ID: "U", Login: "developer", Type: "User"}, Policy: "active_org_member_repository_write_assignee_v1"}}}
}
func fixture() pc.Input {
	target := identity("engine", 48, "I48")
	return pc.Input{Primary: eo.Plan{Assignments: []eo.Assignment{{Resource: res(target, "issue"), RoadmapOrder: 10000}}}, PullRequests: []pc.PullRequest{pr(identity("engine", 48, "P48"), target)}}
}
func clone(in pc.Input) pc.Input {
	out := in
	out.Primary.Assignments = append([]eo.Assignment(nil), in.Primary.Assignments...)
	for i, a := range out.Primary.Assignments {
		if a.AnchorPhase != nil {
			p := *a.AnchorPhase
			out.Primary.Assignments[i].AnchorPhase = &p
		}
	}
	out.Primary.Clear = append([]storage.Resource(nil), in.Primary.Clear...)
	out.Primary.RoadmapRoots = append([]storage.Resource(nil), in.Primary.RoadmapRoots...)
	out.Primary.PhaseOrder = append([]storage.Resource(nil), in.Primary.PhaseOrder...)
	out.PullRequests = append([]pc.PullRequest(nil), in.PullRequests...)
	out.Owned = append([]pc.OwnedField(nil), in.Owned...)
	return out
}
func check(t *testing.T, in pc.Input, want pc.Plan) {
	t.Helper()
	before := clone(in)
	got, err := pc.Evaluate(in)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	if !reflect.DeepEqual(in, before) {
		t.Fatal("caller input mutated")
	}
	for _, a := range got.Assignments {
		for _, c := range got.Clear {
			if a.PR == c {
				t.Fatal("assignment overlaps clear")
			}
		}
	}
}
func failure(t *testing.T, in pc.Input, kind error) {
	t.Helper()
	before := clone(in)
	got, err := pc.Evaluate(in)
	if !errors.Is(err, kind) || !reflect.DeepEqual(got, pc.Plan{}) {
		t.Fatalf("got %+v, %v; want zero plan and %v", got, err, kind)
	}
	if !reflect.DeepEqual(in, before) {
		t.Fatal("failed evaluation mutated input")
	}
}
func empty() pc.Plan { return pc.Plan{Assignments: []pc.Assignment{}, Clear: []storage.Resource{}} }

func TestEmptyAndUnaccepted(t *testing.T) {
	check(t, pc.Input{}, empty())
	in := fixture()
	in.PullRequests = nil
	check(t, in, empty())
	for _, d := range []pb.Decision{{}, {Outcome: pb.Rejected, Reason: pb.MissingTarget}} {
		in = fixture()
		in.PullRequests[0].Binding = d
		check(t, in, empty())
	}
	b, _ := json.Marshal(empty())
	if string(b) != `{"Assignments":[],"Clear":[]}` {
		t.Fatal(string(b))
	}
}

func TestCapacityKindsAndExactNamespaces(t *testing.T) {
	for _, kind := range []sp.IssueType{sp.Phase, sp.Task, sp.Feature} {
		for _, base := range []int{10000, 11000, 10010, 99990} {
			if kind == sp.Phase && base%1000 != 0 {
				continue // #36 permits only Task/Feature direct children.
			}
			for _, count := range []int{0, 1, 2, 7, 9, 10} {
				t.Run(fmt.Sprintf("%s/%d/%d", kind, base, count), func(t *testing.T) {
					in := fixture()
					target := in.PullRequests[0].Binding.Evidence.Target
					in.Primary.Assignments[0].RoadmapOrder = base
					if base%1000 != 0 {
						anchor := res(identity("engine", 1, "PH"), "issue")
						in.Primary.Assignments[0].AnchorPhase = &anchor
						in.Primary.Clear = []storage.Resource{anchor}
					}
					in.PullRequests = nil
					want := empty()
					for n := count; n > 0; n-- {
						p := pr(identity("other", n, fmt.Sprintf("P%d", n)), target)
						p.Binding.Evidence.Kind = kind
						in.PullRequests = append(in.PullRequests, p)
					}
					for n := 1; n <= count; n++ {
						want.Assignments = append(want.Assignments, pc.Assignment{PR: res(identity("other", n, fmt.Sprintf("P%d", n)), "pull_request"), Target: res(target, "issue"), RoadmapOrder: base + n})
					}
					if count == 10 {
						failure(t, in, pc.ErrCapacity)
					} else {
						check(t, in, want)
					}
				})
			}
		}
	}
}

func TestAllRejectionReasonsAndLifecycleOwnership(t *testing.T) {
	// #46 distinguishes current denials from missing/unresolved/changing authority.
	reasons := []pb.Reason{pb.MissingPR, pb.InvalidTarget, pb.MissingTarget, pb.OutsideDeployment, pb.UnresolvedTarget, pb.UnsupportedTarget, pb.IneligibleTarget, pb.MissingProvenance, pb.InvalidProvenance, pb.MissingHead, pb.HeadMismatch, pb.InvalidDeveloper, pb.InactiveMembership, pb.InsufficientPermission, pb.AssigneeMismatch, pb.Changed}
	for _, reason := range reasons {
		for _, state := range []pc.State{pc.Open, pc.Closed, pc.Merged} {
			for _, owned := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%t", reason, state, owned), func(t *testing.T) {
					in := fixture()
					p := &in.PullRequests[0]
					p.State = state
					p.Binding = pb.Decision{Outcome: pb.Rejected, Reason: reason}
					want := empty()
					if owned {
						in.Owned = []pc.OwnedField{{PR: p.Identity, Target: identity("historical", 7, "OLD")}}
						if state != pc.Open || (reason != pb.MissingPR && reason != pb.UnresolvedTarget && reason != pb.Changed) {
							want.Clear = append(want.Clear, p.Resource)
						}
					}
					check(t, in, want)
				})
			}
		}
	}
	for _, state := range []pc.State{pc.Closed, pc.Merged} {
		in := fixture()
		in.PullRequests[0].State = state
		check(t, in, empty())
		in.Owned = []pc.OwnedField{{PR: in.PullRequests[0].Identity, Target: in.PullRequests[0].Binding.Evidence.Target}}
		want := empty()
		want.Clear = append(want.Clear, in.PullRequests[0].Resource)
		check(t, in, want)
	}
	in := fixture()
	in.Owned = []pc.OwnedField{{PR: in.PullRequests[0].Identity, Target: identity("old", 2, "old-target")}}
	want := empty()
	want.Assignments = []pc.Assignment{{PR: in.PullRequests[0].Resource, Target: in.Primary.Assignments[0].Resource, RoadmapOrder: 10001}}
	check(t, in, want)
}

func TestRebaseCompactionAndReopening(t *testing.T) {
	for _, child := range []bool{false, true} {
		for _, base := range []int{11000, 10000} {
			for removed := -1; removed < 3; removed++ {
				in := fixture()
				target := in.PullRequests[0].Binding.Evidence.Target
				value := base
				if child {
					value += 10
					anchor := res(identity("engine", 1, "PH"), "issue")
					in.Primary.Clear = []storage.Resource{anchor}
					in.Primary.Assignments[0].AnchorPhase = &anchor
				}
				in.Primary.Assignments[0].RoadmapOrder = value
				in.PullRequests = nil
				want := empty()
				for n := 0; n < 3; n++ {
					p := pr(identity("engine", n+1, fmt.Sprintf("P%d", n)), target)
					if n == removed {
						p.State = pc.Closed
						in.Owned = append(in.Owned, pc.OwnedField{PR: p.Identity, Target: target})
						want.Clear = append(want.Clear, p.Resource)
					} else {
						want.Assignments = append(want.Assignments, pc.Assignment{PR: p.Resource, Target: res(target, "issue"), RoadmapOrder: value + len(want.Assignments) + 1})
					}
					in.PullRequests = append(in.PullRequests, p)
				}
				check(t, in, want)
				check(t, in, want)
				if removed >= 0 {
					added := clone(in)
					p := pr(identity("engine", 4, "NEW"), target)
					added.PullRequests = append(added.PullRequests, p)
					next := pc.Plan{Assignments: append([]pc.Assignment(nil), want.Assignments...), Clear: append([]storage.Resource(nil), want.Clear...)}
					next.Assignments = append(next.Assignments, pc.Assignment{PR: p.Resource, Target: res(target, "issue"), RoadmapOrder: value + 3})
					check(t, added, next)
				}
				// Reopening/addition recomputes slots without any historical numeric input.
				if removed >= 0 {
					in.PullRequests[removed].State = pc.Open
					want = empty()
					for n, p := range in.PullRequests {
						want.Assignments = append(want.Assignments, pc.Assignment{PR: p.Resource, Target: res(target, "issue"), RoadmapOrder: value + n + 1})
					}
					check(t, in, want)
				}
			}
		}
	}
}

func TestMalformedBoundaryMatrix(t *testing.T) {
	cases := []struct {
		name string
		kind error
		edit func(*pc.Input)
	}{
		{"resource kind", pc.ErrInvalid, func(i *pc.Input) { i.PullRequests[0].Resource.Kind = "issue" }},
		{"empty node", pc.ErrInvalid, func(i *pc.Input) { i.PullRequests[0].Identity.ID = "" }},
		{"bad repo", pc.ErrInvalid, func(i *pc.Input) { i.PullRequests[0].Identity.Repository.Name = "bad/name" }},
		{"missing repo ID", pc.ErrInvalid, func(i *pc.Input) { i.PullRequests[0].Identity.Repository.ID = "" }},
		{"zero PR", pc.ErrInvalid, func(i *pc.Input) { i.PullRequests[0].Identity.Number = 0 }},
		{"negative PR", pc.ErrInvalid, func(i *pc.Input) { i.PullRequests[0].Resource.Number = -1 }},
		{"wrong node", pc.ErrInvalid, func(i *pc.Input) { i.PullRequests[0].Resource.NodeID = "wrong" }},
		{"unknown lifecycle", pc.ErrInvalid, func(i *pc.Input) { i.PullRequests[0].State = "DRAFT" }},
		{"duplicate PR", pc.ErrBinding, func(i *pc.Input) { i.PullRequests = append(i.PullRequests, i.PullRequests[0]) }},
		{"conflicting duplicate PR", pc.ErrInvalid, func(i *pc.Input) {
			p := i.PullRequests[0]
			p.Identity.ID = "other"
			p.Resource.NodeID = "other"
			i.PullRequests = append(i.PullRequests, p)
		}},
		{"accepted reason", pc.ErrBinding, func(i *pc.Input) { i.PullRequests[0].Binding.Reason = pb.Changed }},
		{"rejected evidence", pc.ErrBinding, func(i *pc.Input) {
			i.PullRequests[0].Binding.Outcome = pb.Rejected
			i.PullRequests[0].Binding.Reason = pb.MissingTarget
		}},
		{"unknown evidence", pc.ErrBinding, func(i *pc.Input) { i.PullRequests[0].Binding.Outcome = "" }},
		{"invalid reason", pc.ErrBinding, func(i *pc.Input) { i.PullRequests[0].Binding = pb.Decision{Outcome: pb.Rejected, Reason: "invented"} }},
		{"missing accepted evidence", pc.ErrBinding, func(i *pc.Input) { i.PullRequests[0].Binding.Evidence = pb.Evidence{} }},
		{"another accepted PR", pc.ErrBinding, func(i *pc.Input) { i.PullRequests[0].Binding.Evidence.PR = identity("engine", 9, "P9") }},
		{"zero target", pc.ErrBinding, func(i *pc.Input) { i.PullRequests[0].Binding.Evidence.Target.Number = 0 }},
		{"negative target", pc.ErrBinding, func(i *pc.Input) { i.PullRequests[0].Binding.Evidence.Target.Number = -1 }},
		{"target node mismatch", pc.ErrBinding, func(i *pc.Input) { i.PullRequests[0].Binding.Evidence.Target.ID = "wrong" }},
		{"node reused across kinds", pc.ErrInvalid, func(i *pc.Input) { i.PullRequests[0].Identity.ID = "I48"; i.PullRequests[0].Resource.NodeID = "I48" }},
		{"unassigned target", pc.ErrTarget, func(i *pc.Input) { i.Primary.Assignments = nil }},
		{"cleared target", pc.ErrTarget, func(i *pc.Input) {
			i.Primary.Clear = []storage.Resource{i.Primary.Assignments[0].Resource}
			i.Primary.Assignments = nil
		}},
		{"repo ID conflict", pc.ErrBinding, func(i *pc.Input) {
			i.PullRequests[0].Binding.Evidence.Target.Repository.ID = "wrong"
			i.PullRequests[0].Binding.Evidence.Branch.Repository.ID = "wrong"
		}},
		{"repo ID reused", pc.ErrBinding, func(i *pc.Input) {
			e := &i.PullRequests[0].Binding.Evidence
			e.Target.Repository.Name = "other"
			e.Branch.Repository = e.Target.Repository
		}},
		{"duplicate ownership", pc.ErrOwnership, func(i *pc.Input) {
			o := pc.OwnedField{PR: i.PullRequests[0].Identity, Target: i.PullRequests[0].Binding.Evidence.Target}
			i.Owned = []pc.OwnedField{o, o}
		}},
		{"missing owned PR", pc.ErrOwnership, func(i *pc.Input) {
			i.Owned = []pc.OwnedField{{PR: identity("engine", 99, "P99"), Target: identity("engine", 48, "I48")}}
		}},
		{"invalid owned PR", pc.ErrOwnership, func(i *pc.Input) { i.Owned = []pc.OwnedField{{Target: identity("engine", 48, "I48")}} }},
		{"forged owned target node", pc.ErrOwnership, func(i *pc.Input) {
			i.Owned = []pc.OwnedField{{PR: i.PullRequests[0].Identity, Target: identity("engine", 48, "forged")}}
		}},
		{"owned repo conflict", pc.ErrOwnership, func(i *pc.Input) {
			p := i.PullRequests[0].Identity
			p.Repository.ID = "wrong"
			i.Owned = []pc.OwnedField{{PR: p, Target: i.PullRequests[0].Binding.Evidence.Target}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { in := fixture(); tc.edit(&in); failure(t, in, tc.kind) })
	}
	for _, field := range []string{"policy", "kind", "developer", "branch repo", "branch name", "branch SHA", "target node"} {
		t.Run(field, func(t *testing.T) {
			in := fixture()
			e := &in.PullRequests[0].Binding.Evidence
			switch field {
			case "policy":
				e.Policy = ""
			case "kind":
				e.Kind = sp.Bug
			case "developer":
				e.Developer.Type = "Bot"
			case "branch repo":
				e.Branch.Repository.ID = "wrong"
			case "branch name":
				e.Branch.Name = "bad name"
			case "branch SHA":
				e.Branch.SHA = "bad"
			case "target node":
				e.Target.ID = ""
			}
			failure(t, in, pc.ErrBinding)
		})
	}
}

func TestPrimaryContradictions(t *testing.T) {
	cases := []struct {
		name string
		edit func(*eo.Plan)
	}{
		{"duplicate", func(p *eo.Plan) { p.Assignments = append(p.Assignments, p.Assignments[0]) }},
		{"same resource conflicting value", func(p *eo.Plan) {
			a := p.Assignments[0]
			a.RoadmapOrder = 11000
			p.Assignments = append(p.Assignments, a)
		}},
		{"same value", func(p *eo.Plan) {
			a := p.Assignments[0]
			a.Resource = res(identity("other", 9, "I9"), "issue")
			p.Assignments = append(p.Assignments, a)
		}},
		{"conflicting node", func(p *eo.Plan) { p.Clear = []storage.Resource{p.Assignments[0].Resource}; p.Clear[0].NodeID = "other" }},
		{"reused node", func(p *eo.Plan) { p.Clear = []storage.Resource{res(identity("other", 9, "I48"), "issue")} }},
		{"kind", func(p *eo.Plan) { p.Assignments[0].Resource.Kind = "pull_request" }},
		{"number", func(p *eo.Plan) { p.Assignments[0].Resource.Number = 0 }},
		{"identity", func(p *eo.Plan) { p.Assignments[0].Resource.Owner = "bad/name" }},
		{"low", func(p *eo.Plan) { p.Assignments[0].RoadmapOrder = 9000 }},
		{"high", func(p *eo.Plan) { p.Assignments[0].RoadmapOrder = 100000 }},
		{"reserved", func(p *eo.Plan) { p.Assignments[0].RoadmapOrder = 10001 }},
		{"root stride", func(p *eo.Plan) { p.Assignments[0].RoadmapOrder = 10010 }},
		{"child stride", func(p *eo.Plan) {
			a := res(identity("engine", 1, "PH"), "issue")
			p.Clear = []storage.Resource{a}
			p.Assignments[0].AnchorPhase = &a
			p.Assignments[0].RoadmapOrder = 10011
		}},
		{"missing anchor", func(p *eo.Plan) {
			a := res(identity("engine", 1, "PH"), "issue")
			p.Assignments[0].AnchorPhase = &a
			p.Assignments[0].RoadmapOrder = 10010
		}},
		{"self anchor", func(p *eo.Plan) {
			p.Assignments[0].AnchorPhase = &p.Assignments[0].Resource
			p.Assignments[0].RoadmapOrder = 10010
		}},
		{"assignment and clear", func(p *eo.Plan) { p.Clear = []storage.Resource{p.Assignments[0].Resource} }},
		{"duplicate clear", func(p *eo.Plan) { a := res(identity("engine", 1, "PH"), "issue"); p.Clear = []storage.Resource{a, a} }},
		{"anchor wrong segment", func(p *eo.Plan) {
			a := res(identity("engine", 1, "PH"), "issue")
			p.Assignments = append(p.Assignments, eo.Assignment{Resource: a, RoadmapOrder: 11000})
			p.Assignments[0].AnchorPhase = &a
			p.Assignments[0].RoadmapOrder = 10010
		}},
		{"same anchor two segments", func(p *eo.Plan) {
			a := res(identity("engine", 1, "PH"), "issue")
			p.Clear = []storage.Resource{a}
			p.Assignments[0].AnchorPhase = &a
			p.Assignments[0].RoadmapOrder = 10010
			p.Assignments = append(p.Assignments, eo.Assignment{Resource: res(identity("other", 2, "I2"), "issue"), RoadmapOrder: 11010, AnchorPhase: &a})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { in := fixture(); tc.edit(&in.Primary); failure(t, in, pc.ErrPrimary) })
	}
}

// #36 gives each retained root its own segment, even when the closed Phase
// appears only in Clear. This contradiction is visible without rebuilding topology.
func TestDistinctRetainedAnchorsCannotShareSegment(t *testing.T) {
	for _, activeRoot := range []bool{false, true} {
		t.Run(fmt.Sprint(activeRoot), func(t *testing.T) {
			in := fixture()
			a := res(identity("engine", 1, "PH1"), "issue")
			b := res(identity("other", 1, "PH2"), "issue")
			in.Primary.Assignments[0].RoadmapOrder = 10010
			in.Primary.Assignments[0].AnchorPhase = &a
			in.Primary.Clear = []storage.Resource{a, b}
			if activeRoot {
				in.Primary.Clear = []storage.Resource{a}
				in.Primary.Assignments = append(in.Primary.Assignments, eo.Assignment{Resource: b, RoadmapOrder: 10000})
			} else {
				in.Primary.Assignments = append(in.Primary.Assignments, eo.Assignment{Resource: res(identity("other", 2, "I2"), "issue"), RoadmapOrder: 10020, AnchorPhase: &b})
			}
			failure(t, in, pc.ErrPrimary)
		})
	}
}

func TestCanonicalBytesAndImmutableInputs(t *testing.T) {
	in := fixture()
	target := in.PullRequests[0].Binding.Evidence.Target
	child := identity("other", 48, "CH48")
	anchor := in.Primary.Assignments[0].Resource
	in.Primary.Assignments = append(in.Primary.Assignments, eo.Assignment{Resource: res(child, "issue"), RoadmapOrder: 10010, AnchorPhase: &anchor})
	in.Primary.Clear = []storage.Resource{res(identity("other", 9, "OLD9"), "issue"), res(identity("engine", 9, "OLD10"), "issue")}
	in.Primary.RoadmapRoots = []storage.Resource{anchor}
	in.Primary.PhaseOrder = []storage.Resource{anchor}
	in.PullRequests = []pc.PullRequest{pr(identity("Beta", 2, "P2"), target), pr(identity("alpha", 2, "P3"), target), pr(identity("Alpha", 1, "P1"), target), pr(identity("other", 48, "CP"), child)}
	foreign := identity("zeta", 1, "P0")
	foreign.Repository.Owner = "another-owner"
	in.PullRequests = append(in.PullRequests, pr(foreign, target))
	want := empty()
	for _, x := range []struct{ index, value int }{{4, 10001}, {2, 10002}, {1, 10003}, {0, 10004}, {3, 10011}} {
		p := in.PullRequests[x.index]
		want.Assignments = append(want.Assignments, pc.Assignment{PR: p.Resource, Target: res(p.Binding.Evidence.Target, "issue"), RoadmapOrder: x.value})
	}
	for n, state := range []pc.State{pc.Closed, pc.Merged, pc.Open, pc.Open, pc.Open} {
		p := pr(identity("engine", 80+n, fmt.Sprintf("terminal%d", n)), target)
		p.State = state
		p.Binding = pb.Decision{Outcome: pb.Rejected, Reason: pb.MissingTarget}
		if n == 3 {
			p.Binding = pb.Decision{}
		}
		in.PullRequests = append(in.PullRequests, p)
		if n < 4 {
			in.Owned = append(in.Owned, pc.OwnedField{PR: p.Identity, Target: identity("old", 1, "OLD")})
		}
		if n < 3 {
			want.Clear = append(want.Clear, p.Resource)
		}
	}
	in.Owned = append(in.Owned, pc.OwnedField{PR: in.PullRequests[0].Identity, Target: identity("old", 2, "OLD2")})
	original := clone(in)
	expected, _ := json.Marshal(want)
	for seed := int64(0); seed < 25; seed++ {
		shuffled := clone(in)
		r := rand.New(rand.NewSource(seed))
		r.Shuffle(len(shuffled.Primary.Assignments), func(a, b int) {
			shuffled.Primary.Assignments[a], shuffled.Primary.Assignments[b] = shuffled.Primary.Assignments[b], shuffled.Primary.Assignments[a]
		})
		r.Shuffle(len(shuffled.Primary.Clear), func(a, b int) {
			shuffled.Primary.Clear[a], shuffled.Primary.Clear[b] = shuffled.Primary.Clear[b], shuffled.Primary.Clear[a]
		})
		r.Shuffle(len(shuffled.PullRequests), func(a, b int) {
			shuffled.PullRequests[a], shuffled.PullRequests[b] = shuffled.PullRequests[b], shuffled.PullRequests[a]
		})
		r.Shuffle(len(shuffled.Owned), func(a, b int) { shuffled.Owned[a], shuffled.Owned[b] = shuffled.Owned[b], shuffled.Owned[a] })
		check(t, shuffled, want)
		got, err := pc.Evaluate(shuffled)
		if err != nil {
			t.Fatal(err)
		}
		actual, _ := json.Marshal(got)
		if string(actual) != string(expected) {
			t.Fatalf("seed %d: %s != %s", seed, actual, expected)
		}
	}
	if !reflect.DeepEqual(in, original) {
		t.Fatal("shared nested input modified")
	}
}

func TestMultipleTargetsRebaseIndependently(t *testing.T) {
	for _, values := range [][2]int{{11010, 12000}, {10010, 11000}} {
		in := fixture()
		first := in.PullRequests[0].Binding.Evidence.Target
		second := identity("other", 48, "OTHER48")
		anchor := res(identity("engine", 1, "PH"), "issue")
		in.Primary.Assignments[0].RoadmapOrder = values[0]
		in.Primary.Assignments[0].AnchorPhase = &anchor
		in.Primary.Clear = []storage.Resource{anchor}
		in.Primary.Assignments = append(in.Primary.Assignments, eo.Assignment{Resource: res(second, "issue"), RoadmapOrder: values[1]})
		in.PullRequests = []pc.PullRequest{pr(identity("engine", 1, "PA"), first), pr(identity("other", 1, "PB"), first), pr(identity("engine", 2, "PC"), second), pr(identity("other", 2, "PD"), second)}
		want := empty()
		for n, p := range in.PullRequests {
			want.Assignments = append(want.Assignments, pc.Assignment{PR: p.Resource, Target: res(p.Binding.Evidence.Target, "issue"), RoadmapOrder: values[n/2] + n%2 + 1})
		}
		check(t, in, want)
	}
}

func TestCaseNormalizationAndUnknownOwned(t *testing.T) {
	in := fixture()
	p := &in.PullRequests[0]
	p.Resource.Owner = "PARAMETRON-IO"
	p.Resource.Repository = "ENGINE"
	p.Identity.Repository.Owner = "Parametron-IO"
	p.Identity.Repository.Name = "Engine"
	p.Binding.Evidence.PR = p.Identity
	p.Binding.Evidence.Target.Repository.Owner = "PARAMETRON-IO"
	p.Binding.Evidence.Target.Repository.Name = "ENGINE"
	p.Binding.Evidence.Branch.Repository = p.Binding.Evidence.Target.Repository
	in.Primary.Assignments[0].Resource.Owner = "PARAMETRON-IO"
	in.Primary.Assignments[0].Resource.Repository = "ENGINE"
	in.Owned = []pc.OwnedField{{PR: p.Identity, Target: identity("historical", 1, "OLD")}}
	want := empty()
	want.Assignments = []pc.Assignment{{PR: res(p.Identity, "pull_request"), Target: res(p.Binding.Evidence.Target, "issue"), RoadmapOrder: 10001}}
	check(t, in, want)
	p.Binding = pb.Decision{}
	check(t, in, empty())
}

func TestLateFailuresReturnNoPartialPlan(t *testing.T) {
	for _, kind := range []error{pc.ErrInvalid, pc.ErrBinding, pc.ErrTarget, pc.ErrOwnership, pc.ErrCapacity, pc.ErrPrimary} {
		t.Run(kind.Error(), func(t *testing.T) {
			in := fixture()
			target := in.PullRequests[0].Binding.Evidence.Target
			terminal := pr(identity("engine", 2, "CLOSED"), target)
			terminal.State = pc.Closed
			terminal.Binding = pb.Decision{}
			in.PullRequests = append(in.PullRequests, terminal)
			in.Owned = []pc.OwnedField{{PR: terminal.Identity, Target: target}}
			other := identity("other", 48, "OTHER")
			in.Primary.Assignments = append(in.Primary.Assignments, eo.Assignment{Resource: res(other, "issue"), RoadmapOrder: 11000})
			in.PullRequests = append(in.PullRequests, pr(identity("other", 1, "OTHERPR"), other))
			switch kind {
			case pc.ErrInvalid:
				p := pr(identity("other", 3, "BAD"), other)
				p.State = ""
				in.PullRequests = append(in.PullRequests, p)
			case pc.ErrBinding:
				p := pr(identity("other", 3, "BAD"), other)
				p.Binding.Reason = pb.Changed
				in.PullRequests = append(in.PullRequests, p)
			case pc.ErrTarget:
				in.PullRequests = append(in.PullRequests, pr(identity("other", 3, "BAD"), identity("third", 1, "UNASSIGNED")))
			case pc.ErrOwnership:
				in.Owned = append(in.Owned, pc.OwnedField{PR: identity("other", 99, "ABSENT"), Target: other})
			case pc.ErrCapacity:
				for n := 2; n <= 10; n++ {
					in.PullRequests = append(in.PullRequests, pr(identity("other", n, fmt.Sprintf("EXTRA%d", n)), other))
				}
			case pc.ErrPrimary:
				in.Primary.Assignments = append(in.Primary.Assignments, in.Primary.Assignments[1])
			}
			failure(t, in, kind)
		})
	}
}
