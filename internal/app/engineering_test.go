package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"

	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ef "github.com/parametron-io/parametron-workflow/internal/engineeringflow"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	er "github.com/parametron-io/parametron-workflow/internal/engineeringreconcile"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

type appNarrowFields struct{ t *testing.T }

func (f appNarrowFields) AddProjectItem(context.Context, string, string) (string, error) {
	f.t.Error("Issue semantic composition unexpectedly added PR membership")
	return "", errors.New("unexpected onboarding")
}
func (f appNarrowFields) ConvertPullRequestToDraft(context.Context, string) error {
	f.t.Error("Issue semantic composition unexpectedly converted a PR")
	return errors.New("unexpected onboarding")
}
func (f appNarrowFields) CurrentPullRequestProjectItem(context.Context, string) (github.PullRequestProjectItem, error) {
	f.t.Error("Issue semantic composition unexpectedly read a PR item")
	return github.PullRequestProjectItem{}, errors.New("unexpected onboarding")
}
func (f appNarrowFields) User(context.Context, string) (github.Actor, error) {
	f.t.Error("Issue semantic composition unexpectedly checked a PR developer")
	return github.Actor{}, errors.New("unexpected onboarding")
}
func (f appNarrowFields) OrganizationMembership(context.Context, config.Organization, string) (github.Membership, error) {
	f.t.Error("Issue semantic composition unexpectedly checked membership")
	return github.Membership{}, errors.New("unexpected onboarding")
}
func (f appNarrowFields) RepositoryPermission(context.Context, github.Repository, string) (github.Permission, error) {
	f.t.Error("Issue semantic composition unexpectedly checked permission")
	return github.Permission{}, errors.New("unexpected onboarding")
}

// Deliberately grants only onboarding writes; it does not implement Mutator.
type appOnboardingCapabilities interface {
	github.DraftConverter
	github.DeveloperReads
	github.PullRequestProjectItemReader
	AddProjectItem(context.Context, string, string) (string, error)
}

func (f appNarrowFields) SetProjectOption(context.Context, string, string, string, string) error {
	f.t.Error("unexpected Engineering Status write on CLOSED Issue")
	return nil
}
func (f appNarrowFields) SetProjectDate(context.Context, string, string, string, string) error {
	f.t.Error("unexpected Engineering Start date write")
	return nil
}
func (f appNarrowFields) SetProjectNumber(context.Context, string, string, string, float64) error {
	f.t.Error("unexpected Engineering numeric write on CLOSED Issue")
	return nil
}
func (f appNarrowFields) ClearProjectNumber(context.Context, string, string, string) error {
	f.t.Error("absent Roadmap value must not clear")
	return nil
}
func (f appNarrowFields) CurrentProjectItem(ctx context.Context, id string) (github.CurrentProjectItem, error) {
	if id != "item" {
		f.t.Fatalf("terminal consistency read unrelated item %q", id)
	}
	i := github.Identity{ID: "current", Repository: github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}, Number: 39}
	return github.CurrentProjectItem{ID: id, ProjectID: "engineering", ContentKind: "Issue", Issue: &i}, ctx.Err()
}
func (f appNarrowFields) CompareCommits(context.Context, github.Repository, string, string) (github.Comparison, error) {
	f.t.Error("ordinary CLOSED Issue compared commits")
	return github.Comparison{}, nil
}

