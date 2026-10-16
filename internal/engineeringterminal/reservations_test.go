package engineeringterminal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func assertForcePairs(t *testing.T, s *storage.Store, want int) {
	t.Helper()
	comments, err := s.ProvenanceRecords(context.Background(), forceCommentNS)
	if err != nil {
		t.Fatal(err)
	}
	cycles, err := s.ProvenanceRecords(context.Background(), forceCycleNS)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != want || len(cycles) != want {
		t.Fatalf("partial or unexpected reservations: comments=%d cycles=%d want=%d", len(comments), len(cycles), want)
	}
	for _, c := range comments {
		var accepted ForceAcceptance
		if json.Unmarshal(c.Metadata, &accepted) != nil {
			t.Fatal("invalid acceptance")
		}
		paired, err := s.Provenance(context.Background(), forceCycleNS, forceKey(accepted.Issue, accepted.Closed.ID))
		if err != nil || paired.DeliveryID != c.DeliveryID || !paired.CreatedAt.Equal(c.CreatedAt) || !reflect.DeepEqual(paired.Metadata, c.Metadata) {
			t.Fatal("unpaired reservation", err)
		}
	}
}

func restartForceController(t *testing.T, previous *Controller, path string) (*Controller, *storage.Store) {
	t.Helper()
	s, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cfg := previous.cfg
	cfg.Store = s
	return &Controller{cfg: cfg}, s
}
func TestForceReservationConflictsAcrossRepresentations(t *testing.T) {
	for _, name := range []string{"database", "node", "other-issue", "other-repository"} {
		t.Run(name, func(t *testing.T) {
			f := forceFacts()
			path := filepath.Join(t.TempDir(), "force.sqlite")
			c, s := forceStoreController(t, path, &f)
			ctx := context.Background()
			if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); !ok || err != nil {
				t.Fatal(err)
			}
			original, err := s.ProvenanceRecords(ctx, forceCommentNS)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			c, s = restartForceController(t, c, path)
			f = nextForceCycle(f, "COMMENT2", 12)
			f.Items = f.Items[1:]
			switch name {
			case "database":
				f.Items[len(f.Items)-2].Comment.DatabaseID = 11
			case "node":
				f.Items[len(f.Items)-2].ID = "COMMENT1"
				f.Items[len(f.Items)-2].Comment.ID = "COMMENT1"
			case "other-issue", "other-repository":
				f.Items[len(f.Items)-2].ID = "COMMENT1"
				f.Items[len(f.Items)-2].Comment.ID = "COMMENT1"
				if name == "other-issue" {
					f.Issue.ID = "OTHER_ISSUE"
					f.Issue.Number = 2
				} else {
					f.Issue.Repository.ID = "OTHER_REPO"
					f.Issue.Repository.Name = "other"
				}
				for n := range f.Items {
					f.Items[n].Issue = f.Issue
					if f.Items[n].Comment != nil {
						f.Items[n].Comment.Issue = f.Issue
					}
				}
			}
			if err := f.Validate(f.Issue, "CLOSED"); err != nil {
				t.Fatal("invalid adversarial timeline", err)
			}
			if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); ok || !errors.Is(err, ErrProvenance) {
				t.Fatal("identity reuse was not rejected", err)
			}
			assertForcePairs(t, s, 1)
			after, err := s.ProvenanceRecords(ctx, forceCommentNS)
			if err != nil || !reflect.DeepEqual(original, after) {
				t.Fatal("rejection changed historical reservations", err)
			}
			s.Close()
			c, s = restartForceController(t, c, path)
			if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); ok || !errors.Is(err, ErrProvenance) {
				t.Fatal("restart lost conflict", err)
			}
			assertForcePairs(t, s, 1)
		})
	}
}
func TestForceReservationConcurrentConflictsSQLite(t *testing.T) {
	for _, same := range []string{"database", "node", "exact"} {
		t.Run(same, func(t *testing.T) {
			first, second := forceFacts(), forceFacts()
			if same != "exact" {
				second.Items[1].ID = "ALTERNATE_CLOSE"
				if same == "database" {
					second.Items[0].ID = "ALTERNATE_COMMENT"
					second.Items[0].Comment.ID = "ALTERNATE_COMMENT"
				} else {
					second.Items[0].Comment.DatabaseID = 12
				}
			}
			path := filepath.Join(t.TempDir(), "concurrent.sqlite")
			a, s := forceStoreController(t, path, &first)
			b, _ := forceStoreController(t, path, &second)
			start := make(chan struct{})
			var wg sync.WaitGroup
			ok := make([]bool, 2)
			errs := make([]error, 2)
			for n, c := range []*Controller{a, b} {
				wg.Go(func() {
					<-start
					facts := first
					if n == 1 {
						facts = second
					}
					ok[n], errs[n] = c.acceptedForce(context.Background(), facts, "synthetic-delivery", true)
				})
			}
			close(start)
			wg.Wait()
			successes := 0
			for n := range ok {
				if ok[n] {
					successes++
					if errs[n] != nil {
						t.Fatal(errs[n])
					}
				} else if !errors.Is(errs[n], ErrProvenance) {
					t.Fatal("unexpected conflict failure", errs[n])
				}
			}
			want := 1
			if same == "exact" {
				want = 2
			}
			if successes != want {
				t.Fatalf("successful claimants=%d want=%d errors=%v", successes, want, errs)
			}
			assertForcePairs(t, s, 1)
		})
	}
}
func legacyForceRows(t *testing.T, c *Controller, f github.IssueClosure) (storage.Provenance, storage.Provenance) {
	t.Helper()
	accepted, ok, err := forceCandidate(f)
	if !ok || err != nil {
		t.Fatal(err)
	}
	origin, err := c.cfg.Store.Event(context.Background(), "synthetic-delivery")
	if err != nil {
		t.Fatal(err)
	}
	accepted.Project = c.cfg.Project.ID
	accepted.Origin = *origin.Resource
	data, err := json.Marshal(accepted)
	if err != nil {
		t.Fatal(err)
	}
	row := storage.Provenance{Namespace: forceCommentNS, Key: forceKey(f.Issue, accepted.Comment.ID), DeliveryID: "synthetic-delivery", Metadata: data, CreatedAt: c.cfg.Clock()}
	cycle := row
	cycle.Namespace = forceCycleNS
	cycle.Key = forceKey(f.Issue, accepted.Closed.ID)
	return row, cycle
}
func TestForceReservationLegacyAndCorruptHistory(t *testing.T) {
	for _, name := range []string{"valid", "missing-cycle", "missing-comment", "metadata-disagreement", "corrupt-json", "wrong-delivery", "database-missing", "comment-issue", "boundary", "wrong-key"} {
		t.Run(name, func(t *testing.T) {
			f := forceFacts()
			path := filepath.Join(t.TempDir(), "legacy.sqlite")
			c, s := forceStoreController(t, path, &f)
			ctx := context.Background()
			comment, cycle := legacyForceRows(t, c, f)
			switch name {
			case "metadata-disagreement":
				cycle.Metadata = append([]byte(" "), cycle.Metadata...)
			case "corrupt-json":
				comment.Metadata = []byte("not-json")
				cycle.Metadata = comment.Metadata
			case "wrong-delivery":
				r := storage.Resource{Owner: "synthetic-org", Repository: "other", Kind: "issue", Number: 2, NodeID: "OTHER"}
				if _, err := s.InsertDelivery(ctx, storage.Delivery{ID: "other-delivery", EventName: "issues", Payload: []byte(`{}`), ReceivedAt: f.CreatedAt, Resource: &r}); err != nil {
					t.Fatal(err)
				}
				comment.DeliveryID = "other-delivery"
				cycle.DeliveryID = "other-delivery"
			case "database-missing", "comment-issue", "boundary":
				var a ForceAcceptance
				if json.Unmarshal(comment.Metadata, &a) != nil {
					t.Fatal("legacy decode")
				}
				if name == "database-missing" {
					a.Comment.DatabaseID = 0
				} else if name == "comment-issue" {
					a.Comment.Issue.ID = "OTHER"
				} else {
					a.Boundary.ID = "UNKNOWN"
				}
				data, err := json.Marshal(a)
				if err != nil {
					t.Fatal(err)
				}
				comment.Metadata = data
				cycle.Metadata = data
			case "wrong-key":
				comment.Key = "wrong-key"
			}
			if err := s.Transaction(ctx, func(tx *storage.Tx) error {
				if name != "missing-comment" {
					if _, err := tx.RecordProvenance(comment); err != nil {
						return err
					}
				}
				if name != "missing-cycle" {
					_, err := tx.RecordProvenance(cycle)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			c, s = restartForceController(t, c, path)
			beforeComments, _ := s.ProvenanceRecords(ctx, forceCommentNS)
			beforeCycles, _ := s.ProvenanceRecords(ctx, forceCycleNS)
			ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true)
			if name == "valid" {
				if !ok || err != nil {
					t.Fatal("valid legacy authority rejected", err)
				}
			} else if ok || err == nil {
				t.Fatal("invalid legacy authority accepted")
			}
			// An unrelated new candidate must not hide or repair corrupt old history.
			if name != "valid" {
				f = nextForceCycle(f, "NEW_COMMENT", 12)
				if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); ok || err == nil {
					t.Fatal("new acceptance bypassed corrupt history")
				}
			}
			afterComments, _ := s.ProvenanceRecords(ctx, forceCommentNS)
			afterCycles, _ := s.ProvenanceRecords(ctx, forceCycleNS)
			if !reflect.DeepEqual(beforeComments, afterComments) || !reflect.DeepEqual(beforeCycles, afterCycles) {
				t.Fatal("legacy history silently changed")
			}
		})
	}
}
func TestForceReservationPersistenceFailureAndCancellation(t *testing.T) {
	f := forceFacts()
	path := filepath.Join(t.TempDir(), "rollback.sqlite")
	c, s := forceStoreController(t, path, &f)
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if ok, err := c.acceptedForce(canceled, f, "synthetic-delivery", true); ok || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled acceptance", err)
	}
	assertForcePairs(t, s, 0)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TRIGGER fail_force_cycle BEFORE INSERT ON provenance WHEN NEW.namespace='force_close_cycle_v1' BEGIN SELECT RAISE(ABORT,'synthetic persistence failure'); END"); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); ok || err == nil {
		t.Fatal("failed persistence authorized")
	}
	assertForcePairs(t, s, 0)
	if _, err := db.Exec("DROP TRIGGER fail_force_cycle"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	c, s = restartForceController(t, c, path)
	assertForcePairs(t, s, 0)
	if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); !ok || err != nil {
		t.Fatal("rollback prevented later recovery", err)
	}
	assertForcePairs(t, s, 1)
}
func TestForceReservationMultipleLegacyPairs(t *testing.T) {
	f := forceFacts()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	c, s := forceStoreController(t, path, &f)
	ctx := context.Background()
	for n := 1; n <= 32; n++ {
		f = forceFacts()
		f.Issue.ID = fmt.Sprintf("ISSUE-%d", n)
		f.Issue.Number = n
		f.Items[0].ID = fmt.Sprintf("COMMENT-%d", n)
		f.Items[0].Comment.ID = f.Items[0].ID
		f.Items[0].Comment.DatabaseID = int64(n)
		f.Items[1].ID = fmt.Sprintf("CLOSE-%d", n)
		for j := range f.Items {
			f.Items[j].Issue = f.Issue
			if f.Items[j].Comment != nil {
				f.Items[j].Comment.Issue = f.Issue
			}
		}
		comment, cycle := legacyForceRows(t, c, f)
		if err := s.Transaction(ctx, func(tx *storage.Tx) error {
			if _, err := tx.RecordProvenance(comment); err != nil {
				return err
			}
			_, err := tx.RecordProvenance(cycle)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	c, s = restartForceController(t, c, path)
	if ok, err := c.acceptedForce(ctx, f, "", false); !ok || err != nil {
		t.Fatal("multiple legitimate legacy pairs rejected", err)
	}
	assertForcePairs(t, s, 32)
	f = nextForceCycle(f, "NEW-COMMENT", 99)
	f.UpdatedAt = f.UpdatedAt.Add(time.Second)
	if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); !ok || err != nil {
		t.Fatal("new comment after legacy scan rejected", err)
	}
	assertForcePairs(t, s, 33)
}
