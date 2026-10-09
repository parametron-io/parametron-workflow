package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type consumerFunc func(context.Context, observe.PolicyInput) error

func (f consumerFunc) Evaluate(ctx context.Context, p observe.PolicyInput) error { return f(ctx, p) }

func fixture(t *testing.T) (Config, *github.Fake) {
	t.Helper()
	source, schema := preparationFixture()
	f := &github.Fake{
		DiscoverSchemaFunc: func(context.Context, config.SourceConfig) (config.Schema, error) { return schema, nil },
		IssueFunc: func(_ context.Context, r github.Ref) (github.Issue, error) {
			return github.Issue{Identity: github.Identity{ID: fmt.Sprint("I", r.Number), Repository: github.Repository{ID: "R", Owner: "org", Name: "repo"}, Number: r.Number}, Title: "current", Body: "current body", State: "CLOSED", Labels: []string{"z", "a"}}, nil
		},
		ProjectItemsFunc: func(_ context.Context, node, project string, fields map[string]config.FieldKind) ([]github.ProjectItem, error) {
			if fields["status"] != config.SingleSelect {
				t.Error("missing expected fields")
			}
			return []github.ProjectItem{{ID: project + "item", ProjectID: project, Content: &github.Content{ID: node, Kind: "Issue"}, Values: map[string]github.FieldValue{"status": {Kind: config.SingleSelect, OptionID: "done"}}}}, nil
		},
	}
	return Config{Source: source, Client: f, DataDir: filepath.Join(t.TempDir(), "data"), Listen: "127.0.0.1:0", WebhookSecret: []byte("secret"), Concurrency: 2, PollInterval: time.Second, RetryDelay: time.Minute}, f
}
func openRuntime(t *testing.T, c Config) *runtime {
	t.Helper()
	r, err := compose(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.store.Close() })
	return r
}
func signed(id string, number int) *http.Request {
	body := []byte(fmt.Sprintf(`{"repository":{"owner":{"login":"org"},"name":"repo"},"issue":{"number":%d,"title":"stale","body":"stale","state":"open","labels":["stale"]}}`, number))
	req := httptest.NewRequest("POST", "/webhooks/github", bytes.NewReader(body))
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}
func accept(t *testing.T, r *runtime, id string, n int) storage.Event {
	t.Helper()
	rec := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(rec, signed(id, n))
	if rec.Code != 204 {
		t.Fatalf("receipt: %d", rec.Code)
	}
	e, err := r.store.Event(context.Background(), id)
	if err != nil {
		t.Fatal("204 without durable event", err)
	}
	return e
}
func step(t *testing.T, r *runtime) {
	t.Helper()
	if err := r.worker.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func state(t *testing.T, r *runtime, id string, status storage.Status, attempts int64) storage.Event {
	t.Helper()
	e, err := r.store.Event(context.Background(), id)
	if err != nil || e.State.Status != status || e.State.Attempts != attempts {
		t.Fatalf("state %s: %+v %v", id, e.State, err)
	}
	return e
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("channel deadline")
		var v T
		return v
	}
}

func TestFoundationHappyDuplicate(t *testing.T) {
	c, _ := fixture(t)
	var inputs []observe.PolicyInput
	c.Consumer = consumerFunc(func(_ context.Context, p observe.PolicyInput) error { inputs = append(inputs, p); return nil })
	r := openRuntime(t, c)
	first := accept(t, r, "happy", 14)
	second := accept(t, r, "happy", 14)
	if first.Sequence != second.Sequence {
		t.Fatal("duplicate sequence")
	}
	work, err := r.store.Work(context.Background())
	if err != nil || len(work) != 1 {
		t.Fatal(work, err)
	}
	step(t, r)
	e := state(t, r, "happy", storage.Completed, 1)
	if e.Resource == nil || e.Resource.Number != 14 || e.Resource.NodeID != "" {
		t.Fatal("binding", e.Resource)
	}
	if len(inputs) != 1 {
		t.Fatal("handoff count", len(inputs))
	}
	p := inputs[0]
	o := p.Observed
	if p.Sequence != first.Sequence || p.DeliveryID != "happy" || o.Resource.NodeID != "I14" || o.Issue.Title != "current" || o.Issue.Body != "current body" || o.Issue.State != "CLOSED" || !reflect.DeepEqual(o.Issue.Labels, []string{"a", "z"}) || len(o.Projects) != 2 || o.Projects[0].Items[0].Fields[config.Status].StatusRole != config.Done {
		t.Fatal("current normalized handoff", p)
	}
	accept(t, r, "happy", 14)
	step(t, r)
	state(t, r, "happy", storage.Completed, 1)
	if len(inputs) != 1 {
		t.Fatal("completed revived")
	}
	rec := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/other", nil))
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
}

