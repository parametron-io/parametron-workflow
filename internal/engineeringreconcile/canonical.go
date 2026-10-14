package engineeringreconcile

import (
	"context"
	"errors"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/prreview"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// CanonicalStatus reuses #35/#36 and validated completed activity/accepted PR
// overlays. Field ownership is deliberately absent from canonical calculation;
// ordinary reconciliation retains its own Set-Status gate.
func (r *Reconciler) CanonicalStatus(ctx context.Context, current ec.Context, target storage.Resource) (config.StatusRole, error) {
	_, plan, _, err := Compute(current)
	if err != nil {
		return "", err
	}
	var status config.StatusRole
	for _, d := range plan.Decisions {
		if d.Resource != target {
			continue
		}
		switch d.Status {
		case ep.Backlog:
			status = config.Backlog
		case ep.Blocked:
			status = config.Blocked
		case ep.Ready:
			status = config.Ready
		default:
			return "", ErrInvalid
		}
	}
	if status == "" {
		return "", ErrInvalid
	}
	state, err := r.cfg.Activity.ActivityState(ctx, target)
	if err != nil {
		return "", err
	}
	if state.Completed {
		status = config.InProgress
	}
	nodes := map[storage.Resource]ec.IssueNode{}
	for _, n := range current.Nodes {
		nodes[n.Resource] = n
	}
	if nodes[target].Type == sp.Phase {
		for _, edge := range current.Parents {
			child := nodes[edge.Child]
			if edge.Parent != target || child.Type != sp.Task && child.Type != sp.Feature {
				continue
			}
			activity, err := r.cfg.Activity.ActivityState(ctx, edge.Child)
			if err != nil {
				return "", err
			}
			if activity.Completed {
				status = config.InProgress
			}
		}
	}
	if r.cfg.Completion != nil {
		overlay, active, err := r.cfg.Completion.TargetStatus(ctx, target)
		if err != nil {
			return "", err
		}
		if active {
			if overlay != config.InProgress {
				return "", ErrInvalid
			}
			status = overlay
		}
	}
	if r.cfg.Review != nil {
		var overlay config.StatusRole
		var active bool
		if review, ok := r.cfg.Review.(interface {
			CanonicalTargetStatus(context.Context, storage.Resource) (config.StatusRole, bool, error)
		}); ok {
			overlay, active, err = review.CanonicalTargetStatus(ctx, target)
		} else {
			overlay, active, err = r.cfg.Review.TargetStatus(ctx, target)
		}
		if errors.Is(err, prreview.ErrSuperseded) {
			return "", err
		}
		if err != nil {
			return "", err
		}
		if active {
			if overlay != config.InProgress && overlay != config.InReview {
				return "", ErrInvalid
			}
			status = overlay
		}
	}
	return status, ctx.Err()
}
