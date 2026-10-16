package storage

import (
	"bytes"
	"context"
)

// RecordForceClose checks the owner's immutable identity constraints against all
// prior reservations inside the same immediate transaction as both inserts.
// This includes legacy records: their metadata is the database-ID reservation,
// so an absent secondary index can never make an old identity available again.
// The callback may read delivery evidence through the supplied transaction-local
// reader; it must not call Store methods or perform external side effects.
// Interpretation of acceptance metadata remains with the terminal owner.
func (s *Store) RecordForceClose(ctx context.Context, comment, cycle Provenance, validate func([]Provenance, func(string) (Event, error)) error) error {
	if validate == nil || comment.Namespace != "force_close_comment_v1" || cycle.Namespace != "force_close_cycle_v1" || comment.DeliveryID != cycle.DeliveryID || !bytes.Equal(comment.Metadata, cycle.Metadata) || !comment.CreatedAt.Equal(cycle.CreatedAt) || len(comment.Metadata) > 1048576 {
		return ErrInvalid
	}
	return s.Transaction(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(ctx, "SELECT namespace,stable_key,delivery_id,metadata,created_at FROM provenance WHERE namespace IN (?,?) ORDER BY namespace,stable_key", comment.Namespace, cycle.Namespace)
		if err != nil {
			return storageError(err)
		}
		defer rows.Close()
		var history []Provenance
		for rows.Next() {
			var p Provenance
			var created string
			if err := rows.Scan(&p.Namespace, &p.Key, &p.DeliveryID, &p.Metadata, &created); err != nil {
				return err
			}
			p.CreatedAt, err = parseStamp(created)
			if err != nil {
				return err
			}
			history = append(history, p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := validate(history, tx.Event); err != nil {
			return err
		}
		if _, err := tx.RecordProvenance(comment); err != nil {
			return err
		}
		_, err = tx.RecordProvenance(cycle)
		return err
	})
}
