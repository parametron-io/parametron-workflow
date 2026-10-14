package prdevelopment

import (
	"context"
	"errors"
	"fmt"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"reflect"
	"sync"
	"testing"
)

// This fixture supplies bounded provider results, never evaluates authorization.
// Only DevelopmentWriter capabilities are available to the controller.
type developmentFixture struct {
	in                   Input
	writes               []Action
	barriers, reads      int
	hook                 func(*developmentFixture)
	readHook             func(*observe.ObservedState, int)
	failAt               int
	applyOnError, ignore bool
	cancel               context.CancelFunc
	entered, release     chan struct{}
}

func (f *developmentFixture) Evaluate(ctx context.Context, r storage.Resource) (prbinding.Decision, error) {
	if f.entered != nil {
		close(f.entered)
		f.entered = nil
		select {
		case <-f.release:
		case <-ctx.Done():
			return prbinding.Decision{}, ctx.Err()
		}
	}
	f.barriers++
	if f.hook != nil {
		f.hook(f)
	}
	return f.in.Binding, ctx.Err()
}
func (f *developmentFixture) Completed(ctx context.Context, r storage.Resource) (pronboarding.Completion, error) {
	return f.in.Onboarding, ctx.Err()
}
func (f *developmentFixture) ReadCurrent(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	f.reads++
	o := observe.ObservedState{Presence: observe.Present, Resource: r, Projects: []observe.ProjectState{{Profile: "engineering", ID: "ENG", Items: []observe.ProjectItem{{ID: "ITEM"}}}, {Profile: "bug_tracker", ID: "BUG"}}}
	if r.Kind == "pull_request" {
		p := f.in.PR
		o.PullRequest = &p
	} else {
		i := f.in.Target
		o.Issue = &i
	}
	if f.readHook != nil {
		f.readHook(&o, f.reads)
	}
	return o, ctx.Err()
}
func (f *developmentFixture) DevelopmentReferences(ctx context.Context, pr github.Identity) (github.DevelopmentReferences, error) {
	return f.in.References, ctx.Err()
}

var errDevelopmentWrite = errors.New("injected write failure")

