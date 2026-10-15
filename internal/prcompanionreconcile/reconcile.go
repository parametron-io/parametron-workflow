// Package prcompanionreconcile applies only #48's numeric companion plan. It
// requires the caller's existing Engineering lane; it owns no lifecycle policy.
package prcompanionreconcile

import (
	"context"
	"errors"
	"maps"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	er "github.com/parametron-io/parametron-workflow/internal/engineeringreconcile"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	pc "github.com/parametron-io/parametron-workflow/internal/prcompanion"
	"github.com/parametron-io/parametron-workflow/internal/prdiscovery"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid    = errors.New("pr companion reconciliation: invalid configuration or identity")
	ErrChanged    = errors.New("pr companion reconciliation: changed current evidence")
	ErrIncomplete = errors.New("pr companion reconciliation: incomplete observation")
	ErrProject    = errors.New("pr companion reconciliation: contradictory Project identity")
	ErrOwnership  = errors.New("pr companion reconciliation: corrupt or stale ownership")
	ErrAmbiguous  = errors.New("pr companion reconciliation: uncertain first acquisition")
	ErrStorage    = errors.New("pr companion reconciliation: local storage failure")
)

type Discovery interface {
	Discover(context.Context) (prdiscovery.Result, error)
}
type Snapshots interface {
	ResolveSnapshot(context.Context) (ec.Snapshot, error)
}
type Reader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
}
type OwnershipStore interface {
	CompanionFields(context.Context, string, string) ([]storage.CompanionField, error)
	SaveCompanionField(context.Context, int64, storage.CompanionField) error
	Event(context.Context, string) (storage.Event, error)
}
type Config struct {
	Repositories []github.Repository
	Project      config.ResolvedProject
	BugProject   string
	Discovery    Discovery
	Snapshots    Snapshots
	Reader       Reader
	Numbers      github.NumericFields
	Store        OwnershipStore
	Clock        func() time.Time
}
type Controller struct{ cfg Config }

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
func validID(v string) bool { return v != "" && !strings.ContainsAny(v, " \t\r\n") }
func New(c Config) (*Controller, error) {
	if !validID(c.Project.ID) || !validID(c.BugProject) || c.Project.ID == c.BugProject || !validID(c.Project.Fields[config.RoadmapOrder]) || len(c.Repositories) == 0 || len(c.Repositories) > prdiscovery.MaxRepositories {
		return nil, ErrInvalid
	}
	field := c.Project.Fields[config.RoadmapOrder]
	if field == c.Project.ID || field == c.BugProject {
		return nil, ErrInvalid
	}
	for _, d := range []any{c.Discovery, c.Snapshots, c.Reader, c.Numbers, c.Store} {
		if absent(d) {
			return nil, ErrInvalid
		}
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for _, r := range c.Repositories {
		n := strings.ToLower(r.Owner + "/" + r.Name)
		if !github.ValidBranchRepository(r) || r.Owner != "parametron-io" || names[n] || ids[r.ID] || r.ID == c.Project.ID || r.ID == c.BugProject || r.ID == field {
			return nil, ErrInvalid
		}
		names[n], ids[r.ID] = true, true
	}
	for role, id := range c.Project.Fields {
		if role != config.RoadmapOrder && id == c.Project.Fields[config.RoadmapOrder] {
			return nil, ErrInvalid
		}
	}
	c.Repositories = append([]github.Repository(nil), c.Repositories...)
	c.Project.Fields = maps.Clone(c.Project.Fields)
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return &Controller{c}, nil
}
func resource(i github.Identity, kind string) storage.Resource {
	return storage.Resource{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Kind: kind, Number: int64(i.Number), NodeID: i.ID}
}
func key(r storage.Resource) string {
	return r.Owner + "/" + r.Repository + "/" + r.Kind + "/" + fmtNumber(r.Number)
}
func (c *Controller) configured(i github.Identity) bool {
	if !github.ValidDevelopmentIdentity(i) {
		return false
	}
	for _, r := range c.cfg.Repositories {
		if i.Repository.ID == r.ID && strings.EqualFold(i.Repository.Owner, r.Owner) && strings.EqualFold(i.Repository.Name, r.Name) {
			return true
		}
	}
	return false
}

type view struct {
	Discovery prdiscovery.Result
	Context   ec.Context
	Primary   eo.Plan
	Plan      pc.Plan
	Ownership map[storage.Resource]record
	Items     map[storage.Resource]observe.ProjectItem
}

func (c *Controller) item(o observe.ObservedState) (observe.ProjectItem, error) {
	var found *observe.ProjectState
	for n := range o.Projects {
		p := &o.Projects[n]
		if p.ID == c.cfg.Project.ID || p.Profile == config.Engineering {
			if found != nil || p.ID != c.cfg.Project.ID || p.Profile != config.Engineering {
				return observe.ProjectItem{}, ErrProject
			}
			found = p
		}
	}
	if found == nil {
		return observe.ProjectItem{}, ErrIncomplete
	}
	if len(found.Items) == 0 {
		return observe.ProjectItem{}, ErrIncomplete
	}
	if len(found.Items) != 1 || !validID(found.Items[0].ID) || found.Items[0].Archived {
		return observe.ProjectItem{}, ErrProject
	}
	i := found.Items[0]
	if v, ok := i.Fields[config.RoadmapOrder]; ok && (v.Kind != config.Number || v.Date != "" || v.OptionID != "" || math.IsNaN(v.Number) || math.IsInf(v.Number, 0)) {
		return observe.ProjectItem{}, ErrProject
	}
	return i, nil
}

// load validates the COMPLETE plan and effect target union before returning any
// executable effects. Missing membership never removes a PR from #48 eligibility.
func (c *Controller) load(ctx context.Context) (view, error) {
	return c.loadWithPending(ctx, nil)
}
func (c *Controller) loadWithPending(ctx context.Context, pending *record) (view, error) {
	var v view
	s, err := c.cfg.Snapshots.ResolveSnapshot(ctx)
	if err != nil {
		return v, err
	}
	current, err := s.CanonicalContext()
	if err != nil {
		return v, err
	}
	_, _, primary, err := er.Compute(current)
	if err != nil {
		return v, err
	}
	d, err := c.cfg.Discovery.Discover(ctx)
	if err != nil {
		return v, err
	}
	// Discovery invokes binding and can be lengthy. Recheck the complete Issue
	// authority after it; stable PR reads never freeze primary roadmap facts.
	latest, err := c.cfg.Snapshots.ResolveSnapshot(ctx)
	if err != nil {
		return v, err
	}
	latestContext, err := latest.CanonicalContext()
	if err != nil {
		return v, err
	}
	if !reflect.DeepEqual(current, latestContext) {
		return v, ErrChanged
	}
	if d.Completeness != prdiscovery.Complete || d.Freshness != prdiscovery.StableReads {
		return v, ErrIncomplete
	}
	if d.Project != c.cfg.Project.ID || d.BugProject != c.cfg.BugProject || len(d.Repositories) != len(c.cfg.Repositories) || len(d.Entries) > prdiscovery.MaxPullRequests {
		return v, ErrInvalid
	}
	repos := map[github.Repository]bool{}
	for _, r := range c.cfg.Repositories {
		repos[r] = true
	}
	for _, r := range d.Repositories {
		if !repos[r] {
			return v, ErrInvalid
		}
		delete(repos, r)
	}
	in := pc.Input{Primary: primary}
	entries := map[storage.Resource]prdiscovery.Entry{}
	for _, e := range d.Entries {
		if e.PR.Presence != observe.Present || e.PR.PullRequest == nil || e.PR.Issue != nil || e.PR.Resource != e.Resource || !c.configured(e.PR.PullRequest.Identity) || resource(e.PR.PullRequest.Identity, "pull_request") != e.Resource {
			return v, ErrInvalid
		}
		if _, dup := entries[e.Resource]; dup {
			return v, ErrInvalid
		}
		// An unresolved OPEN candidate could change the complete eligible set;
		// it cannot be silently excluded to permit another PR's numeric effect.
		if e.Binding.Outcome == "" {
			return v, ErrIncomplete
		}
		if e.Binding.Outcome == prbinding.Rejected {
			switch e.Binding.Reason {
			case prbinding.Changed, prbinding.MissingPR:
				return v, ErrChanged
			case prbinding.UnresolvedTarget:
				return v, ErrIncomplete
			}
		}
		entries[e.Resource] = e
		in.PullRequests = append(in.PullRequests, pc.PullRequest{Resource: e.Resource, Identity: e.PR.PullRequest.Identity, State: pc.State(e.PR.PullRequest.State), Binding: e.Binding})
		if e.Binding.Outcome == prbinding.Accepted {
			if e.Target == nil || e.Target.Issue == nil || e.Target.PullRequest != nil || !c.configured(e.Binding.Evidence.Target) || resource(e.Binding.Evidence.Target, "issue") != e.Target.Resource {
				return v, ErrInvalid
			}
			if err := s.CheckCurrent(e.Target.Resource, *e.Target); err != nil {
				return v, err
			}
		}
	}
	owned, err := c.ownership(ctx)
	if err != nil {
		return v, err
	}
	for r, o := range owned {
		e, ok := entries[r]
		if !ok || e.PR.PullRequest.Identity != o.PR {
			return v, ErrOwnership
		}
		item, err := c.item(e.PR)
		if err != nil {
			return v, errors.Join(ErrOwnership, err)
		}
		if item.ID != o.Item {
			return v, ErrOwnership
		}
		if o.Acquisition.CompletedAt.IsZero() {
			if pending == nil || !reflect.DeepEqual(o, *pending) {
				return v, ErrAmbiguous
			}
		} else {
			in.Owned = append(in.Owned, pc.OwnedField{PR: o.PR, Target: o.Acquisition.Binding.Evidence.Target})
		}
	}
	plan, err := pc.Evaluate(in)
	if err != nil {
		return v, err
	}
	items := map[storage.Resource]observe.ProjectItem{}
	seenItems := map[string]storage.Resource{}
	add := func(r storage.Resource) error {
		e, ok := entries[r]
		if !ok {
			return ErrInvalid
		}
		i, err := c.item(e.PR)
		if err != nil {
			return err
		}
		if old, ok := seenItems[i.ID]; ok && old != r {
			return ErrProject
		}
		seenItems[i.ID] = r
		items[r] = i
		return nil
	}
	for _, a := range plan.Assignments {
		if err := add(a.PR); err != nil {
			return v, err
		}
		e := entries[a.PR]
		if e.Target == nil || e.Target.Resource != a.Target {
			return v, ErrInvalid
		}
		if _, err := c.item(*e.Target); err != nil {
			return v, err
		}
	}
	for _, r := range plan.Clear {
		if _, ok := owned[r]; !ok {
			return v, ErrOwnership
		}
		if err := add(r); err != nil {
			return v, err
		}
	}
	return view{d, current, primary, plan, owned, items}, ctx.Err()
}
func sameAuthority(a, b view) bool {
	if !reflect.DeepEqual(a.Context, b.Context) || !reflect.DeepEqual(a.Primary, b.Primary) || !reflect.DeepEqual(a.Plan, b.Plan) || len(a.Discovery.Entries) != len(b.Discovery.Entries) {
		return false
	}
	for n, e := range a.Discovery.Entries {
		f := b.Discovery.Entries[n]
		if e.Resource != f.Resource || e.Binding != f.Binding || e.PR.PullRequest.State != f.PR.PullRequest.State {
			return false
		}
	}
	return true
}

// Reconcile must run under the existing process-local Engineering lane. Each
// resource is considered at most once; a failed/changed attempt returns without
// internal retries. Earlier remote successes are never rolled back or replayed.
func (c *Controller) Reconcile(ctx context.Context, deliveryID string) (resultErr error) {
	completedEffects := 0
	defer func() {
		// A shared request budget can expire after real progress. Another fresh
		// attempt skips verified matches and can continue; a permanently oversized
		// first observation still retains discovery's terminal bound failure.
		if completedEffects > 0 && (errors.Is(resultErr, github.ErrRequestLimit) || errors.Is(resultErr, prdiscovery.ErrLimit)) {
			resultErr = ErrChanged
		}
	}()
	ctx = github.WithRequestLimit(ctx, prdiscovery.MaxRequests)
	origin, err := c.origin(ctx, deliveryID)
	if err != nil {
		return err
	}
	initial, err := c.load(ctx)
	if err != nil {
		return err
	}
	type effect struct {
		pr    storage.Resource
		value *int
	}
	effects := []effect{}
	// Clears precede sets to free terminal slots, without inventing allocation.
	for _, r := range initial.Plan.Clear {
		if _, present := initial.Items[r].Fields[config.RoadmapOrder]; !present {
			continue
		}
		effects = append(effects, effect{pr: r})
	}
	for _, a := range initial.Plan.Assignments {
		if _, owned := initial.Ownership[a.PR]; owned {
			if value, present := initial.Items[a.PR].Fields[config.RoadmapOrder]; present && value.Number == float64(a.RoadmapOrder) {
				continue
			}
		}
		v := a.RoadmapOrder
		effects = append(effects, effect{a.PR, &v})
	}
	for _, effect := range effects {
		if err := ctx.Err(); err != nil {
			return err
		}
		first, err := c.load(ctx)
		if err != nil {
			return err
		}
		fresh, err := c.load(ctx)
		if err != nil {
			return err
		}
		if !sameAuthority(initial, fresh) || !reflect.DeepEqual(first, fresh) {
			return ErrChanged
		}
		item, ok := fresh.Items[effect.pr]
		if !ok {
			return ErrChanged
		}
		o, owned := fresh.Ownership[effect.pr]
		observed, present := item.Fields[config.RoadmapOrder]
		matches := effect.value == nil && !present || effect.value != nil && present && observed.Number == float64(*effect.value)
		if owned && matches {
			continue
		}
		var e prdiscovery.Entry
		for _, entry := range fresh.Discovery.Entries {
			if entry.Resource == effect.pr {
				e = entry
				break
			}
		}
		if !owned {
			if effect.value == nil || e.Binding.Outcome != prbinding.Accepted {
				return ErrOwnership
			}
			o = record{Version: 1, Management: management, PR: e.PR.PullRequest.Identity, Project: c.cfg.Project.ID, Item: item.ID, Field: c.cfg.Project.Fields[config.RoadmapOrder]}
		}
		attempt := attempt{DeliveryID: deliveryID, Origin: origin, State: pc.State(e.PR.PullRequest.State), Binding: e.Binding, Value: effect.value, StartedAt: c.cfg.Clock().UTC()}
		if attempt.StartedAt.IsZero() {
			return ErrInvalid
		}
		if !owned {
			o.Acquisition = attempt
		}
		o.Attempt = attempt
		if err := c.save(ctx, &o); err != nil {
			return err
		}
		// Persisting an intent is not authorization or proof of a remote write.
		// Check ALL authority again, including exact value/item and the new record.
		if _, err := c.loadPending(ctx, o, fresh); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if effect.value == nil {
			err = c.cfg.Numbers.ClearProjectNumber(ctx, o.Project, o.Item, o.Field)
		} else {
			err = c.cfg.Numbers.SetProjectNumber(ctx, o.Project, o.Item, o.Field, float64(*effect.value))
		}
		if err != nil {
			return err
		}
		actual, err := c.cfg.Reader.ReadCurrent(ctx, effect.pr)
		if err != nil {
			return err
		}
		if actual.Resource != effect.pr || actual.Presence != observe.Present || actual.PullRequest == nil || actual.Issue != nil || actual.PullRequest.Identity != o.PR {
			return ErrChanged
		}
		after, err := c.item(actual)
		if err != nil {
			return err
		}
		if after.ID != o.Item {
			return ErrOwnership
		}
		number, has := after.Fields[config.RoadmapOrder]
		if effect.value == nil && has || effect.value != nil && (!has || number.Number != float64(*effect.value)) {
			return ErrChanged
		}
		o.Attempt.CompletedAt = c.cfg.Clock().UTC()
		if o.Attempt.CompletedAt.Before(o.Attempt.StartedAt) {
			return ErrInvalid
		}
		if !owned {
			o.Acquisition = o.Attempt
		}
		if err := c.save(ctx, &o); err != nil {
			return err
		}
		completedEffects++
	}
	return ctx.Err()
}
