package prreview

import (
	"context"
	"errors"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type exactOnboarding struct {
	f     *fixture
	calls int
}

func (o *exactOnboarding) Completed(ctx context.Context, r storage.Resource) (pronboarding.Completion, error) {
	o.calls++
	if r != o.f.pr.Resource || r.NodeID == "" {
		return pronboarding.Completion{}, pronboarding.ErrInvalid
	}
	return o.f.Completed(ctx, r)
}

type currentReadFunc func(context.Context, storage.Resource) (observe.ObservedState, error)

func (f currentReadFunc) ReadCurrent(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	return f(ctx, r)
}

func TestPreReviewCanonicalIdentityBoundaries(t *testing.T) {
	for _, name := range []string{"locator", "exact", "wrong-node", "wrong-repository", "wrong-number", "wrong-kind", "binding-changed", "missing-observation", "wrong-observation", "invalid-completion", "not-onboarded", "newer-ready", "rejected-binding"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.in.Facts.Transitions = nil
			f.pr.Projects[0].Items[0].Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "REVIEW"}, StatusRole: config.InReview}
			o := &exactOnboarding{f: f}
			f.cfg.Onboarding = o
			f.cfg.Native = nativeReviewFunc(func(ctx context.Context, i github.Identity, k string) (github.NativeLifecycle, error) {
				if i != f.in.PR.Identity || k != "pull_request" {
					return github.NativeLifecycle{}, ErrInvalid
				}
				return github.NativeLifecycle{Resource: i, Kind: k, State: "OPEN"}, ctx.Err()
			})
			r := f.pr.Resource
			r.NodeID = ""
			var want error
			write := false
			switch name {
			case "locator":
				write = true
			case "exact":
				r = f.pr.Resource
				write = true
			case "wrong-node":
				r.NodeID = "OTHER"
				want = ErrInvalid
			case "wrong-repository":
				r.Repository = "other"
				want = ErrInvalid
			case "wrong-number":
				r.Number++
				want = ErrInvalid
			case "wrong-kind":
				r.Kind = "issue"
				want = ErrInvalid
			case "binding-changed":
				f.bindHook = func(n int) {
					if n == 2 {
						f.in.Binding.Evidence.Target.ID = "OTHER"
					}
				}
				want = ErrChanged
			case "missing-observation", "wrong-observation":
				f.cfg.Reader = currentReadFunc(func(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
					o, err := f.ReadCurrent(ctx, r)
					if name == "missing-observation" {
						o.PullRequest = nil
					} else {
						o.Resource.NodeID = "OTHER"
					}
					return o, err
				})
				want = ErrChanged
			case "invalid-completion":
				f.in.Onboarding.Binding.Target.ID = "OTHER"
				want = ErrRejected
			case "not-onboarded":
				f.in.Onboarding.Completed = false
			case "newer-ready":
				f.in.Facts = policyInput(t).Facts
			case "rejected-binding":
				f.in.Binding = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingPR}
				want = ErrChanged
			}
			f.rebuild()
			err := f.c.Converge(context.Background(), r)
			if !errors.Is(err, want) {
				t.Fatalf("expected %v, got %v", want, err)
			}
			if f.drafts != 0 {
				t.Fatal("continuous convergence replayed Draft")
			}
			if write {
				if len(f.writes) != 1 || f.writes[0] != "PRITEM:PROGRESS" || o.calls < 2 {
					t.Fatal("canonical identity did not repair only PR Status", f.writes, o.calls)
				}
			} else if len(f.writes) != 0 {
				t.Fatal("unauthorized or newer state mutated", f.writes)
			}
		})
	}
}
