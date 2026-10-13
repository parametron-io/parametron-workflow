package pronboarding

import (
	"context"
	"errors"
	"testing"
	"time"

	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func TestCompletionExportIsHistoricalAndReadOnly(t *testing.T) {
	f := auditSetup(t)
	c, err := f.c.Completed(context.Background(), f.o.Resource)
	if err != nil || c.Completed {
		t.Fatal(c, err)
	}
	f.noWrites()
	if err = f.handle(); err != nil {
		t.Fatal(err)
	}
	beforeAdds, beforeStatus, beforeDraft := f.adds, f.statuses, f.drafts
	c, err = f.c.Completed(context.Background(), f.o.Resource)
	if err != nil || !c.Completed || c.DeliveryID != "original" || c.Resource != f.o.Resource || c.Project != "ENG" || !c.CompletedAt.Equal(time.Unix(1700000000, 0)) {
		t.Fatal(c, err)
	}
	f.d = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.HeadMismatch}
	f.o.PullRequest.Draft = false
	again, err := f.c.Completed(context.Background(), f.o.Resource)
	if err != nil || !again.Completed {
		t.Fatal(again, err)
	} // Consumer still owes fresh authorization.
	if f.adds != beforeAdds || f.statuses != beforeStatus || f.drafts != beforeDraft {
		t.Fatal("completion repeated onboarding")
	}
	wrong := f.o.Resource
	wrong.NodeID = "OTHER"
	if _, err = f.c.Completed(context.Background(), wrong); err == nil {
		t.Fatal("wrong node exported")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = f.c.Completed(ctx, f.o.Resource); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestCompletionRejectsInterruptedStages(t *testing.T) {
	for _, stage := range []string{"status_done", "draft_done"} {
		t.Run(stage, func(t *testing.T) {
			f := auditSetup(t)
			f.cfg.Store = auditStore{Store: f.store, fail: stage}
			f.rebuild()
			if err := f.handle(); !errors.Is(err, ErrStorage) {
				t.Fatal(err)
			}
			adds, statuses, drafts := f.adds, f.statuses, f.drafts
			_, err := f.c.Completed(context.Background(), f.o.Resource)
			if !errors.Is(err, ErrAmbiguous) {
				t.Fatal(err)
			}
			if f.adds != adds || f.statuses != statuses || f.drafts != drafts {
				t.Fatal("read repeated effect")
			}
		})
	}
}

type completionHistoryStore struct {
	eb.Store
	missingOrigin bool
	stage         string
}

func (s completionHistoryStore) Event(ctx context.Context, id string) (storage.Event, error) {
	if s.missingOrigin {
		return storage.Event{}, storage.ErrNotFound
	}
	return s.Store.Event(ctx, id)
}
func (s completionHistoryStore) Provenance(ctx context.Context, ns, key string) (storage.Provenance, error) {
	p, err := s.Store.Provenance(ctx, ns, key)
	if err == nil && ns == prefix+s.stage+"_v1" {
		p.CreatedAt = p.CreatedAt.Add(-time.Second)
	}
	return p, err
}
func TestCompletionRejectsInconsistentChronologyAndMissingOrigin(t *testing.T) {
	for _, stage := range []string{"prepared", "status_done", "draft_done", "completed", "missing_origin"} {
		t.Run(stage, func(t *testing.T) {
			f := auditSetup(t)
			if err := f.handle(); err != nil {
				t.Fatal(err)
			}
			beforeAdds, beforeStatus, beforeDraft := f.adds, f.statuses, f.drafts
			f.cfg.Store = completionHistoryStore{Store: f.store, missingOrigin: stage == "missing_origin", stage: stage}
			f.rebuild()
			c, err := f.c.Completed(context.Background(), f.o.Resource)
			if err == nil || c.Completed {
				t.Fatal("exported inconsistent completion", c, err)
			}
			if f.adds != beforeAdds || f.statuses != beforeStatus || f.drafts != beforeDraft {
				t.Fatal("completion read wrote")
			}
		})
	}
}
