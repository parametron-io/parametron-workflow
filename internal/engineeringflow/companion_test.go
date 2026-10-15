package engineeringflow

import (
	"context"
	"errors"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/github"
	cr "github.com/parametron-io/parametron-workflow/internal/prcompanionreconcile"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type companionFunc func(context.Context, string) error

func (f companionFunc) Reconcile(c context.Context, id string) error { return f(c, id) }

func TestCompanionErrorPrecedenceAndLane(t *testing.T) {
	lifecycle := errors.New("earlier lifecycle failure")
	for _, numeric := range []error{nil, cr.ErrChanged, cr.ErrAmbiguous} {
		t.Run(func() string {
			if numeric == nil {
				return "success"
			}
			return numeric.Error()
		}(), func(t *testing.T) {
			f := newFlowFixture(t)
			called := 0
			f.flow.cfg.Companions = companionFunc(func(ctx context.Context, id string) error {
				called++
				if id != "delivery" || len(f.flow.lane) != 1 {
					t.Error("companion outside lane or wrong delivery")
				}
				return numeric
			})
			got := f.flow.withCompanions(context.Background(), "delivery", func() error { return lifecycle })
			want := lifecycle
			if numeric != nil {
				want = numeric
			}
			if !errors.Is(got, want) || called != 1 {
				t.Fatal(got, called)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := f.flow.withCompanions(ctx, "delivery", func() error { t.Error("cancelled lifecycle ran"); return nil }); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if called != 1 {
				t.Fatal("cancelled companion ran")
			}
		})
	}
}
func TestCompanionInvocationAcrossPRRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, event, body string
		calls             int
	}{
		{"ordinary open", "pull_request", "{}", 1},
		{"project notification", "projects_v2_item", "{}", 1},
		{"supported review", "pull_request_review", `{"action":"submitted","pull_request":{"node_id":"PR","number":49},"review":{"node_id":"REV","state":"approved","submitted_at":"2026-10-13T10:00:00Z","user":{"node_id":"U","login":"dev","type":"User"}}}`, 1},
		{"unsupported review", "pull_request_review", `{"action":"edited"}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlowFixture(t)
			cfg := f.flow.cfg
			cfg.Enricher = nil
			cfg.Semantic = nil
			cfg.Onboarding = onboardingFunc(func(context.Context, storage.Event) error { return nil })
			cfg.PRProjectItems = prItemFunc(func(context.Context, string) (github.PullRequestProjectItem, error) {
				return github.PullRequestProjectItem{}, nil
			})
			cfg.Review = reviewProbe{}
			calls := 0
			cfg.Companions = companionFunc(func(context.Context, string) error { calls++; return nil })
			var err error
			f.flow, err = New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r := f.resource(49)
			r.Kind = "pull_request"
			r.NodeID = "PR"
			if err := f.flow.Process(context.Background(), storage.Event{Resource: &r, Delivery: storage.Delivery{ID: "delivery", EventName: tc.event, Payload: []byte(tc.body)}}); err != nil {
				t.Fatal(err)
			}
			if calls != tc.calls {
				t.Fatal(calls, tc.calls)
			}
		})
	}
}
func TestCompanionLaneSerializesPRAndIssueWork(t *testing.T) {
	f := newFlowFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	calls := 0
	f.flow.cfg.Companions = companionFunc(func(context.Context, string) error { calls++; close(entered); <-release; return nil })
	go func() { done <- f.flow.withCompanions(context.Background(), "PR", func() error { return nil }) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	waiter := &laneAttemptContext{Context: ctx, attempt: make(chan struct{})}
	waiting := make(chan error, 1)
	go func() {
		waiting <- f.flow.withCompanions(waiter, "Issue", func() error { t.Error("Issue bypassed lane"); return nil })
	}()
	<-waiter.attempt
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
