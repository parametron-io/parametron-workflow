// Package app composes the controller foundation and explicitly injected semantic
// convergence and explicitly enabled deployment-wide Engineering execution.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ea "github.com/parametron-io/parametron-workflow/internal/engineeringactivity"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ef "github.com/parametron-io/parametron-workflow/internal/engineeringflow"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	er "github.com/parametron-io/parametron-workflow/internal/engineeringreconcile"
	et "github.com/parametron-io/parametron-workflow/internal/engineeringterminal"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	pc "github.com/parametron-io/parametron-workflow/internal/prcompanion"
	pcr "github.com/parametron-io/parametron-workflow/internal/prcompanionreconcile"
	"github.com/parametron-io/parametron-workflow/internal/prcompletion"
	"github.com/parametron-io/parametron-workflow/internal/prdiscovery"
	po "github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/prreview"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/semanticreconcile"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/webhook"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

const DatabaseName = "workflow.sqlite"
const ShutdownTimeout = 10 * time.Second

// Config separates deployment bindings from process settings. All operational
// settings are explicit; the command supplies documented defaults. Client is
// shared by discovery and observation. Consumer defaults to FoundationSink.
type Config struct {
	// Engineering enables Phase-4 composition; zero retains the foundation path.
	Engineering   bool
	Source        config.SourceConfig
	Client        github.Client
	DataDir       string
	Listen        string
	WebhookSecret []byte
	Concurrency   int
	PollInterval  time.Duration
	RetryDelay    time.Duration
	Consumer      observe.Consumer
	// Runner optionally enables durable semantic integration. Engineering runs
	// without it only when the complete snapshot already has Type authority.
	// SemanticConsumer receives the enriched handoff.
	Runner           semantic.Runner
	Mutator          github.Mutator
	SemanticConsumer semanticflow.Consumer
	Clock            func() time.Time
	// Deployment is the result of Prepare, never an alternative startup input.
	Deployment *config.ResolvedConfig
}

// FoundationSink acknowledges a normalized handoff without policy, mutations,
// classification, logging input, or fabricated decision persistence.
type FoundationSink struct{}

func (FoundationSink) Evaluate(context.Context, observe.PolicyInput) error { return nil }

type SemanticFoundationSink struct{}

func (SemanticFoundationSink) Evaluate(context.Context, semanticflow.PolicyInput) error { return nil }

