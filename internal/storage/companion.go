package storage

import (
	"context"
	"fmt"
)

// CompanionField is opaque package-owned management evidence. Revision is a
// local compare-and-swap generation, never a GitHub effect receipt.
type CompanionField struct {
	Key, Item, Project, Field string
	Revision                  int64
	Metadata                  []byte
}

func (s *Store) CompanionFields(ctx context.Context, project, field string) ([]CompanionField, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stable_key,item_id,project_id,field_id,revision,metadata FROM pr_companion_fields WHERE project_id=? AND field_id=? ORDER BY stable_key LIMIT 4097`, project, field)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CompanionField{}
	for rows.Next() {
		var r CompanionField
		if err := rows.Scan(&r.Key, &r.Item, &r.Project, &r.Field, &r.Revision, &r.Metadata); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveCompanionField commits only a local state transition. Exact field identity
// is immutable and a stale generation cannot replace another attempt.
func (s *Store) SaveCompanionField(ctx context.Context, previous int64, r CompanionField) error {
	if previous < 0 || r.Revision != previous+1 || !nonblank(r.Key) || !nonblank(r.Item) || !nonblank(r.Project) || !nonblank(r.Field) || len(r.Metadata) == 0 {
		return ErrInvalid
	}
	return s.Transaction(ctx, func(tx *Tx) error {
		var n int64
		if previous == 0 {
			result, err := tx.tx.ExecContext(ctx, `INSERT INTO pr_companion_fields(stable_key,item_id,project_id,field_id,revision,metadata) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, r.Key, r.Item, r.Project, r.Field, r.Revision, r.Metadata)
			if err != nil {
				return storageError(err)
			}
			n, err = result.RowsAffected()
			if err != nil {
				return err
			}
		} else {
			result, err := tx.tx.ExecContext(ctx, `UPDATE pr_companion_fields SET revision=?,metadata=? WHERE stable_key=? AND item_id=? AND project_id=? AND field_id=? AND revision=?`, r.Revision, r.Metadata, r.Key, r.Item, r.Project, r.Field, previous)
			if err != nil {
				return storageError(err)
			}
			n, err = result.RowsAffected()
			if err != nil {
				return err
			}
		}
		if n != 1 {
			return fmt.Errorf("%w: companion generation", ErrConflict)
		}
		return nil
	})
}
