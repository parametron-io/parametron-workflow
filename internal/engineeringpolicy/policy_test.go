package engineeringpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"

	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func node(repo string, number int64, typ sp.IssueType) ec.IssueNode {
	return ec.IssueNode{Resource: storage.Resource{Owner: "parametron-io", Repository: repo, Kind: "issue", Number: number, NodeID: fmt.Sprintf("%s-%d", repo, number)}, State: ec.Open, Type: typ, TypeSource: ec.AcceptedSemantic, AcceptedClassification: &sp.IssueClassification{Type: typ, Labels: []sp.Label{}, Priority: sp.Medium, Effort: sp.M}}
}
func input(nodes ...ec.IssueNode) Input {
	in := Input{Context: ec.Context{Root: nodes[0].Resource, Nodes: nodes}}
	for _, n := range nodes {
		if n.Type == sp.Phase {
			in.PhaseOrder = append(in.PhaseOrder, n.Resource)
		}
	}
	return in
}
func parent(in *Input, child, parent int) {
	c, p := &in.Context.Nodes[child], in.Context.Nodes[parent]
	c.Intent.Parent = &intent.IssueRef{Repository: p.Resource.Repository, Number: p.Resource.Number}
	in.Context.Parents = append(in.Context.Parents, ec.ParentEdge{Child: c.Resource, Parent: p.Resource})
}
func dependency(in *Input, blocker, blocked int) {
	in.Context.Dependencies = append(in.Context.Dependencies, ec.DependencyEdge{Blocker: in.Context.Nodes[blocker].Resource, Blocked: in.Context.Nodes[blocked].Resource})
}
func manual(n *ec.IssueNode) {
	n.TypeSource = ec.ManualNative
	n.AcceptedClassification = nil
	n.Intent.Explicit.Classification = intent.Boolean{Value: false, Explicit: true}
}
func evaluate(t *testing.T, in Input) Plan {
	t.Helper()
	p, err := Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func decision(t *testing.T, p Plan, r storage.Resource) Decision {
	t.Helper()
	for _, d := range p.Decisions {
		if d.Resource == r {
			return d
		}
	}
	t.Fatalf("missing decision for %v", r)
	return Decision{}
}

func TestPhasePolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(*Input)
		statuses []Status
		active   []bool
	}{
		{"single", func(in *Input) { in.Context.Nodes = in.Context.Nodes[:1]; in.PhaseOrder = in.PhaseOrder[:1] }, []Status{Ready}, []bool{true}},
		{"later backlog", func(*Input) {}, []Status{Ready, Backlog}, []bool{true, false}},
		{"earlier blocked", func(in *Input) { dependency(in, 2, 0) }, []Status{Blocked, Ready}, []bool{false, true}},
		{"closed blocker", func(in *Input) { dependency(in, 2, 0); in.Context.Nodes[2].State = ec.Closed }, []Status{Ready, Backlog}, []bool{true, false}},
		{"closed phase", func(in *Input) { in.Context.Nodes[0].State = ec.Closed }, []Status{"", Ready}, []bool{false, true}},
		{"two repositories", func(in *Input) {
			in.Context.Nodes[1] = node("freecad", 20, sp.Phase)
			in.PhaseOrder[1] = in.Context.Nodes[1].Resource
		}, []Status{Ready, Ready}, []bool{true, true}},
		{"cross repo dependency", func(in *Input) {
			in.Context.Nodes[0] = node("freecad", 10, sp.Phase)
			in.Context.Root = in.Context.Nodes[0].Resource
			in.PhaseOrder[0] = in.Context.Nodes[0].Resource
			dependency(in, 0, 1)
		}, []Status{Ready, Blocked}, []bool{true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input(node("engine", 10, sp.Phase), node("engine", 20, sp.Phase), node("engine", 30, sp.Bug))
			tc.setup(&in)
			p := evaluate(t, in)
			ready := map[string]int{}
			for _, d := range p.Decisions {
				if d.Status == Ready {
					ready[d.Resource.Repository]++
				}
			}
			for repo, n := range ready {
				if n > 1 {
					t.Fatalf("multiple Ready Phases in %s", repo)
				}
			}
			for i, want := range tc.statuses {
				if want == "" {
					for _, d := range p.Decisions {
						if d.Resource == in.Context.Nodes[i].Resource {
							t.Fatal("closed decision")
						}
					}
					continue
				}
				d := decision(t, p, in.Context.Nodes[i].Resource)
				if d.Status != want || d.PhaseActive != tc.active[i] {
					t.Fatalf("%+v, want %s active=%v", d, want, tc.active[i])
				}
			}
			for _, d := range p.Decisions {
				for _, n := range in.Context.Nodes {
					if n.Type == sp.Bug && d.Resource == n.Resource {
						t.Fatal("Bug lifecycle decision")
					}
				}
			}
		})
	}
}

func TestPhaseOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order []int
		edge  bool
		bad   bool
	}{
		{"complete", []int{0, 1}, false, false},
		{"missing", []int{0}, false, true},
		{"duplicate", []int{0, 0, 1}, false, true},
		{"unknown", []int{0, 1, 4}, false, true},
		{"task", []int{0, 1, 2}, false, true},
		{"feature", []int{0, 1, 3}, false, true},
		{"bug", []int{0, 1, 5}, false, true},
		{"blocker after blocked", []int{1, 0}, true, true},
		{"valid dependency", []int{0, 1}, true, false},
		{"unrelated supplied reverse", []int{1, 0}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input(node("engine", 1, sp.Phase), node("engine", 99, sp.Phase), node("engine", 2, sp.Task), node("engine", 3, sp.Feature), node("engine", 4, sp.Task), node("engine", 5, sp.Bug))
			in.PhaseOrder = nil
			for _, i := range tc.order {
				r := in.Context.Nodes[i].Resource
				if i == 4 {
					r.Number = 999
				}
				in.PhaseOrder = append(in.PhaseOrder, r)
			}
			if tc.edge {
				dependency(&in, 0, 1)
			}
			p, err := Evaluate(in)
			if tc.bad {
				if !errors.Is(err, ErrPhaseOrder) || !reflect.DeepEqual(p, Plan{}) {
					t.Fatalf("%+v %v", p, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.edge {
				d := decision(t, p, in.PhaseOrder[0])
				if d.Status != Ready {
					t.Fatal("supplied order ignored")
				}
			}
		})
	}
	t.Run("node evidence mismatch", func(t *testing.T) {
		in := input(node("engine", 1, sp.Phase))
		in.PhaseOrder[0].NodeID = "other"
		_, err := Evaluate(in)
		if !errors.Is(err, ErrPhaseOrder) {
			t.Fatal(err)
		}
	})
	t.Run("closed edge still orders", func(t *testing.T) {
		in := input(node("engine", 1, sp.Phase), node("engine", 2, sp.Phase))
		in.Context.Nodes[0].State = ec.Closed
		dependency(&in, 0, 1)
		slices.Reverse(in.PhaseOrder)
		_, err := Evaluate(in)
		if !errors.Is(err, ErrPhaseOrder) {
			t.Fatal(err)
		}
	})
}

func TestChildGating(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		setup                     func(*Input)
		status                    Status
		active, blocked, eligible bool
	}{
		{"active", func(*Input) {}, Ready, true, false, true},
		{"blocked child", func(in *Input) { dependency(in, 3, 2) }, Ready, true, true, false},
		{"backlog parent", func(in *Input) { slices.Reverse(in.PhaseOrder) }, Backlog, false, false, false},
		{"blocked parent", func(in *Input) { dependency(in, 3, 0) }, Backlog, false, false, false},
		{"closed parent", func(in *Input) { in.Context.Nodes[0].State = ec.Closed }, Backlog, false, false, false},
		{"cross repo", func(in *Input) { in.Context.Nodes[2] = node("freecad", 48, sp.Task) }, Ready, true, false, true},
		{"inactive parent blocked child", func(in *Input) { slices.Reverse(in.PhaseOrder); dependency(in, 3, 2) }, Backlog, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input(node("engine", 45, sp.Phase), node("engine", 46, sp.Phase), node("engine", 48, sp.Task), node("engine", 49, sp.Task))
			tc.setup(&in)
			parent(&in, 2, 0)
			before, _ := json.Marshal(in)
			d := decision(t, evaluate(t, in), in.Context.Nodes[2].Resource)
			if d.Status != tc.status || d.PhaseActive != tc.active || d.HasActiveBlocker != tc.blocked || d.ExecutionEligible != tc.eligible || d.ParentPhase == nil || *d.ParentPhase != in.Context.Nodes[0].Resource {
				t.Fatalf("%+v", d)
			}
			after, _ := json.Marshal(in)
			if string(before) != string(after) {
				t.Fatal("input mutated")
			}
			if tc.name == "active" && len(in.Context.Dependencies) != 0 {
				t.Fatal("Parent created dependency")
			}
		})
	}
	for _, typ := range []sp.IssueType{sp.Task, sp.Feature, sp.Bug} {
		t.Run("invalid parent "+string(typ), func(t *testing.T) {
			in := input(node("engine", 1, typ), node("engine", 2, sp.Task))
			parent(&in, 1, 0)
			_, err := Evaluate(in)
			if !errors.Is(err, ErrParent) {
				t.Fatal(err)
			}
		})
	}
}