func classify(err error) worker.Classification {
	if retryable, category, ok := semanticflow.FailureCategory(err); ok {
		return worker.Classification{Retryable: retryable, Category: category}
	}
	for _, item := range []struct {
		err       error
		category  string
		retryable bool
	}{
		{pcr.ErrOwnership, "pr_companion_ownership", false},
		{pcr.ErrAmbiguous, "pr_companion_ambiguous", false},
		{pcr.ErrStorage, "pr_companion_storage", false},
		{pcr.ErrInvalid, "pr_companion_invalid", false},
		{pcr.ErrProject, "pr_companion_project", false},
		{pc.ErrCapacity, "pr_companion_capacity", false},
		{pc.ErrInvalid, "pr_companion_policy_invalid", false},
		{pc.ErrBinding, "pr_companion_binding", false},
		{pc.ErrOwnership, "pr_companion_ownership", false},
		{pc.ErrPrimary, "pr_companion_primary", false},
		{pc.ErrTarget, "pr_companion_target", true},
		{pcr.ErrChanged, "pr_companion_changed", true},
		{pcr.ErrIncomplete, "pr_companion_incomplete", true},
		{github.ErrRequestLimit, "pr_companion_budget", true},
		{prdiscovery.ErrInvalid, "pr_discovery_invalid", false},
		{prdiscovery.ErrLimit, "pr_discovery_limit", false},
		{prdiscovery.ErrChanged, "pr_discovery_changed", true},
		{prdiscovery.ErrIncomplete, "pr_discovery_incomplete", true},
		{github.ErrNativeLifecycleChanged, "native_lifecycle_changed", true},
		{prcompletion.ErrChanged, "pr_completion_changed", true},
		{prcompletion.ErrInvalid, "pr_completion_invalid", false},
		{prcompletion.ErrRejected, "pr_completion_rejected", false},
		{prcompletion.ErrProject, "pr_completion_project", false},
		{prcompletion.ErrProvenance, "pr_completion_provenance", false},
		{prcompletion.ErrAmbiguous, "pr_completion_ambiguous", false},
		{prcompletion.ErrStorage, "pr_completion_storage", false},
		{prcompletion.ErrReopened, "pr_completion_reopened", false},
		{et.ErrInvalid, "engineering_terminal_invalid", false},
		{github.ErrReviewFactsChanged, "pr_review_changed", true},
		{prreview.ErrInvalid, "pr_review_invalid", false},
		{prreview.ErrRejected, "pr_review_rejected", false},
		{prreview.ErrChanged, "pr_review_changed", true},
		{prreview.ErrSuperseded, "pr_review_superseded", false},
		{prreview.ErrCorrelation, "pr_review_correlation", false},
		{prreview.ErrProvenance, "pr_review_provenance", false},
		{prreview.ErrAmbiguous, "pr_review_ambiguous", false},
		{prreview.ErrProject, "pr_review_project", false},
		{prreview.ErrStorage, "pr_review_storage", false},
		{po.ErrInvalid, "pr_onboarding_invalid", false},
		{po.ErrRejected, "pr_onboarding_rejected", false},
		{po.ErrChanged, "pr_onboarding_changed", true},
		{po.ErrProject, "pr_onboarding_project", false},
		{po.ErrProvenance, "pr_onboarding_provenance", false},
		{po.ErrAmbiguous, "pr_onboarding_ambiguous", false},
		{po.ErrStorage, "pr_onboarding_storage", false},
		{prbinding.ErrInvalid, "pr_binding_invalid", false},
		{ec.ErrChanged, "engineering_changed", true},
		{ec.ErrIncomplete, "engineering_incomplete", true},
		{ea.ErrStorage, "engineering_activity_storage", false},
		{eb.ErrStorage, "engineering_branch_storage", false},
		{ec.ErrDeployment, "engineering_deployment", false},
		{ec.ErrParentCycle, "engineering_parent_cycle", false},
		{ec.ErrDependencyCycle, "engineering_dependency_cycle", false},
		{ec.ErrMissing, "engineering_missing", false},
		{ec.ErrInvalid, "engineering_graph", false},
		{ep.ErrInvalid, "engineering_policy", false},
		{ep.ErrParent, "engineering_parent", false},
		{ep.ErrLifecycle, "engineering_lifecycle", false},
		{ep.ErrPhaseOrder, "engineering_phase_order", false},
		{eo.ErrRoadmapCapacity, "engineering_capacity", false},
		{eo.ErrRoadmapHierarchy, "engineering_hierarchy", false},
		{eo.ErrInvalid, "engineering_order", false},
		{eo.ErrPhaseOrder, "engineering_phase_order", false},
		{eo.ErrRoadmap, "engineering_roadmap", false},
		{ea.ErrProvenance, "engineering_activity_provenance", false},
		{eb.ErrProvenance, "engineering_branch_provenance", false},
		{eb.ErrAmbiguous, "engineering_branch_ambiguous", false},
		{ea.ErrProject, "engineering_project", false},
		{er.ErrProject, "engineering_project", false},
		{ea.ErrInvalid, "engineering_activity", false},
		{eb.ErrInvalid, "engineering_branch", false},
		{er.ErrInvalid, "engineering_configuration", false},
		{ef.ErrInvalid, "engineering_notification", false},
	} {
		if errors.Is(err, item.err) {
			return worker.Classification{Retryable: item.retryable, Category: item.category}
		}
	}
	category := "local_processor"
	for _, item := range []struct {
		err      error
		category string
	}{
		{observe.ErrIdentity, "observe_identity"}, {observe.ErrRepository, "observe_repository"},
		{observe.ErrBinding, "observe_binding"}, {observe.ErrObservation, "observe_observation"},
		{observe.ErrConfiguration, "observe_configuration"},
		{semanticreconcile.ErrReconcile, "semantic_reconcile"},
	} {
		if errors.Is(err, item.err) {
			category = item.category
			break
		}
	}
	return worker.Classification{Category: category}
}

