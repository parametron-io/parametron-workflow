package engineeringactivity

import (
	"context"
	"errors"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// State is validated read-only first-activity authority. DeliveryID is the
// original accepted push, never the ordinary event discovering this state.
type State struct {
	Accepted, Completed bool
	WorkItem            storage.Resource
	DeliveryID          string
	Evidence            Evidence
}
type StateReader interface {
	ActivityState(context.Context, storage.Resource) (State, error)
}

func (c *Controller) ActivityState(ctx context.Context, resource storage.Resource) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	b, err := c.cfg.Provenance.Completed(ctx, resource)
	if err != nil {
		return State{}, err
	}
	if !b.Completed {
		// Orphan activity facts must not disappear behind absent branch completion.
		for _, ns := range []string{firstNamespace, completionNamespace} {
			_, err := c.cfg.Store.Provenance(ctx, ns, key(identity(resource)))
			if err == nil {
				return State{}, ErrProvenance
			}
			if !errors.Is(err, storage.ErrNotFound) {
				return State{}, storageError(ctx, err)
			}
		}
		return State{}, ctx.Err()
	}
	name, err := eb.Name(resource)
	if err != nil || b.CreatedAt.IsZero() || b.Evidence.WorkItem != identity(resource) || b.Evidence.Version != 1 || b.Evidence.Branch != name || !github.ValidCommitSHA(b.Evidence.BaseSHA) {
		return State{}, ErrProvenance
	}
	configured := false
	for _, repo := range c.cfg.Repositories {
		if repo == b.Evidence.Repository {
			configured = true
		}
	}
	if !configured {
		return State{}, ErrProvenance
	}
	first, e, accepted, err := c.read(ctx, firstNamespace, b.Evidence, b.CreatedAt)
	if err != nil {
		return State{}, err
	}
	completion, done, completed, err := c.read(ctx, completionNamespace, b.Evidence, b.CreatedAt)
	if err != nil {
		return State{}, err
	}
	if completed && (!accepted || done != e || completion.DeliveryID != first.DeliveryID) {
		return State{}, ErrProvenance
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	return State{Accepted: accepted, Completed: completed, WorkItem: b.Evidence.WorkItem, DeliveryID: first.DeliveryID, Evidence: e}, nil
}
