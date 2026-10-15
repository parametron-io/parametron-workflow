package app

import (
	"fmt"
	"reflect"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ef "github.com/parametron-io/parametron-workflow/internal/engineeringflow"
	er "github.com/parametron-io/parametron-workflow/internal/engineeringreconcile"
	et "github.com/parametron-io/parametron-workflow/internal/engineeringterminal"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/prcompanionreconcile"
	"github.com/parametron-io/parametron-workflow/internal/prcompletion"
	"github.com/parametron-io/parametron-workflow/internal/prdiscovery"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/prreview"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/semanticreconcile"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

func missingCapability(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	return r.Kind() == reflect.Pointer && r.IsNil()
}

// Engineering uses narrow capabilities on the same shared client/transport,
// deployment and Store. Validation completes before serving HTTP.
func composeEngineering(c Config, d config.ResolvedConfig, store *storage.Store, resolver worker.ResourceResolver, reader *observe.Processor) (*ef.Flow, error) {
	lister, ok := c.Client.(github.IssueLister)
	if !ok || missingCapability(lister) {
		return nil, er.ErrInvalid
	}
	branches, ok := c.Client.(github.Branches)
	if !ok || missingCapability(branches) {
		return nil, er.ErrInvalid
	}
	comparison, ok := c.Client.(github.CommitComparison)
	if !ok || missingCapability(comparison) {
		return nil, er.ErrInvalid
	}
	fields, ok := c.Client.(github.LifecycleFields)
	if !ok || missingCapability(fields) {
		return nil, er.ErrInvalid
	}
	numbers, ok := c.Client.(github.NumericFields)
	if !ok || missingCapability(numbers) {
		return nil, er.ErrInvalid
	}
	items, ok := c.Client.(github.ProjectItemReader)
	if !ok || missingCapability(items) {
		return nil, er.ErrInvalid
	}
	accepted, err := semanticflow.NewAcceptedLoader(store)
	if err != nil {
		return nil, err
	}
	snapshots, err := ec.New(ec.Config{Deployment: d, Primary: reader, Lister: lister, Accepted: accepted})
	if err != nil {
		return nil, err
	}
	branch, err := eb.New(eb.Config{Repositories: d.Repositories, Branches: branches, Store: store, Clock: c.Clock})
	if err != nil {
		return nil, err
	}
	activityCfg := ea.Config{Repositories: d.Repositories, Project: d.Engineering, Provenance: branch, Branches: branches, Comparison: comparison, Projects: reader, Fields: fields, Store: store}
	activity, err := ea.New(activityCfg)
	if err != nil {
		return nil, err
	}
	prItems, ok := c.Client.(github.PullRequestProjectItemReader)
	if !ok || missingCapability(prItems) {
		return nil, pronboarding.ErrInvalid
	}
	developers, ok := c.Client.(github.DeveloperReads)
	if !ok || missingCapability(developers) {
		return nil, pronboarding.ErrInvalid
	}
	draft, ok := c.Client.(github.DraftConverter)
	if !ok || missingCapability(draft) {
		return nil, pronboarding.ErrInvalid
	}
	projects, ok := c.Client.(pronboarding.Projects)
	if !ok || missingCapability(projects) {
		return nil, pronboarding.ErrInvalid
	}
	binding, err := prbinding.New(prbinding.Config{Deployment: d, Primary: reader, Context: snapshots, Completion: branch, Branches: branches, Developers: developers})
	if err != nil {
		return nil, err
	}
	onboarding, err := pronboarding.New(pronboarding.Config{Deployment: d, Binding: binding, Reader: reader, Projects: projects, Items: prItems, Draft: draft, Store: store, Clock: c.Clock})
	if err != nil {
		return nil, err
	}
	native, ok := c.Client.(github.NativeLifecycleReader)
	if !ok || missingCapability(native) {
		return nil, prcompletion.ErrInvalid
	}
	issueStates, ok := c.Client.(github.IssueStates)
	if !ok || missingCapability(issueStates) {
		return nil, prcompletion.ErrInvalid
	}
	review, err := prreview.New(prreview.Config{Native: native, Deployment: d, Binding: binding, Onboarding: onboarding, Reader: reader, Reviews: c.Client, PRItems: prItems, Items: items, Status: fields, Draft: draft, Store: store, Clock: c.Clock})
	if err != nil {
		return nil, err
	}
	completion, err := prcompletion.New(prcompletion.Config{Deployment: d, Reader: reader, Snapshots: snapshots, Onboarding: onboarding, Binding: binding, Review: review, Native: native, Issues: issueStates, Developers: developers, Items: items, PRItems: prItems, Fields: fields, Store: store, Clock: c.Clock})
	if err != nil {
		return nil, err
	}
	reconciler, err := er.New(er.Config{Completion: completion, Review: review, Project: d.Engineering, Projects: reader, Activity: activity, Fields: fields, Numbers: numbers})
	if err != nil {
		return nil, err
	}
	terminal, err := et.New(et.Config{Project: d.Engineering, BugTracker: d.BugTracker.ID, Snapshots: snapshots, Reader: reader, Canonical: reconciler, Issues: issueStates, Items: items, Fields: fields})
	if err != nil {
		return nil, err
	}
	flowCfg := ef.Config{Completion: completion, Terminal: terminal, Review: review, Onboarding: onboarding, PRProjectItems: prItems, Deployment: d, Resolver: resolver, Reader: reader, Foundation: reader, Snapshot: snapshots, Reconciler: reconciler, Activity: activityCfg, ProjectItems: items, Branches: branch}
	prLister, ok := c.Client.(github.PullRequestLister)
	if !ok || missingCapability(prLister) {
		return nil, fmt.Errorf("%w: missing PullRequestLister", prcompanionreconcile.ErrInvalid)
	}
	repositories := make([]github.Repository, 0, len(d.Repositories))
	for _, repository := range d.Repositories {
		repositories = append(repositories, github.Repository{Owner: repository.Owner, Name: repository.Name, ID: repository.ID})
	}
	discovery, err := prdiscovery.New(prdiscovery.Config{Repositories: repositories, Project: d.Engineering.ID, BugProject: d.BugTracker.ID, Lister: prLister, Binding: binding, Onboarding: onboarding, Reader: reader})
	if err != nil {
		return nil, err
	}
	flowCfg.Companions, err = prcompanionreconcile.New(prcompanionreconcile.Config{Repositories: repositories, Project: d.Engineering, BugProject: d.BugTracker.ID, Discovery: discovery, Snapshots: snapshots, Reader: reader, Numbers: numbers, Store: store, Clock: c.Clock})
	if err != nil {
		return nil, err
	}
	if c.Runner != nil {
		consumer := c.SemanticConsumer
		mutator := c.Mutator
		if mutator != nil {
			consumer, err = semanticreconcile.New(d, reader, mutator)
			if err != nil {
				return nil, err
			}
		}
		if consumer == nil {
			consumer = SemanticFoundationSink{}
		}
		clock := c.Clock
		if clock == nil {
			clock = time.Now
		}
		coordinator, err := semanticflow.New(semanticflow.Config{Store: store, Runner: c.Runner, Primary: reader, Consumer: consumer, Clock: clock})
		if err != nil {
			return nil, err
		}
		foundation, err := observe.NewProcessor(d, c.Client, coordinator)
		if err != nil {
			return nil, err
		}
		flowCfg.Foundation, flowCfg.Enricher, flowCfg.Semantic = foundation, coordinator, consumer
	}
	return ef.New(flowCfg)
}