func TestParentlessAndBlockers(t *testing.T) {
	for _, typ := range []sp.IssueType{sp.Task, sp.Feature} {
		for _, blockerType := range sp.Types() {
			for _, state := range []ec.IssueState{ec.Open, ec.Closed} {
				t.Run(fmt.Sprintf("%s/%s/%s", typ, blockerType, state), func(t *testing.T) {
					in := input(node("engine", 1, typ), node("freecad", 2, blockerType))
					in.Context.Nodes[1].State = state
					dependency(&in, 1, 0)
					before, _ := json.Marshal(in.Context.Dependencies)
					d := decision(t, evaluate(t, in), in.Context.Nodes[0].Resource)
					want := Ready
					if state == ec.Open {
						want = Blocked
					}
					if d.Status != want || d.HasActiveBlocker != (state == ec.Open) || d.ExecutionEligible != (state == ec.Closed) {
						t.Fatalf("%+v", d)
					}
					after, _ := json.Marshal(in.Context.Dependencies)
					if string(before) != string(after) {
						t.Fatal("edge removed")
					}
				})
			}
		}
	}
	in := input(node("engine", 99, sp.Phase))
	for i := int64(1); i <= 20; i++ {
		in.Context.Nodes = append(in.Context.Nodes, node("engine", i, sp.Task))
	}
	p := evaluate(t, in)
	if len(p.ParentlessReady) != 20 || decision(t, p, in.Context.Root).Status != Ready {
		t.Fatal("capacity or roadmap changed")
	}
}

func TestCreateBranchAndOwnership(t *testing.T) {
	for _, typ := range []sp.IssueType{sp.Phase, sp.Task, sp.Feature} {
		for _, branch := range []intent.Boolean{{}, {Explicit: true, Value: false}, {Explicit: true, Value: true}} {
			for _, automation := range []bool{true, false} {
				for _, owned := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/%+v/automation=%v/owned=%v", typ, branch, automation, owned), func(t *testing.T) {
						n := node("engine", 1, typ)
						n.Intent.CreateBranch = branch
						n.Intent.Explicit.Automation = intent.Boolean{Explicit: true, Value: automation}
						n.Intent.Explicit.SetStatus = intent.Boolean{Explicit: true, Value: owned}
						d := decision(t, evaluate(t, input(n)), n.Resource)
						wantBranch := typ != sp.Phase
						if branch.Explicit {
							wantBranch = branch.Value
						}
						if d.Status != Ready || d.CreateBranch != wantBranch || d.ExecutionEligible != wantBranch || d.StatusOwned != owned {
							t.Fatalf("%+v", d)
						}
					})
				}
			}
		}
	}
	d := evaluate(t, input(node("engine", 1, sp.Task))).Decisions[0]
	if !d.StatusOwned {
		t.Fatal("default ownership")
	}
	for _, blocked := range []bool{true, false} {
		in := input(node("engine", 1, sp.Phase), node("engine", 2, sp.Phase), node("engine", 3, sp.Bug))
		in.Context.Nodes[1].Intent.CreateBranch = intent.Boolean{Explicit: true, Value: true}
		if blocked {
			dependency(&in, 2, 1)
		}
		d := decision(t, evaluate(t, in), in.Context.Nodes[1].Resource)
		if d.ExecutionEligible {
			t.Fatal("inactive Phase execution")
		}
	}
}

func TestOwnershipOnlyChangesOwnership(t *testing.T) {
	for _, typ := range []sp.IssueType{sp.Phase, sp.Task, sp.Feature} {
		for _, blocked := range []bool{false, true} {
			in := input(node("engine", 1, typ), node("engine", 2, sp.Bug))
			if blocked {
				dependency(&in, 1, 0)
			}
			before := evaluate(t, in)
			in.Context.Nodes[0].Intent.Explicit.SetStatus = intent.Boolean{Explicit: true, Value: false}
			after := evaluate(t, in)
			if !before.Decisions[0].StatusOwned || after.Decisions[0].StatusOwned {
				t.Fatal("ownership did not change")
			}
			after.Decisions[0].StatusOwned = true
			if !reflect.DeepEqual(before, after) {
				t.Fatal("ownership changed canonical policy")
			}
		}
	}
}

