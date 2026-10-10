package semantic

import "context"

// Fake follows the repository's function-field convention. Closures can record
// calls and return deterministic fixtures. Owners synchronize shared fixtures
// if they choose concurrent use; Fake owns no mutable call recording or globals.
type Fake struct {
	ClassifyIssueFunc func(context.Context, Input) (Result, error)
	ClassifyPRFunc    func(context.Context, Input) (Result, error)
	EstimateIssueFunc func(context.Context, Input) (Result, error)
}

func (f *Fake) Run(ctx context.Context, r Request) (Result, error) {
	if ctx == nil {
		return Result{}, ErrRequest
	}
	if !supported(r.Capability) {
		return Result{}, ErrCapability
	}
	if err := contextFailure(ctx.Err()); err != nil {
		return Result{}, err
	}
	var fn func(context.Context, Input) (Result, error)
	if f != nil {
		switch r.Capability {
		case ClassifyIssue:
			fn = f.ClassifyIssueFunc
		case ClassifyPR:
			fn = f.ClassifyPRFunc
		case EstimateIssue:
			fn = f.EstimateIssueFunc
		}
	}
	if fn == nil {
		return Result{}, ErrUnconfigured
	}
	return fn(ctx, r.Input)
}

var _ Runner = (*Fake)(nil)
var _ Runner = (*configuredRunner)(nil)