func (c Config) validate() error {
	if c.Runner == nil && (c.SemanticConsumer != nil || c.Mutator != nil) || c.Runner != nil && c.Consumer != nil || c.Mutator != nil && c.SemanticConsumer != nil {
		return errors.New("app: semantic composition requires Runner and enriched consumer boundary")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("app: data directory required")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil || strings.TrimSpace(host) != host || port == "" {
		return errors.New("app: invalid listen address")
	}
	// ResolveTCPAddr rejects invalid numeric ports and unknown service names.
	if _, err := net.ResolveTCPAddr("tcp", c.Listen); err != nil {
		return errors.New("app: invalid listen address")
	}
	if len(c.WebhookSecret) == 0 || strings.TrimSpace(string(c.WebhookSecret)) == "" || strings.ContainsAny(string(c.WebhookSecret), "\r\n\x00") {
		return errors.New("app: invalid webhook secret")
	}
	if c.Concurrency < 1 || c.PollInterval < time.Second || c.RetryDelay <= 0 {
		return errors.New("app: positive concurrency/retry delay and poll interval >= 1s required")
	}
	return nil
}

type runtime struct {
	store   *storage.Store
	worker  *worker.Worker
	server  *http.Server
	poll    time.Duration
	ingress *ingress
}

// The admission gate makes handler lifetime explicit even after forced server
// closure. No admitted request may still be using SQLite when Store.Close runs.
type ingress struct {
	mu      sync.Mutex
	closing bool
	active  sync.WaitGroup
	next    http.Handler
}

func (h *ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.active.Add(1)
	h.mu.Unlock()
	defer h.active.Done()
	h.next.ServeHTTP(w, r)
}

func (h *ingress) stop() {
	h.mu.Lock()
	h.closing = true
	h.mu.Unlock()
}

// compose is also the integration test's deterministic Step boundary. It uses
// exactly the same Store, handler, resolver, processor and worker as Run.
func compose(ctx context.Context, c Config) (_ *runtime, result error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	prepared, err := Prepare(ctx, c.Source, c.Client)
	if err != nil {
		return nil, err
	}
	resolver, err := observe.NewResolver(*prepared.Deployment)
	if err != nil {
		return nil, err
	}
	consumer := c.Consumer
	if consumer == nil {
		consumer = FoundationSink{}
	}
	processor, err := observe.NewProcessor(*prepared.Deployment, c.Client, consumer)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(c.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("app: create data directory: %w", err)
	}
	path := filepath.Join(c.DataDir, DatabaseName)
	// Pre-create with restrictive permissions before SQLite creates sidecars.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("app: open database file: %w", err)
	}
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	store, err := storage.Open(ctx, path)
	if err != nil {
		return nil, errors.New("app: initialize database failed")
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, store.Close())
		}
	}()
	if c.Runner != nil && !c.Engineering {
		consumer := c.SemanticConsumer
		if c.Mutator != nil {
			consumer, err = semanticreconcile.New(*prepared.Deployment, processor, c.Mutator)
			if err != nil {
				return nil, err
			}
		}
		if consumer == nil {
			consumer = SemanticFoundationSink{}
		}
		clock := c.Clock
		if clock == nil {
			clock = time.Now
		}
		coordinator, err := semanticflow.New(semanticflow.Config{Store: store, Runner: c.Runner, Primary: processor, Consumer: consumer, Clock: clock})
		if err != nil {
			return nil, err
		}
		processor, err = observe.NewProcessor(*prepared.Deployment, c.Client, coordinator)
		if err != nil {
			return nil, err
		}
	}
	var resourceResolver worker.ResourceResolver = resolver
	var eventProcessor worker.Processor = processor
	if c.Engineering {
		flow, err := composeEngineering(c, *prepared.Deployment, store, resolver, processor)
		if err != nil {
			return nil, err
		}
		resourceResolver, eventProcessor = flow, flow
	}
	w, err := worker.New(worker.Config{Store: store, Resolver: resourceResolver, Processor: eventProcessor, Classifier: classify, Concurrency: c.Concurrency, Clock: c.Clock,
		RetrySchedule: func(_ worker.Classification, _ int64, now time.Time) time.Time { return now.Add(c.RetryDelay) }})
	if err != nil {
		return nil, err
	}
	handler, err := webhook.New(webhook.Config{Store: store, Secret: c.WebhookSecret, Clock: c.Clock})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	gate := &ingress{next: mux}
	return &runtime{store: store, worker: w, server: &http.Server{Handler: gate, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}, poll: c.PollInterval, ingress: gate}, nil
}

// Run prepares schema and SQLite before binding HTTP. It owns one Store until
// both serving and worker cleanup have finished. Cancellation is normal exit.
func Run(ctx context.Context, c Config) error {
	r, err := compose(ctx, c)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return errors.Join(errors.New("app: HTTP listener failed"), r.store.Close())
	}
	return r.run(ctx, listener)
}

func (r *runtime) run(ctx context.Context, listener net.Listener) (result error) {
	defer func() { result = errors.Join(result, r.store.Close()) }()
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	serverResult := make(chan error, 1)
	workerResult := make(chan error, 1)
	go func() { serverResult <- r.server.Serve(listener) }()
	go func() { workerResult <- r.worker.Run(workCtx, r.poll) }()
	var serverDone, workerDone bool
	select {
	case <-ctx.Done():
	case err := <-serverResult:
		serverDone = true
		if !errors.Is(err, http.ErrServerClosed) {
			result = errors.New("app: HTTP serving failed")
		}
	case err := <-workerResult:
		workerDone = true
		if err != nil {
			result = errors.New("app: worker infrastructure failed")
		}
	}
	// Shutdown closes listeners synchronously before waiting for active HTTP work.
	r.ingress.stop()
	shutdownCtx, stop := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer stop()
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- r.server.Shutdown(shutdownCtx) }()
	cancel()
	if err := <-shutdownResult; err != nil {
		result = errors.Join(result, errors.New("app: HTTP shutdown timed out"))
		r.server.Close()
	}
	if !serverDone {
		if err := <-serverResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
			result = errors.Join(result, errors.New("app: HTTP serving failed"))
		}
	}
	if !workerDone {
		if err := <-workerResult; err != nil {
			result = errors.Join(result, errors.New("app: worker infrastructure failed"))
		}
	}
	r.ingress.active.Wait()
	return result
}
