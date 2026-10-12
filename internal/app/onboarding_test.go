package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	po "github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// A read client plus narrow deterministic writes, deliberately without Mutator.
type onboardingRuntimeClient struct {
	*github.Fake
	t                                        *testing.T
	repo                                     github.Repository
	user                                     github.Actor
	issue                                    github.Issue
	pr                                       github.PullRequest
	items                                    map[string]github.ProjectItem
	branch                                   *github.Branch
	adds, statuses, drafts, creates, prReads int
	prFailure                                error
	draftHook                                func(context.Context) error
}

func onboardingRuntimeFixture(t *testing.T) (Config, *onboardingRuntimeClient) {
	t.Helper()
	c := semanticFixture(t)
	base := c.Client.(*github.Fake)
	discovery := base.DiscoverSchemaFunc
	base.DiscoverSchemaFunc = func(ctx context.Context, s config.SourceConfig) (config.Schema, error) {
		schema, err := discovery(ctx, s)
		for _, typ := range []string{"Phase", "Task", "Feature", "Bug"} {
			schema.IssueTypes = append(schema.IssueTypes, config.IssueType{ID: "type-" + typ, Name: typ})
		}
		return schema, err
	}
	f := &onboardingRuntimeClient{Fake: base, t: t, repo: github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}, user: github.Actor{ID: "U", Login: "developer", Type: "User"}, items: map[string]github.ProjectItem{}}
	f.issue = github.Issue{Identity: github.Identity{ID: "I46", Repository: f.repo, Number: 46}, State: "OPEN", Body: "Classification: false", Type: &github.IssueType{ID: "type-Task", Name: "Task"}}
	f.pr = github.PullRequest{Identity: github.Identity{ID: "PR47", Repository: f.repo, Number: 47}, State: "OPEN", Body: "Target: #46\nClassification: false", HeadRepository: f.repo, HeadRef: "development/issue-46", HeadSHA: strings.Repeat("a", 40), Author: &f.user}
	f.items["I46"] = github.ProjectItem{ID: "ISSUEITEM", ProjectID: "engineering", Content: &github.Content{ID: "I46", Kind: "Issue"}, Values: map[string]github.FieldValue{}}
	base.IssueFunc = func(ctx context.Context, r github.Ref) (github.Issue, error) {
		if r.Owner != f.repo.Owner || r.Repository != f.repo.Name || r.Number != 46 {
			t.Errorf("wrong Target refetch: %+v", r)
			return github.Issue{}, errors.New("wrong Target refetch")
		}
		return f.issue, ctx.Err()
	}
	base.PullRequestFunc = func(ctx context.Context, r github.Ref) (github.PullRequest, error) {
		f.prReads++
		if r.Owner != f.repo.Owner || r.Repository != f.repo.Name || r.Number != 47 {
			t.Errorf("wrong PR refetch: %+v", r)
			return github.PullRequest{}, errors.New("wrong PR refetch")
		}
		if f.prFailure != nil {
			return github.PullRequest{}, f.prFailure
		}
		return f.pr, ctx.Err()
	}
	base.ProjectItemsFunc = func(ctx context.Context, node, project string, _ map[string]config.FieldKind) ([]github.ProjectItem, error) {
		if project != "engineering" {
			return nil, ctx.Err()
		}
		if item, ok := f.items[node]; ok {
			return []github.ProjectItem{item}, ctx.Err()
		}
		return nil, ctx.Err()
	}
	c.Client = f
	c.Engineering = true
	c.Concurrency = 1
	return c, f
}
func (f *onboardingRuntimeClient) ListIssues(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
	if r != f.repo {
		f.t.Error(r)
		return nil, errors.New("wrong repository")
	}
	i := f.issue
	return []github.ListedIssue{{Identity: i.Identity, State: i.State, Body: i.Body, Type: i.Type}}, ctx.Err()
}
func (f *onboardingRuntimeClient) DefaultBranch(ctx context.Context, r github.Repository) (github.Branch, error) {
	return github.Branch{Repository: r, Name: "main", SHA: strings.Repeat("a", 40)}, ctx.Err()
}
func (f *onboardingRuntimeClient) Branch(ctx context.Context, r github.Repository, name string) (github.Branch, error) {
	if f.branch == nil {
		return github.Branch{}, &github.MissingBranch{Repository: r, Name: name}
	}
	return *f.branch, ctx.Err()
}
func (f *onboardingRuntimeClient) CreateBranch(ctx context.Context, r github.Repository, name, sha string) (github.Branch, error) {
	f.creates++
	b := github.Branch{Repository: r, Name: name, SHA: sha}
	f.branch = &b
	return b, ctx.Err()
}
func (f *onboardingRuntimeClient) CompareCommits(context.Context, github.Repository, string, string) (github.Comparison, error) {
	f.t.Error("PR onboarding compared commits")
	return github.Comparison{}, errors.New("unexpected comparison")
}
func (f *onboardingRuntimeClient) SetProjectOption(ctx context.Context, p, id, field, option string) error {
	if p != "engineering" || field != "status" {
		f.t.Error(p, field)
		return errors.New("wrong Project field")
	}
	for node, item := range f.items {
		if item.ID == id {
			if node == "PR47" {
				f.statuses++
				if option != "in_progress" {
					f.t.Error(option)
					return errors.New("wrong initial Status")
				}
			}
			item.Values[field] = github.FieldValue{Kind: config.SingleSelect, OptionID: option}
			f.items[node] = item
			return ctx.Err()
		}
	}
	return errors.New("unknown item")
}
func (f *onboardingRuntimeClient) SetProjectDate(context.Context, string, string, string, string) error {
	f.t.Error("unexpected start date")
	return errors.New("unexpected start date")
}
func (f *onboardingRuntimeClient) SetProjectNumber(ctx context.Context, p, id, field string, v float64) error {
	for node, item := range f.items {
		if item.ID == id {
			item.Values[field] = github.FieldValue{Kind: config.Number, Number: v}
			f.items[node] = item
			return ctx.Err()
		}
	}
	return errors.New("unknown item")
}
func (f *onboardingRuntimeClient) ClearProjectNumber(context.Context, string, string, string) error {
	f.t.Error("unexpected number clear")
	return errors.New("unexpected number clear")
}
func (f *onboardingRuntimeClient) AddProjectItem(ctx context.Context, p, node string) (string, error) {
	f.adds++
	if p != "engineering" || node != "PR47" {
		f.t.Error(p, node)
		return "", errors.New("wrong membership identity")
	}
	f.items[node] = github.ProjectItem{ID: "PRITEM", ProjectID: p, Content: &github.Content{ID: node, Kind: "PullRequest"}, Values: map[string]github.FieldValue{}}
	return "PRITEM", ctx.Err()
}
func (f *onboardingRuntimeClient) ConvertPullRequestToDraft(ctx context.Context, id string) error {
	f.drafts++
	if id != f.pr.ID {
		f.t.Error(id)
		return errors.New("wrong PR identity")
	}
	f.pr.Draft = true
	if f.draftHook != nil {
		return f.draftHook(ctx)
	}
	return ctx.Err()
}
func (f *onboardingRuntimeClient) CurrentProjectItem(ctx context.Context, id string) (github.CurrentProjectItem, error) {
	if id != "PRITEM" {
		f.t.Error(id)
		return github.CurrentProjectItem{}, errors.New("wrong Project item identity")
	}
	return github.CurrentProjectItem{ID: id, ProjectID: "engineering", ContentKind: "PullRequest"}, ctx.Err()
}
func (f *onboardingRuntimeClient) CurrentPullRequestProjectItem(ctx context.Context, id string) (github.PullRequestProjectItem, error) {
	if id != "PRITEM" {
		f.t.Error(id)
		return github.PullRequestProjectItem{}, errors.New("wrong PR item identity")
	}
	i := f.pr.Identity
	return github.PullRequestProjectItem{ID: id, ProjectID: "engineering", ContentKind: "PullRequest", PullRequest: &i}, ctx.Err()
}
func (f *onboardingRuntimeClient) User(ctx context.Context, login string) (github.Actor, error) {
	// A different current author must reach deterministic binding rejection,
	// rather than terminating a worker goroutine in this test provider.
	return f.user, ctx.Err()
}
func (f *onboardingRuntimeClient) OrganizationMembership(ctx context.Context, o config.Organization, _ string) (github.Membership, error) {
	return github.Membership{Organization: o, User: f.user, State: "active"}, ctx.Err()
}
func (f *onboardingRuntimeClient) RepositoryPermission(ctx context.Context, r github.Repository, _ string) (github.Permission, error) {
	return github.Permission{Repository: r, User: f.user, Base: "write"}, ctx.Err()
}

