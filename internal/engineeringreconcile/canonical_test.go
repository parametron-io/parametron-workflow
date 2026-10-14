package engineeringreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func TestCanonicalOpenStatusUsesExistingPolicyAndValidatedOverlays(t *testing.T) {
	for _, tc := range []struct {
		name, typ, body string
		activity        bool
		review          config.StatusRole
		want            config.StatusRole
	}{
		{"parentless ready", "Task", "", false, "", config.Ready},
		{"parentless blocked", "Feature", "Blocked-By: #2", false, "", config.Blocked},
		{"inactive parent backlog", "Task", "Parent: #3", false, "", config.Backlog},
		{"reopened phase", "Phase", "Set-Status: false", false, "", config.Ready},
		{"started blocked target", "Task", "Blocked-By: #2\nSet-Status: false", true, "", config.InProgress},
		{"valid review", "Task", "Set-Status: false", true, config.InReview, config.InReview},
		{"valid rework", "Task", "", true, config.InProgress, config.InProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			f.project.StatusOptions[config.InReview] = "review"
			f.add(1, tc.typ, tc.body, "OPEN")
			f.add(2, "Bug", "", "OPEN")
			f.add(3, "Phase", "Blocked-By: #4", "OPEN")
			f.add(4, "Phase", "", "OPEN")
			f.activity[1] = ea.State{Accepted: tc.activity, Completed: tc.activity}
			r, err := New(Config{Project: f.project, Projects: f, Activity: f, Fields: f, Numbers: f, Review: reviewFunc(func(context.Context, storage.Resource) (config.StatusRole, bool, error) {
				return tc.review, tc.review != "", nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			g, err := f.snapshot().Context(f.resource(1))
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.CanonicalStatus(context.Background(), g, f.resource(1))
			if err != nil || got != tc.want {
				t.Fatal(got, err, tc.want)
			}
			if len(f.calls) != 0 {
				t.Fatal("canonical calculation mutated", f.calls)
			}
		})
	}
}
func TestCanonicalRejectsInvalidOverlayAndRetainsTerminalDone(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid_open_overlay", true: "closed_ignores_stale_overlay"}[closed], func(t *testing.T) {
			f := newReconcileFixture(t)
			f.project.StatusOptions[config.InReview] = "review"
			state := "OPEN"
			if closed {
				state = "CLOSED"
			}
			f.add(1, "Task", "", state)
			f.activity[1] = ea.State{Accepted: true, Completed: true}
			f.field(1, config.Status, github.FieldValue{Kind: config.SingleSelect, OptionID: "done"})
			r, err := New(Config{Project: f.project, Projects: f, Activity: f, Fields: f, Numbers: f, Review: reviewFunc(func(context.Context, storage.Resource) (config.StatusRole, bool, error) {
				if closed {
					t.Fatal("terminal target consulted stale review overlay")
				}
				return config.Done, true, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			s := f.snapshot()
			if closed {
				err = r.Reconcile(context.Background(), s, f.resource(1))
				if err != nil || f.items[1].Fields[config.Status].OptionID != "done" {
					t.Fatal(err, f.items)
				}
			} else {
				g, err := s.Context(f.resource(1))
				if err != nil {
					t.Fatal(err)
				}
				_, err = r.CanonicalStatus(context.Background(), g, f.resource(1))
				if !errors.Is(err, ErrInvalid) {
					t.Fatal(err)
				}
			}
		})
	}
}
