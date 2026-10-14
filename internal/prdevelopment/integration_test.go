package prdevelopment

import (
	"context"
	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"path/filepath"
	"testing"
	"time"
)

// Real onboarding writes and validates its own provenance; Development receives
// only the exported completion interface. Lifecycle capabilities panic if used
// after onboarding, making cross-boundary effects visible.
type onboardingIntegration struct {
	*developmentFixture
	lifecycle int
	forbidden bool
	status    bool
}

func (f *onboardingIntegration) ReadCurrent(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	o, err := f.developmentFixture.ReadCurrent(ctx, r)
	if f.status {
		o.Projects[0].Items[0].Fields = map[config.FieldRole]observe.FieldValue{config.Status: {FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "PROGRESS"}, StatusRole: config.InProgress}}
	}
	return o, err
}

func (f *onboardingIntegration) AddProjectItem(context.Context, string, string) (string, error) {
	panic("membership is already present")
}
func (f *onboardingIntegration) SetProjectOption(context.Context, string, string, string, string) error {
	if f.forbidden {
		panic("Development invoked Status")
	}
	f.lifecycle++
	f.status = true
	return nil
}
func (f *onboardingIntegration) ConvertPullRequestToDraft(context.Context, string) error {
	if f.forbidden {
		panic("Development invoked Draft")
	}
	f.lifecycle++
	f.in.PR.Draft = true
	return nil
}
func (f *onboardingIntegration) CurrentPullRequestProjectItem(context.Context, string) (github.PullRequestProjectItem, error) {
	pr := f.in.PR.Identity
	return github.PullRequestProjectItem{ID: "ITEM", ProjectID: "ENG", ContentKind: "PullRequest", PullRequest: &pr}, nil
}
func TestDevelopmentConsumesValidatedOnboardingCompletion(t *testing.T) {
	ctx := context.Background()
	base, _ := fixtureController(t)
	f := &onboardingIntegration{developmentFixture: base}
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "completion.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deployment := config.ResolvedConfig{Organization: config.Organization{ID: "ORG", Login: "parametron-io"}, Repositories: []config.Repository{f.in.PR.Identity.Repository}, Engineering: config.ResolvedProject{ID: "ENG", Fields: map[config.FieldRole]string{config.Status: "STATUS"}, StatusOptions: map[config.StatusRole]string{config.InProgress: "PROGRESS"}}, BugTracker: config.ResolvedProject{ID: "BUG"}}
	clock := func() time.Time { return time.Unix(100, 0) }
	validator, facts := actualBinding(t, f, deployment)
	onboarding, err := pronboarding.New(pronboarding.Config{Deployment: deployment, Binding: validator, Reader: f, Projects: f, Items: f, Draft: f, Store: store, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	r := resource(f.in.PR.Identity, "pull_request")
	_, err = store.InsertDelivery(ctx, storage.Delivery{ID: "integration", EventName: "pull_request", Payload: []byte(`{}`), ReceivedAt: clock(), Resource: &r})
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.Event(ctx, "integration")
	if err != nil {
		t.Fatal(err)
	}
	if err = onboarding.Handle(ctx, event); err != nil {
		t.Fatal(err)
	}
	completion, err := onboarding.Completed(ctx, r)
	if err != nil || !completion.Completed {
		t.Fatalf("completion=%+v err=%v", completion, err)
	}
	before := f.lifecycle
	f.forbidden = true
	cfg := f.config()
	cfg.Binding = validator
	cfg.Onboarding = onboarding
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err = c.Reconcile(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.writes) != 1 || f.lifecycle != before {
		t.Fatalf("writes=%v lifecycle=%d", f.writes, f.lifecycle)
	}
	// Valid historical completion cannot authorize a currently rejected PR.
	facts.denied = true
	f.in.References.Manual = nil
	if err = c.Reconcile(ctx, r); err == nil || len(f.writes) != 1 {
		t.Fatalf("historical completion authorized mutation: %v", err)
	}
}

type currentBindingFacts struct {
	*onboardingIntegration
	denied     bool
	deployment config.ResolvedConfig
}

func (f *currentBindingFacts) ReadPrimary(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	o, err := f.ReadCurrent(ctx, r)
	if r.Kind == "issue" {
		o.Resource = resource(f.in.Target.Identity, "issue")
	} else {
		o.Resource = resource(f.in.PR.Identity, "pull_request")
	}
	o.Projects = nil
	return o, err
}
func (f *currentBindingFacts) User(ctx context.Context, login string) (github.Actor, error) {
	return f.in.Binding.Evidence.Developer, ctx.Err()
}
func (f *currentBindingFacts) OrganizationMembership(ctx context.Context, org config.Organization, login string) (github.Membership, error) {
	return github.Membership{Organization: org, User: f.in.Binding.Evidence.Developer, State: "active"}, ctx.Err()
}
func (f *currentBindingFacts) RepositoryPermission(ctx context.Context, repo github.Repository, login string) (github.Permission, error) {
	base := "write"
	if f.denied {
		base = "read"
	}
	return github.Permission{Repository: repo, User: f.in.Binding.Evidence.Developer, Base: base}, ctx.Err()
}
func (f *currentBindingFacts) AcceptedIssue(context.Context, storage.Resource) (sp.AcceptedIssue, bool, error) {
	return sp.AcceptedIssue{}, false, nil
}

type branchCompletion struct{ b prbinding.Evidence }

func (f branchCompletion) Completed(ctx context.Context, r storage.Resource) (eb.Completion, error) {
	work := resource(f.b.Target, "issue")
	work.NodeID = ""
	return eb.Completion{Completed: true, CreatedAt: time.Unix(1, 0), Evidence: eb.Evidence{Version: 1, WorkItem: work, Repository: f.b.Target.Repository, Branch: f.b.Branch.Name, BaseRef: "main", BaseSHA: f.b.Branch.SHA}}, ctx.Err()
}
func actualBinding(t *testing.T, f *onboardingIntegration, d config.ResolvedConfig) (*prbinding.Validator, *currentBindingFacts) {
	t.Helper()
	d.IssueTypes = map[string]string{"Task": "TASK", "Feature": "FEATURE", "Phase": "PHASE", "Bug": "BUG"}
	f.in.Target.Body = "Classification: false"
	f.in.Target.Type = &github.IssueType{ID: "TASK", Name: "Task"}
	facts := &currentBindingFacts{onboardingIntegration: f, deployment: d}
	lister := github.ListerFake{ListIssuesFunc: func(ctx context.Context, repo github.Repository) ([]github.ListedIssue, error) {
		i := f.in.Target
		return []github.ListedIssue{{Identity: i.Identity, Title: i.Title, Body: i.Body, State: i.State, Type: i.Type}}, ctx.Err()
	}}
	resolver, err := ec.New(ec.Config{Deployment: d, Primary: facts, Lister: &lister, Accepted: facts})
	if err != nil {
		t.Fatal(err)
	}
	branches := github.BranchFake{BranchFunc: func(ctx context.Context, repo github.Repository, name string) (github.Branch, error) {
		return f.in.Binding.Evidence.Branch, ctx.Err()
	}}
	validator, err := prbinding.New(prbinding.Config{Deployment: d, Primary: facts, Context: resolver, Completion: branchCompletion{f.in.Binding.Evidence}, Branches: branches, Developers: facts})
	if err != nil {
		t.Fatal(err)
	}
	return validator, facts
}
