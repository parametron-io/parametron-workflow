package prcompletion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

const attemptNS = "pr_completion_close_attempt_v1"
const completedNS = "pr_completion_close_completed_v1"

type closeEvidence struct {
	Version    int
	Onboarding pronboarding.Completion
	Merge      github.NativeTransition
}
type closeReceipt struct {
	Version     int
	Evidence    closeEvidence
	Disposition string
	Receipt     *github.IssueStateReceipt
}

func closeKey(r storage.Resource) string {
	return fmt.Sprintf("%s/%s/pull_request/%d", r.Owner, r.Repository, r.Number)
}
func (c *Controller) storageError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) {
		return ErrProvenance
	}
	return ErrStorage
}
func (c *Controller) origin(ctx context.Context, id string, r storage.Resource) error {
	e, err := c.cfg.Store.Event(ctx, id)
	if err != nil {
		return c.storageError(ctx, err)
	}
	if e.Delivery.ID != id || e.Resource == nil || e.Resource.Owner != r.Owner || e.Resource.Repository != r.Repository || e.Resource.Kind != r.Kind || e.Resource.Number != r.Number || e.Resource.NodeID != "" && e.Resource.NodeID != r.NodeID || e.Delivery.ReceivedAt.IsZero() {
		return ErrProvenance
	}
	return ctx.Err()
}
func (c *Controller) load(ctx context.Context, ns string, r storage.Resource, out any) (storage.Provenance, bool, error) {
	p, err := c.cfg.Store.Provenance(ctx, ns, closeKey(r))
	if errors.Is(err, storage.ErrNotFound) {
		return p, false, nil
	}
	if err != nil {
		return p, false, c.storageError(ctx, err)
	}
	if err := json.Unmarshal(p.Metadata, out); err != nil {
		return p, false, ErrProvenance
	}
	data, err := json.Marshal(out)
	if err != nil || !bytes.Equal(data, p.Metadata) || p.Namespace != ns || p.Key != closeKey(r) || p.CreatedAt.IsZero() || p.DeliveryID == "" {
		return p, false, ErrProvenance
	}
	if err := c.origin(ctx, p.DeliveryID, r); err != nil {
		return p, false, err
	}
	return p, true, nil
}
func (c *Controller) record(ctx context.Context, ns, id string, r storage.Resource, value any) error {
	if err := c.origin(ctx, id, r); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ErrProvenance
	}
	_, err = c.cfg.Store.RecordProvenance(ctx, storage.Provenance{Namespace: ns, Key: closeKey(r), DeliveryID: id, Metadata: data, CreatedAt: c.cfg.Clock()})
	if err != nil {
		return c.storageError(ctx, err)
	}
	return ctx.Err()
}
func (c *Controller) closeTarget(ctx context.Context, event storage.Event, a authority) error {
	r := a.pr.Resource
	evidence := closeEvidence{Version: 1, Onboarding: a.input.Onboarding, Merge: a.decision.Merge}
	var attempt closeEvidence
	var done closeReceipt
	attempted, present, err := c.load(ctx, attemptNS, r, &attempt)
	if err != nil {
		return err
	}
	completed, finished, err := c.load(ctx, completedNS, r, &done)
	if err != nil {
		return err
	}
	if present && (!reflect.DeepEqual(attempt, evidence) || attempted.CreatedAt.Before(evidence.Onboarding.CompletedAt) || attempted.CreatedAt.Before(evidence.Merge.CreatedAt)) {
		return ErrProvenance
	}
	if finished {
		if done.Version != 1 || !reflect.DeepEqual(done.Evidence, evidence) || completed.CreatedAt.Before(evidence.Merge.CreatedAt) {
			return ErrProvenance
		}
		switch done.Disposition {
		case "mutation":
			if !present || completed.DeliveryID != attempted.DeliveryID || completed.CreatedAt.Before(attempted.CreatedAt) || done.Receipt == nil || done.Receipt.Issue != evidence.Onboarding.Binding.Target || done.Receipt.State != "CLOSED" {
				return ErrProvenance
			}
		case "already_closed":
			if present || done.Receipt != nil {
				return ErrProvenance
			}
		default:
			return ErrProvenance
		}
		// Completed effect never authorizes another close, even if now OPEN.
		return nil
	}
	if present {
		return ErrAmbiguous
	}
	if a.target.Issue.State == "CLOSED" {
		// An explicitly labelled no-op, never attributed to a controller mutation.
		return c.record(ctx, completedNS, event.Delivery.ID, r, closeReceipt{Version: 1, Evidence: evidence, Disposition: "already_closed"})
	}
	facts, err := c.cfg.Native.NativeLifecycle(ctx, a.target.Issue.Identity, "issue")
	if err != nil {
		return err
	}
	if err := ValidateNative(facts, a.target.Issue.Identity, "issue", "OPEN"); err != nil {
		return err
	}
	for _, transition := range facts.Transitions {
		if transition.Kind == "ReopenedEvent" && !transition.CreatedAt.Before(evidence.Merge.CreatedAt) {
			return ErrReopened
		}
	}
	// Recompute current exact PR, Target, Type, membership and developer authority.
	fresh, err := c.fresh(ctx, r)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, fresh) {
		return ErrChanged
	}
	again, err := c.cfg.Native.NativeLifecycle(ctx, a.target.Issue.Identity, "issue")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(facts, again) {
		return ErrChanged
	}
	if err := c.record(ctx, attemptNS, event.Delivery.ID, r, evidence); err != nil {
		return err
	}
	// From this point interruption is indeterminate. A retry never infers success
	// from CLOSED and never issues a second close from historical merge authority.
	fresh, err = c.fresh(ctx, r)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, fresh) {
		return ErrAmbiguous
	}
	again, err = c.cfg.Native.NativeLifecycle(ctx, a.target.Issue.Identity, "issue")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(facts, again) {
		return ErrAmbiguous
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	receipt, err := c.cfg.Issues.CloseIssue(ctx, a.target.Issue.Identity)
	if err != nil {
		return err
	}
	if receipt.Issue != a.target.Issue.Identity || receipt.State != "CLOSED" {
		return ErrAmbiguous
	}
	return c.record(ctx, completedNS, event.Delivery.ID, r, closeReceipt{Version: 1, Evidence: evidence, Disposition: "mutation", Receipt: &receipt})
}
