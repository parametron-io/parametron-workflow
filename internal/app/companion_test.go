package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	er "github.com/parametron-io/parametron-workflow/internal/engineeringreconcile"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	pc "github.com/parametron-io/parametron-workflow/internal/prcompanion"
	cr "github.com/parametron-io/parametron-workflow/internal/prcompanionreconcile"
	"github.com/parametron-io/parametron-workflow/internal/prdiscovery"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func TestCompanionCapabilityRequiredBeforeServing(t *testing.T) {
	c, f := onboardingRuntimeFixture(t)
	c.Client = struct {
		onboardingCore
		appOnboardingCapabilities
		github.NativeLifecycleReader
		github.IssueStates
	}{f, f, f, f}
	if r, err := compose(context.Background(), c); !errors.Is(err, cr.ErrInvalid) || r != nil {
		t.Fatal(r, err)
	}
	if f.prReads+f.creates+f.drafts+f.adds != 0 {
		t.Fatal("startup performed workflow I/O")
	}
}

func TestCompanionCategoriesThroughDurableWorker(t *testing.T) {
	for _, tc := range []struct {
		err      error
		category string
		retry    bool
	}{
		{cr.ErrChanged, "pr_companion_changed", true},
		{cr.ErrIncomplete, "pr_companion_incomplete", true},
		{pc.ErrTarget, "pr_companion_target", true},
		{github.ErrRequestLimit, "pr_companion_budget", true},
		{prdiscovery.ErrChanged, "pr_discovery_changed", true},
		{prdiscovery.ErrIncomplete, "pr_discovery_incomplete", true},
		{cr.ErrInvalid, "pr_companion_invalid", false},
		{cr.ErrProject, "pr_companion_project", false},
		{cr.ErrOwnership, "pr_companion_ownership", false},
		{cr.ErrAmbiguous, "pr_companion_ambiguous", false},
		{cr.ErrStorage, "pr_companion_storage", false},
		{pc.ErrCapacity, "pr_companion_capacity", false},
		{pc.ErrInvalid, "pr_companion_policy_invalid", false},
		{pc.ErrBinding, "pr_companion_binding", false},
		{pc.ErrPrimary, "pr_companion_primary", false},
		{prdiscovery.ErrInvalid, "pr_discovery_invalid", false},
		{prdiscovery.ErrLimit, "pr_discovery_limit", false},
	} {
		t.Run(tc.category, func(t *testing.T) {
			wrapped := fmt.Errorf("wrapped: %w", tc.err)
			classification := classify(wrapped)
			if classification.Category != tc.category || classification.Retryable != tc.retry {
				t.Fatal(classification)
			}
			c, _ := fixture(t)
			c.Consumer = consumerFunc(func(context.Context, observe.PolicyInput) error { return wrapped })
			r := openRuntime(t, c)
			accept(t, r, "delivery", 53)
			step(t, r)
			want := storage.Failed
			if tc.retry {
				want = storage.Retryable
			}
			e := state(t, r, "delivery", want, 1)
			if e.State.ErrorCategory != tc.category {
				t.Fatal(e.State)
			}
			step(t, r)
			state(t, r, "delivery", want, 1)
		})
	}
}