func TestClosedWorkAndManualAutomation(t *testing.T) {
	for _, typ := range sp.Types() {
		in := input(node("engine", 1, typ))
		in.Context.Nodes[0].State = ec.Closed
		p := evaluate(t, in)
		if len(p.Decisions) != 0 || len(p.ParentlessReady) != 0 {
			t.Fatalf("closed %s received work: %+v", typ, p)
		}
	}
	for _, typ := range []sp.IssueType{sp.Phase, sp.Task, sp.Feature} {
		n := node("engine", 1, typ)
		manual(&n)
		n.Intent.Explicit.Classification = intent.Boolean{}
		n.Intent.Explicit.Automation = intent.Boolean{Explicit: true, Value: false}
		d := evaluate(t, input(n)).Decisions[0]
		if d.Status != Ready || !d.StatusOwned || d.CreateBranch != (typ != sp.Phase) || d.ExecutionEligible != (typ != sp.Phase) {
			t.Fatalf("manual Automation suppressed lifecycle: %+v", d)
		}
	}
}

func TestRank(t *testing.T) {
	in := input(node("engine", 900, sp.Phase))
	// Reverse input/identity desirability to prove metadata drives relative order.
	var want []storage.Resource
	number := int64(100)
	for _, priority := range sp.Priorities() {
		for _, effort := range sp.Efforts() {
			n := node("engine", number, sp.Task)
			number--
			n.AcceptedClassification.Priority = priority
			n.AcceptedClassification.Effort = effort
			n.AcceptedClassification.Labels = []sp.Label{"runtime", "docs"}
			in.Context.Nodes = append(in.Context.Nodes, n)
			want = append(want, n.Resource)
		}
	}
	a, b := node("engine", 1, sp.Feature), node("engine", 2, sp.Task)
	manual(&a)
	manual(&b)
	in.Context.Nodes = append(in.Context.Nodes, b, a)
	want = append(want, a.Resource, b.Resource)
	p := evaluate(t, in)
	if !reflect.DeepEqual(p.ParentlessReady, want) {
		t.Fatalf("rank order: %v", p.ParentlessReady)
	}
	for _, n := range []ec.IssueNode{a, b} {
		d := decision(t, p, n.Resource)
		if d.Status != Ready || !d.ExecutionEligible || d.ParentlessRank == nil || *d.ParentlessRank != (Rank{}) {
			t.Fatalf("manual metadata fabricated: %+v", d)
		}
	}
	// Labels cannot change rank; identical metadata uses identity.
	x, y := node("engine", 1, sp.Task), node("engine", 2, sp.Task)
	y.AcceptedClassification.Labels = []sp.Label{"runtime", "docs"}
	p = evaluate(t, input(y, x))
	if !reflect.DeepEqual(p.ParentlessReady, []storage.Resource{x.Resource, y.Resource}) {
		t.Fatal("label modifier or missing tie-break")
	}
}

