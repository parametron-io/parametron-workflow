package semanticflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

type consumerFunc func(context.Context, PolicyInput) error

func (f consumerFunc) Evaluate(ctx context.Context, in PolicyInput) error { return f(ctx, in) }

type observeSink struct{}

func (observeSink) Evaluate(context.Context, observe.PolicyInput) error { return nil }

func deployment() config.ResolvedConfig {
	project := func(id string, bug bool) config.ResolvedProject {
		p := config.ResolvedProject{ID: id, Fields: map[config.FieldRole]string{}, StatusOptions: map[config.StatusRole]string{}}
		for _, role := range []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate} {
			p.Fields[role] = id + string(role)
		}
		for _, role := range []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done} {
			p.StatusOptions[role] = id + string(role)
		}
		if bug {
			p.Fields[config.PriorityScore] = id + "score"
			p.StatusOptions[config.ToTriage] = id + "triage"
		} else {
			p.Fields[config.RoadmapOrder] = id + "roadmap_order"
			p.StatusOptions[config.Blocked] = id + "blocked"
		}
		return p
	}
	return config.ResolvedConfig{Organization: config.Organization{Login: "parametron-io"}, Repositories: []config.Repository{{ID: "repo-id", Owner: "parametron-io", Name: "repo"}}, Engineering: project("engineering", false), BugTracker: project("bugs", true)}
}
func result(t *testing.T, cap semantic.Capability) semantic.Result {
	t.Helper()
	catalog, err := semantic.LoadCatalog(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	a, err := catalog.Lookup(cap)
	if err != nil {
		t.Fatal(err)
	}
	out := `{"type":"Task","labels":["docs","workflow"],"priority":"High","effort":"S"}`
	if cap == semantic.ClassifyPR {
		out = `{"labels":["docs","workflow"]}`
	}
	return semantic.Result{Output: json.RawMessage(out), Provenance: semantic.Provenance{Capability: cap, Provider: "fake", Model: "cheap", PromptIdentity: a.Prompt.Identity, PromptDigest: a.Prompt.Digest, SchemaIdentity: a.Schema.Identity, SchemaDigest: a.Schema.Digest}}
}

type harness struct {
	t         *testing.T
	path      string
	store     *storage.Store
	worker    *worker.Worker
	processor *observe.Processor
	flow      *Coordinator
	fake      *github.Fake
	runner    semantic.Runner
	mu        sync.Mutex
	issue     github.Issue
	pr        github.PullRequest
	missing   bool
	inputs    []PolicyInput
	consume   func(context.Context, PolicyInput) error
	clock     atomic.Int64
	calls     atomic.Int64
	projects  atomic.Int64
}

func newHarness(t *testing.T, kind string) *harness {
	t.Helper()
	h := &harness{t: t, path: filepath.Join(t.TempDir(), "workflow.sqlite")}
	h.clock.Store(1700000000)
	id := github.Identity{ID: "current-node", Repository: deployment().Repositories[0], Number: 27}
	h.issue = github.Issue{Identity: id, Title: "title A", Body: "body A", Type: &github.IssueType{Name: "Bug"}}
	h.pr = github.PullRequest{Identity: id, Title: "title A", Body: "Target: #3"}
	h.fake = &github.Fake{
		IssueFunc: func(context.Context, github.Ref) (github.Issue, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.missing {
				return github.Issue{}, &github.Error{Category: github.NotFound}
			}
			return h.issue, nil
		},
		PullRequestFunc: func(context.Context, github.Ref) (github.PullRequest, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.missing {
				return github.PullRequest{}, &github.Error{Category: github.NotFound}
			}
			return h.pr, nil
		},
		ProjectItemsFunc: func(context.Context, string, string, map[string]config.FieldKind) ([]github.ProjectItem, error) {
			h.projects.Add(1)
			return nil, nil
		},
	}
	h.runner = &semantic.Fake{
		ClassifyIssueFunc: func(_ context.Context, input semantic.Input) (semantic.Result, error) {
			h.calls.Add(1)
			if input.Title != "title A" || input.Body != "body A" {
				t.Error("incorrect current input", input)
			}
			return result(t, semantic.ClassifyIssue), nil
		},
		ClassifyPRFunc: func(_ context.Context, input semantic.Input) (semantic.Result, error) {
			h.calls.Add(1)
			if input.Title != "title A" || input.Body != "Target: #3" {
				t.Error("incorrect PR input")
			}
			return result(t, semantic.ClassifyPR), nil
		},
		EstimateIssueFunc: func(context.Context, semantic.Input) (semantic.Result, error) {
			t.Error("Estimate executed during ingress")
			return semantic.Result{}, semantic.ErrCapability
		},
	}
	h.open()
	t.Cleanup(func() { h.store.Close() })
	h.insert("initial", kind)
	return h
}
func (h *harness) now() time.Time { return time.Unix(h.clock.Load(), 0).UTC() }
func (h *harness) open() {
	h.t.Helper()
	var err error
	h.store, err = storage.Open(context.Background(), h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	h.compose()
}
func (h *harness) compose() {
	h.t.Helper()
	primary, err := observe.NewProcessor(deployment(), h.fake, observeSink{})
	if err != nil {
		h.t.Fatal(err)
	}
	h.flow, err = New(Config{Store: h.store, Runner: h.runner, Primary: primary, Clock: h.now, Consumer: consumerFunc(func(ctx context.Context, in PolicyInput) error {
		h.mu.Lock()
		h.inputs = append(h.inputs, in)
		h.mu.Unlock()
		if h.consume != nil {
			return h.consume(ctx, in)
		}
		return nil
	})})
	if err != nil {
		h.t.Fatal(err)
	}
	h.processor, err = observe.NewProcessor(deployment(), h.fake, h.flow)
	if err != nil {
		h.t.Fatal(err)
	}
	resolver, err := observe.NewResolver(deployment())
	if err != nil {
		h.t.Fatal(err)
	}
	h.worker, err = worker.New(worker.Config{Store: h.store, Resolver: resolver, Processor: h.processor, Clock: h.now, Concurrency: 2,
		Classifier: func(err error) worker.Classification {
			retry, cat, ok := FailureCategory(err)
			if !ok {
				cat = "local_processor"
			}
			return worker.Classification{Retryable: retry, Category: cat}
		},
		RetrySchedule: func(_ worker.Classification, _ int64, now time.Time) time.Time { return now.Add(time.Minute) },
	})
	if err != nil {
		h.t.Fatal(err)
	}
}
func (h *harness) insert(id, kind string) bool {
	h.t.Helper()
	payload := fmt.Sprintf(`{"repository":{"owner":{"login":"parametron-io"},"name":"repo"},"%s":{"number":27,"title":"historical","body":"historical"}}`, kind)
	inserted, err := h.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: "notification", Payload: []byte(payload), ReceivedAt: h.now()})
	if err != nil {
		h.t.Fatal(err)
	}
	return inserted
}
func (h *harness) step() {
	h.t.Helper()
	if err := h.worker.Step(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}
func (h *harness) state(id string, status storage.Status, category string) storage.Event {
	h.t.Helper()
	e, err := h.store.Event(context.Background(), id)
	if err != nil || e.State.Status != status || e.State.ErrorCategory != category {
		h.t.Fatalf("state: %+v %v", e.State, err)
	}
	return e
}
func (h *harness) completion(kind string) storage.Provenance {
	h.t.Helper()
	p, err := h.store.Provenance(context.Background(), Namespace, "parametron-io/repo/"+kind+"/27")
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}
func (h *harness) noCompletion(kind string) {
	h.t.Helper()
	_, err := h.store.Provenance(context.Background(), Namespace, "parametron-io/repo/"+kind+"/27")
	if !errors.Is(err, storage.ErrNotFound) {
		h.t.Fatal("unexpected completion", err)
	}
}

func TestGatingAndMissing(t *testing.T) {
	for _, tc := range []struct {
		body     string
		missing  bool
		required bool
	}{
		{"", false, true}, {"Automation: false", false, false}, {"Classification: false", false, false},
		{"Automation: false\nClassification: true", false, false}, {"", true, false},
	} {
		t.Run(tc.body+fmt.Sprint(tc.missing), func(t *testing.T) {
			h := newHarness(t, "issue")
			h.issue.Body, h.missing = tc.body, tc.missing
			h.runner = &semantic.Fake{ClassifyIssueFunc: func(_ context.Context, input semantic.Input) (semantic.Result, error) {
				h.calls.Add(1)
				if input.Body != tc.body {
					t.Error("wrong body")
				}
				return result(t, semantic.ClassifyIssue), nil
			}}
			h.compose()
			h.step()
			h.state("initial", storage.Completed, "")
			in := h.inputs[0]
			if in.Classification.Required() != tc.required {
				t.Fatal(in.Classification)
			}
			if tc.required {
				if h.calls.Load() != 1 || in.Classification.State() != Accepted {
					t.Fatal("classification bypassed by observed Issue Type")
				}
			} else {
				if h.calls.Load() != 0 || in.Classification.State() != NotRequired {
					t.Fatal("disabled classification ran")
				}
				h.noCompletion("issue")
				if _, ok := in.Classification.Issue(); ok {
					t.Fatal("fabricated acceptance")
				}
			}
			if !tc.missing {
				expected, _ := intent.Parse(tc.body, intent.Context{Organization: "parametron-io", Repository: "repo"})
				if !reflect.DeepEqual(in.Intent, expected) {
					t.Fatal("explicit intent changed")
				}
			}
		})
	}
}

func TestDurableIssueAndPRReuse(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			h.step()
			h.state("initial", storage.Completed, "")
			p := h.completion(kind)
			if p.CreatedAt != h.now() || p.DeliveryID != "initial" {
				t.Fatal(p)
			}
			for _, raw := range []string{"title A", "body A", "Target:", "historical", "Output", "prompt text", "labels\\\""} {
				if bytes.Contains(p.Metadata, []byte(raw)) {
					t.Fatal("raw semantic data persisted", raw)
				}
			}
			if !bytes.Contains(p.Metadata, []byte(`"format":1`)) || !bytes.Contains(p.Metadata, []byte(`"prompt_digest":"sha256:`)) {
				t.Fatal("missing compatibility/provenance")
			}
			in := h.inputs[0]
			if in.Classification.State() != Accepted || h.calls.Load() != 1 || h.projects.Load() != 2 {
				t.Fatal("missing acceptance or unnecessary revalidation Projects")
			}
			if kind == "issue" {
				value, ok := in.Classification.Issue()
				if !ok || value.Classification.Type != semanticpolicy.Task || !reflect.DeepEqual(value.Classification.Labels, []semanticpolicy.Label{"workflow", "docs"}) {
					t.Fatal(value)
				}
				value.Classification.Labels[0] = "corrupt"
				again, _ := in.Classification.Issue()
				if again.Classification.Labels[0] != "workflow" {
					t.Fatal("aliased handoff")
				}
			} else {
				value, ok := in.Classification.PR()
				if !ok || len(value.Classification.Labels) != 2 || in.Intent.Target == nil || in.Intent.Target.Number != 3 {
					t.Fatal("PR/Target handoff")
				}
			}
			if h.insert("initial", kind) {
				t.Fatal("duplicate delivery inserted")
			}
			h.runner = &semantic.Fake{} // Would fail if invoked under outage.
			h.compose()
			h.insert("later", kind)
			h.step()
			h.state("later", storage.Completed, "")
			if h.calls.Load() != 1 || len(h.inputs) != 2 || h.inputs[1].Classification.State() != Accepted || !bytes.Equal(p.Metadata, h.completion(kind).Metadata) {
				t.Fatal("later event reclassified")
			}
		})
	}
}

