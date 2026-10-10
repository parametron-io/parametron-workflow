package semanticflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type providerFunc func(context.Context, semantic.ExecutionRequest) (json.RawMessage, error)

func (f providerFunc) Execute(ctx context.Context, r semantic.ExecutionRequest) (json.RawMessage, error) {
	return f(ctx, r)
}

func TestConfiguredRunnerErrorsUnderWorker(t *testing.T) {
	for _, tc := range []struct {
		name, category string
		kind           semantic.ProviderKind
		timeout        bool
		retry          bool
	}{
		{"transient", "semantic_transient", semantic.Transient, false, true},
		{"rate", "semantic_rate_limited", semantic.RateLimited, false, true},
		{"permanent", "semantic_permanent", semantic.Permanent, false, false},
		{"unknown", "semantic_provider_unknown", semantic.ProviderUnknown, false, false},
		{"timeout", "semantic_timeout", semantic.ProviderUnknown, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "issue")
			catalog, err := semantic.LoadCatalog(os.DirFS("../.."))
			if err != nil {
				t.Fatal(err)
			}
			provider := providerFunc(func(ctx context.Context, r semantic.ExecutionRequest) (json.RawMessage, error) {
				h.calls.Add(1)
				if r.Capability != semantic.ClassifyIssue || r.Input.Title != "title A" {
					t.Error("incorrect semantic request")
				}
				if tc.timeout {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				// A nested GitHub error must never hijack semantic failure classification.
				return nil, &semantic.ProviderError{Kind: tc.kind, Cause: errors.Join(errors.New("RAW SECRET"), &github.Error{Category: github.Transient})}
			})
			timeout := 10 * time.Second
			if tc.timeout {
				timeout = 10 * time.Millisecond
			}
			h.runner, err = semantic.NewRunner(semantic.DeploymentConfig{Cheap: semantic.Selection{Provider: "fake", Model: "cheap"}}, catalog, map[string]semantic.Provider{"fake": provider}, timeout)
			if err != nil {
				t.Fatal(err)
			}
			h.compose()
			h.step()
			status := storage.Failed
			if tc.retry {
				status = storage.Retryable
			}
			h.state("initial", status, tc.category)
			h.noCompletion("issue")
			if h.calls.Load() != 1 || len(h.inputs) != 0 {
				t.Fatal("retry/failed acceptance")
			}
		})
	}
}