func TestFoundationRestart(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(fmt.Sprint("claimed-", claimed), func(t *testing.T) {
			c, f := fixture(t)
			calls := 0
			c.Consumer = consumerFunc(func(context.Context, observe.PolicyInput) error { calls++; return nil })
			a := openRuntime(t, c)
			accept(t, a, "restart", 14)
			var old storage.Event
			if claimed {
				var err error
				old, err = a.store.Claim(context.Background(), "restart", time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := a.store.Close(); err != nil {
				t.Fatal(err)
			}
			b := openRuntime(t, c)
			// Opening alone preserves evidence; Worker owns recovery.
			if claimed {
				state(t, b, "restart", storage.Processing, 1)
			} else {
				state(t, b, "restart", storage.Pending, 0)
			}
			if err := b.worker.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if claimed {
				current := f.IssueFunc
				f.IssueFunc = func(ctx context.Context, ref github.Ref) (github.Issue, error) {
					state(t, b, "restart", storage.Processing, 2)
					if err := b.store.Settle(ctx, "restart", old.State.Attempts, storage.Failed, nil, "stale"); !errors.Is(err, storage.ErrConflict) {
						t.Error("stale claim overwrote recovered active attempt", err)
					}
					return current(ctx, ref)
				}
			}
			step(t, b)
			attempts := int64(1)
			if claimed {
				attempts = 2
			}
			state(t, b, "restart", storage.Completed, attempts)
			if calls != 1 {
				t.Fatal(calls)
			}
			if claimed && !errors.Is(b.store.Settle(context.Background(), "restart", old.State.Attempts, storage.Failed, nil, "stale"), storage.ErrConflict) {
				t.Fatal("stale claim settled")
			}
			accept(t, b, "restart", 14)
			step(t, b)
			state(t, b, "restart", storage.Completed, attempts)
		})
	}
}

func TestFoundationRetry(t *testing.T) {
	c, f := fixture(t)
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	c.Clock = func() time.Time { return now }
	reads, handoffs := 0, 0
	current := f.IssueFunc
	f.IssueFunc = func(ctx context.Context, r github.Ref) (github.Issue, error) {
		reads++
		if reads == 1 {
			return github.Issue{}, &github.Error{Category: github.Transient}
		}
		return current(ctx, r)
	}
	c.Consumer = consumerFunc(func(context.Context, observe.PolicyInput) error { handoffs++; return nil })
	a := openRuntime(t, c)
	accept(t, a, "retry", 14)
	step(t, a)
	e := state(t, a, "retry", storage.Retryable, 1)
	if e.State.ErrorCategory != "transient" || e.State.NextAttemptAt == nil || !e.State.NextAttemptAt.Equal(now.Add(c.RetryDelay)) || handoffs != 0 {
		t.Fatal(e.State, handoffs)
	}
	a.store.Close()
	b := openRuntime(t, c)
	if err := b.worker.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted := state(t, b, "retry", storage.Retryable, 1)
	if !persisted.State.NextAttemptAt.Equal(*e.State.NextAttemptAt) {
		t.Fatal("retry lost")
	}
	now = now.Add(c.RetryDelay - time.Nanosecond)
	step(t, b)
	state(t, b, "retry", storage.Retryable, 1)
	if reads != 1 {
		t.Fatal("early read")
	}
	now = now.Add(time.Nanosecond)
	step(t, b)
	state(t, b, "retry", storage.Completed, 2)
	if reads != 2 || handoffs != 1 {
		t.Fatal(reads, handoffs)
	}
}

func TestFoundationOrdering(t *testing.T) {
	c, f := fixture(t)
	entered := make(chan int, 3)
	release := make(chan struct{})
	current := f.IssueFunc
	var mu sync.Mutex
	active := map[int]bool{}
	var inputs []observe.PolicyInput
	f.IssueFunc = func(ctx context.Context, ref github.Ref) (github.Issue, error) {
		mu.Lock()
		if active[ref.Number] {
			t.Error("overlap")
		}
		active[ref.Number] = true
		mu.Unlock()
		entered <- ref.Number
		if ref.Number == 14 {
			select {
			case <-release:
			case <-ctx.Done():
				return github.Issue{}, ctx.Err()
			}
		}
		return current(ctx, ref)
	}
	c.Consumer = consumerFunc(func(_ context.Context, p observe.PolicyInput) error {
		mu.Lock()
		defer mu.Unlock()
		inputs = append(inputs, p)
		active[int(p.Observed.Resource.Number)] = false
		return nil
	})
	r := openRuntime(t, c)
	a := accept(t, r, "first", 14)
	b := accept(t, r, "second", 14)
	accept(t, r, "unrelated", 15)
	done := make(chan error, 1)
	go func() { done <- r.worker.Step(context.Background()) }()
	x, y := receive(t, entered), receive(t, entered)
	if !((x == 14 && y == 15) || (x == 15 && y == 14)) {
		t.Fatal(x, y)
	}
	// Both lanes reached current reads while the first same-resource read is blocked.
	select {
	case n := <-entered:
		t.Fatal("later same-resource read ran early", n)
	default:
	}
	close(release)
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var same []observe.PolicyInput
	for _, p := range inputs {
		if p.Observed.Resource.Number == 14 {
			same = append(same, p)
		}
	}
	if len(same) != 2 || same[0].Sequence != a.Sequence || same[1].Sequence != b.Sequence || a.Sequence >= b.Sequence {
		t.Fatal(same)
	}
	for _, id := range []string{"first", "second", "unrelated"} {
		state(t, r, id, storage.Completed, 1)
	}
}

func TestFoundationShutdownReopen(t *testing.T) {
	c, f := fixture(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	cleanup := make(chan struct{})
	current := f.IssueFunc
	f.IssueFunc = func(ctx context.Context, _ github.Ref) (github.Issue, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-cleanup
		return github.Issue{}, ctx.Err()
	}
	r := openRuntime(t, c)
	accept(t, r, "shutdown", 14)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	closed := make(chan struct{})
	go func() { done <- r.run(ctx, &closingListener{Listener: listener, closed: closed}) }()
	receive(t, started)
	cancel()
	receive(t, cancelled)
	receive(t, closed)
	rejected := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(rejected, signed("after-shutdown", 14))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatal("shutdown admission", rejected.Code)
	}
	if _, err := r.store.Event(context.Background(), "after-shutdown"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("accepted after shutdown", err)
	}
	// Store remains available while cancellation-aware processor finishes cleanup.
	state(t, r, "shutdown", storage.Processing, 1)
	select {
	case err := <-done:
		t.Fatal("returned before cleanup", err)
	default:
	}
	close(cleanup)
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Event(context.Background(), "shutdown"); err == nil {
		t.Fatal("store still open")
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + listener.Addr().String() + "/webhooks/github")
	if err == nil {
		resp.Body.Close()
		t.Fatal("new traffic accepted")
	}
	f.IssueFunc = current
	handoffs := 0
	c.Consumer = consumerFunc(func(context.Context, observe.PolicyInput) error { handoffs++; return nil })
	b := openRuntime(t, c)
	state(t, b, "shutdown", storage.Pending, 1)
	step(t, b)
	state(t, b, "shutdown", storage.Completed, 2)
	if handoffs != 1 {
		t.Fatal(handoffs)
	}
}

type closingListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *closingListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func TestStartupFailures(t *testing.T) {
	for _, name := range []string{"source", "discovery", "project", "field", "ambiguous", "kind", "secret", "secret-newline", "directory", "concurrency", "poll", "retry", "listen", "client"} {
		t.Run(name, func(t *testing.T) {
			c, f := fixture(t)
			source, schema := preparationFixture()
			switch name {
			case "source":
				c.Source = config.SourceConfig{}
			case "discovery":
				f.DiscoverSchemaFunc = func(context.Context, config.SourceConfig) (config.Schema, error) {
					return config.Schema{}, errors.New("untrusted provider body")
				}
			case "project":
				schema.Projects = schema.Projects[1:]
			case "field":
				schema.Projects[0].Fields = schema.Projects[0].Fields[1:]
			case "ambiguous":
				schema.Projects = append(schema.Projects, schema.Projects[0])
			case "kind":
				schema.Projects[0].Fields[0].Kind = config.Date
			case "secret":
				c.WebhookSecret = nil
			case "secret-newline":
				c.WebhookSecret = []byte("a\nb")
			case "directory":
				if err := os.WriteFile(c.DataDir, []byte("file"), 0600); err != nil {
					t.Fatal(err)
				}
			case "concurrency":
				c.Concurrency = 0
			case "poll":
				c.PollInterval = time.Millisecond
			case "retry":
				c.RetryDelay = 0
			case "listen":
				c.Listen = "invalid"
			case "client":
				c.Client = nil
			}
			if name == "project" || name == "field" || name == "ambiguous" || name == "kind" {
				f.DiscoverSchemaFunc = func(context.Context, config.SourceConfig) (config.Schema, error) { return schema, nil }
				c.Source = source
			}
			if err := Run(context.Background(), c); err == nil {
				t.Fatal("startup accepted")
			}
		})
	}
}

func TestLocalClassifier(t *testing.T) {
	for err, want := range map[error]string{observe.ErrIdentity: "observe_identity", observe.ErrRepository: "observe_repository", observe.ErrBinding: "observe_binding", observe.ErrObservation: "observe_observation", errors.New("arbitrary sensitive text"): "local_processor"} {
		got := classify(fmt.Errorf("wrapped: %w", err))
		if got.Retryable || got.Category != want {
			t.Fatal(got)
		}
	}
}

// failureListener models an HTTP infrastructure failure after a worker enters
// processing; its barrier avoids racing shutdown against startup.
type failureListener struct {
	net.Listener
	fail <-chan struct{}
}

func (l failureListener) Accept() (net.Conn, error) {
	<-l.fail
	return nil, errors.New("untrusted listener detail")
}

func TestRuntimeFatalHTTP(t *testing.T) {
	c, f := fixture(t)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	f.IssueFunc = func(ctx context.Context, _ github.Ref) (github.Issue, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return github.Issue{}, ctx.Err()
	}
	r := openRuntime(t, c)
	accept(t, r, "fatal", 14)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fail := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- r.run(context.Background(), failureListener{l, fail}) }()
	receive(t, entered)
	close(fail)
	receive(t, cancelled)
	if err := receive(t, done); err == nil {
		t.Fatal("fatal error hidden")
	}
	b := openRuntime(t, c)
	state(t, b, "fatal", storage.Pending, 1)
}