func TestDisableAndReenablePreservesCompletion(t *testing.T) {
	h := newHarness(t, "issue")
	h.step()
	p := h.completion("issue")
	h.runner = &semantic.Fake{}
	h.compose()
	h.issue.Body = "Classification: false"
	h.insert("disabled", "issue")
	h.step()
	if in := h.inputs[1]; in.Classification.State() != NotRequired || !in.Intent.Explicit.Classification.Explicit {
		t.Fatal(in)
	}
	h.issue.Title, h.issue.Body = "new ordinary title", "Classification: true"
	h.insert("reenabled", "issue")
	h.step()
	if h.inputs[2].Classification.State() != Accepted || !bytes.Equal(p.Metadata, h.completion("issue").Metadata) {
		t.Fatal("initial completion invalidated")
	}
}

func TestPendingFailuresAndWorkerCategories(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		output   string
		category string
		retry    bool
	}{
		{"transient", &semantic.ProviderError{Kind: semantic.Transient, Cause: errors.New("RAW SECRET")}, "", "semantic_transient", true},
		{"rate", &semantic.ProviderError{Kind: semantic.RateLimited}, "", "semantic_rate_limited", true},
		{"timeout", semantic.ErrTimeout, "", "semantic_timeout", true},
		{"cancelled", semantic.ErrCancelled, "", "semantic_cancelled", true},
		{"permanent", &semantic.ProviderError{Kind: semantic.Permanent}, "", "semantic_permanent", false},
		{"unknown", errors.New("RAW SECRET"), "", "semantic_provider_unknown", false},
		{"response", semantic.ErrResponse, "", "semantic_response", false},
		{"malformed", nil, `{`, "semantic_response", false},
		{"partial", nil, `{}`, "semantic_response", false},
		{"policy", nil, `{"type":"Task","labels":["help wanted"],"priority":"High","effort":"S"}`, "semantic_policy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "issue")
			h.runner = &semantic.Fake{ClassifyIssueFunc: func(context.Context, semantic.Input) (semantic.Result, error) {
				h.calls.Add(1)
				r := result(t, semantic.ClassifyIssue)
				if tc.output != "" {
					r.Output = json.RawMessage(tc.output)
				}
				return r, tc.err
			}}
			h.compose()
			// Test the explicit Pending representation independently of worker settlement.
			e, _ := h.store.Event(context.Background(), "initial")
			r := storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: "issue", Number: 27}
			e.Resource = &r
			primary, err := h.processor.ReadPrimary(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			in, err := h.flow.Enrich(context.Background(), observe.PolicyInput{DeliveryID: "initial", Observed: primary})
			if err == nil || in.Classification.State() != Pending {
				t.Fatal("failed classification lost Pending", err)
			}
			if _, ok := in.Classification.Issue(); ok {
				t.Fatal("failure fabricated acceptance")
			}
			h.noCompletion("issue")
			h.calls.Store(0)
			h.step()
			status := storage.Failed
			if tc.retry {
				status = storage.Retryable
			}
			h.state("initial", status, tc.category)
			h.noCompletion("issue")
			if h.calls.Load() != 1 || len(h.inputs) != 0 {
				t.Fatal("internal retry or failed downstream handoff")
			}
		})
	}
}

