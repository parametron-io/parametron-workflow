package prreview

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

const (
	startNS   = "pr_review_cycle_v1"
	attemptNS = "pr_review_draft_attempt_v1"
	doneNS    = "pr_review_draft_done_v1"
)

type startEvidence struct {
	Version    int                     `json:"version"`
	Resource   storage.Resource        `json:"resource"`
	Binding    prbinding.Evidence      `json:"binding"`
	Project    string                  `json:"project"`
	Field      string                  `json:"field"`
	Onboarding pronboarding.Completion `json:"onboarding"`
	Ready      github.ReviewTransition `json:"ready"`
	Previous   string                  `json:"previous"`
}
type effectEvidence struct {
	Version int                     `json:"version"`
	Start   startEvidence           `json:"start"`
	Review  github.ReviewSubmission `json:"review"`
}
type receiptEvidence struct {
	Version int                     `json:"version"`
	Effect  effectEvidence          `json:"effect"`
	Draft   github.ReviewTransition `json:"draft"`
}
type record struct {
	start             startEvidence
	startAt           time.Time
	attempt           *effectEvidence
	attemptAt, doneAt time.Time
	effectDelivery    string
	draft             *github.ReviewTransition
}

func cycleKey(s startEvidence) string { return key(s.Resource) + "/" + s.Ready.ID }
func (r *record) cycle() Cycle {
	c := Cycle{Ready: r.start.Ready, DraftCompleted: !r.doneAt.IsZero()}
	if r.draft != nil {
		c.Draft, c.DraftAt = *r.draft, r.draft.CreatedAt
	}
	if r.attempt != nil {
		review := r.attempt.Review
		c.Rework = &review
	}
	return c
}
func (c *Controller) configured(i github.Identity) bool {
	if !validID(i.ID) || i.Number <= 0 {
		return false
	}
	for _, r := range c.cfg.Deployment.Repositories {
		if sameRepository(i.Repository, r) {
			return true
		}
	}
	return false
}
func (c *Controller) validBinding(b prbinding.Evidence) bool {
	return validEvidence(b) && c.configured(b.PR) && c.configured(b.Target)
}
func (c *Controller) validStart(s startEvidence) bool {
	o := s.Onboarding
	t := s.Ready
	return s.Version == 1 && s.Resource == resource(s.Binding.PR, "pull_request") && c.validBinding(s.Binding) && s.Project == c.cfg.Deployment.Engineering.ID && s.Field == c.cfg.Deployment.Engineering.Fields[config.Status] && o.Completed && o.Resource == s.Resource && o.Project == s.Project && validID(o.DeliveryID) && !o.CompletedAt.IsZero() && sameBinding(o.Binding, s.Binding, false) && c.validBinding(o.Binding) && validID(t.ID) && t.Kind == "ReadyForReviewEvent" && t.PR == s.Binding.PR && t.Actor != nil && github.SameUser(*t.Actor, s.Binding.Developer) && t.CreatedAt.After(o.CompletedAt) && (s.Previous == "" || validID(s.Previous) && s.Previous != t.ID)
}