func TestIssueDrivenCompanionRebaseProductionWorker(t *testing.T) {
	c, f, _ := completionRuntimeFixture(t)
	c.Runner = nil
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "branch", "issues")
	step(t, r)
	state(t, r, "branch", storage.Completed, 1)
	onboardingNotification(t, r, f, "onboard", "pull_request")
	step(t, r)
	state(t, r, "onboard", storage.Completed, 1)
	check := func(want float64) {
		t.Helper()
		assertActualCompanionPolicy(t, c, r)
		if got := f.items[f.pr.ID].Values["roadmap_order"]; got.Kind != config.Number || got.Number != want {
			t.Fatal(got, want)
		}
		rows, err := r.store.CompanionFields(context.Background(), "engineering", "roadmap_order")
		if err != nil || len(rows) != 1 || rows[0].Item != "PRITEM" {
			t.Fatal(rows, err)
		}
		if f.drafts != 1 || f.adds != 1 {
			t.Fatal("numeric recovery replayed onboarding", f.drafts, f.adds)
		}
	}
	check(10001)
	earlier := f.issue
	earlier.ID = "EARLIER"
	earlier.Number = 45
	earlier.Body += "\nCreate-Branch: false"
	f.children = []github.Issue{earlier}
	f.items[earlier.ID] = github.ProjectItem{ID: "CHILDITEM", ProjectID: "engineering", Content: &github.Content{ID: earlier.ID, Kind: "Issue"}, Values: map[string]github.FieldValue{}}
	onboardingNotification(t, r, f, "earlier-root", "issues")
	step(t, r)
	state(t, r, "earlier-root", storage.Completed, 1)
	check(11001)
	r.store.Close()
	r = openRuntime(t, c)
	f.children[0].State = "CLOSED"
	onboardingNotification(t, r, f, "compact", "issues")
	step(t, r)
	state(t, r, "compact", storage.Completed, 1)
	check(10001)
	// A Project notification repairs drift without replaying initialization.
	item := f.items[f.pr.ID]
	item.Values["roadmap_order"] = github.FieldValue{Kind: config.Number, Number: 0.5}
	f.items[f.pr.ID] = item
	onboardingNotification(t, r, f, "project-drift", "projects_v2_item")
	step(t, r)
	state(t, r, "project-drift", storage.Completed, 1)
	check(10001)
	// No Development link was needed for either Issue-driven rebase.
	if len(f.issue.LinkedPullRequests) != 0 {
		t.Fatal("fixture accidentally supplied a Development link")
	}
}

func TestVerifiedNumericRetryProductionWorkerRestart(t *testing.T) {
	for _, category := range []github.Category{github.Transient, github.RateLimited} {
		t.Run(string(category), func(t *testing.T) {
			c, f, now := completionRuntimeFixture(t)
			r := openRuntime(t, c)
			onboardingNotification(t, r, f, "branch", "issues")
			step(t, r)
			onboardingNotification(t, r, f, "onboard", "pull_request")
			step(t, r)
			state(t, r, "onboard", storage.Completed, 1)
			item := f.items[f.pr.ID]
			item.Values["roadmap_order"] = github.FieldValue{Kind: config.Number, Number: 0}
			f.items[f.pr.ID] = item
			failed := false
			f.numberHook = func(ctx context.Context, id string) error {
				if id == "PRITEM" && !failed {
					failed = true
					return &github.Error{Category: category}
				}
				return ctx.Err()
			}
			onboardingNotification(t, r, f, "numeric-retry", "projects_v2_item")
			step(t, r)
			e := state(t, r, "numeric-retry", storage.Retryable, 1)
			if e.State.ErrorCategory != string(category) {
				t.Fatal(e.State)
			}
			r.store.Close()
			r = openRuntime(t, c)
			*now = now.Add(c.RetryDelay)
			// Current desired state wins on retry, rather than replaying the failed value.
			earlier := f.issue
			earlier.ID = "EARLIER"
			earlier.Number = 45
			earlier.Body += "\nCreate-Branch: false"
			f.children = []github.Issue{earlier}
			f.items[earlier.ID] = github.ProjectItem{ID: "CHILDITEM", ProjectID: "engineering", Content: &github.Content{ID: earlier.ID, Kind: "Issue"}, Values: map[string]github.FieldValue{}}
			step(t, r)
			state(t, r, "numeric-retry", storage.Completed, 2)
			if got := f.items[f.pr.ID].Values["roadmap_order"].Number; got != 11001 {
				t.Fatal("replayed stale number", got)
			}
			if f.drafts != 1 || f.adds != 1 {
				t.Fatal("one-shot replay", f.drafts, f.adds)
			}
		})
	}
}