func TestInvalidIntentIsTerminal(t *testing.T) {
	h := newHarness(t, "issue")
	h.issue.Body = "Classification: yes"
	h.step()
	h.state("initial", storage.Failed, "semantic_intent")
	h.noCompletion("issue")
	if h.calls.Load() != 0 {
		t.Fatal("invalid intent ran model")
	}
}

// Barriers prove an edit can happen while a real worker attempt is in flight.
func TestInFlightEditsRejectStaleAndRetry(t *testing.T) {
	for _, change := range []string{"title", "body", "directive", "missing", "outputs"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t, "issue")
			started, release := make(chan semantic.Input, 1), make(chan struct{})
			h.runner = &semantic.Fake{ClassifyIssueFunc: func(_ context.Context, in semantic.Input) (semantic.Result, error) {
				if h.calls.Add(1) == 1 {
					started <- in
					<-release
				} else {
					h.mu.Lock()
					title, body := h.issue.Title, h.issue.Body
					h.mu.Unlock()
					if in.Title != title || in.Body != body {
						t.Error("retry did not observe new input")
					}
				}
				return result(t, semantic.ClassifyIssue), nil
			}}
			h.compose()
			done := make(chan error, 1)
			go func() { done <- h.worker.Step(context.Background()) }()
			select {
			case in := <-started:
				if in.Title != "title A" || in.Body != "body A" {
					t.Fatal("wrong initial input")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("model not started")
			}
			h.mu.Lock()
			switch change {
			case "title":
				h.issue.Title = "title B"
			case "body":
				h.issue.Body = "body B"
			case "directive":
				h.issue.Body = "Classification: false"
			case "missing":
				h.missing = true
			case "outputs":
				h.issue.Type = &github.IssueType{Name: "Feature"}
				h.issue.Labels = []string{"security"}
			}
			h.mu.Unlock()
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not settle")
			}
			if change == "outputs" {
				h.state("initial", storage.Completed, "")
				h.completion("issue")
				if h.calls.Load() != 1 {
					t.Fatal("output drift retried")
				}
				return
			}
			e := h.state("initial", storage.Retryable, "semantic_stale")
			if e.State.Attempts != 1 || len(h.inputs) != 0 {
				t.Fatal("stale result escaped")
			}
			h.noCompletion("issue")
			h.step()
			if h.calls.Load() != 1 {
				t.Fatal("retry ignored eligibility")
			}
			h.clock.Add(60)
			h.step()
			e = h.state("initial", storage.Completed, "")
			if e.State.Attempts != 2 {
				t.Fatal("retry count")
			}
			if change == "directive" || change == "missing" {
				if h.calls.Load() != 1 || h.inputs[0].Classification.State() != NotRequired {
					t.Fatal("disabled/missing resource classified again")
				}
				h.noCompletion("issue")
			} else {
				if h.calls.Load() != 2 || h.inputs[0].Classification.State() != Accepted {
					t.Fatal("fresh result not accepted")
				}
				p := h.completion("issue")
				var record completion
				json.Unmarshal(p.Metadata, &record)
				i, _ := intent.Parse(h.issue.Body, intent.Context{Organization: "parametron-io", Repository: "repo"})
				r, _ := normalizedResource(*e.Resource)
				if record.InputDigest != fingerprint(r, h.issue.Title, h.issue.Body, i) {
					t.Fatal("old input accepted")
				}
			}
		})
	}
}