func onboardingNotification(t *testing.T, r *runtime, id, event string) {
	t.Helper()
	var payload string
	switch event {
	case "issues":
		payload = `{"repository":{"owner":{"login":"parametron-io"},"name":"repo"},"issue":{"number":46}}`
	case "projects_v2_item":
		payload = `{"action":"edited","projects_v2_item":{"node_id":"PRITEM","project_node_id":"engineering"}}`
	default:
		payload = `{"repository":{"owner":{"login":"parametron-io"},"name":"repo"},"pull_request":{"number":47,"draft":false,"body":"Target: #999"}}`
	}
	_, err := r.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: event, Payload: []byte(payload), ReceivedAt: time.Unix(1700000000, 0)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcceptedOnboardingComposedWorkerAndRestart(t *testing.T) {
	for _, runner := range []bool{false, true} {
		t.Run(fmt.Sprintf("runner_%t", runner), func(t *testing.T) {
			c, f := onboardingRuntimeFixture(t)
			if runner {
				c.Runner = &semantic.Fake{ClassifyPRFunc: func(context.Context, semantic.Input) (semantic.Result, error) {
					t.Error("Classification false called Runner")
					return semantic.Result{}, errors.New("unexpected Runner invocation")
				}}
			}
			r := openRuntime(t, c)
			onboardingNotification(t, r, "branch", "issues")
			step(t, r)
			state(t, r, "branch", storage.Completed, 1)
			if f.creates != 1 {
				t.Fatal("no actual branch provenance", f.creates)
			}
			onboardingNotification(t, r, "pr", "pull_request")
			step(t, r)
			state(t, r, "pr", storage.Completed, 1)
			if f.adds != 1 || f.statuses != 1 || f.drafts != 1 || f.prReads < 2 {
				t.Fatal(f.adds, f.statuses, f.drafts, f.prReads)
			}
			p, err := r.store.Provenance(context.Background(), "pr_onboarding_completed_v1", "parametron-io/repo/pull_request/47")
			if err != nil {
				t.Fatal(err)
			}
			if err = r.store.Close(); err != nil {
				t.Fatal(err)
			}
			r = openRuntime(t, c)
			f.pr.Draft = false
			item := f.items["PR47"]
			item.Values["status"] = github.FieldValue{Kind: config.SingleSelect, OptionID: "in_review"}
			f.items["PR47"] = item
			onboardingNotification(t, r, "ready", "pull_request")
			step(t, r)
			state(t, r, "ready", storage.Completed, 1)
			onboardingNotification(t, r, "project", "projects_v2_item")
			step(t, r)
			e := state(t, r, "project", storage.Completed, 1)
			if e.Resource == nil || e.Resource.Kind != "pull_request" || e.Resource.NodeID != "PR47" {
				t.Fatal(e)
			}
			delete(f.items, "PR47")
			onboardingNotification(t, r, "repair", "pull_request")
			step(t, r)
			state(t, r, "repair", storage.Completed, 1)
			if f.adds != 2 || f.statuses != 1 || f.drafts != 1 || f.pr.Draft {
				t.Fatal("replayed ingress after readiness", f.adds, f.statuses, f.drafts)
			}
			later, err := r.store.Provenance(context.Background(), p.Namespace, p.Key)
			if err != nil || string(later.Metadata) != string(p.Metadata) || later.DeliveryID != "pr" || !later.CreatedAt.Equal(p.CreatedAt) {
				t.Fatal("completion changed", err)
			}
			f.pr.Author = &github.Actor{ID: "other", Login: "outsider", Type: "User"}
			delete(f.items, "PR47")
			onboardingNotification(t, r, "rejected", "pull_request")
			step(t, r)
			e = state(t, r, "rejected", storage.Failed, 1)
			if e.State.ErrorCategory != "pr_onboarding_rejected" || f.adds != 2 {
				t.Fatal(e.State, f.adds)
			}
		})
	}
}

func TestComposedOnboardingProviderRetryAndMissingAuthority(t *testing.T) {
	c, f := onboardingRuntimeFixture(t)
	now := time.Unix(1700000000, 0)
	c.Clock = func() time.Time { return now }
	r := openRuntime(t, c)
	// Observed lookalike branch, Draft and Project membership are insufficient.
	b := github.Branch{Repository: f.repo, Name: f.pr.HeadRef, SHA: f.pr.HeadSHA}
	f.branch = &b
	f.pr.Draft = true
	f.items["PR47"] = github.ProjectItem{ID: "PRITEM", ProjectID: "engineering", Content: &github.Content{ID: "PR47", Kind: "PullRequest"}, Values: map[string]github.FieldValue{"status": {Kind: config.SingleSelect, OptionID: "in_progress"}}}
	onboardingNotification(t, r, "manual", "pull_request")
	step(t, r)
	e := state(t, r, "manual", storage.Failed, 1)
	if e.State.ErrorCategory != "pr_onboarding_rejected" || f.adds+f.statuses+f.drafts != 0 {
		t.Fatal(e.State)
	}
	if _, err := r.store.Provenance(context.Background(), "pr_onboarding_completed_v1", "parametron-io/repo/pull_request/47"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("observed state manufactured completion", err)
	}
	f.branch = nil
	f.pr.Draft = false
	delete(f.items, "PR47")
	onboardingNotification(t, r, "branch", "issues")
	step(t, r)
	state(t, r, "branch", storage.Completed, 1)
	f.prFailure = &github.Error{Category: github.Transient}
	onboardingNotification(t, r, "retry", "pull_request")
	step(t, r)
	e = state(t, r, "retry", storage.Retryable, 1)
	if e.State.NextAttemptAt == nil {
		t.Fatal(e)
	}
	if err := r.store.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(c.RetryDelay)
	f.prFailure = nil
	r = openRuntime(t, c)
	step(t, r)
	state(t, r, "retry", storage.Completed, 2)
	if f.adds != 1 || f.statuses != 1 || f.drafts != 1 {
		t.Fatal(f.adds, f.statuses, f.drafts)
	}
}

func TestOnboardingErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err   error
		cat   string
		retry bool
	}{{po.ErrInvalid, "pr_onboarding_invalid", false}, {po.ErrRejected, "pr_onboarding_rejected", false}, {po.ErrChanged, "pr_onboarding_changed", true}, {po.ErrProject, "pr_onboarding_project", false}, {po.ErrProvenance, "pr_onboarding_provenance", false}, {po.ErrAmbiguous, "pr_onboarding_ambiguous", false}, {po.ErrStorage, "pr_onboarding_storage", false}} {
		for _, err := range []error{tc.err, fmt.Errorf("wrapped: %w", tc.err)} {
			got := classify(err)
			if got.Category != tc.cat || got.Retryable != tc.retry {
				t.Fatal(got, tc)
			}
		}
	}
}

