package engineeringactivity

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func TestActivityStateWithoutFirstActivityIsEmpty(t *testing.T) {
	for _, completedBranch := range []bool{false, true} {
		name := "without completed branch"
		if completedBranch {
			name = "with completed branch"
		}
		t.Run(name, func(t *testing.T) {
			// The existing #38 fixture establishes real durable #37 completion
			// when requested, but has never accepted first activity.
			f := setup(t, completedBranch)
			got, err := f.controller.ActivityState(context.Background(), resource(38))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, State{}) {
				t.Errorf("no accepted first activity must return empty State; got %#v", got)
			}
			if f.reads != 0 || f.comparisons != 0 || len(f.writes) != 0 {
				t.Fatalf("read-only state performed external work: branch reads=%d comparisons=%d writes=%v", f.reads, f.comparisons, f.writes)
			}
		})
	}
}

func assertStateReadOnly(t *testing.T, f *fixture) {
	t.Helper()
	if f.reads != 0 || f.comparisons != 0 || len(f.writes) != 0 {
		t.Fatalf("state read performed external effects: reads=%d comparisons=%d writes=%v", f.reads, f.comparisons, f.writes)
	}
}

func TestActivityStateAcceptedCompletionAndRestart(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted unfinished", true: "completed"}[completed], func(t *testing.T) {
			f := setup(t, true)
			if !completed {
				f.failure = "item38:date"
			}
			result, err := f.handle(input(sp.Task, ""))
			if completed && err != nil || !completed && err == nil || !result.Accepted || result.Completed != completed {
				t.Fatal(result, err)
			}
			f.reads, f.comparisons, f.writes = 0, 0, nil
			want := State{Accepted: true, Completed: completed, WorkItem: identity(resource(38)), DeliveryID: "push", Evidence: result.Evidence}
			got, err := f.controller.ActivityState(context.Background(), resource(38))
			if err != nil || got != want {
				t.Fatal(got, want, err)
			}
			assertStateReadOnly(t, f)
			got.WorkItem.Number = 99
			got.DeliveryID = "fabricated"
			got.Evidence.HeadSHA = base
			f.restart()
			again, err := f.controller.ActivityState(context.Background(), resource(38))
			if err != nil || again != want {
				t.Fatal(again, want, err)
			}
			assertStateReadOnly(t, f)
		})
	}
}

func TestActivityStateCorruptProvenanceFailsClosed(t *testing.T) {
	for _, bad := range []string{"completion without first", "different evidence", "different delivery", "malformed", "noncanonical", "unknown field", "duplicate key", "bad branch", "bad timestamp", "pre-creation", "equal-creation", "unbound delivery"} {
		t.Run(bad, func(t *testing.T) {
			f := setup(t, true)
			result, err := f.handle(input(sp.Task, ""))
			if err != nil || !result.Completed {
				t.Fatal(result, err)
			}
			branch, err := f.branch.Completed(context.Background(), resource(38))
			if err != nil {
				t.Fatal(err)
			}
			f.delivery("other-push", true, acceptedAt)
			if bad == "pre-creation" || bad == "equal-creation" {
				at := branch.CreatedAt
				if bad == "pre-creation" {
					at = at.Add(-time.Nanosecond)
				}
				f.delivery("early-push", true, at)
			}
			f.controller.cfg.Store = faultyStore{Store: f.store, read: func(ctx context.Context, ns, k string) (storage.Provenance, error) {
				if bad == "completion without first" && ns == firstNamespace {
					return storage.Provenance{}, storage.ErrNotFound
				}
				p, err := f.store.Provenance(ctx, ns, k)
				if err != nil {
					return p, err
				}
				if ns != completionNamespace {
					return p, nil
				}
				var e Evidence
				if err := json.Unmarshal(p.Metadata, &e); err != nil {
					t.Fatal(err)
				}
				switch bad {
				case "different evidence":
					e.HeadSHA = strings.Repeat("c", 40)
				case "different delivery":
					p.DeliveryID = "other-push"
				case "malformed":
					p.Metadata = []byte("{")
					return p, nil
				case "noncanonical":
					p.Metadata = append([]byte(" "), p.Metadata...)
					return p, nil
				case "unknown field", "duplicate key":
					field := `,"extra":true}`
					if bad == "duplicate key" {
						field = `,"version":1}`
					}
					p.Metadata = append(p.Metadata[:len(p.Metadata)-1], []byte(field)...)
					return p, nil
				case "bad branch":
					e.Branch.Branch = "manual"
				case "bad timestamp":
					p.CreatedAt = time.Time{}
				case "pre-creation", "equal-creation":
					event, err := f.store.Event(ctx, "early-push")
					if err != nil {
						t.Fatal(err)
					}
					p.DeliveryID = "early-push"
					e.ReceivedAt = event.Delivery.ReceivedAt.UTC()
					e.StartDate = e.ReceivedAt.Format(time.DateOnly)
				case "unbound delivery":
					p.DeliveryID = "fabricated"
				}
				p.Metadata, err = json.Marshal(e)
				return p, err
			}}
			f.reads, f.comparisons, f.writes = 0, 0, nil
			got, err := f.controller.ActivityState(context.Background(), resource(38))
			if !errors.Is(err, ErrProvenance) || got != (State{}) {
				t.Fatal(got, err)
			}
			assertStateReadOnly(t, f)
		})
	}
}

