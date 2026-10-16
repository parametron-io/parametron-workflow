package storage

import (
	"bytes"
	"context"
)

// RecordForceClose atomically reserves both the comment and native closure
// identities. Existing generic provenance remains append-only; neither index can
// be repurposed for an alternative closure or an alternative comment on retry.
func (s *Store) RecordForceClose(ctx context.Context, comment, cycle Provenance) error {
	if comment.Namespace != "force_close_comment_v1" || cycle.Namespace != "force_close_cycle_v1" || comment.DeliveryID != cycle.DeliveryID || !bytes.Equal(comment.Metadata, cycle.Metadata) || !comment.CreatedAt.Equal(cycle.CreatedAt) || len(comment.Metadata) > 1048576 {
		return ErrInvalid
	}
	return s.Transaction(ctx, func(tx *Tx) error {
		if _, err := tx.RecordProvenance(comment); err != nil {
			return err
		}
		_, err := tx.RecordProvenance(cycle)
		return err
	})
}
