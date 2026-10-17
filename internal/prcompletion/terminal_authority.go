package prcompletion

import (
	"context"
	"errors"
	"reflect"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// AuthorizesClosure is the read-only #50 authority accessor for the terminal
// owner. It never closes an Issue or turns historical onboarding into a grant.
// A native reopen at/after the merge invalidates that merge for a later cycle.
func (c *Controller) AuthorizesClosure(ctx context.Context, target storage.Resource, facts github.IssueClosure) (bool, error) {
	if err := facts.Validate(facts.Issue, "CLOSED"); err != nil {
		return false, err
	}
	if resource(facts.Issue, "issue") != target {
		return false, ErrInvalid
	}
	_, boundary, _, ok := facts.LatestClose()
	if !ok {
		return false, ErrChanged
	}
	records, err := c.cfg.Onboarding.Completions(ctx)
	if err != nil {
		return false, err
	}
	for _, o := range records {
		if resource(o.Binding.Target, "issue") != target {
			continue
		}
		observed, err := c.read(ctx, o.Resource)
		if err != nil {
			return false, err
		}
		if observed.PullRequest == nil || observed.PullRequest.State != "MERGED" {
			continue
		}
		a, err := c.fresh(ctx, o.Resource)
		if err != nil {
			if errors.Is(err, ErrRejected) {
				continue
			}
			return false, err
		}
		if a.decision.Outcome != Merged || !a.decision.CloseTarget {
			continue
		}
		if a.target.Resource != target || a.input.Target.Identity != facts.Issue || a.input.Target.State != "CLOSED" {
			return false, ErrChanged
		}
		merge := a.decision.Merge
		// #50 also permits the explicitly labelled already-closed Target case.
		// A later verified merge can complete an already-closed Target; requiring
		// native Close to follow Merge would revoke that existing authority.
		if boundary.ID != "" && !boundary.CreatedAt.Before(merge.CreatedAt) {
			continue
		}
		expected := closeEvidence{Version: 1, Onboarding: a.input.Onboarding, Merge: merge}
		var attempt closeEvidence
		var done closeReceipt
		attempted, present, err := c.load(ctx, attemptNS, o.Resource, &attempt)
		if err != nil {
			return false, err
		}
		completed, finished, err := c.load(ctx, completedNS, o.Resource, &done)
		if err != nil {
			return false, err
		}
		if present && (!reflect.DeepEqual(attempt, expected) || attempted.CreatedAt.Before(merge.CreatedAt) || attempted.CreatedAt.Before(o.CompletedAt)) {
			return false, ErrProvenance
		}
		if present && !finished {
			return false, ErrAmbiguous
		}
		if finished {
			if done.Version != 1 || !reflect.DeepEqual(done.Evidence, expected) || completed.CreatedAt.Before(merge.CreatedAt) {
				return false, ErrProvenance
			}
			switch done.Disposition {
			case "mutation":
				if !present || completed.DeliveryID != attempted.DeliveryID || completed.CreatedAt.Before(attempted.CreatedAt) || done.Receipt == nil || done.Receipt.Issue != facts.Issue || done.Receipt.State != "CLOSED" {
					return false, ErrProvenance
				}
			case "already_closed":
				if present || done.Receipt != nil {
					return false, ErrProvenance
				}
			default:
				return false, ErrProvenance
			}
		}
		// An already-native-closed Target may precede the existing controller's
		// labelled no-op receipt. Fresh accepted implementation merge authority is
		// sufficient; an unknown mutation attempt above is never bypassed.
		check, err := c.fresh(ctx, o.Resource)
		if err != nil {
			return false, err
		}
		if !reflect.DeepEqual(a, check) {
			return false, ErrChanged
		}
		return true, ctx.Err()
	}
	return false, ctx.Err()
}
