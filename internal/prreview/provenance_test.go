package prreview

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// Alter read-side provenance independently of the production encoding and real
// SQLite insertion rules, to exercise corruption that an operator/import could expose.
type alteredStore struct {
	*storage.Store
	edit func(string, []storage.Provenance) []storage.Provenance
}

func (s alteredStore) ProvenanceRecords(ctx context.Context, ns string) ([]storage.Provenance, error) {
	ps, err := s.Store.ProvenanceRecords(ctx, ns)
	if err != nil {
		return nil, err
	}
	return s.edit(ns, ps), nil
}
func TestProvenanceRejectsCorruptionWithoutEffects(t *testing.T) {
	for _, name := range []string{"noncanonical", "invalid_json", "unsupported_version", "wrong_project", "wrong_target", "wrong_branch", "wrong_pr", "wrong_delivery", "orphan_attempt", "missing_attempt", "reordered_times", "receipt_without_attempt", "wrong_receipt", "duplicate_start"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.handle(f.ready("READY1", f.in.Candidate.At))
			f.handle(f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute)))
			before := len(f.writes)
			f.cfg.Store = alteredStore{Store: f.store, edit: func(ns string, ps []storage.Provenance) []storage.Provenance {
				if name == "orphan_attempt" && ns == startNS {
					return nil
				}
				if (name == "missing_attempt" || name == "receipt_without_attempt") && ns == attemptNS {
					return nil
				}
				if len(ps) == 0 {
					return ps
				}
				p := &ps[0]
				if ns == startNS {
					var s startEvidence
					if json.Unmarshal(p.Metadata, &s) != nil {
						t.Fatal("fixture")
					}
					switch name {
					case "noncanonical":
						p.Metadata = append([]byte(" "), p.Metadata...)
						return ps
					case "invalid_json":
						p.Metadata = []byte("{")
						return ps
					case "unsupported_version":
						s.Version = 2
					case "wrong_project":
						s.Project = "OTHER"
					case "wrong_target":
						s.Binding.Target.ID = "OTHER"
					case "wrong_branch":
						s.Binding.Branch.Name = "manual"
					case "wrong_pr":
						s.Resource.NodeID = "OTHER"
					case "wrong_delivery":
						p.DeliveryID = "REWORK1"
					case "duplicate_start":
						return append(ps, ps[0])
					}
					p.Metadata, _ = json.Marshal(s)
				}
				if name == "reordered_times" && ns == doneNS {
					p.CreatedAt = p.CreatedAt.Add(-time.Hour)
				}
				if name == "wrong_receipt" && ns == doneNS {
					var r receiptEvidence
					json.Unmarshal(p.Metadata, &r)
					r.Effect.Review.ID = "OTHER"
					p.Metadata, _ = json.Marshal(r)
				}
				return ps
			}}
			f.rebuild()
			err := f.c.Converge(context.Background(), f.pr.Resource)
			if !errors.Is(err, ErrProvenance) {
				t.Fatal(err)
			}
			if len(f.writes) != before || f.drafts != 1 {
				t.Fatal("corrupt evidence wrote", f.writes)
			}
		})
	}
}
func TestProvenanceInsertionAndCancellation(t *testing.T) {
	f := setup(t)
	ps, err := f.store.ProvenanceRecords(context.Background(), startNS)
	if err != nil || len(ps) != 0 {
		t.Fatal(ps, err)
	}
	f.handle(f.ready("READY1", f.in.Candidate.At))
	ps, err = f.store.ProvenanceRecords(context.Background(), startNS)
	if err != nil || len(ps) != 1 {
		t.Fatal(ps, err)
	}
	inserted, err := f.store.RecordProvenance(context.Background(), ps[0])
	if err != nil || inserted {
		t.Fatal(inserted, err)
	}
	p := ps[0]
	p.Metadata = []byte(`{}`)
	if _, err = f.store.RecordProvenance(context.Background(), p); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := len(f.writes)
	if err = f.c.Converge(ctx, f.pr.Resource); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(f.writes) != before {
		t.Fatal("cancelled write")
	}
}
func TestCompetingPRCannotUseTargetReviewOwnership(t *testing.T) {
	f := setup(t)
	f.handle(f.ready("READY1", f.in.Candidate.At))
	before := len(f.writes)
	// Both bindings are independently accepted by the #46 boundary; #49 must
	// reject competition before persisting a second cycle or writing either item.
	f.in.Binding.Evidence.PR.ID = "PR2"
	f.in.Binding.Evidence.PR.Number = 50
	f.in.PR.Identity = f.in.Binding.Evidence.PR
	f.in.Onboarding.Binding = f.in.Binding.Evidence
	f.pr.Resource = resource(f.in.PR.Identity, "pull_request")
	f.in.Onboarding.Resource = f.pr.Resource
	f.in.Facts.PR = f.in.PR.Identity
	f.in.Facts.Transitions = nil
	dev := f.in.Binding.Evidence.Developer
	at := f.now.Add(time.Minute)
	f.in.Facts.Transitions = append(f.in.Facts.Transitions, github.ReviewTransition{ID: "READY_OTHER", Kind: "ReadyForReviewEvent", PR: f.in.PR.Identity, Actor: &dev, CreatedAt: at})
	e := f.event("competing", Candidate{Kind: "ready", PRID: "PR2", Number: 50, Actor: dev, At: at})
	err := f.c.Handle(context.Background(), e)
	if !errors.Is(err, ErrRejected) || len(f.writes) != before || f.drafts != 0 {
		t.Fatal(err, f.writes)
	}
	ps, err := f.store.ProvenanceRecords(context.Background(), startNS)
	if err != nil || len(ps) != 1 {
		t.Fatal(ps, err)
	}
}

