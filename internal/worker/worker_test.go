package worker

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type resolverFunc func(context.Context, storage.Event) (storage.Resource, error)

func (f resolverFunc) Resolve(c context.Context, e storage.Event) (storage.Resource, error) {
	return f(c, e)
}

type processorFunc func(context.Context, storage.Event) error

func (f processorFunc) Process(c context.Context, e storage.Event) error { return f(c, e) }

var now = time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
var resource = storage.Resource{Owner: "Org", Repository: "Repo", Kind: "issue", Number: 1}

func open(t *testing.T, path string) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func insert(t *testing.T, s *storage.Store, id string, r *storage.Resource) {
	t.Helper()
	if _, err := s.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: "notification", Payload: []byte(`{}`), ReceivedAt: now, Resource: r}); err != nil {
		t.Fatal(err)
	}
}
func state(t *testing.T, s *storage.Store, id string) storage.Event {
	t.Helper()
	e, err := s.Event(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func makeWorker(t *testing.T, s *storage.Store, r resolverFunc, p processorFunc, clock func() time.Time) *Worker {
	t.Helper()
	w, err := New(Config{Store: s, Resolver: r, Processor: p, Concurrency: 2, Clock: clock, Classifier: func(error) Classification { return Classification{false, "test_terminal"} }, RetrySchedule: func(_ Classification, _ int64, t time.Time) time.Time { return t.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("barrier timeout")
		var zero T
		return zero
	}
}
func TestFIFOAndConcurrentResources(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db"))
	for _, id := range []string{"a", "b", "c", "other"} {
		insert(t, s, id, nil)
	}
	var resolved []string
	var mu sync.Mutex
	var order []string
	active := false
	starts := make(chan string, 4)
	release := make(chan struct{})
	w := makeWorker(t, s, func(_ context.Context, e storage.Event) (storage.Resource, error) {
		resolved = append(resolved, e.Delivery.ID)
		r := resource
		if e.Delivery.ID == "other" {
			r.Number = 2
		}
		return r, nil
	}, func(_ context.Context, e storage.Event) error {
		if e.Resource.Number == 1 {
			mu.Lock()
			if active {
				t.Error("overlap")
			}
			active = true
			order = append(order, e.Delivery.ID)
			mu.Unlock()
		}
		starts <- e.Delivery.ID
		if e.Delivery.ID == "a" || e.Delivery.ID == "other" {
			<-release
		}
		if e.Resource.Number == 1 {
			mu.Lock()
			active = false
			mu.Unlock()
		}
		return nil
	}, nil)
	done := make(chan error, 1)
	go func() { done <- w.Step(context.Background()) }()
	first, second := receive(t, starts), receive(t, starts)
	if !((first == "a" && second == "other") || (first == "other" && second == "a")) {
		t.Fatal(first, second)
	}
	close(release)
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"a", "b", "c"}) || !reflect.DeepEqual(resolved, []string{"a", "b", "c", "other"}) {
		t.Fatal(order, resolved)
	}
	for _, id := range resolved {
		e := state(t, s, id)
		if e.State.Status != storage.Completed || e.State.Attempts != 1 || e.Resource.Owner != "org" {
			t.Fatal(e)
		}
	}
	w.cfg.Resolver = resolverFunc(func(context.Context, storage.Event) (storage.Resource, error) {
		t.Fatal("completed work resolved")
		return resource, nil
	})
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestRetryBlocksLaneAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := open(t, path)
	insert(t, s, "a", &resource)
	insert(t, s, "b", &resource)
	clock := now
	var calls []string
	resolver := resolverFunc(func(context.Context, storage.Event) (storage.Resource, error) {
		t.Fatal("bound event resolved")
		return resource, nil
	})
	p := processorFunc(func(_ context.Context, e storage.Event) error {
		calls = append(calls, e.Delivery.ID)
		if e.Delivery.ID == "a" && e.State.Attempts == 1 {
			return &github.Error{Category: github.RateLimited}
		}
		return nil
	})
	w := makeWorker(t, s, resolver, p, func() time.Time { return clock })
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := state(t, s, "a")
	if e.State.Status != storage.Retryable || e.State.Attempts != 1 || e.State.ErrorCategory != "rate_limited" || !e.State.NextAttemptAt.Equal(now.Add(time.Hour)) {
		t.Fatal(e)
	}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatal(calls)
	}
	s.Close()
	s = open(t, path)
	w = makeWorker(t, s, resolver, p, func() time.Time { return clock })
	if err := w.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(time.Hour)
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"a", "a", "b"}) {
		t.Fatal(calls)
	}
	if e = state(t, s, "a"); e.State.Attempts != 2 || e.State.Status != storage.Completed {
		t.Fatal(e)
	}
	s.Close()
	s = open(t, path)
	w = makeWorker(t, s, resolver, p, nil)
	if err := w.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatal(calls)
	}
}
func TestCancellationAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := open(t, path)
	insert(t, s, "a", &resource)
	insert(t, s, "b", &resource)
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := resolverFunc(func(context.Context, storage.Event) (storage.Resource, error) { return resource, nil })
	w := makeWorker(t, s, r, func(ctx context.Context, e storage.Event) error {
		if e.Delivery.ID != "a" {
			t.Error("queued work started")
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, nil)
	done := make(chan error, 1)
	go func() { done <- w.Step(ctx) }()
	receive(t, started)
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	if e := state(t, s, "a"); e.State.Status != storage.Pending || e.State.Attempts != 1 || e.State.ErrorCategory != "" {
		t.Fatal(e)
	}
	if e := state(t, s, "b"); e.State.Attempts != 0 {
		t.Fatal(e)
	}
	s.Close()
	s = open(t, path)
	var calls int
	w = makeWorker(t, s, r, func(context.Context, storage.Event) error { calls++; return nil }, nil)
	if err := w.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || state(t, s, "a").State.Attempts != 2 {
		t.Fatal(calls)
	}
}
func TestResolutionRetryBarrierAndTerminal(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db"))
	insert(t, s, "a", nil)
	insert(t, s, "b", nil)
	clock := now
	var resolutions, processed int
	w := makeWorker(t, s, func(context.Context, storage.Event) (storage.Resource, error) {
		resolutions++
		if resolutions == 1 {
			return storage.Resource{}, &github.Error{Category: github.Transient}
		}
		return resource, nil
	}, func(context.Context, storage.Event) error { processed++; return errors.New("unsafe provider message") }, func() time.Time { return clock })
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resolutions != 1 || processed != 0 {
		t.Fatal(resolutions, processed)
	}
	clock = now.Add(time.Hour)
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := state(t, s, "a")
	if e.State.Status != storage.Failed || e.State.ErrorCategory != "test_terminal" || e.State.Attempts != 2 {
		t.Fatal(e)
	}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if processed != 2 {
		t.Fatal(processed)
	}
	if err := w.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if processed != 2 {
		t.Fatal(processed)
	}
}
func TestInterruptedClaimRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := open(t, path)
	insert(t, s, "a", nil)
	if _, err := s.Claim(context.Background(), "a", now); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(t, path)
	w := makeWorker(t, s, func(context.Context, storage.Event) (storage.Resource, error) { return resource, nil }, func(_ context.Context, e storage.Event) error {
		if e.State.Attempts != 2 {
			t.Error(e.State)
		}
		return nil
	}, nil)
	if err := w.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state(t, s, "a").State.Status != storage.Completed {
		t.Fatal("not completed")
	}
}

