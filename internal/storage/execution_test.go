package storage

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestClaimSettlementRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.sqlite")
	s := openTest(t, path)
	insertTest(t, s, fixture("one"))
	var wg sync.WaitGroup
	claims := make(chan Event, 16)
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := s.Claim(testContext, "one", testTime)
			if err == nil {
				claims <- e
			} else {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(claims)
	close(failures)
	if len(claims) != 1 {
		t.Fatalf("claims %d", len(claims))
	}
	old := <-claims
	for err := range failures {
		if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if e := eventTest(t, s, "one"); e.State.Status != Processing || e.State.Attempts != 1 {
		t.Fatal(e)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, path)
	if err := s.RecoverInterrupted(testContext); err != nil {
		t.Fatal(err)
	}
	e := eventTest(t, s, "one")
	if e.State.Attempts != 1 || e.State.Status != Pending || e.Resource == nil {
		t.Fatal(e)
	}
	current, err := s.Claim(testContext, "one", testTime)
	if err != nil || current.State.Attempts != 2 {
		t.Fatal(current, err)
	}
	if err := s.Settle(testContext, "one", old.State.Attempts, Completed, nil, ""); !errors.Is(err, ErrConflict) {
		t.Fatal("stale settlement", err)
	}
	next := testTime.Add(time.Hour)
	if err := s.Settle(testContext, "one", 2, Retryable, &next, "transient"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(testContext, "one", testTime); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	current, err = s.Claim(testContext, "one", next)
	if err != nil || current.State.Attempts != 3 {
		t.Fatal(current, err)
	}
	if err = s.Settle(testContext, "one", 3, Completed, nil, ""); err != nil {
		t.Fatal(err)
	}
	if inserted, err := s.InsertDelivery(testContext, fixture("one")); inserted || err != nil {
		t.Fatal(inserted, err)
	}
	s.Close()
	s = openTest(t, path)
	if err = s.RecoverInterrupted(testContext); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(testContext, "one", next); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if e = eventTest(t, s, "one"); e.State.Attempts != 3 || e.State.Status != Completed {
		t.Fatal(e)
	}
	insertTest(t, s, fixture("failed"))
	e, err = s.Claim(testContext, "failed", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(testContext, "failed", e.State.Attempts, Failed, nil, "permanent_request"); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverInterrupted(testContext); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(testContext, "failed", next); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestClaimsAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	a := openTest(t, path)
	b := openTest(t, path)
	insertTest(t, a, fixture("one"))
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, s := range []*Store{a, b} {
		go func(s *Store) { <-start; _, err := s.Claim(testContext, "one", testTime); results <- err }(s)
	}
	close(start)
	success := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 || eventTest(t, a, "one").State.Attempts != 1 {
		t.Fatal(success)
	}
}
