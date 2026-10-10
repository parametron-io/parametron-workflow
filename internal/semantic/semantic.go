// Package semantic provides bounded cheap execution, not semantic policy or
// workflow authority. It depends only on the standard library.
package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

type Capability string

const (
	ClassifyIssue Capability = "classify_issue"
	ClassifyPR    Capability = "classify_pr"
	EstimateIssue Capability = "estimate_issue"
)

func supported(c Capability) bool {
	return c == ClassifyIssue || c == ClassifyPR || c == EstimateIssue
}

// ContextJSON is a serialized structured context object produced by Go policy.
// It carries semantic data only, never execution settings or authority.
type ContextJSON string

// MarshalJSON preserves context as an object, rather than quoting it as prose.
func (c ContextJSON) MarshalJSON() ([]byte, error) {
	if !json.Valid([]byte(c)) {
		return nil, ErrRequest
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(c), &object) != nil || object == nil {
		return nil, ErrRequest
	}
	return []byte(c), nil
}

// Input contains semantic content only. Policy supplies normalized planning
// context and owns eligibility. No transport, secrets, callbacks or provider payloads
// belong here; callers must select content rather than pass runtime state.
type Input struct {
	Title   string      `json:"title"`
	Body    string      `json:"body"`
	Context ContextJSON `json:"context,omitempty"`
}
type Request struct {
	Capability Capability
	Input      Input
}

// Result is untrusted JSON for later Go-owned schema/allowlist validation.
type Result struct {
	Output     json.RawMessage
	Provenance Provenance
}
type Provenance struct {
	Capability     Capability
	Provider       string
	Model          string
	PromptIdentity string
	PromptDigest   string
	SchemaIdentity string
	SchemaDigest   string
}
type Runner interface {
	Run(context.Context, Request) (Result, error)
}

// ExecutionRequest is constructed by the runner, never by workflow policy.
// Adapter secrets belong to adapter construction, not this envelope.
type ExecutionRequest struct {
	Capability Capability
	Model      string
	Prompt     Asset
	Schema     Asset
	Input      Input
}

// Provider must respect context and perform exactly one attempt, without retries
// or fallback. Runner does not spawn a goroutine to force adapter cancellation.
type Provider interface {
	Execute(context.Context, ExecutionRequest) (json.RawMessage, error)
}

type configuredRunner struct {
	selection Selection
	catalog   Catalog
	provider  Provider
	timeout   time.Duration
}

// NewRunner resolves a single cheap provider/model at construction. It snapshots
// catalog data and retains only the selected adapter, not the registry map.
func NewRunner(config DeploymentConfig, catalog Catalog, providers map[string]Provider, timeout time.Duration) (Runner, error) {
	if config.Validate() != nil || timeout <= 0 {
		return nil, ErrConfiguration
	}
	p := providers[config.Cheap.Provider]
	if nilProvider(p) {
		return nil, ErrConfiguration
	}
	for _, c := range capabilities() {
		if _, err := catalog.Lookup(c); err != nil {
			return nil, err
		}
	}
	snapshot := Catalog{assets: make(map[Capability]Assets)}
	for _, c := range capabilities() {
		snapshot.assets[c] = catalog.assets[c]
	}
	return &configuredRunner{config.Cheap, snapshot, p, timeout}, nil
}
func nilProvider(p Provider) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface:
		return v.IsNil()
	}
	return false
}
func (r *configuredRunner) Run(parent context.Context, request Request) (Result, error) {
	if parent == nil {
		return Result{}, ErrRequest
	}
	if !supported(request.Capability) {
		return Result{}, ErrCapability
	}
	if err := contextFailure(parent.Err()); err != nil {
		return Result{}, err
	}
	assets, err := r.catalog.Lookup(request.Capability)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	if err := contextFailure(ctx.Err()); err != nil {
		return Result{}, err
	}
	output, err := r.provider.Execute(ctx, ExecutionRequest{request.Capability, r.selection.Model, assets.Prompt, assets.Schema, request.Input})
	// Context state takes precedence even if an adapter returns a late success.
	if failure := contextFailure(parent.Err()); failure != nil {
		return Result{}, failure
	}
	if failure := contextFailure(ctx.Err()); failure != nil {
		return Result{}, failure
	}
	if err != nil {
		if failure := contextFailure(err); failure != nil {
			return Result{}, failure
		}
		kind := ProviderUnknown
		var pe *ProviderError
		if errors.As(err, &pe) {
			kind = normalizedKind(pe.Kind)
		}
		return Result{}, &ExecutionError{Kind: kind, cause: err}
	}
	// JSON object syntax only; schemas and semantic values remain #26's job.
	if !json.Valid(output) {
		return Result{}, ErrResponse
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(output, &object) != nil || object == nil {
		return Result{}, ErrResponse
	}
	return Result{append(json.RawMessage(nil), output...), Provenance{
		request.Capability, r.selection.Provider, r.selection.Model,
		assets.Prompt.Identity, assets.Prompt.Digest, assets.Schema.Identity, assets.Schema.Digest,
	}}, nil
}
