package github

import (
	"context"
	"errors"
	"sync/atomic"
)

var ErrRequestLimit = errors.New("github: observation request limit")

type requestBudgetKey struct{}
type requestBudget struct {
	remaining atomic.Int64
	parent    *requestBudget
}

// WithRequestLimit bounds total transport requests in a caller-owned observation,
// including nested existing readers. It does not retry, replace deadlines, or
// affect contexts outside this invocation. Nested budgets also consume parents.
// Alternate adapters must independently honor their bounded reader contracts.
func WithRequestLimit(ctx context.Context, maximum int) context.Context {
	parent, _ := ctx.Value(requestBudgetKey{}).(*requestBudget)
	b := &requestBudget{parent: parent}
	if maximum > 0 {
		b.remaining.Store(int64(maximum))
	}
	return context.WithValue(ctx, requestBudgetKey{}, b)
}
func consumeRequest(ctx context.Context) error {
	b, _ := ctx.Value(requestBudgetKey{}).(*requestBudget)
	for ; b != nil; b = b.parent {
		if b.remaining.Add(-1) < 0 {
			return ErrRequestLimit
		}
	}
	return nil
}