func TestInvalidContext(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Input)
		kind error
	}{
		{"missing root", func(in *Input) { in.Context.Root.Number = 999 }, ErrInvalid},
		{"root evidence", func(in *Input) { in.Context.Root.NodeID = "bad" }, ErrInvalid},
		{"duplicate node", func(in *Input) { in.Context.Nodes = append(in.Context.Nodes, in.Context.Nodes[0]) }, ErrInvalid},
		{"duplicate semantic identity", func(in *Input) {
			n := in.Context.Nodes[0]
			n.Resource.NodeID = "different"
			in.Context.Nodes = append(in.Context.Nodes, n)
		}, ErrInvalid},
		{"duplicate node ID", func(in *Input) { in.Context.Nodes[1].Resource.NodeID = in.Context.Nodes[0].Resource.NodeID }, ErrInvalid},
		{"unnormalized owner", func(in *Input) { in.Context.Nodes[0].Resource.Owner = "Parametron-io" }, ErrInvalid},
		{"unsupported owner", func(in *Input) { in.Context.Nodes[0].Resource.Owner = "elsewhere" }, ErrInvalid},
		{"unnormalized repo", func(in *Input) { in.Context.Nodes[0].Resource.Repository = "Engine" }, ErrInvalid},
		{"bad repo", func(in *Input) { in.Context.Nodes[0].Resource.Repository = "bad/repo" }, ErrInvalid},
		{"bad kind", func(in *Input) { in.Context.Nodes[0].Resource.Kind = "pull_request" }, ErrInvalid},
		{"bad number", func(in *Input) { in.Context.Nodes[0].Resource.Number = 0 }, ErrInvalid},
		{"missing node ID", func(in *Input) { in.Context.Nodes[0].Resource.NodeID = "" }, ErrInvalid},
		{"bad state", func(in *Input) { in.Context.Nodes[0].State = "Done" }, ErrInvalid},
		{"bad Type", func(in *Input) { in.Context.Nodes[0].Type = "Other" }, ErrInvalid},
		{"bad TypeSource", func(in *Input) { in.Context.Nodes[0].TypeSource = "other" }, ErrInvalid},
		{"missing accepted", func(in *Input) { in.Context.Nodes[0].AcceptedClassification = nil }, ErrInvalid},
		{"accepted mismatch", func(in *Input) { in.Context.Nodes[0].AcceptedClassification.Type = sp.Task }, ErrInvalid},
		{"bad priority", func(in *Input) { in.Context.Nodes[0].AcceptedClassification.Priority = "other" }, ErrInvalid},
		{"bad effort", func(in *Input) { in.Context.Nodes[0].AcceptedClassification.Effort = "other" }, ErrInvalid},
		{"bad label", func(in *Input) { in.Context.Nodes[0].AcceptedClassification.Labels = []sp.Label{"other"} }, ErrInvalid},
		{"fabricated manual", func(in *Input) { in.Context.Nodes[0].TypeSource = ec.ManualNative }, ErrInvalid},
		{"manual classification required", func(in *Input) {
			manual(&in.Context.Nodes[0])
			in.Context.Nodes[0].Intent.Explicit.Classification = intent.Boolean{}
		}, ErrInvalid},
		{"bad parent endpoint", func(in *Input) { parent(in, 1, 0); in.Context.Parents[0].Parent.Number = 999 }, ErrParent},
		{"bad child endpoint", func(in *Input) { parent(in, 1, 0); in.Context.Parents[0].Child.Number = 999 }, ErrParent},
		{"duplicate parent", func(in *Input) {
			parent(in, 1, 0)
			in.Context.Parents = append(in.Context.Parents, in.Context.Parents[0])
		}, ErrParent},
		{"multiple parents", func(in *Input) {
			parent(in, 1, 0)
			in.Context.Parents = append(in.Context.Parents, ec.ParentEdge{Child: in.Context.Nodes[1].Resource, Parent: in.Context.Nodes[2].Resource})
		}, ErrParent},
		{"self parent", func(in *Input) { parent(in, 1, 1) }, ErrParent},
		{"parent intent mismatch", func(in *Input) { parent(in, 1, 0); in.Context.Nodes[1].Intent.Parent.Number = 99 }, ErrParent},
		{"parent without intent", func(in *Input) { parent(in, 1, 0); in.Context.Nodes[1].Intent.Parent = nil }, ErrParent},
		{"intent without parent", func(in *Input) { parent(in, 1, 0); in.Context.Parents = nil }, ErrParent},
		{"bad blocker endpoint", func(in *Input) { dependency(in, 1, 0); in.Context.Dependencies[0].Blocker.Number = 999 }, ErrLifecycle},
		{"bad target endpoint", func(in *Input) { dependency(in, 1, 0); in.Context.Dependencies[0].Blocked.Number = 999 }, ErrLifecycle},
		{"duplicate dependency", func(in *Input) {
			dependency(in, 1, 0)
			in.Context.Dependencies = append(in.Context.Dependencies, in.Context.Dependencies[0])
		}, ErrLifecycle},
		{"self dependency", func(in *Input) { dependency(in, 1, 1) }, ErrLifecycle},
		{"dependency cycle", func(in *Input) { dependency(in, 1, 0); dependency(in, 0, 1) }, ErrLifecycle},
		{"parent cycle", func(in *Input) { parent(in, 0, 2); parent(in, 2, 0) }, ErrLifecycle},
		{"Phase non-Phase parent", func(in *Input) { parent(in, 0, 1) }, ErrParent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := input(node("engine", 1, sp.Phase), node("engine", 2, sp.Task), node("engine", 3, sp.Phase))
			tc.edit(&in)
			p, err := Evaluate(in)
			var typed *Error
			if !errors.Is(err, tc.kind) || !errors.As(err, &typed) || !reflect.DeepEqual(p, Plan{}) {
				t.Fatalf("plan=%+v err=%v", p, err)
			}
		})
	}
}

