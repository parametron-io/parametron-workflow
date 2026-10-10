package semanticflow

import (
	"context"
	"errors"

	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// Failure contains only a code-owned identifier and retry decision. Provider and
// database diagnostics are deliberately not retained or unwrapped here.
type Failure struct {
	category  string
	retryable bool
}

func (f *Failure) Error() string { return f.category }

var (
	ErrStale       = &Failure{"semantic_stale", true}
	ErrIntent      = &Failure{"semantic_intent", false}
	ErrCompletion  = &Failure{"semantic_completion", false}
	ErrPersistence = &Failure{"semantic_persistence", false}
	ErrPolicy      = &Failure{"semantic_policy", false}
)

// FailureCategory supplies the worker-local taxonomy without coupling semantic
// execution or policy to worker/storage/app.
func FailureCategory(err error) (retryable bool, category string, ok bool) {
	var f *Failure
	if errors.As(err, &f) {
		return f.retryable, f.category, true
	}
	return false, "", false
}
func persistence(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) {
		return ErrCompletion
	}
	// Storage supplies no safe transient DB taxonomy. Do not invent one.
	return ErrPersistence
}
func execution(err error) error {
	if errors.Is(err, semantic.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return &Failure{"semantic_timeout", true}
	}
	if errors.Is(err, semantic.ErrCancelled) || errors.Is(err, context.Canceled) {
		return &Failure{"semantic_cancelled", true}
	}
	var execution *semantic.ExecutionError
	var provider *semantic.ProviderError
	kind := semantic.ProviderUnknown
	if errors.As(err, &execution) {
		kind = execution.Kind
	} else if errors.As(err, &provider) {
		kind = provider.Kind
	}
	if errors.Is(err, semantic.ErrProvider) || execution != nil || provider != nil {
		switch kind {
		case semantic.RateLimited:
			return &Failure{"semantic_rate_limited", true}
		case semantic.Transient:
			return &Failure{"semantic_transient", true}
		case semantic.Permanent:
			return &Failure{"semantic_permanent", false}
		default:
			return &Failure{"semantic_provider_unknown", false}
		}
	}
	if errors.Is(err, semantic.ErrResponse) || errors.Is(err, semanticpolicy.ErrMalformed) || errors.Is(err, semanticpolicy.ErrMissing) || errors.Is(err, semanticpolicy.ErrUnknown) || errors.Is(err, semanticpolicy.ErrDuplicate) || errors.Is(err, semanticpolicy.ErrType) {
		return &Failure{"semantic_response", false}
	}
	for _, policy := range []error{semanticpolicy.ErrIssueType, semanticpolicy.ErrLabel, semanticpolicy.ErrPriority, semanticpolicy.ErrEffort, semanticpolicy.ErrProvenance} {
		if errors.Is(err, policy) {
			return ErrPolicy
		}
	}
	return &Failure{"semantic_provider_unknown", false}
}
