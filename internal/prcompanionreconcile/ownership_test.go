package prcompanionreconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type rowsStore struct {
	OwnershipStore
	rows []storage.CompanionField
	err  error
}

func (s rowsStore) CompanionFields(context.Context, string, string) ([]storage.CompanionField, error) {
	return s.rows, s.err
}
func TestOwnershipRejectsCorruptRecords(t *testing.T) {
	mutations := map[string]func(*record){
		"version":         func(o *record) { o.Version++ },
		"management":      func(o *record) { o.Management = "human" },
		"project":         func(o *record) { o.Project = "BUG" },
		"field":           func(o *record) { o.Field = "SCORE" },
		"item":            func(o *record) { o.Item = o.PR.ID },
		"revision":        func(o *record) { o.Revision++ },
		"delivery":        func(o *record) { o.Acquisition.DeliveryID = "absent" },
		"origin":          func(o *record) { o.Acquisition.Origin.Number++ },
		"target":          func(o *record) { o.Acquisition.Binding.Evidence.Target.ID = o.PR.ID },
		"branch":          func(o *record) { o.Acquisition.Binding.Evidence.Branch.SHA = "invalid" },
		"developer":       func(o *record) { o.Acquisition.Binding.Evidence.Developer.Type = "Bot" },
		"policy":          func(o *record) { o.Acquisition.Binding.Evidence.Policy = "observed" },
		"number":          func(o *record) { v := 10000; o.Acquisition.Value = &v },
		"timestamp":       func(o *record) { o.Acquisition.CompletedAt = o.Acquisition.StartedAt.Add(-time.Second) },
		"missing receipt": func(o *record) { o.Acquisition.CompletedAt = time.Time{} },
		"contradictory attempt": func(o *record) {
			o.Attempt.Binding = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.InvalidDeveloper}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 1)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			rows, err := h.store.CompanionFields(context.Background(), "ENG", "NUMBER")
			if err != nil {
				t.Fatal(err)
			}
			var o record
			if err := json.Unmarshal(rows[0].Metadata, &o); err != nil {
				t.Fatal(err)
			}
			mutate(&o)
			rows[0].Metadata, _ = json.Marshal(o)
			h.c.cfg.Store = rowsStore{OwnershipStore: h.store, rows: rows}
			if err := h.run(); !errors.Is(err, ErrOwnership) {
				t.Fatal(err)
			}
			if len(h.writes) != 1 {
				t.Fatal(h.writes)
			}
		})
	}
}
func TestOwnershipCanonicalBytesLimitsAndStorageErrors(t *testing.T) {
	for _, name := range []string{"noncanonical", "oversized", "duplicate key", "duplicate item", "limit", "storage", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 1)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			rows, _ := h.store.CompanionFields(context.Background(), "ENG", "NUMBER")
			want := ErrOwnership
			var storeErr error
			switch name {
			case "noncanonical":
				rows[0].Metadata = append(rows[0].Metadata, ' ')
			case "oversized":
				rows[0].Metadata = make([]byte, 32769)
			case "duplicate key":
				rows = append(rows, rows[0])
			case "duplicate item":
				dup := rows[0]
				dup.Key = "other"
				rows = append(rows, dup)
			case "limit":
				rows = make([]storage.CompanionField, prOwnershipLimit+1)
			case "storage":
				storeErr = errors.New("disk unavailable")
				want = ErrStorage
			case "cancelled":
				storeErr = context.Canceled
				want = context.Canceled
			}
			h.c.cfg.Store = rowsStore{OwnershipStore: h.store, rows: rows, err: storeErr}
			if err := h.run(); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if len(h.writes) != 1 {
				t.Fatal(h.writes)
			}
		})
	}
}
func TestDurableCASConflictPreventsTransport(t *testing.T) {
	h := newHarness(t, 1)
	h.c.cfg.Store = &storeHook{OwnershipStore: h.store, save: func(int64) error { return storage.ErrConflict }}
	if err := h.run(); !errors.Is(err, ErrOwnership) {
		t.Fatal(err)
	}
	if len(h.writes) != 0 {
		t.Fatal(h.writes)
	}
}
func TestOwnedRetargetRebaseClearReopenMultipleCycles(t *testing.T) {
	h := newHarness(t, 2)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	original := h.entries[0].Binding.Evidence.Target
	for cycle := 0; cycle < 3; cycle++ {
		h.restart()
		// Insert a canonical earlier root. Real #36 computation must rebase both companions.
		earlier := h.issues[0]
		earlier.ID = "EARLIER"
		earlier.Number = 1
		h.issues = append(h.issues, earlier)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		for _, a := range h.expected().Assignments {
			for i, e := range h.entries {
				if e.Resource == a.PR {
					if got, _ := h.value(i); got != float64(a.RoadmapOrder) {
						t.Fatal(got, a)
					}
				}
			}
		}
		h.issues = h.issues[:1]
		// A new accepted Target grants current authority independently of acquisition.
		next := h.issues[0]
		next.ID = fmt.Sprint("TARGET", cycle)
		next.Number = 50 + cycle
		h.issues = append(h.issues, next)
		h.entries[0].Binding.Evidence.Target = next.Identity
		h.entries[0].Binding.Evidence.Branch.Name = fmt.Sprint("development/issue-", next.Number)
		target := *h.entries[0].Target
		target.Resource = resource(next.Identity, "issue")
		target.Issue = &next
		h.entries[0].Target = &target
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		h.entries[0].Binding = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.InvalidDeveloper}
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if _, ok := h.value(0); ok {
			t.Fatal("owned rejected number retained")
		}
		// The acquisition Target remains historical. It must never replace the fresh binding.
		rows, err := h.c.ownership(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rows[h.entries[0].Resource].Acquisition.Binding.Evidence.Target != original {
			t.Fatal("acquisition rewritten")
		}
		h.entries[0].Binding = h.entries[1].Binding
		h.entries[0].Binding.Evidence.PR = h.entries[0].PR.PullRequest.Identity
		target = *h.entries[1].Target
		h.entries[0].Target = &target
		h.issues = h.issues[:1]
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestUnknownBindingDoesNotClearHumanValue(t *testing.T) {
	h := newHarness(t, 1)
	h.drift(0, 10001)
	h.entries[0].Binding = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.InvalidDeveloper}
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if len(h.writes) != 0 {
		t.Fatal("human value cleared")
	}
	h.entries[0].Binding = prbinding.Decision{}
	if err := h.run(); !errors.Is(err, ErrIncomplete) {
		t.Fatal(err)
	}
	if got, ok := h.value(0); !ok || got != 10001 {
		t.Fatal(got, ok)
	}
}
func TestIssueSnapshotChangesDuringDiscovery(t *testing.T) {
	h := newHarness(t, 1)
	h.hook = func(n int) {
		if n == 1 {
			h.issues[0].State = "CLOSED"
		}
	}
	if err := h.run(); !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
	if len(h.writes) != 0 {
		t.Fatal(h.writes)
	}
}
func TestBoundedEmptyAndNineCompanions(t *testing.T) {
	for _, n := range []int{0, 1, 9} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			h := newHarness(t, n)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if len(h.writes) != n || h.discoveries != 1+3*n {
				t.Fatal(h.writes, h.discoveries)
			}
		})
	}
}
func TestOwnedClearRequiresExactCurrentItemAndType(t *testing.T) {
	for _, mode := range []string{"archived", "absent", "wrong type", "duplicate", "wrong project"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, 1)
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			h.entries[0].PR.PullRequest.State = "CLOSED"
			p := &h.entries[0].PR.Projects[0]
			switch mode {
			case "archived":
				p.Items[0].Archived = true
			case "absent":
				p.Items = nil
			case "wrong type":
				v := p.Items[0].Fields[config.RoadmapOrder]
				v.Kind = config.SingleSelect
				p.Items[0].Fields[config.RoadmapOrder] = v
			case "duplicate":
				p.Items = append(p.Items, p.Items[0])
			case "wrong project":
				p.ID = "BUG"
			}
			if err := h.run(); !errors.Is(err, ErrOwnership) {
				t.Fatal(err)
			}
			if len(h.writes) != 1 {
				t.Fatal(h.writes)
			}
		})
	}
}
func TestVerifiedRecoveryFollowsReopenAfterPartialClear(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	h.entries[0].PR.PullRequest.State = "CLOSED"
	h.writeHook = func(context.Context) error { return &github.Error{Category: github.Transient} }
	if err := h.run(); err == nil {
		t.Fatal("lost clear response")
	}
	h.writeHook = nil
	h.entries[0].PR.PullRequest.State = "OPEN"
	h.restart()
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got, ok := h.value(0); !ok || got != float64(h.expected().Assignments[0].RoadmapOrder) {
		t.Fatal(got, ok)
	}
}

func TestConcurrentFirstAcquisitionCASAuthorizesOnlyOneTransport(t *testing.T) {
	a, b := newHarness(t, 1), newHarness(t, 1)
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	save := func(previous int64) error {
		if previous == 0 {
			arrived <- struct{}{}
			<-release
		}
		return nil
	}
	a.c.cfg.Store = &storeHook{OwnershipStore: a.store, save: save}
	b.c.cfg.Store = &storeHook{OwnershipStore: a.store, save: save}
	results := make(chan error, 2)
	go func() { results <- a.run() }()
	go func() { results <- b.run() }()
	<-arrived
	<-arrived
	close(release)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, ErrOwnership) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 || len(a.writes)+len(b.writes) != 1 {
		t.Fatal(successes, conflicts, a.writes, b.writes)
	}
}
