package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestCompanionMigrationV1Upgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.sqlite")
	s := openTest(t, path)
	insertTest(t, s, fixture("one"))
	p := provenanceFixture()
	if err := s.Transaction(testContext, func(tx *Tx) error { return func() error { _, err := tx.RecordProvenance(p); return err }() }); err != nil {
		t.Fatal(err)
	}
	// Preserve the released v1 tables and data; remove both later tables.
	if _, err := s.db.Exec("DROP TABLE workflow_corrections"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE pr_companion_fields"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	before := eventTest(t, s, "one")
	s.Close()
	for range 2 {
		s = openTest(t, path)
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
			t.Fatal(version, err)
		}
		if got := eventTest(t, s, "one"); !reflect.DeepEqual(before, got) {
			t.Fatal("delivery changed", got)
		}
		got, err := s.Provenance(testContext, p.Namespace, p.Key)
		if err != nil || !bytesEqualProvenance(got, p) {
			t.Fatal("provenance changed", got, err)
		}
		if rows, err := s.CompanionFields(testContext, "ENG", "NUM"); err != nil || len(rows) != 0 {
			t.Fatal(rows, err)
		}
		s.Close()
	}
}

func TestCompanionMigrationRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed.sqlite")
	s := openTest(t, path)
	// A conflicting partial migration must fail without advancing version.
	if _, err := s.db.Exec("DROP TABLE pr_companion_fields"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TABLE pr_companion_fields (broken TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if reopened, err := Open(testContext, path); !errors.Is(err, ErrSchema) || reopened != nil {
		t.Fatal(reopened, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatal(version, err)
	}
}

func TestCompanionCASAndImmutableIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.sqlite")
	s := openTest(t, path)
	r := CompanionField{Key: "pr", Item: "ITEM", Project: "ENG", Field: "NUM", Revision: 1, Metadata: []byte("opaque")}
	if err := s.SaveCompanionField(testContext, 0, r); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*CompanionField){
		func(r *CompanionField) { r.Item = "replacement" },
		func(r *CompanionField) { r.Project = "BUG" },
		func(r *CompanionField) { r.Field = "score" },
		func(r *CompanionField) { r.Key = "other" },
	} {
		next := r
		next.Revision = 2
		change(&next)
		if err := s.SaveCompanionField(testContext, 1, next); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if err := s.SaveCompanionField(testContext, 0, r); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	duplicate := r
	duplicate.Key = "another"
	if err := s.SaveCompanionField(testContext, 0, duplicate); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			next := r
			next.Revision = 2
			results <- s.SaveCompanionField(testContext, 1, next)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal(successes, conflicts)
	}
	if err := s.SaveCompanionField(testContext, 0, r); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testContext)
	cancel()
	next := r
	next.Revision = 3
	if err := s.SaveCompanionField(ctx, 2, next); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.Close()
	s = openTest(t, path)
	rows, err := s.CompanionFields(testContext, "ENG", "NUM")
	if err != nil || len(rows) != 1 || rows[0].Revision != 2 || string(rows[0].Metadata) != "opaque" {
		t.Fatal(rows, err)
	}
}

func TestCorrectionMigrationV2UpgradePreservesCompanion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.sqlite")
	s := openTest(t, path)
	row := CompanionField{Key: "PR", Item: "ITEM", Project: "ENG", Field: "NUM", Revision: 1, Metadata: []byte("synthetic")}
	if err := s.SaveCompanionField(testContext, 0, row); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE workflow_corrections"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for range 2 {
		s = openTest(t, path)
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
			t.Fatal(version, err)
		}
		got, err := s.CompanionFields(testContext, "ENG", "NUM")
		if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], row) {
			t.Fatal("v2 companion changed", got, err)
		}
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM workflow_corrections").Scan(&count); err != nil || count != 0 {
			t.Fatal(count, err)
		}
		s.Close()
	}
}