func TestUncertainInitialNumberIsTerminalAfterWorkerRestart(t *testing.T) {
	c, f, now := completionRuntimeFixture(t)
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "branch", "issues")
	step(t, r)
	calls := 0
	f.numberHook = func(ctx context.Context, id string) error {
		if id == "PRITEM" {
			calls++
			return &github.Error{Category: github.Transient}
		}
		return ctx.Err()
	}
	onboardingNotification(t, r, f, "onboard", "pull_request")
	step(t, r)
	state(t, r, "onboard", storage.Retryable, 1)
	if calls != 1 || f.items[f.pr.ID].Values["roadmap_order"].Number != 10001 {
		t.Fatal(calls, f.items)
	}
	r.store.Close()
	r = openRuntime(t, c)
	*now = now.Add(c.RetryDelay)
	step(t, r)
	e := state(t, r, "onboard", storage.Failed, 2)
	if e.State.ErrorCategory != "pr_companion_ambiguous" || calls != 1 || f.drafts != 1 || f.adds != 1 {
		t.Fatal(e.State, calls, f.drafts, f.adds)
	}
}

type multiCompanionClient struct {
	*completionRuntimeClient
	other       github.Repository
	second      *github.PullRequest
	listFailure error
}

func (f *multiCompanionClient) ListIssues(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
	if r == f.other {
		return nil, ctx.Err()
	}
	return f.completionRuntimeClient.ListIssues(ctx, r)
}
func (f *multiCompanionClient) ListPullRequests(ctx context.Context, r github.Repository) ([]github.ListedPullRequest, error) {
	if f.listFailure != nil {
		return nil, f.listFailure
	}
	out := []github.ListedPullRequest{}
	if r == f.repo && f.prVisible {
		out = append(out, github.ListedPullRequest{Identity: f.pr.Identity, State: f.pr.State})
	}
	if f.second != nil && r == f.second.Repository {
		out = append(out, github.ListedPullRequest{Identity: f.second.Identity, State: f.second.State})
	}
	return out, ctx.Err()
}
func TestMultiPRCrossRepositoryRebaseProductionWorker(t *testing.T) {
	c, base, _ := completionRuntimeFixture(t)
	other := github.Repository{ID: "ADAPTER", Owner: "parametron-io", Name: "adapter"}
	f := &multiCompanionClient{completionRuntimeClient: base, other: other}
	c.Client = f
	c.Source.Repositories = append(c.Source.Repositories, other.Name)
	discover := base.DiscoverSchemaFunc
	base.DiscoverSchemaFunc = func(ctx context.Context, s config.SourceConfig) (config.Schema, error) {
		d, err := discover(ctx, s)
		d.Repositories = append(d.Repositories, other)
		return d, err
	}
	readPR := base.PullRequestFunc
	base.PullRequestFunc = func(ctx context.Context, ref github.Ref) (github.PullRequest, error) {
		if f.second != nil && ref.Repository == other.Name && ref.Number == f.second.Number {
			return *f.second, ctx.Err()
		}
		return readPR(ctx, ref)
	}
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "branch", "issues")
	step(t, r)
	onboardingNotification(t, r, f, "onboard", "pull_request")
	step(t, r)
	state(t, r, "onboard", storage.Completed, 1)
	second := f.pr
	second.ID = "OTHERPR"
	second.Number = 47
	second.Repository = other
	second.Body = "Target: parametron-io/repo#46\nClassification: false"
	f.second = &second
	f.items[second.ID] = github.ProjectItem{ID: "OTHERITEM", ProjectID: "engineering", Content: &github.Content{ID: second.ID, Kind: "PullRequest"}, Values: map[string]github.FieldValue{}}
	check := func(first, second float64) {
		t.Helper()
		assertActualCompanionPolicy(t, c, r)
		if a, b := f.items[f.pr.ID].Values["roadmap_order"].Number, f.items["OTHERPR"].Values["roadmap_order"].Number; a != first || b != second {
			t.Fatal(a, b, first, second)
		}
	}
	onboardingNotification(t, r, f, "discover-unlinked", "issues")
	step(t, r)
	state(t, r, "discover-unlinked", storage.Completed, 1)
	check(10002, 10001)
	earlier := f.issue
	earlier.ID = "EARLIER"
	earlier.Number = 45
	earlier.Body += "\nCreate-Branch: false"
	f.children = []github.Issue{earlier}
	f.items[earlier.ID] = github.ProjectItem{ID: "CHILDITEM", ProjectID: "engineering", Content: &github.Content{ID: earlier.ID, Kind: "Issue"}, Values: map[string]github.FieldValue{}}
	onboardingNotification(t, r, f, "rebase-both", "issues")
	step(t, r)
	state(t, r, "rebase-both", storage.Completed, 1)
	check(11002, 11001)
	r.store.Close()
	r = openRuntime(t, c)
	f.children[0].State = "CLOSED"
	onboardingNotification(t, r, f, "compact-both", "issues")
	step(t, r)
	state(t, r, "compact-both", storage.Completed, 1)
	check(10002, 10001)
	// An unrelated unresolved PR must block the whole companion set.
	item := f.items[f.pr.ID]
	item.Values["roadmap_order"] = github.FieldValue{Kind: config.Number, Number: 0}
	f.items[f.pr.ID] = item
	f.listFailure = github.ErrPullRequestIncomplete
	onboardingNotification(t, r, f, "incomplete-other", "issues")
	step(t, r)
	state(t, r, "incomplete-other", storage.Retryable, 1)
	if f.items[f.pr.ID].Values["roadmap_order"].Number != 0 {
		t.Fatal("partial plan executed")
	}
	if f.drafts != 1 || f.adds != 1 {
		t.Fatal("Issue fanout replayed onboarding")
	}
}