func TestActivityStateStorageAndBranchCorruption(t *testing.T) {
	for _, bad := range []string{"first storage", "completion storage", "orphan storage", "branch storage", "branch metadata", "branch timestamp", "canceled"} {
		t.Run(bad, func(t *testing.T) {
			f := setup(t, bad != "orphan storage")
			want := ErrStorage
			ctx := context.Background()
			switch bad {
			case "first storage", "completion storage", "orphan storage":
				namespace := firstNamespace
				if bad == "completion storage" {
					namespace = completionNamespace
				}
				f.controller.cfg.Store = faultyStore{Store: f.store, read: func(ctx context.Context, ns, k string) (storage.Provenance, error) {
					if ns == namespace {
						return storage.Provenance{}, errors.New("private storage diagnostic")
					}
					return f.store.Provenance(ctx, ns, k)
				}}
			case "branch storage":
				want = eb.ErrStorage
				f.controller.cfg.Provenance = evidenceFunc(func(context.Context, storage.Resource) (eb.Completion, error) { return eb.Completion{}, eb.ErrStorage })
			case "branch metadata", "branch timestamp":
				want = ErrProvenance
				b, err := f.branch.Completed(ctx, resource(38))
				if err != nil {
					t.Fatal(err)
				}
				if bad == "branch timestamp" {
					b.CreatedAt = time.Time{}
				} else {
					b.Evidence.BaseSHA = "invalid"
				}
				f.controller.cfg.Provenance = evidenceFunc(func(context.Context, storage.Resource) (eb.Completion, error) { return b, nil })
			case "canceled":
				want = context.Canceled
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := f.controller.ActivityState(ctx, resource(38))
			if !errors.Is(err, want) || got != (State{}) || strings.Contains(err.Error(), "private") {
				t.Fatal(got, err)
			}
			assertStateReadOnly(t, f)
		})
	}
}

func TestActivityStateOrphanFactsWithoutBranchCompletion(t *testing.T) {
	for _, completionOnly := range []bool{false, true} {
		f := setup(t, true)
		result, err := f.handle(input(sp.Task, ""))
		if err != nil || !result.Completed {
			t.Fatal(result, err)
		}
		f.controller.cfg.Provenance = evidenceFunc(func(context.Context, storage.Resource) (eb.Completion, error) { return eb.Completion{}, nil })
		if completionOnly {
			f.controller.cfg.Store = faultyStore{Store: f.store, read: func(ctx context.Context, ns, k string) (storage.Provenance, error) {
				if ns == firstNamespace {
					return storage.Provenance{}, storage.ErrNotFound
				}
				return f.store.Provenance(ctx, ns, k)
			}}
		}
		f.reads, f.comparisons, f.writes = 0, 0, nil
		got, err := f.controller.ActivityState(context.Background(), resource(38))
		if !errors.Is(err, ErrProvenance) || got != (State{}) {
			t.Fatal(got, err)
		}
		assertStateReadOnly(t, f)
	}
}
