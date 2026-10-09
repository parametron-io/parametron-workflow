package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"
)

var testContext = context.Background()
var testTime = time.Date(2026, 10, 9, 10, 11, 12, 345678901, time.FixedZone("fixture", 3*60*60))

func fixture(id string) Delivery {
	return Delivery{ID: id, EventName: "issues", Payload: []byte(`{"action":"edited","issue":{"number":10}}`), ReceivedAt: testTime, Resource: &Resource{Owner: "Parametron-IO", Repository: "Parametron-Workflow", Kind: "issue", Number: 10, NodeID: "I_10"}}
}
func openTest(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(testContext, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func insertTest(t *testing.T, s *Store, d Delivery) {
	t.Helper()
	inserted, err := s.InsertDelivery(testContext, d)
	if err != nil || !inserted {
		t.Fatalf("insert: %v %v", inserted, err)
	}
}
func eventTest(t *testing.T, s *Store, id string) Event {
	t.Helper()
	e, err := s.Event(testContext, id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func provenanceFixture() Provenance {
	return Provenance{Namespace: "test.fact", Key: "resource-10", DeliveryID: "one", Metadata: []byte(`{"fact":true}`), CreatedAt: testTime}
}

func TestInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.sqlite")
	s := openTest(t, path)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("version %d: %v", version, err)
	}
	for pragma, want := range map[string]any{"foreign_keys": int64(1), "journal_mode": "wal", "synchronous": int64(2), "busy_timeout": int64(5000)} {
		var got any
		if err := s.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %v, want %v: %v", pragma, got, want, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, path)
	if _, err := s.Event(testContext, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Provenance(testContext, "missing", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestSchemaRejection(t *testing.T) {
	for _, tc := range []struct{ name, setup string }{
		{"future", "PRAGMA user_version=2"},
		{"unversioned", "CREATE TABLE unrelated (value TEXT)"},
		{"similarly named application table", "CREATE TABLE sqliteOther (value TEXT)"},
		{"missing", "PRAGMA user_version=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(tc.setup); err != nil {
				t.Fatal(err)
			}
			db.Close()
			s, err := Open(testContext, path)
			if s != nil {
				s.Close()
				t.Fatal("unexpected store")
			}
			if !errors.Is(err, ErrSchema) {
				t.Fatal(err)
			}
		})
	}
	t.Run("altered current schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.sqlite")
		s := openTest(t, path)
		if _, err := s.db.Exec("ALTER TABLE processing ADD COLUMN surprise TEXT"); err != nil {
			t.Fatal(err)
		}
		s.Close()
		if reopened, err := Open(testContext, path); !errors.Is(err, ErrSchema) {
			if reopened != nil {
				reopened.Close()
			}
			t.Fatal(err)
		}
	})
}

func TestMigrationRollback(t *testing.T) {
	// Inject a broken next migration to prove both initial DDL and user_version
	// roll back together. No parallel tests mutate migration history.
	original := migrations
	migrations = append(append([][]string{}, original...), []string{"CREATE TABLE partial (id INTEGER)", "INVALID SQL"})
	defer func() { migrations = original }()
	path := filepath.Join(t.TempDir(), "events.sqlite")
	if s, err := Open(testContext, path); !errors.Is(err, ErrSchema) {
		if s != nil {
			s.Close()
		}
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, count int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if version != 0 || count != 0 {
		t.Fatalf("partial migration: version=%d objects=%d", version, count)
	}
	migrations = original
	s := openTest(t, path)
	insertTest(t, s, fixture("one"))
	// A failing later migration also preserves the previous schema and records.
	s.Close()
	migrations = append(append([][]string{}, original...), []string{"ALTER TABLE processing ADD COLUMN partial TEXT", "INVALID SQL"})
	if s, err := Open(testContext, path); !errors.Is(err, ErrSchema) {
		if s != nil {
			s.Close()
		}
		t.Fatal(err)
	}
	migrations = original
	s = openTest(t, path)
	eventTest(t, s, "one")
}

func TestRestartDurabilityAndDeduplication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.sqlite")
	s := openTest(t, path)
	d := fixture("one")
	insertTest(t, s, d)
	next := testTime.Add(time.Hour)
	state := ProcessState{Status: Retryable, Attempts: 3, NextAttemptAt: &next, ErrorCategory: "rate_limited"}
	p := provenanceFixture()
	err := s.Transaction(testContext, func(tx *Tx) error {
		if err := tx.UpdateState(d.ID, state); err != nil {
			return err
		}
		inserted, err := tx.RecordProvenance(p)
		if !inserted {
			t.Error("expected provenance insertion")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	before := eventTest(t, s, d.ID)
	if before.Resource == nil || before.Resource.Owner != "parametron-io" {
		t.Fatalf("resource: %+v", before.Resource)
	}
	s.Close()
	s = openTest(t, path)
	after := eventTest(t, s, d.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("restart changed state: %+v -> %+v", before, after)
	}
	got, err := s.Provenance(testContext, p.Namespace, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(p.CreatedAt) || got.CreatedAt.Location() != time.UTC || !bytesEqualProvenance(got, p) {
		t.Fatalf("provenance: %+v", got)
	}
	d.ReceivedAt = d.ReceivedAt.Add(time.Hour) // Redelivery reception time is not evidence conflict.
	inserted, err := s.InsertDelivery(testContext, d)
	if err != nil || inserted {
		t.Fatalf("duplicate: %v %v", inserted, err)
	}
	if got := eventTest(t, s, d.ID); !reflect.DeepEqual(got, before) {
		t.Fatal("duplicate mutated existing state")
	}
	inserted, err = s.RecordProvenance(testContext, p)
	if err != nil || inserted {
		t.Fatalf("provenance duplicate: %v %v", inserted, err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM provenance").Scan(&count); err != nil || count != 1 {
		t.Fatalf("count %d: %v", count, err)
	}
	for _, mutate := range []func(*Delivery){func(d *Delivery) { d.Payload = []byte("different") }, func(d *Delivery) { d.EventName = "pull_request" }, func(d *Delivery) { r := *d.Resource; r.Number++; d.Resource = &r }, func(d *Delivery) { d.Resource = nil }} {
		conflict := d
		mutate(&conflict)
		if inserted, err = s.InsertDelivery(testContext, conflict); inserted || !errors.Is(err, ErrConflict) {
			t.Fatalf("conflicting delivery: %v %v", inserted, err)
		}
	}
	conflict := p
	conflict.Metadata = []byte("different")
	if inserted, err = s.RecordProvenance(testContext, conflict); inserted || !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting provenance: %v %v", inserted, err)
	}
	// The final identity protection lives in SQLite, independent of the API.
	_, err = s.db.Exec(`INSERT INTO deliveries(delivery_id,event_name,payload,received_at,accepted_resource) SELECT delivery_id,event_name,payload,received_at,accepted_resource FROM deliveries WHERE delivery_id='one'`)
	if !errors.Is(storageError(err), ErrConflict) {
		t.Fatalf("database duplicate accepted: %v", err)
	}
	_, err = s.db.Exec(`INSERT INTO provenance SELECT * FROM provenance`)
	if !errors.Is(storageError(err), ErrConflict) {
		t.Fatalf("database provenance duplicate accepted: %v", err)
	}
}
func bytesEqualProvenance(a, b Provenance) bool {
	return a.Namespace == b.Namespace && a.Key == b.Key && a.DeliveryID == b.DeliveryID && string(a.Metadata) == string(b.Metadata)
}

func TestResourcesAndWork(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "events.sqlite"))
	d := fixture("one")
	insertTest(t, s, d)
	for _, id := range []string{"two", "three", "four"} {
		d.ID = id
		insertTest(t, s, d)
	}
	if err := s.UpdateState(testContext, "two", ProcessState{Status: Completed}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateState(testContext, "three", ProcessState{Status: Processing, Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateState(testContext, "four", ProcessState{Status: Retryable, Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Resource{
		{Owner: "other", Repository: "parametron-workflow", Kind: "issue", Number: 10},
		{Owner: "parametron-io", Repository: "other", Kind: "issue", Number: 10},
		{Owner: "parametron-io", Repository: "parametron-workflow", Kind: "pull_request", Number: 10},
		{Owner: "parametron-io", Repository: "parametron-workflow", Kind: "issue", Number: 11},
	} {
		other := fixture(r.Owner + r.Repository + r.Kind + strconv.FormatInt(r.Number, 10))
		other.Resource = &r
		insertTest(t, s, other)
		work, err := s.WorkByResource(testContext, r)
		if err != nil || len(work) != 1 {
			t.Fatalf("distinct resource: %v %v", work, err)
		}
	}
	query := *fixture("unused").Resource
	query.NodeID = "" // Node ID is not the work key.
	work, err := s.WorkByResource(testContext, query)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, e := range work {
		ids = append(ids, e.Delivery.ID)
	}
	if !reflect.DeepEqual(ids, []string{"one", "three", "four"}) {
		t.Fatal(ids)
	}
	unknown := fixture("unresolved")
	unknown.Resource = nil
	insertTest(t, s, unknown)
	if eventTest(t, s, unknown.ID).Resource != nil {
		t.Fatal("unexpected resolved identity")
	}
	all, err := s.Work(testContext)
	if err != nil || len(all) != 8 || all[len(all)-1].Delivery.ID != unknown.ID {
		t.Fatalf("unfinished work including unresolved event: %v %v", all, err)
	}
	if err = s.BindResource(testContext, unknown.ID, query); err != nil {
		t.Fatal(err)
	}
	if err = s.BindResource(testContext, unknown.ID, query); err != nil {
		t.Fatal(err)
	}
	query.Number++
	if err = s.BindResource(testContext, unknown.ID, query); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = s.BindResource(testContext, "missing", query); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	unknown.ReceivedAt = unknown.ReceivedAt.Add(time.Minute)
	if inserted, err := s.InsertDelivery(testContext, unknown); err != nil || inserted {
		t.Fatalf("redelivery after resolution: %v %v", inserted, err)
	}
}

func TestTransactions(t *testing.T) {
	for _, mode := range []string{"commit", "callback failure", "constraint failure", "panic"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.sqlite")
			s := openTest(t, path)
			d := fixture("one")
			d.Resource = nil
			insertTest(t, s, d)
			before := eventTest(t, s, d.ID)
			sentinel := errors.New("callback failure")
			var escaped *Tx
			run := func() error {
				return s.Transaction(testContext, func(tx *Tx) error {
					escaped = tx
					current, err := tx.Event(d.ID)
					if err != nil || current.State.Attempts != 0 {
						t.Fatalf("transaction read: %+v %v", current, err)
					}
					next := testTime.Add(time.Hour)
					if err := tx.UpdateState(d.ID, ProcessState{Status: Retryable, Attempts: 5, NextAttemptAt: &next, ErrorCategory: "transient"}); err != nil {
						return err
					}
					if err := tx.BindResource(d.ID, *fixture("unused").Resource); err != nil {
						return err
					}
					if _, err := tx.RecordProvenance(provenanceFixture()); err != nil {
						return err
					}
					switch mode {
					case "callback failure":
						return sentinel
					case "constraint failure":
						p := provenanceFixture()
						p.Key = "missing"
						p.DeliveryID = "missing"
						_, err := tx.RecordProvenance(p)
						return err
					case "panic":
						panic(sentinel)
					}
					return nil
				})
			}
			if mode == "panic" {
				func() {
					defer func() {
						if recover() != sentinel {
							t.Error("panic not preserved")
						}
					}()
					run()
				}()
			} else {
				err := run()
				switch mode {
				case "commit":
					if err != nil {
						t.Fatal(err)
					}
				case "callback failure":
					if !errors.Is(err, sentinel) {
						t.Fatal(err)
					}
				case "constraint failure":
					if !errors.Is(err, ErrConflict) {
						t.Fatal(err)
					}
				}
			}
			if err := escaped.UpdateState(d.ID, ProcessState{Status: Completed}); !errors.Is(err, ErrTransactionDone) {
				t.Fatal(err)
			}
			if _, err := escaped.Event(d.ID); !errors.Is(err, ErrTransactionDone) {
				t.Fatal(err)
			}
			s.Close()
			s = openTest(t, path)
			after := eventTest(t, s, d.ID)
			_, err := s.Provenance(testContext, "test.fact", "resource-10")
			if mode == "commit" {
				if after.State.Attempts != 5 || after.State.NextAttemptAt == nil || after.Resource == nil || err != nil {
					t.Fatalf("commit lost: %+v %v", after, err)
				}
			} else {
				if !reflect.DeepEqual(before, after) || !errors.Is(err, ErrNotFound) {
					t.Fatalf("partial transaction: %+v %v", after, err)
				}
			}
		})
	}
}

func TestStateReplacementAndValidation(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "events.sqlite"))
	insertTest(t, s, fixture("one"))
	next := testTime.Add(time.Hour)
	if err := s.UpdateState(testContext, "one", ProcessState{Status: Retryable, Attempts: 2, NextAttemptAt: &next, ErrorCategory: "transient"}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []Status{Pending, Processing, Retryable, Completed, Failed} {
		if err := s.UpdateState(testContext, "one", ProcessState{Status: status, Attempts: 2}); err != nil {
			t.Fatal(err)
		}
		got := eventTest(t, s, "one")
		if got.State.Status != status || got.State.NextAttemptAt != nil || got.State.ErrorCategory != "" {
			t.Fatal(got.State)
		}
	}
	before := eventTest(t, s, "one")
	zero := time.Time{}
	for _, state := range []ProcessState{{Status: "Ready"}, {Status: Pending, Attempts: -1}, {Status: Retryable, NextAttemptAt: &zero}, {Status: Failed, ErrorCategory: "Authorization: secret"}} {
		if err := s.UpdateState(testContext, "one", state); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if got := eventTest(t, s, "one"); !reflect.DeepEqual(got, before) {
			t.Fatal("invalid state partially persisted")
		}
	}
	if err := s.UpdateState(testContext, "missing", ProcessState{Status: Failed}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Delivery){func(d *Delivery) { d.ID = "" }, func(d *Delivery) { d.EventName = "" }, func(d *Delivery) { d.Payload = nil }, func(d *Delivery) { d.ReceivedAt = time.Time{} }, func(d *Delivery) { d.Resource.Number = 0 }} {
		d := fixture("invalid")
		mutate(&d)
		if inserted, err := s.InsertDelivery(testContext, d); inserted || !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid delivery %v %v", inserted, err)
		}
	}
	if _, err := s.Event(testContext, "invalid"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testContext)
	cancel()
	if _, err := s.InsertDelivery(ctx, fixture("cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Event(ctx, "one"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestConcurrentDeliveryDeduplication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.sqlite")
	stores := []*Store{openTest(t, path), openTest(t, path)}
	var wg sync.WaitGroup
	results := make(chan bool, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inserted, err := stores[i%2].InsertDelivery(testContext, fixture("one"))
			results <- inserted
			errs <- err
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	count := 0
	for inserted := range results {
		if inserted {
			count++
		}
	}
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if count != 1 {
		t.Fatalf("inserted %d times", count)
	}
}

func TestOpenLocation(t *testing.T) {
	for _, path := range []string{"", ":memory:", filepath.Join(t.TempDir(), "missing", "events.sqlite")} {
		if s, err := Open(testContext, path); err == nil {
			if s != nil {
				s.Close()
			}
			t.Fatalf("accepted %q", path)
		}
	}
	// Filenames are escaped and cannot inject SQLite DSN options.
	path := filepath.Join(t.TempDir(), "events?mode=memory#file.sqlite")
	s := openTest(t, path)
	insertTest(t, s, fixture("one"))
	s.Close()
	s = openTest(t, path)
	eventTest(t, s, "one")
}

func TestCancelledTransactionRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.sqlite")
	s := openTest(t, path)
	insertTest(t, s, fixture("one"))
	before := eventTest(t, s, "one")
	ctx, cancel := context.WithCancel(testContext)
	defer cancel()
	err := s.Transaction(ctx, func(tx *Tx) error {
		if err := tx.UpdateState("one", ProcessState{Status: Completed, Attempts: 1}); err != nil {
			return err
		}
		if _, err := tx.RecordProvenance(provenanceFixture()); err != nil {
			return err
		}
		cancel()
		return nil
	})
	// database/sql may already have rolled the transaction back on cancellation.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("cancelled commit: %v", err)
	}
	s.Close()
	s = openTest(t, path)
	if after := eventTest(t, s, "one"); !reflect.DeepEqual(after, before) {
		t.Fatal("cancelled transaction committed state")
	}
	if _, err := s.Provenance(testContext, "test.fact", "resource-10"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