func TestLinkedCyclesRejectBrokenHistory(t *testing.T) {
	for _, name := range []string{"missing_previous", "wrong_previous", "missing_first_cycle", "duplicate_ready_time", "next_before_receipt", "onboarding_conflict"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.handle(f.ready("READY1", f.in.Candidate.At))
			f.handle(f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute)))
			f.handle(f.ready("READY2", f.in.Candidate.At.Add(3*time.Minute)))
			before := len(f.writes)
			f.cfg.Store = alteredStore{Store: f.store, edit: func(ns string, ps []storage.Provenance) []storage.Provenance {
				if ns != startNS {
					return ps
				}
				if name == "missing_first_cycle" {
					return ps[1:]
				}
				var s startEvidence
				json.Unmarshal(ps[1].Metadata, &s)
				switch name {
				case "missing_previous":
					s.Previous = ""
				case "wrong_previous":
					s.Previous = "OTHER"
				case "duplicate_ready_time":
					s.Ready.CreatedAt = f.in.Candidate.At
				case "next_before_receipt":
					ps[1].CreatedAt = ps[0].CreatedAt
				case "onboarding_conflict":
					s.Onboarding.DeliveryID = "OTHER"
				}
				ps[1].Metadata, _ = json.Marshal(s)
				return ps
			}}
			f.rebuild()
			if err := f.c.Converge(context.Background(), f.pr.Resource); !errors.Is(err, ErrProvenance) {
				t.Fatal(err)
			}
			if len(f.writes) != before {
				t.Fatal(f.writes)
			}
		})
	}
}
func TestStoredReviewEvidenceCannotOverrideCurrentFacts(t *testing.T) {
	for _, name := range []string{"missing_ready", "changed_native_actor", "manual_draft", "observed_non_draft_without_ready"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.handle(f.ready("READY1", f.in.Candidate.At))
			if name == "observed_non_draft_without_ready" {
				f.handle(f.review("REWORK1", "CHANGES_REQUESTED", f.in.Candidate.At.Add(time.Minute)))
				f.in.PR.Draft = false
			} else {
				switch name {
				case "missing_ready":
					f.in.Facts.Transitions = nil
				case "changed_native_actor":
					f.in.Facts.Transitions[0].Actor = nil
				case "manual_draft":
					f.in.PR.Draft = true
				}
			}
			before := len(f.writes)
			if err := f.c.Converge(context.Background(), f.pr.Resource); err == nil {
				t.Fatal("historical evidence overrode facts")
			}
			if len(f.writes) != before {
				t.Fatal(f.writes)
			}
		})
	}
}