func (c *Controller) delivery(ctx context.Context, supplied storage.Event, r storage.Resource) error {
	e, err := c.cfg.Store.Event(ctx, supplied.Delivery.ID)
	if err != nil {
		return c.storageError(ctx, err)
	}
	if e.Resource == nil || key(*e.Resource) != key(r) || e.Resource.NodeID != "" && e.Resource.NodeID != r.NodeID || e.Delivery.EventName != supplied.Delivery.EventName || !bytes.Equal(e.Delivery.Payload, supplied.Delivery.Payload) || !e.Delivery.ReceivedAt.Equal(supplied.Delivery.ReceivedAt) || e.Sequence != supplied.Sequence {
		return ErrProvenance
	}
	return ctx.Err()
}
func canonicalMetadata(data []byte, out any) bool {
	if json.Unmarshal(data, out) != nil {
		return false
	}
	encoded, err := json.Marshal(out)
	return err == nil && bytes.Equal(encoded, data)
}
func (c *Controller) origin(ctx context.Context, p storage.Provenance, r storage.Resource, ready *github.ReviewTransition, review *github.ReviewSubmission) error {
	e, err := c.cfg.Store.Event(ctx, p.DeliveryID)
	if err != nil {
		return c.storageError(ctx, err)
	}
	if e.Delivery.ID != p.DeliveryID || e.Resource == nil || key(*e.Resource) != key(r) || e.Resource.NodeID != "" && e.Resource.NodeID != r.NodeID || e.Delivery.ReceivedAt.IsZero() || p.CreatedAt.Before(e.Delivery.ReceivedAt) {
		return ErrProvenance
	}
	candidate, ok, err := Notification(e.Delivery)
	if err != nil || !ok || candidate.PRID != r.NodeID || candidate.Number != r.Number {
		return ErrProvenance
	}
	if ready != nil {
		if candidate.Kind != "ready" || !candidate.At.Equal(ready.CreatedAt) || ready.Actor == nil || !github.SameUser(candidate.Actor, *ready.Actor) || ready.CreatedAt.After(e.Delivery.ReceivedAt) {
			return ErrProvenance
		}
	} else if review != nil {
		if candidate.Kind != "review" || candidate.ReviewID != review.ID || candidate.State != review.State || !candidate.At.Equal(review.SubmittedAt) || review.Author == nil || !sameActor(candidate.Actor, *review.Author) || review.SubmittedAt.After(e.Delivery.ReceivedAt) {
			return ErrProvenance
		}
	} else {
		return ErrProvenance
	}
	return ctx.Err()
}
func (c *Controller) load(ctx context.Context) ([]*record, error) {
	byKey := map[string]*record{}
	for _, ns := range []string{startNS, attemptNS, doneNS} {
		ps, err := c.cfg.Store.ProvenanceRecords(ctx, ns)
		if err != nil {
			return nil, c.storageError(ctx, err)
		}
		for _, p := range ps {
			if p.Namespace != ns || p.Key == "" || !validID(p.DeliveryID) || p.CreatedAt.IsZero() || p.CreatedAt.Location() != time.UTC {
				return nil, ErrProvenance
			}
			if ns == startNS {
				var s startEvidence
				if !canonicalMetadata(p.Metadata, &s) || !c.validStart(s) || cycleKey(s) != p.Key || byKey[p.Key] != nil || p.CreatedAt.Before(s.Ready.CreatedAt) {
					return nil, ErrProvenance
				}
				if err := c.origin(ctx, p, s.Resource, &s.Ready, nil); err != nil {
					return nil, err
				}
				byKey[p.Key] = &record{start: s, startAt: p.CreatedAt}
				continue
			}
			var effect effectEvidence
			var receipt receiptEvidence
			state := byKey[p.Key]
			if ns == doneNS {
				if !canonicalMetadata(p.Metadata, &receipt) || receipt.Version != 1 {
					return nil, ErrProvenance
				}
				effect = receipt.Effect
			} else if !canonicalMetadata(p.Metadata, &effect) {
				return nil, ErrProvenance
			}
			if effect.Version != 1 || state == nil || !reflect.DeepEqual(effect.Start, state.start) || effect.Review.PR != state.start.Binding.PR || !validID(effect.Review.ID) || effect.Review.State != "CHANGES_REQUESTED" || effect.Review.Author == nil || !validID(effect.Review.Author.ID) || strings.TrimSpace(effect.Review.Author.Login) == "" || effect.Review.Author.Type == "" || !effect.Review.SubmittedAt.After(state.start.Ready.CreatedAt) || p.CreatedAt.Before(state.startAt) || p.CreatedAt.Before(effect.Review.SubmittedAt) {
				return nil, ErrProvenance
			}
			if err := c.origin(ctx, p, state.start.Resource, nil, &effect.Review); err != nil {
				return nil, err
			}
			if ns == attemptNS {
				if state.attempt != nil {
					return nil, ErrProvenance
				}
				state.attempt, state.attemptAt, state.effectDelivery = &effect, p.CreatedAt, p.DeliveryID
			} else {
				draft := receipt.Draft
				if draft.Actor != nil && (!validID(draft.Actor.ID) || strings.TrimSpace(draft.Actor.Login) == "" || draft.Actor.Type == "") {
					return nil, ErrProvenance
				}
				if state.attempt == nil || !reflect.DeepEqual(*state.attempt, effect) || state.effectDelivery != p.DeliveryID || p.CreatedAt.Before(state.attemptAt) || !state.doneAt.IsZero() || !validID(draft.ID) || draft.Kind != "ConvertToDraftEvent" || draft.PR != state.start.Binding.PR || draft.CreatedAt.Before(effect.Review.SubmittedAt) || !draft.CreatedAt.After(state.start.Ready.CreatedAt) || draft.CreatedAt.After(p.CreatedAt) {
					return nil, ErrProvenance
				}
				state.doneAt = p.CreatedAt
				state.draft = &draft
			}
		}
	}
	out := make([]*record, 0, len(byKey))
	for _, r := range byKey {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return cycleKey(out[i].start) < cycleKey(out[j].start) })
	prs := map[github.Identity]bool{}
	resources := map[string]github.Identity{}
	for _, r := range out {
		k := key(r.start.Resource)
		if previous, ok := resources[k]; ok && previous != r.start.Binding.PR {
			return nil, ErrProvenance
		}
		resources[k] = r.start.Binding.PR
		prs[r.start.Binding.PR] = true
	}
	for pr := range prs {
		if _, err := latest(out, pr); err != nil {
			return nil, err
		}
	}
	return out, ctx.Err()
}
func latest(records []*record, pr github.Identity) (*record, error) {
	var sorted []*record
	for _, r := range records {
		if r.start.Binding.PR == pr {
			sorted = append(sorted, r)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Ready.CreatedAt.Before(sorted[j].start.Ready.CreatedAt) })
	var previous *record
	for _, r := range sorted {
		if previous == nil {
			if r.start.Previous != "" {
				return nil, ErrProvenance
			}
		} else if r.start.Previous != previous.start.Ready.ID || !r.start.Ready.CreatedAt.After(previous.start.Ready.CreatedAt) || previous.attempt == nil || previous.doneAt.IsZero() || previous.draft == nil || !r.start.Ready.CreatedAt.After(previous.draft.CreatedAt) || !sameBinding(r.start.Binding, previous.start.Binding, false) || !reflect.DeepEqual(r.start.Onboarding, previous.start.Onboarding) || r.startAt.Before(previous.doneAt) {
			return nil, ErrProvenance
		}
		previous = r
	}
	return previous, nil
}
func (c *Controller) record(ctx context.Context, ns, k, delivery string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ErrProvenance
	}
	_, err = c.cfg.Store.RecordProvenance(ctx, storage.Provenance{Namespace: ns, Key: k, DeliveryID: delivery, Metadata: data, CreatedAt: c.cfg.Clock().UTC()})
	if err != nil {
		return c.storageError(ctx, err)
	}
	return ctx.Err()
}