func TestRuntimeFatalWorker(t *testing.T) {
	c, _ := fixture(t)
	r := openRuntime(t, c)
	// A closed authority Store is an infrastructure error, not an event failure.
	if err := r.store.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.run(context.Background(), l) }()
	if err := receive(t, done); err == nil {
		t.Fatal("worker failure hidden")
	}
	if conn, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("HTTP listener remained open")
	}
}

func TestIngressShutdownWaits(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h := &ingress{next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(204) })}
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil)); close(done) }()
	receive(t, entered)
	h.stop()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != 503 {
		t.Fatal(rec.Code)
	}
	waited := make(chan struct{})
	go func() { h.active.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("did not wait for active request")
	default:
	}
	close(release)
	receive(t, done)
	receive(t, waited)
}

func TestFoundationServing(t *testing.T) {
	c, _ := fixture(t)
	handoff := make(chan observe.PolicyInput, 1)
	c.Consumer = consumerFunc(func(_ context.Context, p observe.PolicyInput) error { handoff <- p; return nil })
	r := openRuntime(t, c)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.run(ctx, l) }()
	req := signed("http", 14)
	req.URL.Scheme = "http"
	req.URL.Host = l.Addr().String()
	req.RequestURI = ""
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	if _, err := r.store.Event(context.Background(), "http"); err != nil {
		t.Fatal("ack before acceptance", err)
	}
	p := receive(t, handoff)
	if p.Observed.Issue.Title != "current" {
		t.Fatal("stale HTTP handoff")
	}
	// Step serializes with Run's snapshot, so this waits for its settlement without
	// sleeping or assuming the consumer callback implies durable completion.
	step(t, r)
	state(t, r, "http", storage.Completed, 1)
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
}
