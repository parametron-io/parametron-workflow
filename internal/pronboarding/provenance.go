package pronboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type evidence struct {
	Version        int                 `json:"version"`
	Resource       storage.Resource    `json:"resource"`
	Binding        prbinding.Evidence  `json:"binding"`
	Organization   config.Organization `json:"organization"`
	Project        string              `json:"project"`
	Field          string              `json:"field"`
	Option         string              `json:"option"`
	Item           string              `json:"item"`
	InitiallyDraft bool                `json:"initially_draft"`
	StatusOwned    bool                `json:"status_owned"`
	StatusPresent  bool                `json:"status_present"`
	Status         observe.FieldValue  `json:"status"`
}

const prefix = "pr_onboarding_"

var stages = []string{"membership_intent", "prepared", "status_attempt", "status_done", "draft_attempt", "draft_done", "completed"}

type facts struct {
	e        evidence
	delivery string
	present  map[string]bool
}

func (c *Controller) storageError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, storage.ErrConflict) {
		return ErrProvenance
	}
	return ErrStorage
}
func (c *Controller) delivery(ctx context.Context, id string, r storage.Resource) error {
	e, err := c.cfg.Store.Event(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrProvenance
	}
	if err != nil {
		return c.storageError(ctx, err)
	}
	if e.Delivery.ID != id || e.Resource == nil || canonical(*e.Resource) != canonical(r) || e.Resource.NodeID != "" && e.Resource.NodeID != r.NodeID {
		return ErrProvenance
	}
	return ctx.Err()
}
func (c *Controller) validEvidence(e evidence, r storage.Resource) bool {
	if e.StatusPresent {
		role := config.StatusRole("")
		for k, v := range c.cfg.Deployment.Engineering.StatusOptions {
			if v == e.Status.OptionID {
				role = k
			}
		}
		if e.Status.Date != "" || e.Status.Number != 0 || e.Status.StatusRole != role {
			return false
		}
	}
	return e.Version == 1 && e.Resource == r && c.validBinding(e.Binding, canonical(r)) && e.Binding.PR.ID == r.NodeID && e.Organization == c.cfg.Deployment.Organization && e.Project == c.cfg.Deployment.Engineering.ID && e.Field == c.cfg.Deployment.Engineering.Fields[config.Status] && e.Option == c.cfg.Deployment.Engineering.StatusOptions[config.InProgress] && (e.Item == "" || validID(e.Item)) && (!e.StatusPresent && e.Status == (observe.FieldValue{}) || e.StatusPresent && e.Status.Kind == config.SingleSelect && validID(e.Status.OptionID))
}
func (c *Controller) load(ctx context.Context, r storage.Resource) (facts, error) {
	f := facts{present: map[string]bool{}}
	for _, stage := range stages {
		ns := prefix + stage + "_v1"
		p, err := c.cfg.Store.Provenance(ctx, ns, key(r))
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return f, c.storageError(ctx, err)
		}
		var e evidence
		err = json.Unmarshal(p.Metadata, &e)
		data, _ := json.Marshal(e)
		if err != nil || !bytes.Equal(data, p.Metadata) || !c.validEvidence(e, r) || p.Namespace != ns || p.Key != key(r) || p.DeliveryID == "" || p.CreatedAt.IsZero() {
			return f, ErrProvenance
		}
		if stage == "membership_intent" {
			if e.Item != "" || e.StatusPresent || e.Status != (observe.FieldValue{}) {
				return f, ErrProvenance
			}
			f.e, f.delivery = e, p.DeliveryID
		} else if stage == "prepared" {
			if !validID(e.Item) {
				return f, ErrProvenance
			}
			if f.present["membership_intent"] && (p.DeliveryID != f.delivery || !sameBinding(f.e.Binding, e.Binding, false) || f.e.InitiallyDraft != e.InitiallyDraft || f.e.StatusOwned != e.StatusOwned) {
				return f, ErrProvenance
			}
			f.e = e
			f.delivery = p.DeliveryID
		} else if !f.present["prepared"] || e != f.e || p.DeliveryID != f.delivery {
			return f, ErrProvenance
		}
		if err := c.delivery(ctx, p.DeliveryID, r); err != nil {
			return f, err
		}
		f.present[stage] = true
	}
	if (f.present["draft_attempt"] || f.present["draft_done"]) && !f.present["status_done"] || f.present["status_attempt"] && (!f.e.StatusOwned || f.e.Status.OptionID == f.e.Option) || f.present["draft_attempt"] && f.e.InitiallyDraft || f.present["completed"] && (!f.present["status_done"] || !f.present["draft_done"]) {
		return f, ErrProvenance
	}
	return f, ctx.Err()
}
func (c *Controller) record(ctx context.Context, f *facts, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.validEvidence(f.e, f.e.Resource) {
		return ErrProvenance
	}
	data, err := json.Marshal(f.e)
	if err != nil {
		return ErrProvenance
	}
	_, err = c.cfg.Store.RecordProvenance(ctx, storage.Provenance{Namespace: prefix + stage + "_v1", Key: key(f.e.Resource), DeliveryID: f.delivery, Metadata: data, CreatedAt: c.cfg.Clock()})
	if err != nil {
		return c.storageError(ctx, err)
	}
	f.present[stage] = true
	return ctx.Err()
}