func (f *developmentFixture) effect(ctx context.Context, pr, i github.Identity, add bool) error {
	if pr != f.in.Binding.Evidence.PR {
		panic("wrong PR endpoint")
	}
	f.writes = append(f.writes, Action{Add: add, Target: i})
	fail := f.failAt == len(f.writes)
	if !f.ignore && (!fail || f.applyOnError) {
		if add {
			f.in.References.Manual = append(f.in.References.Manual, i)
		} else {
			out := []github.Identity{}
			for _, v := range f.in.References.Manual {
				if v != i {
					out = append(out, v)
				}
			}
			f.in.References.Manual = out
		}
	}
	if f.cancel != nil {
		f.cancel()
	}
	if fail {
		return errDevelopmentWrite
	}
	return ctx.Err()
}
func (f *developmentFixture) AddDevelopmentReference(ctx context.Context, pr, i github.Identity) error {
	return f.effect(ctx, pr, i, true)
}
func (f *developmentFixture) RemoveDevelopmentReference(ctx context.Context, pr, i github.Identity) error {
	return f.effect(ctx, pr, i, false)
}
func (f *developmentFixture) config() Config {
	return Config{Repositories: []github.Repository{f.in.PR.Identity.Repository}, Project: "ENG", BugProject: "BUG", Binding: f, Onboarding: f, Reader: f, Relationships: f, Writer: f}
}
func fixtureController(t *testing.T) (*developmentFixture, *Controller) {
	t.Helper()
	f := &developmentFixture{in: developmentInput(t)}
	c, err := New(f.config())
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}
func TestDevelopmentControllerConvergenceAndRecovery(t *testing.T) {
	for _, mode := range []string{"success", "second removal fails", "add fails", "uncertain success", "not observable", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, c := fixtureController(t)
			target := f.in.Target.Identity
			a, b := target, target
			a.ID = "A"
			a.Number = 49
			b.ID = "B"
			b.Number = 50
			f.in.References.Manual = []github.Identity{b, a}
			switch mode {
			case "second removal fails":
				f.failAt = 2
			case "add fails":
				f.failAt = 3
			case "uncertain success":
				f.failAt = 1
				f.applyOnError = true
			case "not observable":
				f.ignore = true
			}
			ctx := context.Background()
			if mode == "cancel" {
				ctx, f.cancel = context.WithCancel(ctx)
			}
			err := c.Reconcile(ctx, resource(f.in.PR.Identity, "pull_request"))
			if mode == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected bounded failure")
			}
			if (mode == "second removal fails" || mode == "add fails" || mode == "uncertain success") && !errors.Is(err, errDevelopmentWrite) {
				t.Fatalf("write failure was not preserved: %v", err)
			}
			if mode == "not observable" {
				if !errors.Is(err, ErrChanged) || len(f.writes) != 1 {
					t.Fatalf("%v %v", err, f.writes)
				}
				return
			}
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) || len(f.writes) != 1 {
					t.Fatalf("%v %v", err, f.writes)
				}
				f.cancel = nil
			}
			before := len(f.writes)
			f.failAt = 0
			if err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request")); err != nil {
				t.Fatal(err)
			}
			want := []Action{{Target: a}, {Target: b}, {Add: true, Target: target}}
			if mode == "second removal fails" {
				want = []Action{{Target: a}, {Target: b}, {Target: b}, {Add: true, Target: target}}
			}
			if mode == "add fails" {
				want = append(want, Action{Add: true, Target: target})
			}
			if !reflect.DeepEqual(f.writes, want) {
				t.Fatalf("%v", f.writes)
			}
			if mode == "uncertain success" && len(f.writes) != 3 {
				t.Fatalf("replayed successful effect: %v", f.writes)
			}
			if len(f.in.References.Manual) != 1 || f.in.References.Manual[0] != target {
				t.Fatal("not converged")
			}
			after := len(f.writes)
			if err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request")); err != nil || len(f.writes) != after {
				t.Fatalf("replay %v", err)
			}
			if f.reads != 2*f.barriers || before == 0 {
				t.Fatalf("reads=%d barriers=%d", f.reads, f.barriers)
			}
		})
	}
}
func TestDevelopmentControllerFreshness(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Input)
	}{
		{"body", func(i *Input) { i.PR.Body = "Target: #49" }},
		{"head", func(i *Input) { i.PR.HeadSHA = "changed" }},
		{"author", func(i *Input) { i.PR.Author = nil }},
		{"target state", func(i *Input) { i.Target.State = "CLOSED" }},
		{"target body", func(i *Input) { i.Target.Body = "Create-Branch: false" }},
		{"target identity", func(i *Input) { i.Target.Identity.ID = "OTHER" }},
		{"onboarding", func(i *Input) { i.Onboarding.Completed = false }},
		{"binding", func(i *Input) { i.Binding.Outcome = prbinding.Rejected }},
		{"references", func(i *Input) { i.References.Manual = []github.Identity{i.Target.Identity} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := fixtureController(t)
			f.hook = func(f *developmentFixture) {
				if f.barriers == 2 {
					tc.change(&f.in)
				}
			}
			err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request"))
			if err == nil || len(f.writes) != 0 {
				t.Fatalf("%v writes=%v", err, f.writes)
			}
		})
	}
	f, c := fixtureController(t)
	f.in.Binding.Outcome = prbinding.Rejected
	if err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request")); !errors.Is(err, ErrRejected) || f.reads != 0 || len(f.writes) != 0 {
		t.Fatalf("%v reads=%d", err, f.reads)
	}
	f, c = fixtureController(t)
	f.hook = func(f *developmentFixture) {
		if f.barriers == 3 {
			f.in.Onboarding.Completed = false
		}
	}
	if err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request")); err == nil || len(f.writes) != 1 {
		t.Fatalf("%v %v", err, f.writes)
	}
}
func TestDevelopmentControllerConstruction(t *testing.T) {
	f, _ := fixtureController(t)
	for _, change := range []func(*Config){func(c *Config) { c.Binding = nil }, func(c *Config) { c.Onboarding = nil }, func(c *Config) { c.Reader = nil }, func(c *Config) { c.Relationships = nil }, func(c *Config) { c.Writer = nil }, func(c *Config) { var p *developmentFixture; c.Binding = p }, func(c *Config) { c.Repositories = nil }, func(c *Config) { c.Repositories = append(c.Repositories, c.Repositories[0]) }, func(c *Config) { c.Project = "bad id" }, func(c *Config) { c.BugProject = c.Project }, func(c *Config) { c.Repositories[0].Owner = "other" }} {
		cfg := f.config()
		change(&cfg)
		if _, err := New(cfg); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%v", err)
		}
	}
	cfg := f.config()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repositories[0].ID = "CHANGED"
	if !c.configured(f.in.PR.Identity) {
		t.Fatal("configuration slice aliased")
	}
}
func TestDevelopmentControllerSerialization(t *testing.T) {
	f, c := fixtureController(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request"))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(f.writes) != 1 {
		t.Fatalf("%v", f.writes)
	}
	// Occupy the lane directly to deterministically test waiter cancellation.
	c.lane <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Reconcile(ctx, resource(f.in.PR.Identity, "pull_request")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-c.lane
}
func TestDevelopmentSharedTargetIndependentPRs(t *testing.T) {
	f, c := fixtureController(t)
	if err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request")); err != nil {
		t.Fatal(err)
	}
	g := &developmentFixture{in: developmentInput(t)}
	g.in.PR.Identity.ID = "PR2"
	g.in.PR.Identity.Number = 81
	g.in.Binding.Evidence.PR = g.in.PR.Identity
	g.in.Onboarding.Binding = g.in.Binding.Evidence
	g.in.Onboarding.Resource = resource(g.in.PR.Identity, "pull_request")
	g.in.References.PR = g.in.PR.Identity
	second, err := New(g.config())
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Reconcile(context.Background(), g.in.Onboarding.Resource); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 1 || len(g.writes) != 1 || f.writes[0].Target != g.writes[0].Target {
		t.Fatal("shared target lost independent relationship")
	}
	g.in.Binding.Outcome = prbinding.Rejected
	g.in.References.Manual = nil
	if err = second.Reconcile(context.Background(), g.in.Onboarding.Resource); !errors.Is(err, ErrRejected) || len(g.writes) != 1 {
		t.Fatalf("shared target granted authorization: %v", err)
	}
}