func TestEngineeringSemanticCompositionUsesOnlyExplicitMutationAuthority(t *testing.T) {
	for _, mode := range []string{"runner only", "explicit Mutator", "custom consumer"} {
		t.Run(mode, func(t *testing.T) {
			c := semanticFixture(t)
			base := c.Client.(*github.Fake)
			discover := base.DiscoverSchemaFunc
			base.DiscoverSchemaFunc = func(ctx context.Context, source config.SourceConfig) (config.Schema, error) {
				s, err := discover(ctx, source)
				for _, typ := range []string{"Phase", "Task", "Feature", "Bug"} {
					s.IssueTypes = append(s.IssueTypes, config.IssueType{ID: "type-" + typ, Name: typ})
				}
				for i := range s.Projects {
					for j := range s.Projects[i].Fields {
						f := &s.Projects[i].Fields[j]
						names := []string{}
						if f.Name == "priority" {
							names = []string{"Critical", "High", "Medium", "Low"}
						}
						if f.Name == "effort" {
							names = []string{"XS", "S", "M", "L", "XL", "Unknown"}
						}
						for _, n := range names {
							f.Options = append(f.Options, config.Option{ID: n, Name: n})
						}
					}
				}
				return s, err
			}
			typ := github.IssueType{ID: "type-Bug", Name: "Bug"}
			current := func(n int) github.Issue {
				return github.Issue{Identity: github.Identity{ID: "current", Repository: github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}, Number: n}, State: "CLOSED", Title: "current title", Body: "Classification: true", Type: &typ}
			}
			base.IssueFunc = func(ctx context.Context, r github.Ref) (github.Issue, error) { return current(r.Number), ctx.Err() }
			base.ProjectItemsFunc = func(ctx context.Context, node, project string, fields map[string]config.FieldKind) ([]github.ProjectItem, error) {
				if project != "engineering" {
					return nil, nil
				}
				return []github.ProjectItem{{ID: "item", ProjectID: project, Content: &github.Content{ID: node, Kind: "Issue"}, Values: map[string]github.FieldValue{"status": {Kind: config.SingleSelect, OptionID: "done"}, "priority": {Kind: config.SingleSelect, OptionID: "Medium"}, "effort": {Kind: config.SingleSelect, OptionID: "Unknown"}}}}, ctx.Err()
			}
			catalog, err := semantic.LoadCatalog(os.DirFS("../.."))
			if err != nil {
				t.Fatal(err)
			}
			asset, err := catalog.Lookup(semantic.ClassifyIssue)
			if err != nil {
				t.Fatal(err)
			}
			models, custom, mutations := 0, 0, 0
			c.Runner = &semantic.Fake{ClassifyIssueFunc: func(context.Context, semantic.Input) (semantic.Result, error) {
				models++
				return semantic.Result{Output: json.RawMessage(`{"type":"Task","labels":[],"priority":"Medium","effort":"Unknown"}`), Provenance: semantic.Provenance{Capability: semantic.ClassifyIssue, Provider: "fake", Model: "cheap", PromptIdentity: asset.Prompt.Identity, PromptDigest: asset.Prompt.Digest, SchemaIdentity: asset.Schema.Identity, SchemaDigest: asset.Schema.Digest}}, nil
			}}
			if mode == "explicit Mutator" {
				c.Mutator = &github.MutationFake{SetIssueTypeFunc: func(ctx context.Context, node, id string) error {
					if node != "current" || id != "type-Task" {
						t.Error(node, id)
					}
					mutations++
					typ = github.IssueType{ID: id, Name: "Task"}
					return ctx.Err()
				}}
			}
			if mode == "custom consumer" {
				c.SemanticConsumer = semanticConsumerFunc(func(ctx context.Context, in semanticflow.PolicyInput) error {
					custom++
					if in.Classification.State() != semanticflow.Accepted {
						t.Error("unaccepted handoff")
					}
					return ctx.Err()
				})
			}
			narrow := appNarrowFields{t}
			surfaces := newRuntimeSurfaces(base)
			c.FeedbackActor = runtimeBot
			// Preserve this test's semantic-only purpose with independently
			// correlated native closure authority for the eventual Task.
			if err := surfaces.closureComment(current(39).Identity, "Force-Close: semantic composition fixture"); err != nil {
				t.Fatal(err)
			}
			lister := &github.ListerFake{ListIssuesFunc: func(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
				i := current(39)
				return []github.ListedIssue{{Identity: i.Identity, Title: i.Title, Body: i.Body, State: i.State, Type: i.Type}}, ctx.Err()
			}}
			c.Client = engineeringCapabilityClient{PullRequestLister: &github.PullRequestListerFake{ListPullRequestsFunc: func(ctx context.Context, _ github.Repository) ([]github.ListedPullRequest, error) {
				return nil, ctx.Err()
			}}, Client: base, IssueLister: lister, Branches: github.BranchFake{}, CommitComparison: narrow, LifecycleFields: narrow, NumericFields: narrow, ProjectItemReader: narrow, appOnboardingCapabilities: narrow, NativeLifecycleReader: base, IssueStates: base, runtimeSurfaces: surfaces}
			c.Engineering = true
			r := openRuntime(t, c)
			for _, id := range []string{"first", "later"} {
				_, err := r.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: "issues", Payload: []byte(`{"repository":{"owner":{"login":"parametron-io"},"name":"repo"},"issue":{"number":39}}`), ReceivedAt: time.Unix(1700000000, 0)})
				if err != nil {
					t.Fatal(err)
				}
				step(t, r)
				state(t, r, id, storage.Completed, 1)
			}
			if models != 1 {
				t.Fatal("durable accepted completion reran model", models)
			}
			if mode == "explicit Mutator" {
				if mutations != 1 || custom != 0 {
					t.Fatal(mutations, custom)
				}
			} else if mutations != 0 || typ.Name != "Bug" {
				t.Fatal("narrow Engineering writes implied semantic authority", mutations, typ)
			}
			if mode == "custom consumer" && custom != 2 {
				t.Fatal(custom)
			}
		})
	}
}

// The CLI's production Transport must satisfy every narrow Engineering capability.
var _ interface {
	appOnboardingCapabilities
	github.Client
	github.IssueLister
	github.Branches
	github.CommitComparison
	github.LifecycleFields
	github.NumericFields
	github.ProjectItemReader
} = (*github.Transport)(nil)

func TestEngineeringFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		err   error
		cat   string
		retry bool
	}{
		{ec.ErrChanged, "engineering_changed", true}, {ec.ErrIncomplete, "engineering_incomplete", true},
		{ea.ErrStorage, "engineering_activity_storage", false}, {eb.ErrStorage, "engineering_branch_storage", false},
		{ec.ErrDeployment, "engineering_deployment", false}, {ec.ErrParentCycle, "engineering_parent_cycle", false}, {ec.ErrDependencyCycle, "engineering_dependency_cycle", false}, {ec.ErrMissing, "engineering_missing", false}, {ec.ErrInvalid, "engineering_graph", false},
		{ep.ErrInvalid, "engineering_policy", false}, {ep.ErrParent, "engineering_parent", false}, {ep.ErrLifecycle, "engineering_lifecycle", false}, {ep.ErrPhaseOrder, "engineering_phase_order", false},
		{eo.ErrRoadmapCapacity, "engineering_capacity", false}, {eo.ErrRoadmapHierarchy, "engineering_hierarchy", false}, {eo.ErrInvalid, "engineering_order", false}, {eo.ErrPhaseOrder, "engineering_phase_order", false}, {eo.ErrRoadmap, "engineering_roadmap", false},
		{ea.ErrProvenance, "engineering_activity_provenance", false}, {eb.ErrProvenance, "engineering_branch_provenance", false}, {eb.ErrAmbiguous, "engineering_branch_ambiguous", false}, {ea.ErrProject, "engineering_project", false}, {er.ErrProject, "engineering_project", false}, {ea.ErrInvalid, "engineering_activity", false}, {eb.ErrInvalid, "engineering_branch", false}, {er.ErrInvalid, "engineering_configuration", false}, {ef.ErrInvalid, "engineering_notification", false},
	} {
		t.Run(tc.cat+"/"+tc.err.Error(), func(t *testing.T) {
			for _, err := range []error{tc.err, fmt.Errorf("wrapped: %w", tc.err)} {
				got := classify(err)
				if got.Category != tc.cat || got.Retryable != tc.retry {
					t.Fatal(got, tc)
				}
			}
		})
	}
}

type engineeringCapabilityClient struct {
	*runtimeSurfaces
	github.PullRequestLister
	github.NativeLifecycleReader
	github.IssueStates
	appOnboardingCapabilities
	github.Client
	github.IssueLister
	github.Branches
	github.CommitComparison
	github.LifecycleFields
	github.NumericFields
	github.ProjectItemReader
}

func withoutEngineeringCapability(c engineeringCapabilityClient, name string) github.Client {
	switch name {
	case "lister":
		return struct {
			github.Client
			github.Branches
			github.CommitComparison
			github.LifecycleFields
			github.NumericFields
			github.ProjectItemReader
		}{c.Client, c.Branches, c.CommitComparison, c.LifecycleFields, c.NumericFields, c.ProjectItemReader}
	case "branches":
		return struct {
			github.Client
			github.IssueLister
			github.CommitComparison
			github.LifecycleFields
			github.NumericFields
			github.ProjectItemReader
		}{c.Client, c.IssueLister, c.CommitComparison, c.LifecycleFields, c.NumericFields, c.ProjectItemReader}
	case "comparison":
		return struct {
			github.Client
			github.IssueLister
			github.Branches
			github.LifecycleFields
			github.NumericFields
			github.ProjectItemReader
		}{c.Client, c.IssueLister, c.Branches, c.LifecycleFields, c.NumericFields, c.ProjectItemReader}
	case "fields":
		return struct {
			github.Client
			github.IssueLister
			github.Branches
			github.CommitComparison
			github.NumericFields
			github.ProjectItemReader
		}{c.Client, c.IssueLister, c.Branches, c.CommitComparison, c.NumericFields, c.ProjectItemReader}
	case "numbers":
		return struct {
			github.Client
			github.IssueLister
			github.Branches
			github.CommitComparison
			github.LifecycleFields
			github.ProjectItemReader
		}{c.Client, c.IssueLister, c.Branches, c.CommitComparison, c.LifecycleFields, c.ProjectItemReader}
	case "items":
		return struct {
			github.Client
			github.IssueLister
			github.Branches
			github.CommitComparison
			github.LifecycleFields
			github.NumericFields
		}{c.Client, c.IssueLister, c.Branches, c.CommitComparison, c.LifecycleFields, c.NumericFields}
	}
	panic(name)
}

func TestEngineeringEnabledRequiresEveryNarrowCapability(t *testing.T) {
	for _, name := range []string{"lister", "branches", "comparison", "fields", "numbers", "items"} {
		t.Run(name, func(t *testing.T) {
			c, f := fixture(t)
			// Capability checking occurs before any Engineering constructor uses these.
			caps := engineeringCapabilityClient{Client: f}
			c.Client = withoutEngineeringCapability(caps, name)
			c.Engineering = true
			_, err := compose(context.Background(), c)
			if !errors.Is(err, er.ErrInvalid) {
				t.Fatal(err)
			}
		})
	}
	// With Engineering disabled the old foundation works without these capabilities.
	c, _ := fixture(t)
	r := openRuntime(t, c)
	if r.worker == nil || r.store == nil {
		t.Fatal("foundation composition lost")
	}
}

func TestEngineeringStorageFailuresAreTerminalNotRetryLoops(t *testing.T) {
	for _, err := range []error{ea.ErrStorage, eb.ErrStorage} {
		got := classify(errors.Join(errors.New("private persistence diagnostic"), err))
		if got.Retryable || got.Category == "local_processor" {
			t.Fatal(got)
		}
		var _ worker.Classification = got
	}
}
