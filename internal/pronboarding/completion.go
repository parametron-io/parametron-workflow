package pronboarding

import (
	"context"
	"encoding/json"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// Completion is validated historical onboarding evidence, not current binding
// authorization. Consumers must obtain fresh effect-specific authorization.
// Ordinary onboarding/review effects still require a fresh accepted binding.
type Completion struct {
	Completed   bool
	Resource    storage.Resource
	Binding     prbinding.Evidence
	Project     string
	DeliveryID  string
	CompletedAt time.Time
}

// Completed exposes only fully completed, identity-bound onboarding. It neither
// repairs membership nor repeats initial Status or Draft effects.
func (c *Controller) Completed(ctx context.Context, r storage.Resource) (Completion, error) {
	if err := ctx.Err(); err != nil {
		return Completion{}, err
	}
	if r.Kind != "pull_request" || r.Number <= 0 || !validID(r.NodeID) || canonical(r).Owner != r.Owner || canonical(r).Repository != r.Repository {
		return Completion{}, ErrInvalid
	}
	f, err := c.load(ctx, r)
	if err != nil {
		return Completion{}, err
	}
	if f.present["status_attempt"] && !f.present["status_done"] || f.present["draft_attempt"] && !f.present["draft_done"] {
		return Completion{}, ErrAmbiguous
	}
	if !f.present["completed"] {
		return Completion{}, nil
	}
	// load validates the canonical metadata, supported version, paired stages,
	// exact identities and originating durable delivery for every present fact.
	// Also verify chronological stage ordering before exporting a time barrier.
	origin, err := c.cfg.Store.Event(ctx, f.delivery)
	if err != nil {
		return Completion{}, c.storageError(ctx, err)
	}
	if origin.Delivery.ReceivedAt.IsZero() {
		return Completion{}, ErrProvenance
	}
	last := origin.Delivery.ReceivedAt
	for _, stage := range stages {
		if !f.present[stage] {
			continue
		}
		p, err := c.cfg.Store.Provenance(ctx, prefix+stage+"_v1", key(r))
		if err != nil {
			return Completion{}, c.storageError(ctx, err)
		}
		if p.CreatedAt.Before(last) {
			return Completion{}, ErrProvenance
		}
		last = p.CreatedAt
	}
	if err := ctx.Err(); err != nil {
		return Completion{}, err
	}
	return Completion{Completed: true, Resource: r, Binding: f.e.Binding, Project: f.e.Project, DeliveryID: f.delivery, CompletedAt: last}, nil
}

// Completions enumerates validated package-owned completion evidence. Consumers
// must still revalidate current authority; raw provenance never leaves here.
func (c *Controller) Completions(ctx context.Context) ([]Completion, error) {
	reader, ok := c.cfg.Store.(interface {
		ProvenanceRecords(context.Context, string) ([]storage.Provenance, error)
	})
	if !ok || absent(reader) {
		return nil, ErrInvalid
	}
	records, err := reader.ProvenanceRecords(ctx, prefix+"completed_v1")
	if err != nil {
		return nil, c.storageError(ctx, err)
	}
	out := make([]Completion, 0, len(records))
	for _, p := range records {
		var e evidence
		if json.Unmarshal(p.Metadata, &e) != nil {
			return nil, ErrProvenance
		}
		completed, err := c.Completed(ctx, e.Resource)
		if err != nil {
			return nil, err
		}
		if !completed.Completed || p.Key != key(e.Resource) || p.Namespace != prefix+"completed_v1" || p.DeliveryID != completed.DeliveryID || !p.CreatedAt.Equal(completed.CompletedAt) {
			return nil, ErrProvenance
		}
		out = append(out, completed)
	}
	return out, ctx.Err()
}