func TestDevelopmentMembershipRequiredAtEveryBarrier(t *testing.T) {
	for _, at := range []int{1, 3} {
		for _, mode := range []string{"missing", "archived", "duplicate", "invalid item", "wrong project", "bug membership", "unknown project"} {
			t.Run(mode+string(rune('0'+at)), func(t *testing.T) {
				f, c := fixtureController(t)
				f.readHook = func(o *observe.ObservedState, n int) {
					if n < at {
						return
					}
					switch mode {
					case "missing":
						o.Projects[0].Items = nil
					case "archived":
						o.Projects[0].Items[0].Archived = true
					case "duplicate":
						o.Projects = append(o.Projects, o.Projects[0])
					case "invalid item":
						o.Projects[0].Items[0].ID = ""
					case "wrong project":
						o.Projects[0].ID = "OTHER"
					case "bug membership":
						o.Projects[1].Items = []observe.ProjectItem{{ID: "BUGITEM"}}
					case "unknown project":
						o.Projects[1].Profile = "other"
					}
				}
				err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request"))
				if err == nil || len(f.writes) != 0 {
					t.Fatalf("err=%v writes=%v", err, f.writes)
				}
			})
		}
	}
}
func TestDevelopmentControllerHistoricalNoWrites(t *testing.T) {
	for _, state := range []string{"CLOSED", "MERGED"} {
		for _, present := range []bool{false, true} {
			f, c := fixtureController(t)
			f.in.PR.State = state
			f.in.Target.State = "CLOSED"
			if present {
				f.in.References.Manual = []github.Identity{f.in.Target.Identity}
			}
			if err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request")); err != nil || len(f.writes) != 0 {
				t.Fatalf("%s present=%v err=%v writes=%v", state, present, err, f.writes)
			}
		}
	}
}

func TestDevelopmentReorderedBarriers(t *testing.T) {
	f, c := fixtureController(t)
	a, b := f.in.Target.Identity, f.in.Target.Identity
	a.ID = "A"
	a.Number = 49
	b.ID = "B"
	b.Number = 50
	f.in.References.Manual = []github.Identity{b, a}
	f.hook = func(f *developmentFixture) {
		if f.barriers == 2 {
			f.in.References.Manual = []github.Identity{a, b}
		}
	}
	if err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.writes, []Action{{Target: a}, {Target: b}, {Add: true, Target: f.in.Target.Identity}}) {
		t.Fatalf("%v", f.writes)
	}
}
func TestDevelopmentCancellationWaitingForActiveReconciliation(t *testing.T) {
	f, c := fixtureController(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.entered = entered
	f.release = release
	done := make(chan error, 1)
	go func() { done <- c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request")) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Reconcile(ctx, resource(f.in.PR.Identity, "pull_request")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 1 {
		t.Fatalf("%v", f.writes)
	}
}
func TestDevelopmentPersistentDriftIsBounded(t *testing.T) {
	f, c := fixtureController(t)
	f.hook = func(f *developmentFixture) {
		if f.barriers%2 == 1 {
			i := f.in.Target.Identity
			i.ID = fmt.Sprintf("DRIFT-%d", len(f.writes))
			i.Number = 1000 + len(f.writes)
			f.in.References.Manual = []github.Identity{i}
		}
	}
	if err := c.Reconcile(context.Background(), resource(f.in.PR.Identity, "pull_request")); !errors.Is(err, ErrChanged) || len(f.writes) != 256 || f.barriers != 512 {
		t.Fatalf("err=%v effects=%d barriers=%d", err, len(f.writes), f.barriers)
	}
}
