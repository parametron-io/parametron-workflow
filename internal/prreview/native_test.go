package prreview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
)

type nativeReviewFunc func(context.Context, github.Identity, string) (github.NativeLifecycle, error)

func (f nativeReviewFunc) NativeLifecycle(c context.Context, i github.Identity, k string) (github.NativeLifecycle, error) {
	return f(c, i, k)
}

func TestFreshReviewAuthorityAfterNativeReopen(t *testing.T) {
	for _, completedRework := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported_without_rework", true: "valid_completed_rework"}[completedRework], func(t *testing.T) {
			f := setup(t)
			at := f.in.Onboarding.CompletedAt
			facts := github.NativeLifecycle{Resource: f.in.PR.Identity, Kind: "pull_request", State: "OPEN"}
			f.cfg.Native = nativeReviewFunc(func(ctx context.Context, i github.Identity, k string) (github.NativeLifecycle, error) {
				return facts, ctx.Err()
			})
			f.rebuild()
			f.handle(f.ready("READY1", at.Add(time.Minute)))
			if completedRework {
				f.handle(f.review("REWORK1", "CHANGES_REQUESTED", at.Add(2*time.Minute)))
			}
			facts.Transitions = []github.NativeTransition{{ID: "CLOSE", Kind: "ClosedEvent", Resource: f.in.PR.Identity, CreatedAt: at.Add(3 * time.Minute)}, {ID: "REOPEN", Kind: "ReopenedEvent", Resource: f.in.PR.Identity, CreatedAt: at.Add(4 * time.Minute)}}
			s, err := f.c.PRStatus(context.Background(), f.pr.Resource)
			if err != nil || s != config.InProgress {
				t.Fatal("stale interval reused", s, err)
			}
			e := f.ready("READY2", at.Add(5*time.Minute))
			err = f.c.Handle(context.Background(), e)
			if !completedRework {
				if !errors.Is(err, ErrRejected) {
					t.Fatal("unsupported continuation did not fail closed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("fresh valid post-reopen readiness rejected: %v", err)
			}
			s, err = f.c.PRStatus(context.Background(), f.pr.Resource)
			if err != nil || s != config.InReview {
				t.Fatal("current review did not converge", s, err)
			}
			f.restart()
			s, err = f.c.PRStatus(context.Background(), f.pr.Resource)
			if err != nil || s != config.InReview {
				t.Fatal("restart lost current review", s, err)
			}
		})
	}
}

func TestNativeTerminalAndStaleReviewEvidenceHaveNoEffects(t *testing.T) {
	for _, state := range []string{"CLOSED", "MERGED", "OPEN"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			at := f.in.Onboarding.CompletedAt
			facts := github.NativeLifecycle{Resource: f.in.PR.Identity, Kind: "pull_request", State: "OPEN"}
			f.cfg.Native = nativeReviewFunc(func(ctx context.Context, i github.Identity, k string) (github.NativeLifecycle, error) {
				return facts, ctx.Err()
			})
			f.rebuild()
			f.handle(f.ready("READY1", at.Add(time.Minute)))
			f.writes = nil
			f.in.PR.State = state
			facts.State = state
			kind := "ClosedEvent"
			if state == "MERGED" {
				kind = "MergedEvent"
			}
			facts.Transitions = []github.NativeTransition{{ID: "END", Kind: kind, Resource: f.in.PR.Identity, CreatedAt: at.Add(3 * time.Minute)}}
			if state == "OPEN" {
				facts.Transitions = append(facts.Transitions, github.NativeTransition{ID: "REOPEN", Kind: "ReopenedEvent", Resource: f.in.PR.Identity, CreatedAt: at.Add(4 * time.Minute)})
			}
			for _, review := range []string{"APPROVED", "CHANGES_REQUESTED"} {
				e := f.review("OLD"+review, review, at.Add(2*time.Minute))
				err := f.c.Handle(context.Background(), e)
				if err != nil && !errors.Is(err, ErrRejected) {
					t.Fatal(err)
				}
				if len(f.writes) != 0 || f.drafts != 0 {
					t.Fatal("stale review mutated", f.writes, f.drafts)
				}
			}
			if err := f.c.Converge(context.Background(), f.pr.Resource); err != nil {
				t.Fatal(err)
			}
			if len(f.writes) != 0 {
				t.Fatal("terminal convergence reused review")
			}
		})
	}
}
