package engineeringflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	er "github.com/parametron-io/parametron-workflow/internal/engineeringreconcile"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prreview"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

func observeDate(date string) observe.FieldValue {
	return observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Date, Date: date}}
}
func observeOption(option string) observe.FieldValue {
	return observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: option}}
}

type reviewProbe struct {
	handle   func(context.Context, storage.Event) error
	converge func(context.Context, storage.Resource) error
	status   func(context.Context, storage.Resource) (config.StatusRole, bool, error)
}

func (r reviewProbe) Handle(ctx context.Context, e storage.Event) error {
	if r.handle != nil {
		return r.handle(ctx, e)
	}
	return ctx.Err()
}
func (r reviewProbe) Converge(ctx context.Context, s storage.Resource) error {
	if r.converge != nil {
		return r.converge(ctx, s)
	}
	return ctx.Err()
}
func (r reviewProbe) TargetStatus(ctx context.Context, s storage.Resource) (config.StatusRole, bool, error) {
	if r.status != nil {
		return r.status(ctx, s)
	}
	return "", false, ctx.Err()
}
func TestReviewRoutingUsesLaneWithoutSemanticAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, event, payload string
		review, onboard      int
	}{
		{"ready", "pull_request", `{"action":"ready_for_review","pull_request":{"node_id":"PR","number":49,"updated_at":"2026-10-13T10:00:00Z"},"sender":{"node_id":"U","login":"dev","type":"User"}}`, 1, 0},
		{"submitted", "pull_request_review", `{"action":"submitted","pull_request":{"node_id":"PR","number":49},"review":{"node_id":"REV","state":"changes_requested","submitted_at":"2026-10-13T10:00:00Z","user":{"node_id":"U","login":"dev","type":"User"}}}`, 1, 0},
		{"ordinary", "pull_request", `{}`, 0, 1},
		{"opened", "pull_request", `{"action":"opened"}`, 0, 1},
		{"unsupported", "pull_request_review", `{"action":"edited"}`, 0, 0},
		{"project", "projects_v2_item", `{}`, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlowFixture(t)
			cfg := f.flow.cfg
			handled, onboarded, converged := 0, 0, 0
			cfg.Enricher = nil
			cfg.Semantic = nil
			cfg.Onboarding = onboardingFunc(func(context.Context, storage.Event) error { onboarded++; return nil })
			cfg.PRProjectItems = prItemFunc(func(context.Context, string) (github.PullRequestProjectItem, error) {
				return github.PullRequestProjectItem{}, nil
			})
			cfg.Review = reviewProbe{handle: func(context.Context, storage.Event) error {
				handled++
				if len(f.flow.lane) != 1 {
					t.Error("review outside Engineering lane")
				}
				return nil
			}, converge: func(context.Context, storage.Resource) error { converged++; return nil }}
			var err error
			f.flow, err = New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r := f.resource(49)
			r.Kind = "pull_request"
			r.NodeID = "PR"
			e := storage.Event{Resource: &r, Delivery: storage.Delivery{EventName: tc.event, Payload: []byte(tc.payload)}}
			if err = f.flow.Process(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if handled != tc.review || onboarded != tc.onboard || converged != tc.onboard {
				t.Fatal(handled, onboarded, converged)
			}
			if tc.review == 1 {
				for _, call := range f.calls {
					if call == "foundation" {
						t.Fatal("review invoked semantic foundation")
					}
				}
			}
			if tc.name == "unsupported" {
				if _, err = f.flow.Resolve(context.Background(), e); !errors.Is(err, worker.ErrNotApplicable) {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestFirstActivityRecoveryDoesNotOverwriteReview(t *testing.T) {
	for _, mode := range []string{"complete", "unfinished_date", "unfinished_status", "existing_date"} {
		t.Run(mode, func(t *testing.T) {
			f := newFlowFixture(t)
			f.add(1, "Phase", "", "OPEN")
			f.add(2, "Task", "Parent: #1", "OPEN")
			if err := f.flow.Process(context.Background(), f.event("issue", "issues", 2)); err != nil {
				t.Fatal(err)
			}
			f.remote["development/issue-2"] = github.Branch{Repository: f.d.Repositories[0], Name: "development/issue-2", SHA: f.head}
			if mode == "unfinished_date" {
				f.fail = "date:item2"
			}
			if mode == "unfinished_status" {
				f.fail = "status:item2"
			}
			if mode == "existing_date" {
				i := f.items[2]
				i.Fields[config.StartDate] = observeDate("2026-10-01")
				f.items[2] = i
			}
			err := f.flow.Process(context.Background(), f.event("push", "push", 2))
			if strings.HasPrefix(mode, "unfinished") {
				if err == nil {
					t.Fatal("missing interruption")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			f.fail = ""
			// The production flow must retain review while it resumes accepted first activity.
			i := f.items[2]
			i.Fields[config.Status] = observeOption("review")
			f.items[2] = i
			status := config.InReview
			review := reviewProbe{status: func(_ context.Context, r storage.Resource) (config.StatusRole, bool, error) {
				if r == f.resource(2) {
					return status, true, nil
				}
				return "", false, nil
			}}
			cfg := f.flow.cfg
			cfg.Review = review
			cfg.Onboarding = onboardingFunc(func(context.Context, storage.Event) error { return nil })
			cfg.PRProjectItems = prItemFunc(func(context.Context, string) (github.PullRequestProjectItem, error) {
				return github.PullRequestProjectItem{}, nil
			})
			activity, err := ea.New(cfg.Activity)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Reconciler, err = er.New(er.Config{Project: f.d.Engineering, Projects: f, Activity: activity, Fields: f, Numbers: f, Review: review})
			if err != nil {
				t.Fatal(err)
			}
			f.flow, err = New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			f.calls = nil
			if err = f.flow.Process(context.Background(), f.event("retry", "issues", 2)); err != nil {
				t.Fatal(err)
			}
			if f.items[2].Fields[config.Status].OptionID != "review" || f.items[1].Fields[config.Status].OptionID != "progress" {
				t.Fatal(f.items, f.calls)
			}
			for _, call := range f.calls {
				if call == "status:item2" {
					t.Fatal("activity temporarily overwrote review", f.calls)
				}
			}
			state, err := activity.ActivityState(context.Background(), f.resource(2))
			if err != nil || !state.Completed {
				t.Fatal(state, err)
			}
			wantDate := "2026-10-11"
			if mode == "existing_date" {
				wantDate = "2026-10-01"
			}
			if f.items[2].Fields[config.StartDate].Date != wantDate {
				t.Fatal(f.items[2])
			}
			status = config.InProgress
			if err = f.flow.Process(context.Background(), f.event("rework", "issues", 2)); err != nil {
				t.Fatal(err)
			}
			if f.items[2].Fields[config.Status].OptionID != "progress" {
				t.Fatal("rework not restored", f.calls)
			}
		})
	}
}
func TestActivityReviewGuardSuperseded(t *testing.T) {
	f := newFlowFixture(t)
	f.add(1, "Task", "", "OPEN")
	snapshot, err := f.ResolveSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	projects := guardedProjects{reader: f, snapshot: snapshot, resources: map[string]storage.Resource{"item1": f.resource(1)}}
	g := reviewActivityFields{fields: f, projects: projects, project: f.d.Engineering, review: reviewProbe{status: func(context.Context, storage.Resource) (config.StatusRole, bool, error) {
		return "", false, prreview.ErrSuperseded
	}}}
	if err = g.SetProjectOption(context.Background(), "P", "item1", "status", "progress"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c == fmt.Sprint("status:item1") {
			t.Fatal(c)
		}
	}
}

func TestActivityStatusGuardChecksDirectiveDriftAfterOverlay(t *testing.T) {
	f := newFlowFixture(t)
	f.add(1, "Task", "", "OPEN")
	snapshot, err := f.ResolveSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	projects := guardedProjects{reader: f, snapshot: snapshot, resources: map[string]storage.Resource{"item1": f.resource(1)}}
	g := reviewActivityFields{fields: f, projects: projects, project: f.d.Engineering, review: reviewProbe{status: func(context.Context, storage.Resource) (config.StatusRole, bool, error) {
		f.change(1, "Set-Status: false", "OPEN")
		return config.InProgress, true, nil
	}}}
	if err = g.SetProjectOption(context.Background(), "P", "item1", "status", "progress"); err == nil {
		t.Fatal("activity ignored directive drift")
	}
	for _, c := range f.calls {
		if c == "status:item1" {
			t.Fatal("stale activity write")
		}
	}
}