func TestObservedProcessingIsNotAuthority(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db"))
	insert(t, s, "a", &resource)
	insert(t, s, "b", &resource)
	if _, err := s.Claim(context.Background(), "a", now); err != nil {
		t.Fatal(err)
	}
	w := makeWorker(t, s, func(context.Context, storage.Event) (storage.Resource, error) {
		t.Fatal("unexpected resolution")
		return resource, nil
	}, func(context.Context, storage.Event) error { t.Fatal("unowned attempt executed"); return nil }, nil)
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state(t, s, "b").State.Attempts != 0 {
		t.Fatal("passed processing predecessor")
	}
}
func TestResolutionCancellationReleasesClaim(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db"))
	insert(t, s, "a", nil)
	insert(t, s, "b", nil)
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := makeWorker(t, s, func(ctx context.Context, e storage.Event) (storage.Resource, error) {
		close(started)
		<-ctx.Done()
		return storage.Resource{}, ctx.Err()
	}, func(context.Context, storage.Event) error { t.Fatal("processor started"); return nil }, nil)
	done := make(chan error, 1)
	go func() { done <- w.Step(ctx) }()
	receive(t, started)
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	if e := state(t, s, "a"); e.State.Status != storage.Pending || e.State.Attempts != 1 {
		t.Fatal(e)
	}
	if state(t, s, "b").State.Attempts != 0 {
		t.Fatal("new claim after shutdown")
	}
}
func TestDuplicateResolvedDeliveryDoesNotReviveWork(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db"))
	insert(t, s, "a", nil)
	calls := 0
	w := makeWorker(t, s, func(context.Context, storage.Event) (storage.Resource, error) { return resource, nil }, func(context.Context, storage.Event) error { calls++; return nil }, nil)
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	inserted, err := s.InsertDelivery(context.Background(), storage.Delivery{ID: "a", EventName: "notification", Payload: []byte(`{}`), ReceivedAt: now.Add(time.Hour)})
	if inserted || err != nil {
		t.Fatal(inserted, err)
	}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := state(t, s, "a"); e.State.Attempts != 1 || e.State.Status != storage.Completed || calls != 1 || e.Resource == nil {
		t.Fatal(e, calls)
	}
}
func TestClassificationAndConfiguration(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("missing config accepted")
	}
	s := open(t, filepath.Join(t.TempDir(), "db"))
	w := makeWorker(t, s, func(context.Context, storage.Event) (storage.Resource, error) { return resource, nil }, func(context.Context, storage.Event) error { return nil }, nil)
	for _, category := range []github.Category{github.RateLimited, github.Transient, github.NotFound, github.Unauthorized, github.Forbidden, github.Conflict, github.Malformed, github.Permanent} {
		c := w.classify(&github.Error{Category: category})
		if c.Category != string(category) || c.Retryable != (category == github.RateLimited || category == github.Transient) {
			t.Fatal(c)
		}
	}
	if err := w.Run(context.Background(), 0); err == nil {
		t.Fatal("busy poll accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestRunRecoveryAndCancellation(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db"))
	insert(t, s, "a", &resource)
	if _, err := s.Claim(context.Background(), "a", now); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	w := makeWorker(t, s, func(context.Context, storage.Event) (storage.Resource, error) {
		t.Fatal("bound resolution")
		return resource, nil
	}, func(ctx context.Context, e storage.Event) error {
		if e.State.Attempts != 2 {
			t.Error(e.State)
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, nil)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, time.Second) }()
	receive(t, started)
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	if e := state(t, s, "a"); e.State.Status != storage.Pending || e.State.Attempts != 2 {
		t.Fatal(e)
	}
}