func TestRestartAfterAcceptedBeforeSettlement(t *testing.T) {
	h := newHarness(t, "issue")
	r := storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: "issue", Number: 27}
	if err := h.store.BindResource(context.Background(), "initial", r); err != nil {
		t.Fatal(err)
	}
	e, err := h.store.Claim(context.Background(), "initial", h.now())
	if err != nil {
		t.Fatal(err)
	}
	h.consume = func(context.Context, PolicyInput) error { return errors.New("simulate crash before settlement") }
	if err := h.processor.Process(context.Background(), e); err == nil {
		t.Fatal("crash boundary not exercised")
	}
	h.state("initial", storage.Processing, "")
	p := h.completion("issue")
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.runner, h.consume = &semantic.Fake{}, nil
	h.open()
	if err := h.worker.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.step()
	e = h.state("initial", storage.Completed, "")
	if e.State.Attempts != 2 || h.calls.Load() != 1 || !bytes.Equal(p.Metadata, h.completion("issue").Metadata) || h.inputs[1].Classification.State() != Accepted {
		t.Fatal("restart repeated semantic execution")
	}
}

func TestCancellationBeforeAcceptance(t *testing.T) {
	h := newHarness(t, "issue")
	ctx, cancel := context.WithCancel(context.Background())
	h.runner = &semantic.Fake{ClassifyIssueFunc: func(context.Context, semantic.Input) (semantic.Result, error) {
		h.calls.Add(1)
		cancel()
		return result(t, semantic.ClassifyIssue), nil
	}}
	h.compose()
	if err := h.worker.Step(ctx); err != nil {
		t.Fatal(err)
	}
	h.state("initial", storage.Pending, "")
	h.noCompletion("issue")
	if len(h.inputs) != 0 {
		t.Fatal("cancelled success accepted")
	}
}

