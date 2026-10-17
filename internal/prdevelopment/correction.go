package prdevelopment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var ErrCorrection = errors.New("pr development: invalid correction provenance")

type CorrectionStore interface {
	Correction(context.Context, string, string) (storage.Correction, error)
	SaveCorrection(context.Context, int64, storage.Correction) error
}

// CorrectionEvidence records a selected pure-policy removal, separately from a
// successful transport receipt and acknowledgment by verified managed feedback.
// It cannot authorize another Development write.
type CorrectionEvidence struct {
	Version                     int
	Input                       Input
	Action                      Action
	Applied, Reported, Resolved bool
}

func (c *Controller) correction(ctx context.Context, r storage.Resource) (storage.Correction, CorrectionEvidence, error) {
	row, err := c.cfg.Corrections.Correction(ctx, "development_v1", r.NodeID)
	if err != nil {
		return row, CorrectionEvidence{}, err
	}
	var e CorrectionEvidence
	if json.Unmarshal(row.Metadata, &e) != nil {
		return row, e, ErrCorrection
	}
	raw, _ := json.Marshal(e)
	actions, err := Evaluate(e.Input)
	found := false
	for _, a := range actions {
		if a == e.Action && !a.Add {
			found = true
		}
	}
	if row.Owner != "development_v1" || row.ResourceID != r.NodeID || row.Revision <= 0 || !bytes.Equal(raw, row.Metadata) || e.Version != 1 || resource(e.Input.PR.Identity, "pull_request") != r || err != nil || !found {
		return row, e, ErrCorrection
	}
	return row, e, nil
}
func (c *Controller) saveCorrection(ctx context.Context, row storage.Correction, e CorrectionEvidence) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return ErrCorrection
	}
	previous := row.Revision
	row.Revision++
	row.Metadata = raw
	if err := c.cfg.Corrections.SaveCorrection(ctx, previous, row); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, storage.ErrConflict) {
			return ErrChanged
		}
		return errors.Join(ErrCorrection, err)
	}
	return nil
}
func (c *Controller) selectCorrection(ctx context.Context, in Input, a Action) error {
	r := resource(in.PR.Identity, "pull_request")
	row, old, err := c.correction(ctx, r)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if errors.Is(err, storage.ErrNotFound) {
		row = storage.Correction{Owner: "development_v1", ResourceID: r.NodeID}
	}
	// Preserve the original pending explanation across partial removals. It is
	// bounded to one representative contradiction for this exact binding interval.
	if err == nil && !old.Reported && !old.Resolved && sameBinding(old.Input.Binding.Evidence, in.Binding.Evidence) && old.Input.Onboarding == in.Onboarding {
		return nil
	}
	return c.saveCorrection(ctx, row, CorrectionEvidence{Version: 1, Input: in, Action: a})
}
func (c *Controller) markCorrection(ctx context.Context, r storage.Resource, reported bool) error {
	row, e, err := c.correction(ctx, r)
	if err != nil {
		return err
	}
	if reported {
		e.Reported = true
	} else {
		e.Applied = true
	}
	return c.saveCorrection(ctx, row, e)
}

func (c *Controller) appliedCorrection(ctx context.Context, r storage.Resource, a Action) error {
	row, e, err := c.correction(ctx, r)
	if err != nil {
		return err
	}
	// A later removal receipt cannot establish the outcome of an earlier
	// indeterminate removal retained as the representative explanation.
	if e.Action != a || e.Applied {
		return nil
	}
	e.Applied = true
	return c.saveCorrection(ctx, row, e)
}

// CurrentCorrection validates stored producer evidence and current authority.
// A pending explanation lasts only until verified reporting, binding/onboarding
// replacement, or terminal PR/Target state. Unknown evidence never resolves it.
func (c *Controller) CurrentCorrection(ctx context.Context, r storage.Resource) (*CorrectionEvidence, error) {
	if c.cfg.Corrections == nil {
		return nil, ErrIncomplete
	}
	_, e, err := c.correction(ctx, r)
	stored := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	retire := func() (*CorrectionEvidence, error) {
		if stored && !e.Resolved && !e.Reported {
			row, latest, err := c.correction(ctx, r)
			if err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(latest, e) {
				return nil, ErrChanged
			}
			latest.Resolved = true
			return nil, c.saveCorrection(ctx, row, latest)
		}
		return nil, nil
	}
	d, err := c.cfg.Binding.Evaluate(ctx, r)
	if err != nil {
		return nil, err
	}
	if d.Outcome == prbinding.Rejected {
		if d.Reason == prbinding.Changed || d.Reason == prbinding.MissingPR {
			return nil, ErrChanged
		}
		if d.Reason == prbinding.UnresolvedTarget {
			return nil, ErrIncomplete
		}
		return retire()
	}
	if d.Outcome != prbinding.Accepted {
		return nil, ErrInvalid
	}
	p, err := c.cfg.Reader.ReadCurrent(ctx, r)
	if err != nil {
		return nil, err
	}
	if p.PullRequest == nil || p.Resource != r || p.PullRequest.Identity != d.Evidence.PR {
		return nil, ErrChanged
	}
	if p.PullRequest.State == "CLOSED" || p.PullRequest.State == "MERGED" {
		return retire()
	}
	onboarding, err := c.cfg.Onboarding.Completed(ctx, r)
	if err != nil {
		return nil, err
	}
	if !onboarding.Completed {
		if stored {
			return nil, ErrCorrection
		}
		return nil, nil
	}
	current, err := c.fresh(ctx, r)
	if err != nil {
		return nil, err
	}
	if current.Binding != d {
		return nil, ErrChanged
	}
	// Fresh membership, onboarding and complete relationship evidence are
	// required even when no durable correction slot exists. Refresh can observe
	// a genuine current policy removal independently of the mutation path.
	actions, err := Evaluate(current)
	if err != nil {
		return nil, err
	}
	for _, action := range actions {
		if !action.Add {
			if stored && !e.Reported && !e.Resolved && sameBinding(d.Evidence, e.Input.Binding.Evidence) && current.Onboarding == e.Input.Onboarding {
				return &e, ctx.Err()
			}
			return &CorrectionEvidence{Version: 1, Input: current, Action: action}, ctx.Err()
		}
	}
	if !stored || e.Reported || e.Resolved {
		return nil, nil
	}
	if !sameBinding(d.Evidence, e.Input.Binding.Evidence) || current.Onboarding != e.Input.Onboarding {
		return retire()
	}
	return &e, ctx.Err()
}

// AcknowledgeCorrection is called only after the existing feedback controller
// verifies the canonical comment. Lost responses leave evidence pending.
func (c *Controller) AcknowledgeCorrection(ctx context.Context, r storage.Resource, expected CorrectionEvidence) error {
	if c.cfg.Corrections == nil {
		return ErrIncomplete
	}
	_, e, err := c.correction(ctx, r)
	if errors.Is(err, storage.ErrNotFound) || err == nil && e.Reported {
		return nil
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(e, expected) {
		return ErrChanged
	}
	return c.markCorrection(ctx, r, true)
}
