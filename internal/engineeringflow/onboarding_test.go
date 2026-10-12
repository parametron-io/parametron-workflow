package engineeringflow

import (
	"context"
	"errors"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type onboardingFunc func(context.Context, storage.Event) error

func (f onboardingFunc) Handle(ctx context.Context, e storage.Event) error { return f(ctx, e) }

type prItemFunc func(context.Context, string) (github.PullRequestProjectItem, error)

func (f prItemFunc) CurrentPullRequestProjectItem(ctx context.Context, id string) (github.PullRequestProjectItem, error) {
	return f(ctx, id)
}

type prFlowReader struct {
	Reader
	o observe.ObservedState
}

func (r prFlowReader) ReadCurrent(ctx context.Context, _ storage.Resource) (observe.ObservedState, error) {
	return r.o, ctx.Err()
}
func (r prFlowReader) ReadPrimary(ctx context.Context, _ storage.Resource) (observe.ObservedState, error) {
	return r.o, ctx.Err()
}

// The established Engineering contract serializes explicit semantic consumers
// with lifecycle writers. Model execution alone may happen outside the lane.
func TestOnboardingPRSemanticConsumerUsesEngineeringLane(t *testing.T) {
	f := newFlowFixture(t)
	r := storage.Resource{Owner: "parametron-io", Repository: "engine", Kind: "pull_request", Number: 47, NodeID: "PR"}
	o := observe.ObservedState{Resource: r, Presence: observe.Present, PullRequest: &github.PullRequest{Identity: github.Identity{ID: "PR", Repository: f.d.Repositories[0], Number: 47}}}
	cfg := f.flow.cfg
	cfg.Reader = prFlowReader{Reader: f, o: o}
	models, consumers, onboards := 0, 0, 0
	cfg.Foundation = processorFunc(func(context.Context, storage.Event) error {
		t.Error("full semantic processor bypassed Engineering lane")
		return nil
	})
	cfg.Enricher = enrichFunc(func(ctx context.Context, in observe.PolicyInput) (semanticflow.PolicyInput, error) {
		models++
		if len(f.flow.lane) != 0 {
			t.Error("model inside lane")
		}
		return semanticflow.PolicyInput{Current: in}, ctx.Err()
	})
	cfg.Semantic = semanticFunc(func(ctx context.Context, in semanticflow.PolicyInput) error {
		consumers++
		if len(f.flow.lane) != 1 {
			t.Error("explicit semantic authority outside Engineering lane")
		}
		if in.Current.Observed.Resource != r {
			t.Error("wrong current PR identity")
		}
		return ctx.Err()
	})
	cfg.PRProjectItems = prItemFunc(func(context.Context, string) (github.PullRequestProjectItem, error) {
		return github.PullRequestProjectItem{}, nil
	})
	cfg.Onboarding = onboardingFunc(func(ctx context.Context, e storage.Event) error {
		onboards++
		if len(f.flow.lane) != 1 || consumers != 1 {
			t.Error("onboarding not serialized after semantic consumer")
		}
		return ctx.Err()
	})
	var err error
	f.flow, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.flow.Process(context.Background(), storage.Event{Resource: &r}); err != nil {
		t.Fatal(err)
	}
	if models != 1 || consumers != 1 || onboards != 1 {
		t.Fatal(models, consumers, onboards)
	}
}

func TestOnboardingPRLaneCancellationAndFoundationFailure(t *testing.T) {
	f := newFlowFixture(t)
	cfg := f.flow.cfg
	calls := 0
	cfg.Onboarding = onboardingFunc(func(context.Context, storage.Event) error { calls++; return nil })
	cfg.PRProjectItems = prItemFunc(func(context.Context, string) (github.PullRequestProjectItem, error) {
		return github.PullRequestProjectItem{}, nil
	})
	injected := errors.New("foundation failed")
	cfg.Foundation = processorFunc(func(context.Context, storage.Event) error { return injected })
	flow, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := f.resource(47)
	r.Kind = "pull_request"
	if err = flow.Process(context.Background(), storage.Event{Resource: &r}); !errors.Is(err, injected) || calls != 0 {
		t.Fatal(err, calls)
	}
	flow.lane <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = flow.Process(ctx, storage.Event{Delivery: storage.Delivery{EventName: "projects_v2_item"}, Resource: &r}); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal(err, calls)
	}
	<-flow.lane
	if err = flow.Process(context.Background(), storage.Event{Delivery: storage.Delivery{EventName: "projects_v2_item"}, Resource: &r}); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestOnboardingPRSharesLaneAcrossResources(t *testing.T) {
	f := newFlowFixture(t)
	cfg := f.flow.cfg
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	cfg.PRProjectItems = prItemFunc(func(context.Context, string) (github.PullRequestProjectItem, error) {
		return github.PullRequestProjectItem{}, nil
	})
	cfg.Onboarding = onboardingFunc(func(ctx context.Context, e storage.Event) error {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return ctx.Err()
	})
	flow, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, second := f.resource(47), f.resource(48)
	first.Kind = "pull_request"
	second.Kind = "pull_request"
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() {
		firstDone <- flow.Process(context.Background(), storage.Event{Delivery: storage.Delivery{EventName: "projects_v2_item"}, Resource: &first})
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	waiter := &laneAttemptContext{Context: ctx, attempt: make(chan struct{})}
	go func() {
		secondDone <- flow.Process(waiter, storage.Event{Delivery: storage.Delivery{EventName: "projects_v2_item"}, Resource: &second})
	}()
	<-waiter.attempt
	cancel()
	if err = <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	if err = <-firstDone; err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("different PR bypassed deployment lane", calls)
	}
	if err = flow.Process(context.Background(), storage.Event{Delivery: storage.Delivery{EventName: "projects_v2_item"}, Resource: &second}); err != nil || calls != 2 {
		t.Fatal("lane not released", err, calls)
	}
}

func TestOnboardingPRProjectNotificationCurrentIdentity(t *testing.T) {
	for _, variant := range []string{"valid", "wrong_project", "wrong_item", "missing_pr", "both_content", "wrong_repo_id", "wrong_primary"} {
		t.Run(variant, func(t *testing.T) {
			f := newFlowFixture(t)
			cfg := f.flow.cfg
			r := storage.Resource{Owner: "parametron-io", Repository: "engine", Kind: "pull_request", Number: 47, NodeID: "PR"}
			i := github.Identity{ID: r.NodeID, Repository: f.d.Repositories[0], Number: 47}
			f.projectItem = github.CurrentProjectItem{ID: "PRITEM", ProjectID: "P", ContentKind: "PullRequest"}
			item := github.PullRequestProjectItem{ID: "PRITEM", ProjectID: "P", ContentKind: "PullRequest", PullRequest: &i}
			o := observe.ObservedState{Resource: r, Presence: observe.Present, PullRequest: &github.PullRequest{Identity: i}}
			switch variant {
			case "wrong_project":
				item.ProjectID = "other"
			case "wrong_item":
				item.ID = "other"
			case "missing_pr":
				item.PullRequest = nil
			case "both_content":
				item.Issue = &i
			case "wrong_repo_id":
				i.Repository.ID = "other"
			case "wrong_primary":
				o.Resource.NodeID = "other"
			}
			cfg.Reader = prFlowReader{Reader: f, o: o}
			cfg.PRProjectItems = prItemFunc(func(context.Context, string) (github.PullRequestProjectItem, error) { return item, nil })
			cfg.Onboarding = onboardingFunc(func(context.Context, storage.Event) error { t.Error("resolver called onboarding"); return nil })
			flow, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			event := storage.Event{Delivery: storage.Delivery{EventName: "projects_v2_item", Payload: []byte(`{"action":"edited","projects_v2_item":{"node_id":"PRITEM","project_node_id":"P","content_node_id":"stale_payload_PR"}}`)}}
			got, err := flow.Resolve(context.Background(), event)
			if variant == "valid" {
				if err != nil || got != r {
					t.Fatal(got, err)
				}
			} else if err == nil {
				t.Fatal("bad current identity resolved", got)
			}
		})
	}
}