// Independently recompute the actual #36/#48 authority; observed Project numbers
// enter only the final comparison, never the expected allocation.
func assertActualCompanionPolicy(t *testing.T, c Config, r *runtime) {
	t.Helper()
	ctx := context.Background()
	prepared, err := Prepare(ctx, c.Source, c.Client)
	if err != nil {
		t.Fatal(err)
	}
	d := *prepared.Deployment
	reader, err := observe.NewProcessor(d, c.Client, FoundationSink{})
	if err != nil {
		t.Fatal(err)
	}
	accepted, _ := semanticflow.NewAcceptedLoader(r.store)
	snapshots, err := ec.New(ec.Config{Deployment: d, Primary: reader, Lister: c.Client.(github.IssueLister), Accepted: accepted})
	if err != nil {
		t.Fatal(err)
	}
	branches, err := eb.New(eb.Config{Repositories: d.Repositories, Branches: c.Client.(github.Branches), Store: r.store, Clock: c.Clock})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := prbinding.New(prbinding.Config{Deployment: d, Primary: reader, Context: snapshots, Completion: branches, Branches: c.Client.(github.Branches), Developers: c.Client.(github.DeveloperReads)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := snapshots.ResolveSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := s.CanonicalContext()
	if err != nil {
		t.Fatal(err)
	}
	_, _, primary, err := er.Compute(canonical)
	if err != nil {
		t.Fatal(err)
	}
	in := pc.Input{Primary: primary}
	for _, repo := range d.Repositories {
		prs, err := c.Client.(github.PullRequestLister).ListPullRequests(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		for _, pr := range prs {
			resource := storage.Resource{Owner: repo.Owner, Repository: repo.Name, Kind: "pull_request", Number: int64(pr.Number), NodeID: pr.ID}
			decision, err := binding.Evaluate(ctx, resource)
			if err != nil {
				t.Fatal(err)
			}
			in.PullRequests = append(in.PullRequests, pc.PullRequest{Resource: resource, Identity: pr.Identity, State: pc.State(pr.State), Binding: decision})
		}
	}
	plan, err := pc.Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Assignments {
		actual, err := reader.ReadCurrent(ctx, a.PR)
		if err != nil {
			t.Fatal(err)
		}
		matched := false
		for _, p := range actual.Projects {
			if p.ID == d.Engineering.ID {
				for _, i := range p.Items {
					v, ok := i.Fields[config.RoadmapOrder]
					if ok && v.Kind == config.Number && v.Number == float64(a.RoadmapOrder) {
						matched = true
					}
				}
			}
		}
		if !matched {
			t.Fatal("numeric value differs from exact pure plan", a)
		}
	}
}

func TestPRCloseReopenCompanionProductionWorker(t *testing.T) {
	c, f, now := completionRuntimeFixture(t)
	r := openRuntime(t, c)
	onboardingNotification(t, r, f, "branch", "issues")
	step(t, r)
	onboardingNotification(t, r, f, "onboard", "pull_request")
	step(t, r)
	state(t, r, "onboard", storage.Completed, 1)
	*now = now.Add(c.RetryDelay)
	closedAt := *now
	f.pr.State = "CLOSED"
	f.native[f.pr.ID] = github.NativeLifecycle{Resource: f.pr.Identity, Kind: "pull_request", State: "CLOSED", Transitions: []github.NativeTransition{{ID: "CLOSE", Kind: "ClosedEvent", Resource: f.pr.Identity, CreatedAt: closedAt}}}
	onboardingNotification(t, r, f, "close", "pull_request")
	step(t, r)
	state(t, r, "close", storage.Completed, 1)
	if _, present := f.items[f.pr.ID].Values["roadmap_order"]; present {
		t.Fatal("terminal companion was not unset")
	}
	if f.issue.State != "OPEN" || f.closes != 0 {
		t.Fatal("abandonment closed Target")
	}
	r.store.Close()
	r = openRuntime(t, c)
	*now = now.Add(c.RetryDelay)
	f.pr.State = "OPEN"
	native := f.native[f.pr.ID]
	native.State = "OPEN"
	native.Transitions = append(native.Transitions, github.NativeTransition{ID: "REOPEN", Kind: "ReopenedEvent", Resource: f.pr.Identity, CreatedAt: *now})
	f.native[f.pr.ID] = native
	onboardingNotification(t, r, f, "reopen", "pull_request")
	step(t, r)
	state(t, r, "reopen", storage.Completed, 1)
	assertActualCompanionPolicy(t, c, r)
	if f.items[f.pr.ID].Values["roadmap_order"].Number != 10001 || f.drafts != 1 || f.adds != 1 {
		t.Fatal("reopen did not converge without initialization replay", f.items, f.drafts, f.adds)
	}
}

func TestNumericCancellationThroughWorker(t *testing.T) {
	for _, verified := range []bool{false, true} {
		t.Run(fmt.Sprint(verified), func(t *testing.T) {
			c, f, _ := completionRuntimeFixture(t)
			r := openRuntime(t, c)
			onboardingNotification(t, r, f, "branch", "issues")
			step(t, r)
			if verified {
				onboardingNotification(t, r, f, "onboard", "pull_request")
				step(t, r)
				item := f.items[f.pr.ID]
				item.Values["roadmap_order"] = github.FieldValue{Kind: config.Number, Number: 0}
				f.items[f.pr.ID] = item
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			f.numberHook = func(ctx context.Context, id string) error {
				if id == "PRITEM" {
					calls++
					cancel()
					return ctx.Err()
				}
				return ctx.Err()
			}
			onboardingNotification(t, r, f, "interrupt", "pull_request")
			if err := r.worker.Step(ctx); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			state(t, r, "interrupt", storage.Pending, 1)
			r.store.Close()
			r = openRuntime(t, c)
			f.numberHook = nil
			if err := r.worker.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			step(t, r)
			if verified {
				state(t, r, "interrupt", storage.Completed, 2)
				assertActualCompanionPolicy(t, c, r)
			} else {
				e := state(t, r, "interrupt", storage.Failed, 2)
				if e.State.ErrorCategory != "pr_companion_ambiguous" {
					t.Fatal(e.State)
				}
			}
			if calls != 1 || f.drafts != 1 || f.adds != 1 {
				t.Fatal(calls, f.drafts, f.adds)
			}
		})
	}
}