// Static proof of the production narrow interface set, without an implicit
// semantic Mutator.
var _ appOnboardingCapabilities = (*onboardingRuntimeClient)(nil)

type onboardingCore interface {
	github.Client
	github.IssueLister
	github.Branches
	github.CommitComparison
	github.LifecycleFields
	github.NumericFields
	github.ProjectItemReader
}
type onboardingAdder interface {
	AddProjectItem(context.Context, string, string) (string, error)
}

func TestEngineeringRequiresEachOnboardingCapability(t *testing.T) {
	for _, missing := range []string{"draft", "developers", "pr_items", "membership"} {
		t.Run(missing, func(t *testing.T) {
			c, f := onboardingRuntimeFixture(t)
			var core onboardingCore = f
			switch missing {
			case "draft":
				c.Client = struct {
					onboardingCore
					github.DeveloperReads
					github.PullRequestProjectItemReader
					onboardingAdder
				}{core, f, f, f}
			case "developers":
				c.Client = struct {
					onboardingCore
					github.DraftConverter
					github.PullRequestProjectItemReader
					onboardingAdder
				}{core, f, f, f}
			case "pr_items":
				c.Client = struct {
					onboardingCore
					github.DraftConverter
					github.DeveloperReads
					onboardingAdder
				}{core, f, f, f}
			case "membership":
				c.Client = struct {
					onboardingCore
					github.DraftConverter
					github.DeveloperReads
					github.PullRequestProjectItemReader
				}{core, f, f, f}
			}
			_, err := compose(context.Background(), c)
			if !errors.Is(err, po.ErrInvalid) {
				t.Fatal(err)
			}
			if f.adds+f.statuses+f.drafts+f.creates+f.prReads != 0 {
				t.Fatal("construction performed workflow I/O")
			}
		})
	}
}