func TestFingerprintAndKeys(t *testing.T) {
	r := storage.Resource{Owner: "PARAMETRON-IO", Repository: "Repo", Kind: "issue", Number: 27, NodeID: "ignored"}
	key, err := ResourceKey(r)
	if err != nil || key != "parametron-io/repo/issue/27" {
		t.Fatal(key, err)
	}
	a, _ := normalizedResource(r)
	i, _ := intent.Parse("", intent.Context{Organization: a.Owner, Repository: a.Repository})
	first := fingerprint(a, "title", "body", i)
	for n := 0; n < 20; n++ {
		if fingerprint(a, "title", "body", i) != first {
			t.Fatal("unstable fingerprint")
		}
	}
	r.NodeID = "different"
	b, _ := normalizedResource(r)
	if fingerprint(b, "title", "body", i) != first {
		t.Fatal("node evidence affected digest")
	}
	for _, changed := range []resource{{"other", "repo", "issue", 27}, {a.Owner, "other", "issue", 27}, {a.Owner, a.Repository, "pull_request", 27}, {a.Owner, a.Repository, "issue", 28}} {
		if fingerprint(changed, "title", "body", i) == first {
			t.Fatal("resource collision")
		}
	}
	if fingerprint(a, "new", "body", i) == first || fingerprint(a, "title", "new", i) == first {
		t.Fatal("content not fingerprinted")
	}
	i.Explicit.Classification = intent.Boolean{Explicit: true, Value: false}
	if fingerprint(a, "title", "body", i) == first {
		t.Fatal("intent not fingerprinted")
	}
	r.Kind = "pull_request"
	prKey, _ := ResourceKey(r)
	if prKey == key {
		t.Fatal("kind collision")
	}
	if !validDigest(first) || strings.Contains(first, "title") {
		t.Fatal(first)
	}
}

