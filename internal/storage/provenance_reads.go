package storage

import "context"

// ProvenanceRecords enumerates one caller-owned namespace in stable key order.
// Interpretation and validation remain with the namespace owner.
func (s *Store) ProvenanceRecords(ctx context.Context, namespace string) ([]Provenance, error) {
	if !identifier.MatchString(namespace) {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, "SELECT namespace,stable_key,delivery_id,metadata,created_at FROM provenance WHERE namespace=? ORDER BY stable_key", namespace)
	if err != nil {
		return nil, storageError(err)
	}
	defer rows.Close()
	out := []Provenance{}
	for rows.Next() {
		var p Provenance
		var stamp string
		if err := rows.Scan(&p.Namespace, &p.Key, &p.DeliveryID, &p.Metadata, &stamp); err != nil {
			return nil, err
		}
		p.CreatedAt, err = parseStamp(stamp)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
