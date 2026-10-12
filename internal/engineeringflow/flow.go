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
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
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

type Config struct {
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
func (f *Flow) Resolve(ctx context.Context, e storage.Event) (storage.Resource, error) {
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
			return f.cfg.Onboarding.Handle(ctx, e)
		})
	}
	if e.Resource.Kind != "issue" {
		return ErrInvalid
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
		return f.reconcile(ctx, e)
	})
}

type guardedProjects struct {
	reader   ea.ProjectReader
	snapshot ec.Snapshot
}

func (g guardedProjects) ReadCurrent(ctx context.Context, t storage.Resource) (observe.ObservedState, error) {
	o, err := g.reader.ReadCurrent(ctx, t)
	if err != nil {
		return observe.ObservedState{}, err
	}
	if err := g.snapshot.CheckCurrent(t, o); err != nil {
		return observe.ObservedState{}, err
	}
	return o, nil
}
func (f *Flow) reconcile(ctx context.Context, event storage.Event) error {
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
	activityCfg.Projects = guardedProjects{reader: activityCfg.Projects, snapshot: snapshot}
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