func TestRetryKeepsResourceFIFOAndLaterEventReusesAcceptance(t *testing.T) {
	h := newHarness(t, "issue")
	h.runner = &semantic.Fake{ClassifyIssueFunc: func(context.Context, semantic.Input) (semantic.Result, error) {
		if h.calls.Add(1) == 1 {
			return semantic.Result{}, &semantic.ProviderError{Kind: semantic.Transient}
		}
		return result(t, semantic.ClassifyIssue), nil
	}}
	h.compose()
	h.insert("later", "issue")
	h.step()
	h.state("initial", storage.Retryable, "semantic_transient")
	h.state("later", storage.Pending, "")
	if h.calls.Load() != 1 || len(h.inputs) != 0 {
		t.Fatal("later event bypassed failed predecessor")
	}
	h.step()
	if h.calls.Load() != 1 {
		t.Fatal("later event bypassed scheduled retry")
	}
	h.clock.Add(60)
	h.step()
	h.state("initial", storage.Completed, "")
	h.state("later", storage.Completed, "")
	if h.calls.Load() != 2 || len(h.inputs) != 2 || h.inputs[0].Current.DeliveryID != "initial" || h.inputs[1].Current.DeliveryID != "later" {
		t.Fatal("FIFO or one-shot reuse broken")
	}
	if h.completion("issue").DeliveryID != "initial" {
		t.Fatal("later event replaced accepted completion")
	}
}

func TestEquivalentResultsHaveIdenticalMetadata(t *testing.T) {
	first := newHarness(t, "issue")
	first.step()
	second := newHarness(t, "issue")
	second.clock.Add(500)
	second.runner = &semantic.Fake{ClassifyIssueFunc: func(context.Context, semantic.Input) (semantic.Result, error) {
		r := result(t, semantic.ClassifyIssue)
		r.Output = json.RawMessage(`{"effort":"S","priority":"High","labels":["workflow","docs"],"type":"Task"}`)
		return r, nil
	}}
	second.compose()
	second.step()
	if !bytes.Equal(first.completion("issue").Metadata, second.completion("issue").Metadata) {
		t.Fatal("equivalent normalized acceptance changed bytes")
	}
	if first.completion("issue").CreatedAt.Equal(second.completion("issue").CreatedAt) {
		t.Fatal("test did not vary clock")
	}
}
