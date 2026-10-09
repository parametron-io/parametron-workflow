// Package storage persists event evidence and execution state, never workflow policy.
package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"modernc.org/sqlite"
)

var (
	ErrNotFound        = errors.New("storage: not found")
	ErrConflict        = errors.New("storage: conflicting record")
	ErrSchema          = errors.New("storage: incompatible schema")
	ErrInvalid         = errors.New("storage: invalid input")
	ErrTransactionDone = errors.New("storage: transaction finished")
)

// Status describes controller execution, never a GitHub Project status.
type Status string

const (
	Pending    Status = "pending"
	Processing Status = "processing"
	Retryable  Status = "retryable"
	Completed  Status = "completed"
	Failed     Status = "failed"
)

// Resource is a transport identity. NodeID is optional evidence, not part of
// the query key. Owner and repository are canonicalized to lowercase.
type Resource struct {
	Owner      string
	Repository string
	Kind       string
	Number     int64
	NodeID     string
}

// Delivery is immutable accepted evidence. Resource may be nil until resolved.
// ReceivedAt belongs to the first acceptance and is ignored on redelivery.
type Delivery struct {
	ID         string
	EventName  string
	Payload    []byte
	ReceivedAt time.Time
	Resource   *Resource
}

type ProcessState struct {
	Status        Status
	Attempts      int64
	NextAttemptAt *time.Time
	// ErrorCategory is a safe identifier, never a raw provider error message.
	ErrorCategory string
}

type Event struct {
	Sequence int64
	Delivery Delivery
	Resource *Resource // Current durable binding, possibly resolved after acceptance.
	State    ProcessState
}

// Provenance records a generic one-shot fact. Namespace/key are globally unique
// within this database. Resource association is available through DeliveryID.
type Provenance struct {
	Namespace  string
	Key        string
	DeliveryID string
	Metadata   []byte
	CreatedAt  time.Time
}

type Store struct{ db *sql.DB }

