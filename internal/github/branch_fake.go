package github

import "context"

// BranchFake fails unconfigured operations. Callers synchronize recording hooks.
type BranchFake struct {
	DefaultBranchFunc func(context.Context, Repository) (Branch, error)
	BranchFunc        func(context.Context, Repository, string) (Branch, error)
	CreateBranchFunc  func(context.Context, Repository, string, string) (Branch, error)
}

func (f BranchFake) DefaultBranch(ctx context.Context, repo Repository) (Branch, error) {
	if f.DefaultBranchFunc == nil {
		return Branch{}, failure(Permanent)
	}
	return f.DefaultBranchFunc(ctx, repo)
}
func (f BranchFake) Branch(ctx context.Context, repo Repository, name string) (Branch, error) {
	if f.BranchFunc == nil {
		return Branch{}, failure(Permanent)
	}
	return f.BranchFunc(ctx, repo, name)
}
func (f BranchFake) CreateBranch(ctx context.Context, repo Repository, name, sha string) (Branch, error) {
	if f.CreateBranchFunc == nil {
		return Branch{}, failure(Permanent)
	}
	return f.CreateBranchFunc(ctx, repo, name, sha)
}

var _ Branches = BranchFake{}
