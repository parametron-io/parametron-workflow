package semantic

import (
	"context"
	"errors"
)

var (
	ErrConfiguration = errors.New("semantic: invalid configuration")
	ErrRequest       = errors.New("semantic: invalid request")
	ErrCapability    = errors.New("semantic: unsupported capability")
	ErrCancelled     = errors.New("semantic: cancelled")
	ErrTimeout       = errors.New("semantic: deadline exceeded")
	ErrProvider      = errors.New("semantic: provider failure")
	ErrResponse      = errors.New("semantic: malformed structured response")
	ErrAssets        = errors.New("semantic: missing or invalid assets")
	ErrUnconfigured  = errors.New("semantic: unconfigured fake capability")
)

type ProviderKind string

const (
	ProviderUnknown ProviderKind = "unknown"
	RateLimited     ProviderKind = "rate_limited"
	Transient       ProviderKind = "transient"
	Permanent       ProviderKind = "permanent"
)

func normalizedKind(k ProviderKind) ProviderKind {
	switch k {
	case RateLimited, Transient, Permanent:
		return k
	default:
		return ProviderUnknown
	}
}

// ProviderError lets adapters classify failures without message matching. Cause
// is available for inspection, but is never formatted into public diagnostics.
type ProviderError struct {
	Kind  ProviderKind
	Cause error
}

func (e *ProviderError) Error() string { return "semantic provider: " + string(normalizedKind(e.Kind)) }
func (e *ProviderError) Unwrap() error { return e.Cause }

// ExecutionError preserves the adapter category and cause without exposing text.
type ExecutionError struct {
	Kind  ProviderKind
	cause error
}

func (e *ExecutionError) Error() string {
	return "semantic: provider failure (" + string(normalizedKind(e.Kind)) + ")"
}
func (e *ExecutionError) Unwrap() error        { return e.cause }
func (e *ExecutionError) Is(target error) bool { return target == ErrProvider }

type contextError struct{ category, cause error }

func (e *contextError) Error() string        { return e.category.Error() }
func (e *contextError) Unwrap() error        { return e.cause }
func (e *contextError) Is(target error) bool { return target == e.category }
func contextFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return &contextError{ErrCancelled, context.Canceled}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &contextError{ErrTimeout, context.DeadlineExceeded}
	}
	return nil
}
