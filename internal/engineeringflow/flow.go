// Package engineeringflow owns one process-local deployment Engineering lane,
// bounded notification routing and fresh recomputation around first activity.
package engineeringflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"reflect"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	er "github.com/parametron-io/parametron-workflow/internal/engineeringreconcile"
	et "github.com/parametron-io/parametron-workflow/internal/engineeringterminal"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prcompanion"
	"github.com/parametron-io/parametron-workflow/internal/prcompletion"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/prreview"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

var ErrInvalid = errors.New("engineering flow: invalid composition or notification")

type SnapshotResolver interface {
	ResolveSnapshot(context.Context) (ec.Snapshot, error)
}
type Branches interface {
	EnsureFrom(context.Context, eb.Origin, ep.Input) (eb.Result, error)
}
type Enricher interface {
	Enrich(context.Context, observe.PolicyInput) (semanticflow.PolicyInput, error)
}
type Reader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
	ReadPrimary(context.Context, storage.Resource) (observe.ObservedState, error)
}
type Onboarding interface {
	Handle(context.Context, storage.Event) error
}
type Review interface {
	Handle(context.Context, storage.Event) error
	Converge(context.Context, storage.Resource) error
	TargetStatus(context.Context, storage.Resource) (config.StatusRole, bool, error)
}

type CompanionReconciler interface {
	Reconcile(context.Context, string) error
}

// Continuous observes repeatable invariants inside the existing Engineering lane.
// Implementations must not replay event-specific lifecycle effects.
type Continuous interface {
	Converge(context.Context, storage.Event) error
}

type Config struct {
	Continuous     Continuous
	Companions     CompanionReconciler
	Completion     *prcompletion.Controller
	Terminal       *et.Controller
	Review         Review
	Onboarding     Onboarding
	PRProjectItems github.PullRequestProjectItemReader
	Deployment     config.ResolvedConfig
	Resolver       worker.ResourceResolver
	Reader         Reader
	Foundation     worker.Processor
	Enricher       Enricher
	Semantic       semanticflow.Consumer
	Snapshot       SnapshotResolver
	Reconciler     *er.Reconciler
	Activity       ea.Config
	ProjectItems   github.ProjectItemReader
	Branches       Branches
}
type Flow struct {
	cfg  Config
	lane chan struct{}
}

