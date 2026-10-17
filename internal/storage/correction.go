package storage

import (
	"context"
	"database/sql"
	"errors"
)

// Correction retains one bounded current explanation per policy owner/resource.
// Metadata is owner-versioned evidence, never lifecycle mutation authorization.
type Correction struct {
	Owner, ResourceID string
	Revision          int64
	Metadata          []byte
}

func correctionOwner(s string) bool { return s == "development_v1" || s == "phase_closure_v1" }
func (s *Store) Correction(ctx context.Context, owner, id string) (Correction, error) {
	if !correctionOwner(owner) || !nonblank(id) {
		return Correction{}, ErrInvalid
	}
	var r Correction
	err := s.db.QueryRowContext(ctx, `SELECT owner,resource_id,revision,metadata FROM workflow_corrections WHERE owner=? AND resource_id=?`, owner, id).Scan(&r.Owner, &r.ResourceID, &r.Revision, &r.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return Correction{}, ErrNotFound
	}
	return r, err
}
func (s *Store) SaveCorrection(ctx context.Context, previous int64, r Correction) error {
	if !correctionOwner(r.Owner) || !nonblank(r.ResourceID) || previous < 0 || r.Revision != previous+1 || len(r.Metadata) == 0 || len(r.Metadata) > 1048576 {
		return ErrInvalid
	}
	var result sql.Result
	var err error
	if previous == 0 {
		result, err = s.db.ExecContext(ctx, `INSERT INTO workflow_corrections(owner,resource_id,revision,metadata) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, r.Owner, r.ResourceID, r.Revision, r.Metadata)
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE workflow_corrections SET revision=?,metadata=? WHERE owner=? AND resource_id=? AND revision=?`, r.Revision, r.Metadata, r.Owner, r.ResourceID, previous)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// CorrectionResources enumerates only bounded current owner slots, never a
// historical delivery log. Callers resolve exact current identities separately.
func (s *Store) CorrectionResources(ctx context.Context, owner string) ([]string, error) {
	if !correctionOwner(owner) {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT resource_id FROM workflow_corrections WHERE owner=? ORDER BY resource_id LIMIT 4097`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) > 4096 {
		return nil, ErrInvalid
	}
	return ids, rows.Err()
}
