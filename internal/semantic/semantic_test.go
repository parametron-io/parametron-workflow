package semantic_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/build"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/semantic"
)

var capabilities = []semantic.Capability{semantic.ClassifyIssue, semantic.ClassifyPR, semantic.EstimateIssue}

type fakeProvider struct {
	execute func(context.Context, semantic.ExecutionRequest) (json.RawMessage, error)
}

func (f *fakeProvider) Execute(ctx context.Context, r semantic.ExecutionRequest) (json.RawMessage, error) {
	return f.execute(ctx, r)
}
func catalog(t *testing.T) semantic.Catalog {
	t.Helper()
	c, err := semantic.LoadCatalog(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func selection() semantic.DeploymentConfig {
	return semantic.DeploymentConfig{Cheap: semantic.Selection{Provider: "fake", Model: "cheap/model-v1"}}
}
func runner(t *testing.T, p semantic.Provider, timeout time.Duration) semantic.Runner {
	t.Helper()
	r, err := semantic.NewRunner(selection(), catalog(t), map[string]semantic.Provider{"fake": p}, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestConfig(t *testing.T) {
	valid := `{"cheap":{"provider":"fake","model":"model"}}`
	first, err := semantic.ParseConfig(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	second, err := semantic.ParseConfig(strings.NewReader(valid))
	if err != nil || first != second {
		t.Fatal("nondeterministic parse")
	}
	for _, s := range []string{
		`{"cheap":{"provider":"","model":"model"}}`,
		`{"cheap":{"provider":"fake","model":" "}}`,
		`{"cheap":{"provider":"fake"}}`, `{}`, `null`, `[]`,
		`{"cheap":{"provider":"fake","model":"model","api_key":"SECRET"}}`,
		`{"cheap":{"provider":"fake","model":"model"},"organization":"SECRET"}`,
		`{"cheap":{"provider":"fake","provider":"other","model":"model"}}`,
		`{"cheap":{"provider":"fake","\u0070rovider":"other","model":"model"}}`,
		`{"cheap":{"provider":"fake","model":"model"},"cheap":{"provider":"fake","model":"model"}}`,
		valid + ` {}`, `{"SECRET":`,
	} {
		t.Run(s, func(t *testing.T) {
			_, err := semantic.ParseConfig(strings.NewReader(s))
			if !errors.Is(err, semantic.ErrConfiguration) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
		})
	}
	for _, c := range []semantic.DeploymentConfig{{}, {Cheap: semantic.Selection{Provider: " ", Model: "m"}}, {Cheap: semantic.Selection{Provider: "p", Model: ""}}, {Cheap: semantic.Selection{Provider: strings.Repeat("p", 129), Model: "m"}}} {
		if !errors.Is(c.Validate(), semantic.ErrConfiguration) {
			t.Fatal("invalid direct configuration")
		}
	}
}
func TestConstruction(t *testing.T) {
	p := &fakeProvider{}
	for _, tc := range []struct {
		config    semantic.DeploymentConfig
		providers map[string]semantic.Provider
		timeout   time.Duration
		catalog   semantic.Catalog
		want      error
	}{
		{selection(), nil, time.Second, catalog(t), semantic.ErrConfiguration},
		{selection(), map[string]semantic.Provider{"other": p}, time.Second, catalog(t), semantic.ErrConfiguration},
		{selection(), map[string]semantic.Provider{"fake": (*fakeProvider)(nil)}, time.Second, catalog(t), semantic.ErrConfiguration},
		{selection(), map[string]semantic.Provider{"fake": p}, 0, catalog(t), semantic.ErrConfiguration},
		{selection(), map[string]semantic.Provider{"fake": p}, -1, catalog(t), semantic.ErrConfiguration},
		{semantic.DeploymentConfig{}, map[string]semantic.Provider{"fake": p}, time.Second, catalog(t), semantic.ErrConfiguration},
		{selection(), map[string]semantic.Provider{"fake": p}, time.Second, semantic.Catalog{}, semantic.ErrAssets},
	} {
		if _, err := semantic.NewRunner(tc.config, tc.catalog, tc.providers, tc.timeout); !errors.Is(err, tc.want) {
			t.Fatalf("got %v want %v", err, tc.want)
		}
	}
}
func TestExecutionAndProvenance(t *testing.T) {
	for _, c := range capabilities {
		t.Run(string(c), func(t *testing.T) {
			input := semantic.Input{Title: "title SECRET", Body: "body SECRET"}
			output := json.RawMessage(`{"type":"outside-policy","estimate":999,"unknown":"SECRET"}`)
			calls := 0
			p := &fakeProvider{execute: func(ctx context.Context, r semantic.ExecutionRequest) (json.RawMessage, error) {
				calls++
				if r.Capability != c || r.Model != selection().Cheap.Model || r.Input != input {
					t.Fatalf("wrong envelope: %+v", r)
				}
				a, err := catalog(t).Lookup(c)
				if err != nil || r.Prompt != a.Prompt || r.Schema != a.Schema {
					t.Fatal("wrong assets")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing deadline")
				}
				return output, nil
			}}
			providers := map[string]semantic.Provider{"fake": p, "other": &fakeProvider{execute: func(context.Context, semantic.ExecutionRequest) (json.RawMessage, error) {
				t.Fatal("wrong provider")
				return nil, nil
			}}}
			r, err := semantic.NewRunner(selection(), catalog(t), providers, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			delete(providers, "fake") // construction retains the selected adapter
			result, err := r.Run(context.Background(), semantic.Request{Capability: c, Input: input})
			if err != nil || calls != 1 || string(result.Output) != string(output) {
				t.Fatalf("result %v, calls %d", err, calls)
			}
			a, _ := catalog(t).Lookup(c)
			want := semantic.Provenance{Capability: c, Provider: "fake", Model: "cheap/model-v1", PromptIdentity: "prompts/cheap/" + string(c) + ".txt", PromptDigest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(a.Prompt.Content))), SchemaIdentity: "schemas/model/" + string(c) + ".json", SchemaDigest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(a.Schema.Content)))}
			if result.Provenance != want || strings.Contains(fmt.Sprintf("%+v", result.Provenance), "SECRET") {
				t.Fatal("wrong or unsafe provenance")
			}
			output[0] = 'X'
			if result.Output[0] != '{' {
				t.Fatal("output aliases provider buffer")
			}
		})
	}
}
func TestCapabilitiesAndResponses(t *testing.T) {
	calls := 0
	response := json.RawMessage(`{}`)
	r := runner(t, &fakeProvider{execute: func(context.Context, semantic.ExecutionRequest) (json.RawMessage, error) {
		calls++
		return response, nil
	}}, time.Second)
	for _, c := range []semantic.Capability{"", "unknown", "triage", "audit", "validation", "review", "reproduce", "fix"} {
		if _, err := r.Run(context.Background(), semantic.Request{Capability: c}); !errors.Is(err, semantic.ErrCapability) {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("unsupported capability invoked provider")
	}
	if _, err := r.Run(nil, semantic.Request{Capability: semantic.ClassifyIssue}); !errors.Is(err, semantic.ErrRequest) {
		t.Fatal(err)
	}
	for _, s := range []string{"", " ", "prose", "{", "{} {}", "{} trailing", "null", "[]", "42", `"text"`} {
		response = json.RawMessage(s)
		result, err := r.Run(context.Background(), semantic.Request{Capability: semantic.ClassifyIssue})
		if !errors.Is(err, semantic.ErrResponse) || !reflect.DeepEqual(result, semantic.Result{}) {
			t.Fatalf("accepted %q: %v", s, err)
		}
	}
	response = json.RawMessage(` {"estimate":999} `)
	if _, err := r.Run(context.Background(), semantic.Request{Capability: semantic.EstimateIssue}); err != nil {
		t.Fatal("policy validation was introduced", err)
	}
}
func TestCancellation(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := runner(t, &fakeProvider{execute: func(context.Context, semantic.ExecutionRequest) (json.RawMessage, error) { calls++; return nil, nil }}, time.Second)
	_, err := r.Run(ctx, semantic.Request{Capability: semantic.ClassifyIssue})
	if calls != 0 || !errors.Is(err, semantic.ErrCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatal("already cancelled", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	r = runner(t, &fakeProvider{execute: func(derived context.Context, _ semantic.ExecutionRequest) (json.RawMessage, error) {
		calls++
		cancel()
		<-derived.Done()
		return json.RawMessage(`{}`), nil
	}}, time.Second)
	_, err = r.Run(ctx, semantic.Request{Capability: semantic.ClassifyIssue})
	if calls != 1 || !errors.Is(err, semantic.ErrCancelled) {
		t.Fatal("in-flight cancellation", err)
	}
}
func TestTimeout(t *testing.T) {
	calls := 0
	timeout := 10 * time.Millisecond
	r := runner(t, &fakeProvider{execute: func(ctx context.Context, _ semantic.ExecutionRequest) (json.RawMessage, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > timeout {
			t.Fatal("derived deadline missing")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}, timeout)
	_, err := r.Run(context.Background(), semantic.Request{Capability: semantic.EstimateIssue})
	if calls != 1 || !errors.Is(err, semantic.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err, calls)
	}
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = r.Run(parent, semantic.Request{Capability: semantic.EstimateIssue})
	if calls != 1 || !errors.Is(err, semantic.ErrTimeout) {
		t.Fatal("parent deadline", err)
	}
	parent, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	parentDeadline, _ := parent.Deadline()
	r = runner(t, &fakeProvider{execute: func(ctx context.Context, _ semantic.ExecutionRequest) (json.RawMessage, error) {
		calls++
		deadline, _ := ctx.Deadline()
		if !deadline.Equal(parentDeadline) {
			t.Fatal("earlier parent deadline not preserved")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}, time.Second)
	_, err = r.Run(parent, semantic.Request{Capability: semantic.ClassifyPR})
	if calls != 2 || !errors.Is(err, semantic.ErrTimeout) {
		t.Fatal("in-flight parent deadline", err)
	}
}
func TestProviderFailures(t *testing.T) {
	sensitive := errors.New("SECRET rate limited transient credentials raw body")
	for _, kind := range []semantic.ProviderKind{semantic.RateLimited, semantic.Transient, semantic.Permanent, semantic.ProviderUnknown, "SECRET"} {
		underlying := &semantic.ProviderError{Kind: kind, Cause: sensitive}
		calls := 0
		r := runner(t, &fakeProvider{execute: func(context.Context, semantic.ExecutionRequest) (json.RawMessage, error) {
			calls++
			return nil, underlying
		}}, time.Second)
		_, err := r.Run(context.Background(), semantic.Request{Capability: semantic.ClassifyPR})
		var execution *semantic.ExecutionError
		var provider *semantic.ProviderError
		want := kind
		if kind == "SECRET" {
			want = semantic.ProviderUnknown
		}
		if calls != 1 || !errors.Is(err, semantic.ErrProvider) || !errors.As(err, &execution) || execution.Kind != want || !errors.As(err, &provider) || provider != underlying || !errors.Is(err, sensitive) {
			t.Fatal("lost category/cause", err)
		}
		if strings.Contains(fmt.Sprintf("%v %+v %s", err, err, err), "SECRET") || strings.Contains(underlying.Error(), "SECRET") {
			t.Fatal("unsafe error")
		}
	}
	r := runner(t, &fakeProvider{execute: func(context.Context, semantic.ExecutionRequest) (json.RawMessage, error) { return nil, sensitive }}, time.Second)
	_, err := r.Run(context.Background(), semantic.Request{Capability: semantic.ClassifyIssue})
	var execution *semantic.ExecutionError
	if !errors.As(err, &execution) || execution.Kind != semantic.ProviderUnknown || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("substring classification or leak")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		r = runner(t, &fakeProvider{execute: func(context.Context, semantic.ExecutionRequest) (json.RawMessage, error) {
			return nil, fmt.Errorf("SECRET: %w", cause)
		}}, time.Second)
		_, err = r.Run(context.Background(), semantic.Request{Capability: semantic.ClassifyIssue})
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "SECRET") {
			t.Fatal(err)
		}
	}
}
func TestCatalog(t *testing.T) {
	c := catalog(t)
	fixtures := fstest.MapFS{}
	for _, capability := range capabilities {
		a, err := c.Lookup(capability)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := c.Lookup(capability)
		if a != again {
			t.Fatal("unstable catalog")
		}
		if a.Prompt.Digest != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(a.Prompt.Content))) || a.Schema.Digest != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(a.Schema.Content))) || !json.Valid([]byte(a.Schema.Content)) {
			t.Fatal("invalid digest/schema")
		}
		var schema struct {
			Dialect              string                     `json:"$schema"`
			Type                 string                     `json:"type"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties bool                       `json:"additionalProperties"`
		}
		if err := json.Unmarshal([]byte(a.Schema.Content), &schema); err != nil {
			t.Fatal(err)
		}
		fields := map[semantic.Capability][]string{
			semantic.ClassifyIssue: {"type", "labels", "priority", "effort"},
			semantic.ClassifyPR:    {"labels"},
			semantic.EstimateIssue: {"estimate"},
		}[capability]
		if schema.Dialect != "https://json-schema.org/draft/2020-12/schema" || schema.Type != "object" || schema.AdditionalProperties || !reflect.DeepEqual(schema.Required, fields) || len(schema.Properties) != len(fields) {
			t.Fatal("wrong execution schema shape")
		}
		for _, field := range fields {
			if !json.Valid(schema.Properties[field]) {
				t.Fatal("missing field schema", field)
			}
		}
		fixtures[a.Prompt.Identity] = &fstest.MapFile{Data: []byte(a.Prompt.Content)}
		fixtures[a.Schema.Identity] = &fstest.MapFile{Data: []byte(a.Schema.Content)}
	}
	names := make([]string, 0, len(fixtures))
	for name := range fixtures {
		names = append(names, name)
	}
	for _, name := range names {
		file := fixtures[name]
		delete(fixtures, name)
		if _, err := semantic.LoadCatalog(fixtures); !errors.Is(err, semantic.ErrAssets) {
			t.Fatal("missing asset accepted", name)
		}
		fixtures[name] = &fstest.MapFile{Data: []byte(" ")}
		if _, err := semantic.LoadCatalog(fixtures); !errors.Is(err, semantic.ErrAssets) {
			t.Fatal("invalid asset accepted", name)
		}
		fixtures[name] = file
	}
	if _, err := semantic.LoadCatalog(nil); !errors.Is(err, semantic.ErrAssets) {
		t.Fatal(err)
	}
	if _, err := c.Lookup("triage"); !errors.Is(err, semantic.ErrCapability) {
		t.Fatal(err)
	}
	if _, err := (semantic.Catalog{}).Lookup(semantic.ClassifyPR); !errors.Is(err, semantic.ErrAssets) {
		t.Fatal(err)
	}
}
func TestFake(t *testing.T) {
	var recorded []semantic.Request
	fn := func(c semantic.Capability) func(context.Context, semantic.Input) (semantic.Result, error) {
		return func(_ context.Context, input semantic.Input) (semantic.Result, error) {
			recorded = append(recorded, semantic.Request{Capability: c, Input: input})
			return semantic.Result{Output: json.RawMessage(`{"fixture":true}`)}, nil
		}
	}
	f := &semantic.Fake{ClassifyIssueFunc: fn(semantic.ClassifyIssue), ClassifyPRFunc: fn(semantic.ClassifyPR), EstimateIssueFunc: fn(semantic.EstimateIssue)}
	for _, c := range capabilities {
		request := semantic.Request{Capability: c, Input: semantic.Input{Title: string(c)}}
		result, err := f.Run(context.Background(), request)
		if err != nil || string(result.Output) != `{"fixture":true}` || recorded[len(recorded)-1] != request {
			t.Fatal("fake mismatch")
		}
		if _, err := (&semantic.Fake{}).Run(context.Background(), request); !errors.Is(err, semantic.ErrUnconfigured) {
			t.Fatal(err)
		}
	}
	if len(recorded) != 3 {
		t.Fatal(recorded)
	}
	if _, err := f.Run(context.Background(), semantic.Request{Capability: "review"}); !errors.Is(err, semantic.ErrCapability) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Run(ctx, semantic.Request{Capability: semantic.ClassifyIssue}); !errors.Is(err, semantic.ErrCancelled) || len(recorded) != 3 {
		t.Fatal(err)
	}
}

// Inspect Go's package metadata rather than matching source text. The execution
// boundary imports no repository package or external dependency.
func TestStandaloneBoundary(t *testing.T) {
	p, err := build.Default.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range p.Imports {
		if strings.Contains(strings.Split(path, "/")[0], ".") {
			t.Fatalf("non-standard dependency: %s", path)
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(semantic.Request{}), reflect.TypeOf(semantic.Input{}), reflect.TypeOf(semantic.DeploymentConfig{}), reflect.TypeOf(semantic.Selection{})} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			switch field.Name {
			case "Capability", "Input", "Title", "Body", "Context", "Cheap", "Provider", "Model":
			default:
				t.Fatalf("unexpected envelope field %s", field.Name)
			}
		}
	}
}

func TestStructuredContext(t *testing.T) {
	input := semantic.Input{Title: "title", Body: "body", Context: semantic.ContextJSON(`{"type":"Task","labels":[]}`)}
	raw, err := json.Marshal(input)
	if err != nil || string(raw) != `{"title":"title","body":"body","context":{"type":"Task","labels":[]}}` {
		t.Fatal(string(raw), err)
	}
	for _, value := range []string{"", "null", "[]", "true", "{} {}", "SECRET"} {
		_, err := json.Marshal(semantic.ContextJSON(value))
		if !errors.Is(err, semantic.ErrRequest) || strings.Contains(err.Error(), "SECRET") {
			t.Fatal(err)
		}
	}
	calls := 0
	r := runner(t, &fakeProvider{execute: func(_ context.Context, request semantic.ExecutionRequest) (json.RawMessage, error) {
		calls++
		if request.Input != input {
			t.Fatal("context changed")
		}
		return json.RawMessage(`{}`), nil
	}}, time.Second)
	if _, err := r.Run(context.Background(), semantic.Request{Capability: semantic.EstimateIssue, Input: input}); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
}

func TestAssetContentIdentity(t *testing.T) {
	original := catalog(t)
	fixtures := fstest.MapFS{}
	for _, capability := range capabilities {
		a, _ := original.Lookup(capability)
		fixtures[a.Prompt.Identity] = &fstest.MapFile{Data: []byte(a.Prompt.Content)}
		fixtures[a.Schema.Identity] = &fstest.MapFile{Data: []byte(a.Schema.Content)}
	}
	same, err := semantic.LoadCatalog(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range capabilities {
		before, _ := original.Lookup(capability)
		after, _ := same.Lookup(capability)
		if before != after {
			t.Fatal("identical bytes changed identity")
		}
	}
	for _, capability := range capabilities {
		before, _ := original.Lookup(capability)
		for _, selected := range []semantic.Asset{before.Prompt, before.Schema} {
			saved := fixtures[selected.Identity]
			fixtures[selected.Identity] = &fstest.MapFile{Data: []byte(selected.Content + "\n")}
			changed, err := semantic.LoadCatalog(fixtures)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := changed.Lookup(capability)
			got, other := after.Prompt, after.Schema
			if selected.Identity == before.Schema.Identity {
				got, other = after.Schema, after.Prompt
			}
			if got.Identity != selected.Identity || got.Digest == selected.Digest || got.Digest != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(got.Content))) {
				t.Fatal("changed bytes not reflected in digest")
			}
			expectedOther := before.Schema
			if selected.Identity == before.Schema.Identity {
				expectedOther = before.Prompt
			}
			if other != expectedOther {
				t.Fatal("unrelated asset changed")
			}
			unchanged, _ := original.Lookup(capability)
			if unchanged != before {
				t.Fatal("catalog snapshot changed")
			}
			fixtures[selected.Identity] = saved
		}
	}
}
