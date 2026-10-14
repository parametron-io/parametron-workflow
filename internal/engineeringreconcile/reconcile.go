// Package engineeringreconcile converges owned Engineering fields from complete
// current facts. Pure #35/#36 remain the only pre-development policy authorities.
package engineeringreconcile

import (
	"context"
	"errors"
	"maps"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prreview"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid = errors.New("engineering reconciliation: invalid composition")
	ErrProject = errors.New("engineering reconciliation: invalid Project write identity or evidence")
)

type ProjectReader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
}
type ReviewStatus interface {
	TargetStatus(context.Context, storage.Resource) (config.StatusRole, bool, error)
}
type Config struct {
	Completion ReviewStatus
	Review     ReviewStatus
	Project    config.ResolvedProject
	Projects   ProjectReader
	Activity   ea.StateReader
	Fields     github.LifecycleFields
	Numbers    github.NumericFields
}
type Reconciler struct{ cfg Config }

func New(c Config) (*Reconciler, error) {
	if c.Completion != nil && absent(c.Completion) {
		return nil, ErrInvalid
	}
	if c.Review != nil && absent(c.Review) {
		return nil, ErrInvalid
	}
	if absent(c.Projects) || absent(c.Activity) || absent(c.Fields) || absent(c.Numbers) || !validID(c.Project.ID) || !validID(c.Project.Fields[config.Status]) || !validID(c.Project.Fields[config.RoadmapOrder]) || c.Project.Fields[config.Status] == c.Project.Fields[config.RoadmapOrder] {
		return nil, ErrInvalid
	}
	seen := map[string]bool{}
	for _, role := range []config.StatusRole{config.Backlog, config.Blocked, config.Ready, config.InProgress} {
		id := c.Project.StatusOptions[role]
		if !validID(id) || seen[id] {
			return nil, ErrInvalid
		}
		seen[id] = true
	}
	if c.Review != nil {
		id := c.Project.StatusOptions[config.InReview]
		if !validID(id) || seen[id] {
			return nil, ErrInvalid
		}
	}
	c.Project.Fields = maps.Clone(c.Project.Fields)
	c.Project.StatusOptions = maps.Clone(c.Project.StatusOptions)
	return &Reconciler{c}, nil
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
func validID(id string) bool {
	return id != "" && strings.TrimSpace(id) == id && !strings.ContainsAny(id, " \t\r\n")
}
func less(a, b storage.Resource) bool {
	if a.Owner != b.Owner {
		return a.Owner < b.Owner
	}
	if a.Repository != b.Repository {
		return a.Repository < b.Repository
	}
	if a.Number != b.Number {
		return a.Number < b.Number
	}
	return a.NodeID < b.NodeID
}

// Compute consumes the three existing pure APIs without changing their rules.
func Compute(current ec.Context) (ep.Input, ep.Plan, eo.Plan, error) {
	order, err := eo.BuildRoadmapOrder(current)
	if err != nil {
		return ep.Input{}, ep.Plan{}, eo.Plan{}, err
	}
	in := ep.Input{Context: current, PhaseOrder: order.PhaseOrder}
	lifecycle, err := ep.Evaluate(in)
	if err != nil {
		return ep.Input{}, ep.Plan{}, eo.Plan{}, err
	}
	roadmap, err := eo.Evaluate(eo.Input{Context: current, Lifecycle: lifecycle, Order: order})
	if err != nil {
		return ep.Input{}, ep.Plan{}, eo.Plan{}, err
	}
	return in, lifecycle, roadmap, nil
}
func (r *Reconciler) item(ctx context.Context, s ec.Snapshot, target storage.Resource) (observe.ProjectItem, error) {
	o, err := r.cfg.Projects.ReadCurrent(ctx, target)
	if err != nil {
		return observe.ProjectItem{}, err
	}
	if err := s.CheckCurrent(target, o); err != nil {
		return observe.ProjectItem{}, err
	}
	var found *observe.ProjectState
	for i := range o.Projects {
		p := &o.Projects[i]
		if p.Profile == config.Engineering || p.ID == r.cfg.Project.ID {
			if found != nil || p.Profile != config.Engineering || p.ID != r.cfg.Project.ID {
				return observe.ProjectItem{}, ErrProject
			}
			found = p
		}
	}
	if found == nil || len(found.Items) != 1 || found.Items[0].Archived || !validID(found.Items[0].ID) {
		return observe.ProjectItem{}, ErrProject
	}
	item := found.Items[0]
	if v, ok := item.Fields[config.Status]; ok && (v.Kind != config.SingleSelect || !validID(v.OptionID) || v.Date != "" || v.Number != 0) {
		return observe.ProjectItem{}, ErrProject
	}
	if v, ok := item.Fields[config.RoadmapOrder]; ok && (v.Kind != config.Number || v.OptionID != "" || v.Date != "" || math.IsNaN(v.Number) || math.IsInf(v.Number, 0)) {
		return observe.ProjectItem{}, ErrProject
	}
	return item, ctx.Err()
}

// Reconcile refetches the complete target union before any drift writes. Retry
// compares fresh actual values; normal effects have no replay/provenance log.
func (r *Reconciler) Reconcile(ctx context.Context, s ec.Snapshot, trigger storage.Resource) error {
	current, err := s.Context(trigger)
	if err != nil {
		return err
	}
	_, lifecycle, roadmap, err := Compute(current)
	if err != nil {
		return err
	}
	started := map[storage.Resource]bool{}
	nodes := map[storage.Resource]ec.IssueNode{}
	for _, n := range current.Nodes {
		nodes[n.Resource] = n
		state, err := r.cfg.Activity.ActivityState(ctx, n.Resource)
		if err != nil {
			return err
		}
		if state.Completed {
			started[n.Resource] = true
		}
	}
	// Current direct Task/Feature children contribute even after closing. Current
	// reparenting moves the contribution; no historical parent is persisted.
	for _, edge := range current.Parents {
		child, parent := nodes[edge.Child], nodes[edge.Parent]
		if started[edge.Child] && (child.Type == sp.Task || child.Type == sp.Feature) && parent.Type == sp.Phase && parent.State == ec.Open {
			started[edge.Parent] = true
		}
	}
	statuses := map[storage.Resource]config.StatusRole{}
	targets := map[storage.Resource]bool{}
	for _, d := range lifecycle.Decisions {
		if !d.StatusOwned {
			continue
		}
		var status config.StatusRole
		switch d.Status {
		case ep.Backlog:
			status = config.Backlog
		case ep.Blocked:
			status = config.Blocked
		case ep.Ready:
			status = config.Ready
		default:
			return ErrInvalid
		}
		if started[d.Resource] {
			status = config.InProgress
		}
		statuses[d.Resource], targets[d.Resource] = status, true
	}
	for _, a := range roadmap.Assignments {
		targets[a.Resource] = true
	}
	for _, t := range roadmap.Clear {
		targets[t] = true
	}
	ordered := make([]storage.Resource, 0, len(targets))
	for t := range targets {
		ordered = append(ordered, t)
	}
	sort.Slice(ordered, func(i, j int) bool { return less(ordered[i], ordered[j]) })
	items := map[storage.Resource]observe.ProjectItem{}
	ids := map[string]bool{}
	for _, t := range ordered {
		item, err := r.item(ctx, s, t)
		if err != nil {
			return err
		}
		if ids[item.ID] {
			return ErrProject
		}
		ids[item.ID] = true
		items[t] = item
	}
	for _, t := range ordered {
		status, ok := statuses[t]
		if !ok {
			continue
		}
		if r.cfg.Completion != nil {
			overlay, active, err := r.cfg.Completion.TargetStatus(ctx, t)
			if err != nil {
				return err
			}
			if active {
				if overlay != config.InProgress {
					return ErrInvalid
				}
				status = overlay
			}
		}
		if r.cfg.Review != nil {
			review, active, err := r.cfg.Review.TargetStatus(ctx, t)
			if errors.Is(err, prreview.ErrSuperseded) {
				// New native readiness denies older Status effects but supplies no
				// review grant until its explicit delivery is accepted.
				continue
			}
			if err != nil {
				return err
			}
			if active {
				if review != config.InReview && review != config.InProgress {
					return ErrInvalid
				}
				status = review
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Review validation may perform several current reads. Recheck snapshot
		// identity and current Project state immediately before this Status write.
		item := items[t]
		if r.cfg.Review != nil || r.cfg.Completion != nil {
			item, err = r.item(ctx, s, t)
			if err != nil {
				return err
			}
		}
		option := r.cfg.Project.StatusOptions[status]
		if item.Fields[config.Status].OptionID != option {
			if err := r.cfg.Fields.SetProjectOption(ctx, r.cfg.Project.ID, item.ID, r.cfg.Project.Fields[config.Status], option); err != nil {
				return err
			}
		}
	}
	clears := append([]storage.Resource(nil), roadmap.Clear...)
	sort.Slice(clears, func(i, j int) bool { return less(clears[i], clears[j]) })
	for _, t := range clears {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := items[t]
		if _, ok := item.Fields[config.RoadmapOrder]; ok {
			if err := r.cfg.Numbers.ClearProjectNumber(ctx, r.cfg.Project.ID, item.ID, r.cfg.Project.Fields[config.RoadmapOrder]); err != nil {
				return err
			}
		}
	}
	for _, a := range roadmap.Assignments {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := items[a.Resource]
		value := float64(a.RoadmapOrder)
		// #36 bounded integer domain is exactly representable. Finite fractional,
		// zero, reserved-looking and out-of-range observations are ordinary drift.
		got, present := item.Fields[config.RoadmapOrder]
		if !present || got.Number != value {
			if err := r.cfg.Numbers.SetProjectNumber(ctx, r.cfg.Project.ID, item.ID, r.cfg.Project.Fields[config.RoadmapOrder], value); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