func TestComposedOnboardingCancellationNeverReplaysUnresolvedDraft(t *testing.T) {
	c, f := onboardingRuntimeFixture(t)
	r := openRuntime(t, c)
	onboardingNotification(t, r, "branch", "issues")
	step(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.draftHook = func(ctx context.Context) error { cancel(); return ctx.Err() }
	onboardingNotification(t, r, "cancelled", "pull_request")
	if err := r.worker.Step(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	state(t, r, "cancelled", storage.Pending, 1)
	if f.drafts != 1 || f.statuses != 1 {
		t.Fatal("failed to reach interruption", f.drafts, f.statuses)
	}
	if err := r.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.draftHook = nil
	f.pr.Draft = false
	r = openRuntime(t, c)
	if err := r.worker.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	step(t, r)
	e := state(t, r, "cancelled", storage.Failed, 2)
	if e.State.ErrorCategory != "pr_onboarding_ambiguous" || f.drafts != 1 || f.statuses != 1 || f.pr.Draft {
		t.Fatal("ambiguous history replayed", e.State, f.drafts, f.statuses)
	}
}

func TestComposedOnboardingRecoverInterruptedClaim(t *testing.T) {
	c, f := onboardingRuntimeFixture(t)
	r := openRuntime(t, c)
	onboardingNotification(t, r, "branch", "issues")
	step(t, r)
	onboardingNotification(t, r, "interrupted", "pull_request")
	if _, err := r.store.Claim(context.Background(), "interrupted", time.Now()); err != nil {
		t.Fatal(err)
	}
	state(t, r, "interrupted", storage.Processing, 1)
	if err := r.store.Close(); err != nil {
		t.Fatal(err)
	}
	r = openRuntime(t, c)
	if err := r.worker.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	state(t, r, "interrupted", storage.Pending, 1)
	step(t, r)
	state(t, r, "interrupted", storage.Completed, 2)
	if f.adds != 1 || f.statuses != 1 || f.drafts != 1 {
		t.Fatal(f.adds, f.statuses, f.drafts)
	}
}

func TestComposedOnboardingWithExplicitPRSemanticRunner(t *testing.T) {
	c, f := onboardingRuntimeFixture(t)
	now := time.Unix(1700000000, 0)
	c.Clock = func() time.Time { return now }
	catalog, err := semantic.LoadCatalog(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	asset, err := catalog.Lookup(semantic.ClassifyPR)
	if err != nil {
		t.Fatal(err)
	}
	models, consumers := 0, 0
	f.pr.Body = "Target: #46\nClassification: true"
	c.Runner = &semantic.Fake{ClassifyPRFunc: func(ctx context.Context, in semantic.Input) (semantic.Result, error) {
		models++
		if in.Body != f.pr.Body {
			t.Error("Runner used payload body")
		}
		return semantic.Result{Output: json.RawMessage(`{"labels":[]}`), Provenance: semantic.Provenance{Capability: semantic.ClassifyPR, Provider: "fake", Model: "cheap", PromptIdentity: asset.Prompt.Identity, PromptDigest: asset.Prompt.Digest, SchemaIdentity: asset.Schema.Identity, SchemaDigest: asset.Schema.Digest}}, ctx.Err()
	}}
	c.SemanticConsumer = semanticConsumerFunc(func(ctx context.Context, in semanticflow.PolicyInput) error {
		if in.Current.Observed.Resource.Kind == "issue" {
			return ctx.Err()
		}
		consumers++
		if in.Classification.State() != semanticflow.Accepted {
			t.Error("unvalidated semantic handoff")
		}
		if consumers == 1 {
			return &github.Error{Category: github.Transient}
		}
		return ctx.Err()
	})
	r := openRuntime(t, c)
	onboardingNotification(t, r, "branch", "issues")
	step(t, r)
	state(t, r, "branch", storage.Completed, 1)
	onboardingNotification(t, r, "pr", "pull_request")
	step(t, r)
	state(t, r, "pr", storage.Retryable, 1)
	if f.adds+f.statuses+f.drafts != 0 {
		t.Fatal("failed semantic consumer bypassed")
	}
	now = now.Add(c.RetryDelay)
	step(t, r)
	state(t, r, "pr", storage.Completed, 2)
	onboardingNotification(t, r, "duplicate", "pull_request")
	step(t, r)
	state(t, r, "duplicate", storage.Completed, 1)
	if models != 1 || consumers != 3 || f.adds != 1 || f.statuses != 1 || f.drafts != 1 {
		t.Fatal(models, consumers, f.adds, f.statuses, f.drafts)
	}
	if _, ok := any(f).(github.Mutator); ok {
		t.Fatal("onboarding fixture accidentally granted semantic Mutator")
	}
}
