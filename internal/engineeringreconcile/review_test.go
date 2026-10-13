package engineeringreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prreview"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type reviewFunc func(context.Context, storage.Resource) (config.StatusRole, bool, error)

func (f reviewFunc) TargetStatus(ctx context.Context, r storage.Resource) (config.StatusRole, bool, error) {
	return f(ctx, r)
}
func TestReviewOverlayPreservesParentAndNormalOwnership(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "unowned"}[owned], func(t *testing.T) {
			f := newReconcileFixture(t)
			f.project.StatusOptions[config.InReview] = "review"
			f.add(1, "Phase", "", "OPEN")
			body := "Parent: #1"
			if !owned {
				body += "\nSet-Status: false"
			}
			f.add(2, "Task", body, "OPEN")
			f.activity[2] = ea.State{Accepted: true, Completed: true}
			f.field(2, config.StartDate, github.FieldValue{Kind: config.Date, Date: "2026-10-01"})
			status := config.InReview
			r, err := New(Config{Project: f.project, Projects: f, Activity: f, Fields: f, Numbers: f, Review: reviewFunc(func(_ context.Context, target storage.Resource) (config.StatusRole, bool, error) {
				if target == f.resource(2) {
					return status, true, nil
				}
				return "", false, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			for _, next := range []config.StatusRole{config.InReview, config.InProgress} {
				status = next
				if err = r.Reconcile(context.Background(), f.snapshot(), f.resource(2)); err != nil {
					t.Fatal(err)
				}
				if f.items[1].Fields[config.Status].OptionID != "progress" {
					t.Fatal("parent entered review", f.calls)
				}
				want := ""
				if owned {
					want = f.project.StatusOptions[next]
				}
				if f.items[2].Fields[config.Status].OptionID != want || f.items[2].Fields[config.StartDate].Date != "2026-10-01" {
					t.Fatal(f.items)
				}
			}
		})
	}
}
func TestReviewOverlaySupersededAndCurrentIdentityBarrier(t *testing.T) {
	for _, name := range []string{"superseded", "directive_drift"} {
		t.Run(name, func(t *testing.T) {
			f := newReconcileFixture(t)
			f.project.StatusOptions[config.InReview] = "review"
			f.add(1, "Task", "", "OPEN")
			f.field(1, config.Status, github.FieldValue{Kind: config.SingleSelect, OptionID: "review"})
			snapshot := f.snapshot()
			r, err := New(Config{Project: f.project, Projects: f, Activity: f, Fields: f, Numbers: f, Review: reviewFunc(func(_ context.Context, target storage.Resource) (config.StatusRole, bool, error) {
				if name == "superseded" {
					return "", false, prreview.ErrSuperseded
				}
				i := f.issues[1]
				i.Body += "\nSet-Status: false"
				f.issues[1] = i
				return config.InProgress, true, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			err = r.Reconcile(context.Background(), snapshot, f.resource(1))
			if name == "directive_drift" && err == nil {
				t.Fatal("missed changed current directives")
			}
			if name == "superseded" && err != nil {
				t.Fatal(err)
			}
			for _, call := range f.calls {
				if call == "status:item1:progress" || call == "status:item1:ready" {
					t.Fatal("stale overlay wrote", call)
				}
			}
			if name == "directive_drift" && errors.Is(err, prreview.ErrSuperseded) {
				t.Fatal(err)
			}
		})
	}
}