func New(c Config) (*Flow, error) {
	if c.Continuous != nil && absent(c.Continuous) {
		return nil, ErrInvalid
	}
	if c.Companions != nil && absent(c.Companions) {
		return nil, ErrInvalid
	}
	if c.Completion != nil && c.Terminal == nil {
		return nil, ErrInvalid
	}
	if c.Review != nil && (absent(c.Review) || absent(c.Onboarding)) {
		return nil, ErrInvalid
	}
	if absent(c.Resolver) || absent(c.Reader) || absent(c.Foundation) || absent(c.Snapshot) || absent(c.Reconciler) || absent(c.ProjectItems) || absent(c.Branches) || (c.Enricher != nil && (absent(c.Enricher) || absent(c.Semantic))) {
		return nil, ErrInvalid
	}
	if c.Onboarding != nil && (absent(c.Onboarding) || absent(c.PRProjectItems)) {
		return nil, ErrInvalid
	}
	if _, err := ea.New(c.Activity); err != nil {
		return nil, err
	}
	c.Deployment.Repositories = append([]config.Repository(nil), c.Deployment.Repositories...)
	c.Activity.Repositories = append([]github.Repository(nil), c.Activity.Repositories...)
	c.Activity.Project.Fields = maps.Clone(c.Activity.Project.Fields)
	c.Activity.Project.StatusOptions = maps.Clone(c.Activity.Project.StatusOptions)
	return &Flow{cfg: c, lane: make(chan struct{}, 1)}, nil
}
func absent(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func:
		return r.IsNil()
	}
	return false
}
func (f *Flow) withLane(ctx context.Context, fn func() error) error {
	select {
	case f.lane <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-f.lane }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func (f *Flow) withCompanions(ctx context.Context, deliveryID string, fn func() error) error {
	return f.withLane(ctx, func() error {
		return f.convergeCompanions(ctx, deliveryID, fn())
	})
}

// Called only while holding the existing Engineering lane.
func (f *Flow) convergeCompanions(ctx context.Context, deliveryID string, err error) error {
	// Numeric convergence is independent of lifecycle routing and always starts
	// from new complete observations, including after partial lifecycle effects.
	// It cannot replay onboarding, review, Draft or native close actions.
	if f.cfg.Companions != nil && ctx.Err() == nil {
		if numericErr := f.cfg.Companions.Reconcile(ctx, deliveryID); numericErr != nil {
			// Do not let a prior transport error mask terminal ownership
			// ambiguity in the worker's provider-first classification.
			return numericErr
		}
	}
	return err
}

// Continuous consumers start from fresh authority after any partial effects.
// Numeric ownership failures retain precedence over later continuous failures.
func (f *Flow) convergeCurrent(ctx context.Context, e storage.Event, prior error) error {
	numeric := f.convergeCompanions(ctx, e.Delivery.ID, nil)
	var continuous error
	if f.cfg.Continuous != nil && ctx.Err() == nil {
		continuous = f.cfg.Continuous.Converge(ctx, e)
	}
	// Capacity and rejected actions are supported policy outcomes. A failed
	// explanation must be retryable without replaying their one-shot effects.
	if continuous != nil && (errors.Is(numeric, prcompanion.ErrCapacity) || numeric == nil && (prior == nil || errors.Is(prior, pronboarding.ErrRejected) || errors.Is(prior, prreview.ErrRejected) || errors.Is(prior, prcompletion.ErrRejected))) {
		return &worker.EffectError{Primary: continuous, Additional: errors.Join(numeric, prior)}
	}
	if numeric != nil {
		return &worker.EffectError{Primary: numeric, Additional: errors.Join(prior, continuous)}
	}
	if prior != nil {
		return &worker.EffectError{Primary: prior, Additional: continuous}
	}
	return continuous
}

func (f *Flow) withCurrent(ctx context.Context, e storage.Event, fn func() error) error {
	return f.withLane(ctx, func() error { return f.convergeCurrent(ctx, e, fn()) })
}
func (f *Flow) Resolve(ctx context.Context, e storage.Event) (storage.Resource, error) {
	if e.Delivery.EventName == "pull_request_review" {
		_, ok, err := prreview.Notification(e.Delivery)
		if err != nil {
			return storage.Resource{}, err
		}
		if !ok {
			return storage.Resource{}, worker.ErrNotApplicable
		}
	}
	switch e.Delivery.EventName {
	case "push":
		// Notifications outside the configured deployment have no supported
		// current resource to bind. Keep #38's stricter candidate parser intact.
		configured, err := f.pushRepository(e.Delivery.Payload)
		if err != nil {
			return storage.Resource{}, err
		}
		if !configured {
			return storage.Resource{}, worker.ErrNotApplicable
		}
		candidate, ok, err := ea.ResolvePush(ctx, f.cfg.Deployment.Repositories, e.Delivery)
		if err != nil {
			return storage.Resource{}, err
		}
		if !ok {
			return storage.Resource{}, worker.ErrNotApplicable
		}
		return candidate, nil
	case "projects_v2_item":
		return f.resolveProjectItem(ctx, e)
	default:
		return f.cfg.Resolver.Resolve(ctx, e)
	}
}

func (f *Flow) pushRepository(payload []byte) (bool, error) {
	root, err := object(payload)
	if err != nil {
		return false, err
	}
	repo, err := object(root["repository"])
	if err != nil {
		return false, err
	}
	owner, err := object(repo["owner"])
	if err != nil {
		return false, err
	}
	var login, name string
	if json.Unmarshal(owner["login"], &login) != nil || json.Unmarshal(repo["name"], &name) != nil || !github.ValidBranchRepository(github.Repository{ID: "validation", Owner: login, Name: name}) {
		return false, ErrInvalid
	}
	for _, r := range f.cfg.Deployment.Repositories {
		if strings.EqualFold(r.Owner, login) && strings.EqualFold(r.Name, name) {
			return true, nil
		}
	}
	return false, nil
}
func (f *Flow) Process(ctx context.Context, e storage.Event) error {
	if e.Resource == nil {
		return observe.ErrBinding
	}
	if e.Resource.Kind == "pull_request" {
		if f.cfg.Completion != nil {
			handled := false
			err := f.withLane(ctx, func() (err error) {
				defer func() {
					// An unhandled OPEN PR must reach onboarding before numeric
					// preflight requires membership. Terminal/handled routes still
					// converge numbers before their early return, even after errors.
					if handled || err != nil {
						err = f.convergeCurrent(ctx, e, err)
					}
				}()
				result, completionErr := f.cfg.Completion.Handle(ctx, e)
				handled = result.Handled
				if completionErr != nil && (!handled || result.Target.Kind == "" || ctx.Err() != nil) {
					return completionErr
				}
				if !handled || result.Target.Kind == "" {
					return completionErr
				}
				if f.cfg.Terminal != nil {
					if _, err := f.cfg.Terminal.SynchronizeFrom(ctx, result.Target, e.Delivery.ID); err != nil {
						return errors.Join(completionErr, err)
					}
				}
				snapshot, err := f.cfg.Snapshot.ResolveSnapshot(ctx)
				if err != nil {
					return errors.Join(completionErr, err)
				}
				return errors.Join(completionErr, f.cfg.Reconciler.Reconcile(ctx, snapshot, result.Target))
			})
			if err != nil || handled {
				return err
			}
		}
		if f.cfg.Onboarding == nil && f.cfg.Review == nil {
			return f.cfg.Foundation.Process(ctx, e)
		}
		candidate, reviewEvent, err := prreview.Notification(e.Delivery)
		if err != nil {
			return err
		}
		if reviewEvent {
			if f.cfg.Review == nil {
				return prreview.ErrInvalid
			}
			return f.withCurrent(ctx, e, func() error { return f.cfg.Review.Handle(ctx, e) })
		}
		// Unsupported review notifications can reach Process with a durable
		// binding after recovery; they still grant no onboarding/review effects.
		if e.Delivery.EventName == "pull_request_review" || candidate.Kind == "review" {
			return nil
		}
		if f.cfg.Onboarding == nil {
			return f.cfg.Foundation.Process(ctx, e)
		}
		var enriched semanticflow.PolicyInput
		semantic := e.Delivery.EventName != "projects_v2_item" && f.cfg.Enricher != nil
		if e.Delivery.EventName != "projects_v2_item" {
			if semantic {
				o, err := f.cfg.Reader.ReadCurrent(ctx, *e.Resource)
				if err != nil {
					return err
				}
				enriched, err = f.cfg.Enricher.Enrich(ctx, observe.PolicyInput{DeliveryID: e.Delivery.ID, Sequence: e.Sequence, Observed: o})
				if err != nil {
					return err
				}
			} else if err := f.cfg.Foundation.Process(ctx, e); err != nil {
				return err
			}
		}
		return f.withLane(ctx, func() error {
			if semantic {
				if err := f.cfg.Semantic.Evaluate(ctx, enriched); err != nil {
					return err
				}
			}
			if err := f.cfg.Onboarding.Handle(ctx, e); err != nil {
				return f.convergeCurrent(ctx, e, err)
			}
			if f.cfg.Review != nil {
				return f.convergeCurrent(ctx, e, f.cfg.Review.Converge(ctx, *e.Resource))
			}
			return f.convergeCurrent(ctx, e, nil)
		})
	}
	if e.Resource.Kind != "issue" {
		return ErrInvalid
	}
	if f.cfg.Terminal != nil {
		observed, err := f.cfg.Reader.ReadCurrent(ctx, *e.Resource)
		if err != nil {
			return err
		}
		terminal := observed.Issue != nil && observed.Issue.State == "CLOSED"
		for _, p := range observed.Projects {
			if p.ID == f.cfg.Deployment.Engineering.ID {
				for _, item := range p.Items {
					terminal = terminal || item.Fields[config.Status].OptionID == f.cfg.Deployment.Engineering.StatusOptions[config.Done]
				}
			}
		}
		// Native consistency never waits for optional semantic execution.
		if terminal {
			err := f.withCurrent(ctx, e, func() error { return f.reconcile(ctx, e) })
			// Preserve configured semantic processing after consistency correction.
			// Missing Type authority can also be supplied by that path; an
			// incomplete snapshot authorizes no terminal effect.
			if err != nil && !errors.Is(err, ec.ErrIncomplete) || f.cfg.Enricher == nil || e.Delivery.EventName == "push" || e.Delivery.EventName == "projects_v2_item" {
				return err
			}
		}
	}

	var enriched semanticflow.PolicyInput
	semantic := e.Delivery.EventName != "push" && e.Delivery.EventName != "projects_v2_item" && f.cfg.Enricher != nil
	if semantic {
		o, err := f.cfg.Reader.ReadCurrent(ctx, *e.Resource)
		if err != nil {
			return err
		}
		// Only bounded model execution runs outside the global lane. Failure does
		// not bypass enrichment and hand the delivery to Engineering anyway.
		enriched, err = f.cfg.Enricher.Enrich(ctx, observe.PolicyInput{DeliveryID: e.Delivery.ID, Sequence: e.Sequence, Observed: o})
		if err != nil {
			return err
		}
	}
	return f.withLane(ctx, func() error {
		if semantic {
			if err := f.cfg.Semantic.Evaluate(ctx, enriched); err != nil {
				return err
			}
		}
		return f.convergeCurrent(ctx, e, f.reconcile(ctx, e))
	})
}

type guardedProjects struct {
	reader    ea.ProjectReader
	snapshot  ec.Snapshot
	resources map[string]storage.Resource
}

func (g guardedProjects) ReadCurrent(ctx context.Context, t storage.Resource) (observe.ObservedState, error) {
	o, err := g.reader.ReadCurrent(ctx, t)
	if err != nil {
		return observe.ObservedState{}, err
	}
	if err := g.snapshot.CheckCurrent(t, o); err != nil {
		return observe.ObservedState{}, err
	}
	if g.resources != nil {
		for _, project := range o.Projects {
			if project.Profile != config.Engineering {
				continue
			}
			for _, item := range project.Items {
				if previous, ok := g.resources[item.ID]; ok && previous != o.Resource {
					return observe.ObservedState{}, er.ErrProject
				}
				g.resources[item.ID] = o.Resource
			}
		}
	}
	return o, nil
}

// Activity retains its own authorization and Start Date semantics. This guard
// only prevents its earlier Status write from erasing a valid review cycle.
type reviewActivityFields struct {
	fields   github.LifecycleFields
	projects guardedProjects
	review   Review
	project  config.ResolvedProject
}

func (g reviewActivityFields) SetProjectDate(ctx context.Context, project, item, field, date string) error {
	return g.fields.SetProjectDate(ctx, project, item, field, date)
}
func (g reviewActivityFields) SetProjectOption(ctx context.Context, project, item, field, option string) error {
	if project != g.project.ID || field != g.project.Fields[config.Status] || option != g.project.StatusOptions[config.InProgress] {
		return ErrInvalid
	}
	resource, ok := g.projects.resources[item]
	if !ok {
		return er.ErrProject
	}
	status, active, err := g.review.TargetStatus(ctx, resource)
	if errors.Is(err, prreview.ErrSuperseded) {
		return nil
	}
	if err != nil {
		return err
	}
	if active {
		if status == config.InReview {
			return nil
		}
		if status != config.InProgress {
			return ErrInvalid
		}
	}
	// The review read can overlap current directive edits. Recheck the exact
	// snapshot and item before retaining activity's authorized Status write.
	o, err := g.projects.ReadCurrent(ctx, resource)
	if err != nil {
		return err
	}
	found := false
	for _, p := range o.Projects {
		if p.ID == project && p.Profile == config.Engineering {
			for _, i := range p.Items {
				if i.ID == item && !i.Archived {
					found = true
				}
			}
		}
	}
	if !found {
		return er.ErrProject
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return g.fields.SetProjectOption(ctx, project, item, field, option)
}
func (f *Flow) reconcile(ctx context.Context, event storage.Event) error {
	if f.cfg.Terminal != nil {
		if _, err := f.cfg.Terminal.SynchronizeFrom(ctx, *event.Resource, event.Delivery.ID); err != nil {
			return err
		}
	}
	snapshot, err := f.cfg.Snapshot.ResolveSnapshot(ctx)
	if err != nil {
		return err
	}
	current, err := snapshot.Context(*event.Resource)
	if err != nil {
		return err
	}
	in, lifecycle, _, err := er.Compute(current)
	if err != nil {
		return err
	}
	decisions := make(map[storage.Resource]bool, len(lifecycle.Decisions))
	for _, decision := range lifecycle.Decisions {
		decisions[decision.Resource] = true
	}
	activityCfg := f.cfg.Activity
	projects := guardedProjects{reader: activityCfg.Projects, snapshot: snapshot}
	if f.cfg.Review != nil {
		projects.resources = map[string]storage.Resource{}
		activityCfg.Fields = reviewActivityFields{fields: activityCfg.Fields, projects: projects, review: f.cfg.Review, project: activityCfg.Project}
	}
	activityCfg.Projects = projects
	activity, err := ea.New(activityCfg)
	if err != nil {
		return err
	}
	handled := false
	if event.Delivery.EventName == "push" {
		candidate, ok, err := ea.ResolvePush(ctx, f.cfg.Deployment.Repositories, event.Delivery)
		if err != nil {
			return err
		}
		bound := *event.Resource
		bound.NodeID = ""
		if !ok || candidate != bound {
			return ErrInvalid
		}
		if decisions[current.Root] {
			if _, err := activity.Handle(ctx, event.Delivery.ID, in); err != nil {
				return err
			}
			handled = true
		}
	}
	// Context is canonically sorted. Load only validated package-owned facts,
	// including closed work, and resume with the original accepted push delivery.
	for _, n := range current.Nodes {
		state, err := activity.ActivityState(ctx, n.Resource)
		if err != nil {
			return err
		}
		if !state.Accepted || state.Completed {
			continue
		}
		// Acceptance is retained when #35 has no current target Decision (in
		// particular, after closing). It supplies neither completion nor Status
		// authority and must not prevent #36 Clear and current-work compaction.
		if !decisions[n.Resource] {
			continue
		}
		target, err := snapshot.Context(n.Resource)
		if err != nil {
			return err
		}
		input, _, _, err := er.Compute(target)
		if err != nil {
			return err
		}
		if _, err := activity.Handle(ctx, state.DeliveryID, input); err != nil {
			return err
		}
		handled = true
	}
	if handled {
		snapshot, err = f.cfg.Snapshot.ResolveSnapshot(ctx)
		if err != nil {
			return err
		}
	}
	if err := f.cfg.Reconciler.Reconcile(ctx, snapshot, *event.Resource); err != nil {
		return err
	}
	// Field writes may overlap external intent changes. Branch authorization
	// therefore starts from a new complete consistency barrier under this lane.
	snapshot, err = f.cfg.Snapshot.ResolveSnapshot(ctx)
	if err != nil {
		return err
	}
	current, err = snapshot.Context(*event.Resource)
	if err != nil {
		return err
	}
	freshInput, lifecycle, _, err := er.Compute(current)
	if err != nil {
		return err
	}
	// #35 returns Decisions in canonical resource order; #37 independently
	// validates each exact target and the actual durable trigger origin.
	for _, decision := range lifecycle.Decisions {
		if !decision.ExecutionEligible {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		target, err := snapshot.Context(decision.Resource)
		if err != nil {
			return err
		}
		if _, err := f.cfg.Branches.EnsureFrom(ctx, eb.Origin{DeliveryID: event.Delivery.ID, Resource: current.Root}, ep.Input{Context: target, PhaseOrder: freshInput.PhaseOrder}); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// object rejects duplicate identity keys; field values remain opaque and unused.
func object(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrInvalid
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, ErrInvalid
		}
		key, ok := token.(string)
		if !ok {
			return nil, ErrInvalid
		}
		if _, ok := out[key]; ok {
			return nil, ErrInvalid
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, ErrInvalid
		}
		out[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	return out, nil
}
func (f *Flow) resolveProjectItem(ctx context.Context, event storage.Event) (storage.Resource, error) {
	root, err := object(event.Delivery.Payload)
	if err != nil {
		return storage.Resource{}, err
	}
	var action string
	if json.Unmarshal(root["action"], &action) != nil {
		return storage.Resource{}, ErrInvalid
	}
	switch action {
	case "created", "edited", "reordered", "archived", "restored":
	default:
		return storage.Resource{}, worker.ErrNotApplicable
	}
	item, err := object(root["projects_v2_item"])
	if err != nil {
		return storage.Resource{}, err
	}
	var id, project string
	if json.Unmarshal(item["node_id"], &id) != nil || json.Unmarshal(item["project_node_id"], &project) != nil || !validID(id) || !validID(project) {
		return storage.Resource{}, ErrInvalid
	}
	if project != f.cfg.Deployment.Engineering.ID {
		return storage.Resource{}, worker.ErrNotApplicable
	}
	current, err := f.cfg.ProjectItems.CurrentProjectItem(ctx, id)
	if err != nil {
		var ge *github.Error
		if errors.As(err, &ge) && ge.Category == github.NotFound {
			return storage.Resource{}, worker.ErrNotApplicable
		}
		return storage.Resource{}, err
	}
	if current.ID != id {
		return storage.Resource{}, ErrInvalid
	}
	if current.ProjectID != f.cfg.Deployment.Engineering.ID {
		return storage.Resource{}, worker.ErrNotApplicable
	}
	i := current.Issue
	kind := "issue"
	if current.ContentKind == "PullRequest" && f.cfg.Onboarding != nil {
		prItem, err := f.cfg.PRProjectItems.CurrentPullRequestProjectItem(ctx, id)
		if err != nil {
			return storage.Resource{}, err
		}
		if prItem.ID != id || prItem.ProjectID != current.ProjectID || prItem.ContentKind != "PullRequest" || prItem.PullRequest == nil || prItem.Issue != nil {
			return storage.Resource{}, ErrInvalid
		}
		i, kind = prItem.PullRequest, "pull_request"
	} else if current.ContentKind != "Issue" || i == nil {
		return storage.Resource{}, worker.ErrNotApplicable
	}
	var candidate storage.Resource
	for _, repo := range f.cfg.Deployment.Repositories {
		if strings.EqualFold(repo.Owner, i.Repository.Owner) && strings.EqualFold(repo.Name, i.Repository.Name) {
			if repo.ID != i.Repository.ID || !validID(i.ID) || i.Number <= 0 {
				return storage.Resource{}, ErrInvalid
			}
			candidate = storage.Resource{Owner: strings.ToLower(repo.Owner), Repository: strings.ToLower(repo.Name), Kind: kind, Number: int64(i.Number), NodeID: i.ID}
		}
	}
	if candidate.Kind == "" {
		return storage.Resource{}, worker.ErrNotApplicable
	}
	o, err := f.cfg.Reader.ReadPrimary(ctx, candidate)
	if err != nil {
		return storage.Resource{}, err
	}
	if o.Presence == observe.Missing {
		return storage.Resource{}, worker.ErrNotApplicable
	}
	if o.Presence != observe.Present || (kind == "issue" && (o.Issue == nil || o.PullRequest != nil) || kind == "pull_request" && (o.PullRequest == nil || o.Issue != nil)) || o.Resource != candidate {
		return storage.Resource{}, ec.ErrChanged
	}
	return o.Resource, ctx.Err()
}
func validID(v string) bool {
	return v != "" && strings.TrimSpace(v) == v && !strings.ContainsAny(v, " \t\r\n")
}

var _ worker.ResourceResolver = (*Flow)(nil)
var _ worker.Processor = (*Flow)(nil)