func TestPureIntegrationAndDeterminism(t *testing.T) {
	in := input(node("engine", 45, sp.Phase), node("engine", 48, sp.Task), node("engine", 49, sp.Task), node("freecad", 60, sp.Phase), node("freecad", 61, sp.Feature), node("engine", 70, sp.Phase), node("engine", 80, sp.Task))
	manual(&in.Context.Nodes[2])
	manual(&in.Context.Nodes[4])
	manual(&in.Context.Nodes[6])
	in.Context.Nodes[0].Intent.CreateBranch = intent.Boolean{Explicit: true, Value: true}
	in.Context.Nodes[1].Intent.Explicit.SetStatus = intent.Boolean{Explicit: true, Value: false}
	parent(&in, 1, 0)
	parent(&in, 2, 0)
	parent(&in, 4, 3)
	dependency(&in, 3, 5) // OPEN cross-repository Phase blocks engine #70.
	dependency(&in, 4, 2) // A Feature blocks an active Phase's child.
	p := evaluate(t, in)
	for i, want := range []struct {
		status                                   Status
		active, blocker, branch, eligible, owned bool
	}{
		{Ready, true, false, true, true, true},
		{Ready, true, false, true, true, false},
		{Ready, true, true, true, false, true},
		{Ready, true, false, false, false, true},
		{Ready, true, false, true, true, true},
		{Blocked, false, true, false, false, true},
		{Ready, false, false, true, true, true},
	} {
		d := decision(t, p, in.Context.Nodes[i].Resource)
		if d.Status != want.status || d.PhaseActive != want.active || d.HasActiveBlocker != want.blocker || d.CreateBranch != want.branch || d.ExecutionEligible != want.eligible || d.StatusOwned != want.owned {
			t.Fatalf("node %d: %+v", i, d)
		}
	}
	// Add another unblocked Phase, which remains Backlog behind #45.
	in.Context.Nodes = append(in.Context.Nodes, node("engine", 90, sp.Phase))
	in.PhaseOrder = append(in.PhaseOrder, in.Context.Nodes[7].Resource)
	p = evaluate(t, in)
	if decision(t, p, in.Context.Nodes[7].Resource).Status != Backlog {
		t.Fatal("later Phase advanced")
	}
	baseline, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(in)
	rng := rand.New(rand.NewSource(35))
	for i := 0; i < 20; i++ {
		shuffled := in
		shuffled.Context.Nodes = slices.Clone(in.Context.Nodes)
		shuffled.Context.Parents = slices.Clone(in.Context.Parents)
		shuffled.Context.Dependencies = slices.Clone(in.Context.Dependencies)
		shuffled.PhaseOrder = slices.Clone(in.PhaseOrder)
		rng.Shuffle(len(shuffled.Context.Nodes), func(i, j int) {
			shuffled.Context.Nodes[i], shuffled.Context.Nodes[j] = shuffled.Context.Nodes[j], shuffled.Context.Nodes[i]
		})
		rng.Shuffle(len(shuffled.Context.Parents), func(i, j int) {
			shuffled.Context.Parents[i], shuffled.Context.Parents[j] = shuffled.Context.Parents[j], shuffled.Context.Parents[i]
		})
		rng.Shuffle(len(shuffled.Context.Dependencies), func(i, j int) {
			shuffled.Context.Dependencies[i], shuffled.Context.Dependencies[j] = shuffled.Context.Dependencies[j], shuffled.Context.Dependencies[i]
		})
		got, err := json.Marshal(evaluate(t, shuffled))
		if err != nil || string(got) != string(baseline) {
			t.Fatalf("run %d differs: %s %v", i, got, err)
		}
	}
	after, _ := json.Marshal(in)
	if string(original) != string(after) {
		t.Fatal("input mutated")
	}
	// Returned pointer fields are independent of caller-owned intent/resources.
	for _, d := range p.Decisions {
		if d.ParentPhase != nil {
			d.ParentPhase.Number = 999
		}
		if d.ParentlessRank != nil {
			d.ParentlessRank.Priority = sp.Critical
		}
	}
	after, _ = json.Marshal(in)
	if string(original) != string(after) {
		t.Fatal("output aliases input")
	}
}
