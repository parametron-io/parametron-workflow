package semanticflow

import (
	"context"
	"errors"

	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// AcceptedStore exposes reads only; no Runner, scheduling, or writes are needed.
type AcceptedStore interface {
	Provenance(context.Context, string, string) (storage.Provenance, error)
	Event(context.Context, string) (storage.Event, error)
}
type AcceptedReader interface {
	AcceptedIssue(context.Context, storage.Resource) (semanticpolicy.AcceptedIssue, bool, error)
}
type AcceptedLoader struct{ store AcceptedStore }

func NewAcceptedLoader(store AcceptedStore) (*AcceptedLoader, error) {
	if absent(store) {
		return nil, observe.ErrConfiguration
	}
	return &AcceptedLoader{store}, nil
}
func (l *AcceptedLoader) AcceptedIssue(ctx context.Context, r storage.Resource) (semanticpolicy.AcceptedIssue, bool, error) {
	if err := ctx.Err(); err != nil {
		return semanticpolicy.AcceptedIssue{}, false, err
	}
	n, err := normalizedResource(r)
	if err != nil || n.Kind != "issue" {
		return semanticpolicy.AcceptedIssue{}, false, observe.ErrBinding
	}
	key, _ := ResourceKey(r)
	p, err := l.store.Provenance(ctx, Namespace, key)
	if errors.Is(err, storage.ErrNotFound) {
		return semanticpolicy.AcceptedIssue{}, false, nil
	}
	if err != nil {
		return semanticpolicy.AcceptedIssue{}, false, persistence(err)
	}
	c, err := loadCompletion(ctx, l.store, p, n)
	if err != nil {
		return semanticpolicy.AcceptedIssue{}, false, err
	}
	v, ok := c.Issue()
	return v, ok, nil
}