// Open accepts a filesystem filename (not a SQLite DSN). The parent directory
// must exist. It creates the file/schema but chooses no production location.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" {
		return nil, ErrInvalid
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute}
	q := u.Query()
	for _, pragma := range []string{"foreign_keys(ON)", "busy_timeout(5000)", "synchronous(FULL)"} {
		q.Add("_pragma", pragma)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	// One connection serializes short storage transactions, not worker execution.
	// DSN pragmas also apply if database/sql replaces that connection.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fail(err)
	}
	if mode != "wal" {
		return fail(fmt.Errorf("storage: WAL unavailable: %s", mode))
	}
	if err = initialize(ctx, db); err != nil {
		return fail(err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Tx is valid only inside Transaction's callback. Do not call Store methods
// within the callback or share Tx between goroutines.
type Tx struct {
	tx   *sql.Tx
	ctx  context.Context
	done bool
}

// Transaction commits only if fn succeeds. Errors and panics roll back all
// changes. External side effects cannot be made atomic by this transaction.
func (s *Store) Transaction(ctx context.Context, fn func(*Tx) error) error {
	if fn == nil {
		return ErrInvalid
	}
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	tx := &Tx{tx: raw, ctx: ctx}
	defer func() { tx.done = true; raw.Rollback() }()
	if err = fn(tx); err != nil {
		return err
	}
	return raw.Commit()
}

func (s *Store) InsertDelivery(ctx context.Context, delivery Delivery) (bool, error) {
	var inserted bool
	err := s.Transaction(ctx, func(tx *Tx) error { var err error; inserted, err = tx.insertDelivery(delivery); return err })
	if err != nil {
		return false, err
	}
	return inserted, nil
}

func (tx *Tx) insertDelivery(d Delivery) (bool, error) {
	if !nonblank(d.ID) || !nonblank(d.EventName) || len(d.Payload) == 0 || !validTime(d.ReceivedAt) {
		return false, ErrInvalid
	}
	var err error
	d.Resource, err = normalizeResource(d.Resource)
	if err != nil {
		return false, err
	}
	accepted, err := json.Marshal(d.Resource)
	if err != nil {
		return false, err
	}
	result, err := tx.tx.ExecContext(tx.ctx, `INSERT INTO deliveries(delivery_id,event_name,payload,received_at,accepted_resource)
 VALUES(?,?,?,?,?) ON CONFLICT(delivery_id) DO NOTHING`, d.ID, d.EventName, d.Payload, stamp(d.ReceivedAt), accepted)
	if err != nil {
		return false, storageError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count == 0 {
		var name string
		var payload, resource []byte
		err = tx.tx.QueryRowContext(tx.ctx, "SELECT event_name,payload,accepted_resource FROM deliveries WHERE delivery_id=?", d.ID).Scan(&name, &payload, &resource)
		if err != nil {
			return false, err
		}
		if name != d.EventName || !bytes.Equal(payload, d.Payload) || !bytes.Equal(resource, accepted) {
			return false, ErrConflict
		}
		return false, nil
	}
	if _, err = tx.tx.ExecContext(tx.ctx, "INSERT INTO processing VALUES(?, 'pending', 0, NULL, '')", d.ID); err != nil {
		return false, storageError(err)
	}
	if d.Resource != nil {
		if err = tx.BindResource(d.ID, *d.Resource); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *Store) Event(ctx context.Context, id string) (Event, error) {
	return scanEvent(s.db.QueryRowContext(ctx, eventSelect+" WHERE d.delivery_id=?", id))
}

// Event reads inside a transaction, allowing atomic read-then-update decisions.
func (tx *Tx) Event(id string) (Event, error) {
	if tx.done {
		return Event{}, ErrTransactionDone
	}
	return scanEvent(tx.tx.QueryRowContext(tx.ctx, eventSelect+" WHERE d.delivery_id=?", id))
}

// Work returns unfinished events in acceptance order, including unresolved
// resources. It does not filter retry eligibility or claim records.
func (s *Store) Work(ctx context.Context) ([]Event, error) {
	return s.work(ctx, eventSelect+" WHERE p.status IN ('pending','processing','retryable') ORDER BY d.sequence")
}

// WorkByResource returns all unfinished work, including processing records for
// later recovery, in durable acceptance order. It does not claim or schedule.
func (s *Store) WorkByResource(ctx context.Context, resource Resource) ([]Event, error) {
	r, err := normalizeResource(&resource)
	if err != nil {
		return nil, err
	}
	return s.work(ctx, eventSelect+` WHERE r.owner=? AND r.repository=? AND r.kind=? AND r.number=?
 AND p.status IN ('pending','processing','retryable') ORDER BY d.sequence`, r.Owner, r.Repository, r.Kind, r.Number)
}

func (s *Store) work(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) UpdateState(ctx context.Context, id string, state ProcessState) error {
	return s.Transaction(ctx, func(tx *Tx) error { return tx.UpdateState(id, state) })
}

// UpdateState replaces all generic execution metadata atomically. The caller
// owns transition, attempt-accounting, and retry policy.
func (tx *Tx) UpdateState(id string, state ProcessState) error {
	if tx.done {
		return ErrTransactionDone
	}
	if !validStatus(state.Status) || state.Attempts < 0 || (state.NextAttemptAt != nil && !validTime(*state.NextAttemptAt)) || (state.ErrorCategory != "" && !identifier.MatchString(state.ErrorCategory)) {
		return ErrInvalid
	}
	var next any
	if state.NextAttemptAt != nil {
		next = stamp(*state.NextAttemptAt)
	}
	result, err := tx.tx.ExecContext(tx.ctx, "UPDATE processing SET status=?,attempts=?,next_attempt_at=?,error_category=? WHERE delivery_id=?", state.Status, state.Attempts, next, state.ErrorCategory, id)
	if err != nil {
		return storageError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) BindResource(ctx context.Context, id string, resource Resource) error {
	return s.Transaction(ctx, func(tx *Tx) error { return tx.BindResource(id, resource) })
}

// BindResource attaches a resolved transport identity once. It never interprets
// payloads; equivalent repeated bindings are no-ops and conflicts fail.
func (tx *Tx) BindResource(id string, resource Resource) error {
	if tx.done {
		return ErrTransactionDone
	}
	r, err := normalizeResource(&resource)
	if err != nil {
		return err
	}
	result, err := tx.tx.ExecContext(tx.ctx, `INSERT INTO resources VALUES(?,?,?,?,?,?) ON CONFLICT(delivery_id) DO NOTHING`, id, r.Owner, r.Repository, r.Kind, r.Number, r.NodeID)
	if err != nil {
		return storageError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		var existing Resource
		if err = tx.tx.QueryRowContext(tx.ctx, "SELECT owner,repository,kind,number,node_id FROM resources WHERE delivery_id=?", id).Scan(&existing.Owner, &existing.Repository, &existing.Kind, &existing.Number, &existing.NodeID); err != nil {
			return err
		}
		if existing != *r {
			return ErrConflict
		}
	}
	return nil
}

func (s *Store) RecordProvenance(ctx context.Context, p Provenance) (bool, error) {
	var inserted bool
	err := s.Transaction(ctx, func(tx *Tx) error { var err error; inserted, err = tx.RecordProvenance(p); return err })
	if err != nil {
		return false, err
	}
	return inserted, nil
}

func (tx *Tx) RecordProvenance(p Provenance) (bool, error) {
	if tx.done {
		return false, ErrTransactionDone
	}
	if !identifier.MatchString(p.Namespace) || !nonblank(p.Key) || !nonblank(p.DeliveryID) || !validTime(p.CreatedAt) {
		return false, ErrInvalid
	}
	if p.Metadata == nil {
		p.Metadata = []byte{}
	}
	result, err := tx.tx.ExecContext(tx.ctx, `INSERT INTO provenance VALUES(?,?,?,?,?) ON CONFLICT(namespace,stable_key) DO NOTHING`, p.Namespace, p.Key, p.DeliveryID, p.Metadata, stamp(p.CreatedAt))
	if err != nil {
		return false, storageError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count == 0 {
		var id string
		var metadata []byte
		if err = tx.tx.QueryRowContext(tx.ctx, "SELECT delivery_id,metadata FROM provenance WHERE namespace=? AND stable_key=?", p.Namespace, p.Key).Scan(&id, &metadata); err != nil {
			return false, err
		}
		if id != p.DeliveryID || !bytes.Equal(metadata, p.Metadata) {
			return false, ErrConflict
		}
	}
	return count != 0, nil
}

func (s *Store) Provenance(ctx context.Context, namespace, key string) (Provenance, error) {
	var p Provenance
	var created string
	err := s.db.QueryRowContext(ctx, "SELECT namespace,stable_key,delivery_id,metadata,created_at FROM provenance WHERE namespace=? AND stable_key=?", namespace, key).Scan(&p.Namespace, &p.Key, &p.DeliveryID, &p.Metadata, &created)
	if err != nil {
		return p, storageError(err)
	}
	p.CreatedAt, err = parseStamp(created)
	return p, err
}

const eventSelect = `SELECT d.sequence,d.delivery_id,d.event_name,d.payload,d.received_at,d.accepted_resource,
 p.status,p.attempts,p.next_attempt_at,p.error_category,r.owner,r.repository,r.kind,r.number,r.node_id
 FROM deliveries d JOIN processing p ON p.delivery_id=d.delivery_id LEFT JOIN resources r ON r.delivery_id=d.delivery_id`

type scanner interface{ Scan(...any) error }

func scanEvent(row scanner) (Event, error) {
	var e Event
	var received string
	var accepted []byte
	var next, owner, repo, kind, node sql.NullString
	var number sql.NullInt64
	err := row.Scan(&e.Sequence, &e.Delivery.ID, &e.Delivery.EventName, &e.Delivery.Payload, &received, &accepted, &e.State.Status, &e.State.Attempts, &next, &e.State.ErrorCategory, &owner, &repo, &kind, &number, &node)
	if err != nil {
		return e, storageError(err)
	}
	if err = json.Unmarshal(accepted, &e.Delivery.Resource); err != nil {
		return e, err
	}
	if e.Delivery.ReceivedAt, err = parseStamp(received); err != nil {
		return e, err
	}
	if next.Valid {
		parsed, err := parseStamp(next.String)
		if err != nil {
			return e, err
		}
		e.State.NextAttemptAt = &parsed
	}
	if owner.Valid {
		e.Resource = &Resource{owner.String, repo.String, kind.String, number.Int64, node.String}
	}
	return e, nil
}

const timeFormat = "2006-01-02T15:04:05.000000000Z"

func stamp(t time.Time) string               { return t.UTC().Format(timeFormat) }
func parseStamp(s string) (time.Time, error) { return time.Parse(timeFormat, s) }
func validTime(t time.Time) bool             { return !t.IsZero() && t.UTC().Year() >= 1 && t.UTC().Year() <= 9999 }
func nonblank(s string) bool                 { return strings.TrimSpace(s) != "" }

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
var githubName = regexp.MustCompile(`^[a-z0-9_.-]+$`)

func normalizeResource(r *Resource) (*Resource, error) {
	if r == nil {
		return nil, nil
	}
	copy := *r
	copy.Owner = strings.ToLower(copy.Owner)
	copy.Repository = strings.ToLower(copy.Repository)
	if !githubName.MatchString(copy.Owner) || !githubName.MatchString(copy.Repository) || !identifier.MatchString(copy.Kind) || copy.Number <= 0 {
		return nil, ErrInvalid
	}
	return &copy, nil
}
func validStatus(status Status) bool {
	switch status {
	case Pending, Processing, Retryable, Completed, Failed:
		return true
	}
	return false
}
func storageError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var sqliteErr *sqlite.Error
	// SQLite's primary SQLITE_CONSTRAINT code covers referential/check conflicts.
	if errors.As(err, &sqliteErr) && sqliteErr.Code()&255 == 19 {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}
