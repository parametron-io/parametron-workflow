package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type semanticConsumerFunc func(context.Context, semanticflow.PolicyInput) error

func (f semanticConsumerFunc) Evaluate(ctx context.Context, in semanticflow.PolicyInput) error {
	return f(ctx, in)
}

func semanticFixture(t *testing.T) Config {
	c, f := fixture(t)
	source, schema := preparationFixture()
	source.Organization = "parametron-io"
	schema.Organizations[0].Login = source.Organization
	schema.Repositories[0].Owner = source.Organization
	for i := range schema.Projects {
		schema.Projects[i].Owner = source.Organization
	}
	c.Source = source
	f.DiscoverSchemaFunc = func(context.Context, config.SourceConfig) (config.Schema, error) { return schema, nil }
	f.IssueFunc = func(_ context.Context, ref github.Ref) (github.Issue, error) {
		return github.Issue{Identity: github.Identity{ID: "current", Repository: schema.Repositories[0], Number: ref.Number}, Title: "current title", Body: "Classification: true"}, nil
	}
	return c
}

func TestExplicitRunnerActivatesDurableSemanticComposition(t *testing.T) {
	c := semanticFixture(t)
	catalog, err := semantic.LoadCatalog(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	assets, err := catalog.Lookup(semantic.ClassifyIssue)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.Runner = &semantic.Fake{ClassifyIssueFunc: func(_ context.Context, in semantic.Input) (semantic.Result, error) {
		calls++
		if in.Title != "current title" || in.Body != "Classification: true" {
			t.Error("historical semantic input")
		}
		return semantic.Result{Output: json.RawMessage(`{"type":"Task","labels":[],"priority":"Medium","effort":"Unknown"}`), Provenance: semantic.Provenance{Capability: semantic.ClassifyIssue, Provider: "fake", Model: "cheap", PromptIdentity: assets.Prompt.Identity, PromptDigest: assets.Prompt.Digest, SchemaIdentity: assets.Schema.Identity, SchemaDigest: assets.Schema.Digest}}, nil
	}}
	var inputs []semanticflow.PolicyInput
	c.SemanticConsumer = semanticConsumerFunc(func(_ context.Context, in semanticflow.PolicyInput) error { inputs = append(inputs, in); return nil })
	r := openRuntime(t, c)
	insert := func(id string) {
		t.Helper()
		_, err := r.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: "issues", Payload: []byte(`{"repository":{"owner":{"login":"parametron-io"},"name":"repo"},"issue":{"number":27,"title":"historical"}}`), ReceivedAt: time.Unix(1700000000, 0)})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("semantic")
	step(t, r)
	state(t, r, "semantic", storage.Completed, 1)
	if calls != 1 || len(inputs) != 1 || inputs[0].Classification.State() != semanticflow.Accepted {
		t.Fatal("semantic composition inactive")
	}
	if _, err := r.store.Provenance(context.Background(), semanticflow.Namespace, "parametron-io/repo/issue/27"); err != nil {
		t.Fatal(err)
	}
	// Same composition, provider unavailable: durable completion must avoid Runner.
	c.Runner.(*semantic.Fake).ClassifyIssueFunc = func(context.Context, semantic.Input) (semantic.Result, error) {
		t.Error("accepted completion called unavailable provider")
		return semantic.Result{}, errors.New("provider unavailable")
	}
	insert("later-semantic")
	step(t, r)
	state(t, r, "later-semantic", storage.Completed, 1)
	if inputs[1].Classification.State() != semanticflow.Accepted || calls != 1 {
		t.Fatal("accepted result not reused")
	}
}

func TestSemanticLocalWorkerTaxonomy(t *testing.T) {
	for _, tc := range []struct {
		err   error
		cat   string
		retry bool
	}{
		{semanticflow.ErrStale, "semantic_stale", true},
		{semanticflow.ErrCompletion, "semantic_completion", false},
		{semanticflow.ErrIntent, "semantic_intent", false},
		{semanticflow.ErrPersistence, "semantic_persistence", false},
		{semanticflow.ErrPolicy, "semantic_policy", false},
	} {
		got := classify(tc.err)
		if got.Category != tc.cat || got.Retryable != tc.retry {
			t.Fatal(got)
		}
	}
}
