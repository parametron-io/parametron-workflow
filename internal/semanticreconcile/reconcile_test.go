package semanticreconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

type sink struct{}

func (sink) Evaluate(context.Context, observe.PolicyInput) error { return nil }

type harness struct {
	t            *testing.T
	mu           sync.Mutex
	d            config.ResolvedConfig
	issue        github.Issue
	pr           github.PullRequest
	items        map[string][]github.ProjectItem
	store        *storage.Store
	processor    *observe.Processor
	flow         *semanticflow.Coordinator
	r            *Reconciler
	w            *worker.Worker
	client       *github.Fake
	mutator      *github.MutationFake
	runner       *semantic.Fake
	kind         string
	typ          semanticpolicy.IssueType
	calls, reads int
	ops          []string
	now          time.Time
	missing      bool
	readBody     *string
	failAt       string
	cancel       context.CancelFunc
	input        semanticflow.PolicyInput
}

func deployment() config.ResolvedConfig {
	d := config.ResolvedConfig{Organization: config.Organization{ID: "O", Login: "parametron-io"}, Repositories: []config.Repository{{ID: "R", Owner: "parametron-io", Name: "repo"}}, IssueTypes: map[string]string{}}
	for _, typ := range semanticpolicy.Types() {
		d.IssueTypes[string(typ)] = "T" + string(typ)
	}
	project := func(id string, bug bool) config.ResolvedProject {
		p := config.ResolvedProject{ID: id, Fields: map[config.FieldRole]string{}, StatusOptions: map[config.StatusRole]string{}, FieldOptions: map[config.FieldRole]map[string]string{config.Priority: {}, config.Effort: {}}}
		for _, role := range []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate} {
			p.Fields[role] = id + string(role)
		}
		for _, status := range []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done} {
			p.StatusOptions[status] = id + string(status)
		}
		if bug {
			p.Fields[config.PriorityScore] = id + "score"
			p.StatusOptions[config.ToTriage] = id + "triage"
		} else {
			p.Fields[config.RoadmapOrder] = id + "roadmap_order"
			p.StatusOptions[config.Blocked] = id + "blocked"
		}
		for _, v := range semanticpolicy.Priorities() {
			p.FieldOptions[config.Priority][string(v)] = id + string(v)
		}
		for _, v := range semanticpolicy.Efforts() {
			p.FieldOptions[config.Effort][string(v)] = id + string(v)
		}
		return p
	}
	d.Engineering = project("E", false)
	d.BugTracker = project("B", true)
	return d
}
func newHarness(t *testing.T, kind string, typ semanticpolicy.IssueType) *harness {
	t.Helper()
	h := &harness{t: t, d: deployment(), kind: kind, typ: typ, items: map[string][]github.ProjectItem{}, now: time.Unix(1700000000, 0)}
	identity := github.Identity{ID: "I", Number: 28, Repository: h.d.Repositories[0]}
	h.issue = github.Issue{Identity: identity, Title: "title", Body: "Classification: true", Labels: []string{"docs", "dependencies", "custom-human-label"}}
	h.pr = github.PullRequest{Identity: identity, Title: "title", Body: "Classification: true", Labels: slices.Clone(h.issue.Labels)}
	var err error
	h.store, err = storage.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.store.Close() })
	h.client = &github.Fake{IssueFunc: func(context.Context, github.Ref) (github.Issue, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.reads++
		if h.missing {
			return github.Issue{}, &github.Error{Category: github.NotFound}
		}
		v := h.issue
		v.Labels = slices.Clone(v.Labels)
		if v.Type != nil {
			copy := *v.Type
			v.Type = &copy
		}
		return v, nil
	}, PullRequestFunc: func(context.Context, github.Ref) (github.PullRequest, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.reads++
		return h.pr, nil
	}, ProjectItemsFunc: func(_ context.Context, content, p string, _ map[string]config.FieldKind) ([]github.ProjectItem, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := []github.ProjectItem{}
		for _, i := range h.items[p] {
			copy := i
			copy.Values = map[string]github.FieldValue{}
			for k, v := range i.Values {
				copy.Values[k] = v
			}
			out = append(out, copy)
		}
		return out, nil
	}}
	record := func(op string) error {
		h.ops = append(h.ops, op)
		if op == h.failAt {
			h.failAt = ""
			return &github.Error{Category: github.Transient}
		}
		return nil
	}
	h.mutator = &github.MutationFake{
		ResolveLabelsFunc: func(_ context.Context, _ github.Repository, names []string) ([]string, error) {
			return slices.Clone(names), nil
		},
		SetIssueTypeFunc: func(_ context.Context, id, typ string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if id != "I" {
				t.Error(id)
			}
			if err := record("type:" + typ); err != nil {
				return err
			}
			h.issue.Type = &github.IssueType{ID: typ, Name: stringsType(typ)}
			if h.cancel != nil {
				h.cancel()
			}
			return nil
		},
		AddLabelsFunc: func(_ context.Context, id string, labels []string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if err := record("add-labels:" + fmt.Sprint(labels)); err != nil {
				return err
			}
			if kind == "issue" {
				h.issue.Labels = append(h.issue.Labels, labels...)
			} else {
				h.pr.Labels = append(h.pr.Labels, labels...)
			}
			return nil
		},
		RemoveLabelsFunc: func(_ context.Context, id string, labels []string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if err := record("remove-labels:" + fmt.Sprint(labels)); err != nil {
				return err
			}
			remove := func(in []string) []string {
				return slices.DeleteFunc(in, func(l string) bool { return slices.Contains(labels, l) })
			}
			if kind == "issue" {
				h.issue.Labels = remove(h.issue.Labels)
			} else {
				h.pr.Labels = remove(h.pr.Labels)
			}
			return nil
		},
		AddProjectItemFunc: func(_ context.Context, p, id string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if err := record("add-project:" + p); err != nil {
				return "", err
			}
			item := github.ProjectItem{ID: p + "item", ProjectID: p, Content: &github.Content{ID: id, Kind: "Issue"}, Values: map[string]github.FieldValue{}}
			h.items[p] = []github.ProjectItem{item}
			return item.ID, nil
		},
		RemoveProjectItemFunc: func(_ context.Context, p, id string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if err := record("remove-project:" + p); err != nil {
				return err
			}
			if h.items[p][0].ID != id {
				t.Error("wrong item")
			}
			delete(h.items, p)
			return nil
		},
		SetProjectOptionFunc: func(_ context.Context, p, id, field, option string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if err := record("field:" + field + ":" + option); err != nil {
				return err
			}
			if h.items[p][0].ID != id {
				t.Error("wrong item")
			}
			h.items[p][0].Values[field] = github.FieldValue{Kind: config.SingleSelect, OptionID: option}
			return nil
		},
	}
	catalog, err := semantic.LoadCatalog(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	result := func(cap semantic.Capability) (semantic.Result, error) {
		h.calls++
		assets, err := catalog.Lookup(cap)
		if err != nil {
			return semantic.Result{}, err
		}
		raw := `{"labels":["workflow"]}`
		if cap == semantic.ClassifyIssue {
			raw = fmt.Sprintf(`{"type":%q,"labels":["workflow"],"priority":"High","effort":"S"}`, typ)
		}
		return semantic.Result{Output: json.RawMessage(raw), Provenance: semantic.Provenance{Capability: cap, Provider: "fake", Model: "cheap", PromptIdentity: assets.Prompt.Identity, PromptDigest: assets.Prompt.Digest, SchemaIdentity: assets.Schema.Identity, SchemaDigest: assets.Schema.Digest}}, nil
	}
	h.runner = &semantic.Fake{ClassifyIssueFunc: func(context.Context, semantic.Input) (semantic.Result, error) { return result(semantic.ClassifyIssue) }, ClassifyPRFunc: func(context.Context, semantic.Input) (semantic.Result, error) { return result(semantic.ClassifyPR) }}
	h.processor, err = observe.NewProcessor(h.d, h.client, sink{})
	if err != nil {
		t.Fatal(err)
	}
	h.r, err = New(h.d, h.processor, h.mutator)
	if err != nil {
		t.Fatal(err)
	}
	h.flow, err = semanticflow.New(semanticflow.Config{Store: h.store, Runner: h.runner, Primary: h.processor, Consumer: h.r, Clock: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	processor, err := observe.NewProcessor(h.d, h.client, h.flow)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := observe.NewResolver(h.d)
	if err != nil {
		t.Fatal(err)
	}
	h.w, err = worker.New(worker.Config{Store: h.store, Resolver: resolver, Processor: processor, Classifier: func(error) worker.Classification { return worker.Classification{Category: "semantic_reconcile"} }, Concurrency: 1, Clock: func() time.Time { return h.now }, RetrySchedule: func(_ worker.Classification, _ int64, n time.Time) time.Time { return n.Add(time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func stringsType(id string) string { return id[1:] }
func (h *harness) insert(id string) {
	h.t.Helper()
	event, key := "issues", "issue"
	if h.kind == "pull_request" {
		event, key = "pull_request", "pull_request"
	}
	_, err := h.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: event, ReceivedAt: h.now, Payload: []byte(fmt.Sprintf(`{"repository":{"owner":{"login":"parametron-io"},"name":"repo"},%q:{"number":28}}`, key))})
	if err != nil {
		h.t.Fatal(err)
	}
}
func (h *harness) step(id string) {
	h.t.Helper()
	h.insert(id)
	if err := h.w.Step(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	e, err := h.store.Event(context.Background(), id)
	if err != nil || e.State.Status != storage.Completed {
		h.t.Fatalf("%+v %v ops=%v", e.State, err, h.ops)
	}
}
func (h *harness) accepted() semanticflow.PolicyInput {
	h.t.Helper()
	h.insert("accepted")
	r := storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: h.kind, Number: 28}
	if err := h.store.BindResource(context.Background(), "accepted", r); err != nil {
		h.t.Fatal(err)
	}
	o, err := h.processor.ReadCurrent(context.Background(), r)
	if err != nil {
		h.t.Fatal(err)
	}
	in, err := h.flow.Enrich(context.Background(), observe.PolicyInput{DeliveryID: "accepted", Observed: o})
	if err != nil {
		h.t.Fatal(err)
	}
	return in
}
func (h *harness) item(p string, fields map[config.FieldRole]string) {
	binding := h.d.Engineering
	if p == "B" {
		binding = h.d.BugTracker
	}
	values := map[string]github.FieldValue{}
	for role, id := range fields {
		values[binding.Fields[role]] = github.FieldValue{Kind: config.SingleSelect, OptionID: id}
	}
	h.items[p] = []github.ProjectItem{{ID: p + "item", ProjectID: p, Content: &github.Content{ID: "I", Kind: "Issue"}, Values: values}}
}

func TestDesiredStates(t *testing.T) {
	for _, typ := range semanticpolicy.Types() {
		d, err := DesiredIssueState(semanticpolicy.IssueClassification{Type: typ, Priority: semanticpolicy.High, Effort: semanticpolicy.S, Labels: []semanticpolicy.Label{"docs", "workflow"}})
		want := config.Engineering
		if typ == semanticpolicy.Bug {
			want = config.BugTracker
		}
		if err != nil || d.ProjectProfile != want || !reflect.DeepEqual(d.ManagedLabels, []semanticpolicy.Label{"workflow", "docs"}) {
			t.Fatal(d, err)
		}
	}
	d, err := DesiredPRState(semanticpolicy.PRClassification{Labels: []semanticpolicy.Label{"docs", "workflow"}})
	if err != nil || !reflect.DeepEqual(d.ManagedLabels, []semanticpolicy.Label{"workflow", "docs"}) {
		t.Fatal(d, err)
	}
}
func TestPhase3IntegratedClosure(t *testing.T) {
	for _, typ := range semanticpolicy.Types() {
		t.Run(string(typ), func(t *testing.T) {
			h := newHarness(t, "issue", typ)
			other, target := "B", "E"
			if typ == semanticpolicy.Bug {
				other, target = "E", "B"
			}
			h.item(other, nil)
			h.step("initial")
			expected := []string{"type:T" + string(typ), "add-labels:[workflow]", "remove-labels:[docs]", "add-project:" + target, "field:" + target + "priority:" + target + "High", "field:" + target + "effort:" + target + "S", "field:" + target + "status:" + target + "backlog", "remove-project:" + other}
			if !reflect.DeepEqual(h.ops, expected) {
				t.Fatalf("%v", h.ops)
			}
			if !slices.Contains(h.issue.Labels, "dependencies") || !slices.Contains(h.issue.Labels, "custom-human-label") || slices.Contains(h.issue.Labels, "docs") {
				t.Fatal(h.issue.Labels)
			}
			if h.calls != 1 || h.reads != 3 {
				t.Fatal("missing fresh read", h.calls, h.reads)
			}
			if _, err := h.store.Provenance(context.Background(), semanticflow.Namespace, "parametron-io/repo/issue/28"); err != nil {
				t.Fatal(err)
			}
			n := len(h.ops)
			h.insert("initial")
			if err := h.w.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			h.step("later")
			if len(h.ops) != n || h.calls != 1 {
				t.Fatal("non-idempotent")
			}
			h.issue.Type = &github.IssueType{ID: "wrong", Name: string(typ)}
			h.issue.Labels = []string{"docs", "dependencies", "custom-human-label", "new-human"}
			delete(h.items, target)
			h.item(other, nil)
			h.step("drift")
			if h.calls != 1 || len(h.ops) != 2*n || !slices.Contains(h.issue.Labels, "new-human") {
				t.Fatal("drift did not converge", h.ops)
			}
			h.step("after-drift")
			if len(h.ops) != 2*n {
				t.Fatal("non-idempotent drift")
			}
			// Drift fields independently while preserving existing lifecycle Status.
			h.items[target][0].Values[target+"priority"] = github.FieldValue{Kind: config.SingleSelect, OptionID: "wrong"}
			delete(h.items[target][0].Values, target+"effort")
			h.items[target][0].Values[target+"status"] = github.FieldValue{Kind: config.SingleSelect, OptionID: target + "ready"}
			h.step("fields")
			if len(h.ops) != 2*n+2 || h.items[target][0].Values[target+"status"].OptionID != target+"ready" {
				t.Fatal("field/lifecycle boundary", h.ops)
			}
		})
	}
}
func TestPRLabelsOnly(t *testing.T) {
	h := newHarness(t, "pull_request", semanticpolicy.Task)
	h.step("pr")
	if !reflect.DeepEqual(h.ops, []string{"add-labels:[workflow]", "remove-labels:[docs]"}) {
		t.Fatal(h.ops)
	}
	h.step("later")
	if len(h.ops) != 2 || h.calls != 1 {
		t.Fatal(h.ops)
	}
}
func TestFreshDirectiveAndMissingGate(t *testing.T) {
	for _, body := range []string{"Classification: false", "Automation: false", "Classification: yes", "missing"} {
		t.Run(body, func(t *testing.T) {
			h := newHarness(t, "issue", semanticpolicy.Task)
			in := h.accepted()
			h.issue.Body = body
			if body == "missing" {
				h.missing = true
			}
			err := h.r.Evaluate(context.Background(), in)
			if body == "Classification: yes" {
				if !errors.Is(err, ErrReconcile) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(h.ops) != 0 {
				t.Fatal(h.ops)
			}
		})
	}
}
func TestClassificationGate(t *testing.T) {
	h := newHarness(t, "issue", semanticpolicy.Task)
	in := h.accepted()
	in.Current.Observed.Resource.Kind = "pull_request"
	if err := h.r.Evaluate(context.Background(), in); !errors.Is(err, ErrReconcile) {
		t.Fatal(err)
	}
	h.issue.Body = "Classification: false"
	o, _ := h.processor.ReadCurrent(context.Background(), storage.Resource{Owner: "parametron-io", Repository: "repo", Kind: "issue", Number: 28})
	disabled, err := h.flow.Enrich(context.Background(), observe.PolicyInput{Observed: o})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.r.Evaluate(context.Background(), disabled); err != nil {
		t.Fatal(err)
	}
	// Different resource has no durable completion; failed model output exposes Pending.
	o.Resource.Number = 29
	h.runner.ClassifyIssueFunc = func(context.Context, semantic.Input) (semantic.Result, error) {
		return semantic.Result{}, semantic.ErrProvider
	}
	h.issue.Body = ""
	o.Issue.Body = ""
	pending, err := h.flow.Enrich(context.Background(), observe.PolicyInput{Observed: o})
	if err == nil || pending.Classification.State() != semanticflow.Pending {
		t.Fatal(err)
	}
	if err = h.r.Evaluate(context.Background(), pending); !errors.Is(err, ErrReconcile) {
		t.Fatal(err)
	}
	if len(h.ops) != 0 {
		t.Fatal(h.ops)
	}
}
func TestMembershipAndIngress(t *testing.T) {
	for _, typ := range []semanticpolicy.IssueType{semanticpolicy.Task, semanticpolicy.Bug} {
		for _, status := range []string{"", "ready", "in_progress", "in_review", "done", "disabled"} {
			t.Run(string(typ)+status, func(t *testing.T) {
				h := newHarness(t, "issue", typ)
				target, other := "E", "B"
				if typ == semanticpolicy.Bug {
					target, other = "B", "E"
				}
				fields := map[config.FieldRole]string{config.Priority: target + "High", config.Effort: target + "S"}
				if status != "" && status != "disabled" {
					fields[config.Status] = target + status
				}
				h.item(target, fields)
				h.item(other, nil)
				h.issue.Type = &github.IssueType{ID: "T" + string(typ)}
				h.issue.Labels = []string{"workflow", "custom-human-label"}
				if status == "disabled" {
					h.issue.Body = "Set-Status: false"
				}
				h.step("membership")
				expected := []string{}
				if status == "" {
					expected = append(expected, "field:"+target+"status:"+target+"backlog")
				}
				expected = append(expected, "remove-project:"+other)
				if !reflect.DeepEqual(h.ops, expected) {
					t.Fatal(h.ops)
				}
			})
		}
	}
}
func TestDuplicateMembershipFailsBeforeWrites(t *testing.T) {
	h := newHarness(t, "issue", semanticpolicy.Task)
	in := h.accepted()
	h.item("E", nil)
	copy := h.items["E"][0]
	copy.ID = "duplicate"
	h.items["E"] = append(h.items["E"], copy)
	if err := h.r.Evaluate(context.Background(), in); !errors.Is(err, ErrReconcile) || len(h.ops) != 0 {
		t.Fatal(err, h.ops)
	}
}
func TestPartialFailureWorkerRetry(t *testing.T) {
	h := newHarness(t, "issue", semanticpolicy.Task)
	h.item("B", nil)
	h.failAt = "field:Eeffort:ES"
	h.insert("retry")
	if err := h.w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := h.store.Event(context.Background(), "retry")
	if e.State.Status != storage.Retryable {
		t.Fatal(e.State)
	}
	if len(h.items["E"]) != 1 || len(h.items["B"]) != 1 {
		t.Fatal("unsafe route")
	}
	n := len(h.ops)
	h.now = h.now.Add(2 * time.Second)
	if err := h.w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ = h.store.Event(context.Background(), "retry")
	if e.State.Status != storage.Completed || e.State.Attempts != 2 || h.calls != 1 {
		t.Fatal(e.State)
	}
	if !reflect.DeepEqual(h.ops[n:], []string{"field:Eeffort:ES", "field:Estatus:Ebacklog", "remove-project:B"}) {
		t.Fatal(h.ops)
	}
	if slices.Index(h.ops[n:], "type:TTask") >= 0 {
		t.Fatal("type replay")
	}
}
func TestCancellationBetweenWrites(t *testing.T) {
	h := newHarness(t, "issue", semanticpolicy.Task)
	in := h.accepted()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.cancel = cancel
	if err := h.r.Evaluate(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.ops, []string{"type:TTask"}) {
		t.Fatal(h.ops)
	}
}
func TestMissingLabelsFailBeforeWrites(t *testing.T) {
	h := newHarness(t, "issue", semanticpolicy.Task)
	in := h.accepted()
	h.mutator.ResolveLabelsFunc = func(context.Context, github.Repository, []string) ([]string, error) {
		return nil, &github.Error{Category: github.Permanent}
	}
	err := h.r.Evaluate(context.Background(), in)
	var ge *github.Error
	if !errors.As(err, &ge) || ge.Category != github.Permanent || len(h.ops) != 0 {
		t.Fatal(err, h.ops)
	}
}
func TestConstructionRequiresCanonicalBindings(t *testing.T) {
	for _, test := range []string{"type", "priority", "effort", "duplicate-type", "duplicate-option"} {
		t.Run(test, func(t *testing.T) {
			d := deployment()
			switch test {
			case "type":
				delete(d.IssueTypes, "Task")
			case "priority":
				delete(d.Engineering.FieldOptions[config.Priority], "High")
			case "effort":
				delete(d.BugTracker.FieldOptions[config.Effort], "Unknown")
			case "duplicate-type":
				d.IssueTypes["Task"] = d.IssueTypes["Bug"]
			case "duplicate-option":
				d.Engineering.FieldOptions[config.Priority]["Low"] = d.Engineering.FieldOptions[config.Priority]["High"]
			}
			if _, err := New(d, (*observe.Processor)(nil), &github.MutationFake{}); !errors.Is(err, ErrReconcile) {
				t.Fatal(err)
			}
			h := newHarness(t, "issue", semanticpolicy.Task)
			if _, err := New(d, h.processor, h.mutator); !errors.Is(err, ErrReconcile) {
				t.Fatal(err)
			}
		})
	}
}
func TestFreshMetadataIgnoresHandoff(t *testing.T) {
	h := newHarness(t, "issue", semanticpolicy.Task)
	in := h.accepted()
	h.issue.Type = &github.IssueType{ID: "TTask", Name: "Bug"}
	h.issue.Labels = []string{"workflow", "dependencies", "new-human"}
	h.item("E", map[config.FieldRole]string{config.Priority: "EHigh", config.Effort: "ES", config.Status: "Eready"})
	if err := h.r.Evaluate(context.Background(), in); err != nil || len(h.ops) != 0 {
		t.Fatal(err, h.ops)
	}
	h.issue.Type = &github.IssueType{ID: "TBug", Name: "Bug"}
	if err := h.r.Evaluate(context.Background(), in); err != nil || !reflect.DeepEqual(h.ops, []string{"type:TTask"}) {
		t.Fatal(err, h.ops)
	}
}

func TestConcurrentUnmanagedLabelsAndCanonicalDeltaOrder(t *testing.T) {
	h := newHarness(t, "pull_request", semanticpolicy.Task)
	in := h.accepted()
	h.pr.Labels = []string{"external", "docs", "ci", "dependencies", "custom-human-label"}
	add := h.mutator.AddLabelsFunc
	h.mutator.AddLabelsFunc = func(ctx context.Context, id string, labels []string) error {
		h.mu.Lock()
		h.pr.Labels = append(h.pr.Labels, "concurrent-human")
		h.mu.Unlock()
		return add(ctx, id, labels)
	}
	if err := h.r.Evaluate(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.ops, []string{"add-labels:[workflow]", "remove-labels:[docs ci external]"}) || !slices.Contains(h.pr.Labels, "concurrent-human") || !slices.Contains(h.pr.Labels, "dependencies") {
		t.Fatal(h.ops, h.pr.Labels)
	}
}
func TestAcceptedPRKindMismatch(t *testing.T) {
	h := newHarness(t, "pull_request", semanticpolicy.Task)
	in := h.accepted()
	in.Current.Observed.Resource.Kind = "issue"
	if err := h.r.Evaluate(context.Background(), in); !errors.Is(err, ErrReconcile) || len(h.ops) != 0 {
		t.Fatal(err, h.ops)
	}
}
func TestReconcilerOwnsNestedConfiguration(t *testing.T) {
	h := newHarness(t, "issue", semanticpolicy.Task)
	in := h.accepted()
	h.d.IssueTypes["Task"] = "changed"
	h.d.Engineering.FieldOptions[config.Priority]["High"] = "changed"
	h.d.Engineering.Fields[config.Priority] = "changed"
	if err := h.r.Evaluate(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.ops, "type:TTask") || !slices.Contains(h.ops, "field:Epriority:EHigh") {
		t.Fatal(h.ops)
	}
}

func TestNewIngressWithStatusDisabled(t *testing.T) {
	for _, typ := range []semanticpolicy.IssueType{semanticpolicy.Task, semanticpolicy.Bug} {
		t.Run(string(typ), func(t *testing.T) {
			h := newHarness(t, "issue", typ)
			h.issue.Body = "Set-Status: false"
			h.step("no-status")
			p := "E"
			if typ == semanticpolicy.Bug {
				p = "B"
			}
			if _, ok := h.items[p][0].Values[p+"status"]; ok {
				t.Fatal("status owned despite directive")
			}
			if len(h.ops) != 6 {
				t.Fatal(h.ops)
			}
		})
	}
}
func TestUnrelatedProjectsAndNonOwnedFieldsSurvive(t *testing.T) {
	h := newHarness(t, "issue", semanticpolicy.Task)
	h.item("E", map[config.FieldRole]string{config.Priority: "EHigh", config.Effort: "ES", config.Status: "Eready"})
	h.items["E"][0].Values["Eestimate"] = github.FieldValue{Kind: config.Number, Number: 8}
	h.items["E"][0].Values["Estart_date"] = github.FieldValue{Kind: config.Date, Date: "2026-10-10"}
	h.items["unrelated"] = []github.ProjectItem{{ID: "U", ProjectID: "unrelated"}}
	h.step("preserved")
	if len(h.items["unrelated"]) != 1 || h.items["E"][0].Values["Eestimate"].Number != 8 || h.items["E"][0].Values["Estart_date"].Date != "2026-10-10" {
		t.Fatal("non-owned state changed")
	}
	pr := newHarness(t, "pull_request", semanticpolicy.Task)
	pr.items["E"] = []github.ProjectItem{{ID: "PRITEM", ProjectID: "E", Content: &github.Content{ID: "I", Kind: "PullRequest"}, Values: map[string]github.FieldValue{}}}
	pr.step("pr-project")
	if len(pr.ops) != 2 || len(pr.items["E"]) != 1 {
		t.Fatal(pr.ops, pr.items)
	}
}
