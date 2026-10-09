package github

import (
	"context"
	"github.com/parametron-io/parametron-workflow/internal/config"
)

// Fake is configured with explicit functions. Unconfigured calls fail instead
// of returning plausible empty state. Tests can assert arguments and call order
// in closures; shared mutable fixtures should be synchronized by their owner.
type Fake struct {
	DiscoverSchemaFunc func(context.Context, config.SourceConfig) (config.Schema, error)
	RepositoryFunc     func(context.Context, string, string) (Repository, error)
	IssueFunc          func(context.Context, Ref) (Issue, error)
	PullRequestFunc    func(context.Context, Ref) (PullRequest, error)
	ProjectItemsFunc   func(context.Context, string, string, map[string]config.FieldKind) ([]ProjectItem, error)
}

func (f *Fake) DiscoverSchema(ctx context.Context, s config.SourceConfig) (config.Schema, error) {
	if f.DiscoverSchemaFunc == nil {
		return config.Schema{}, failure(Permanent)
	}
	return f.DiscoverSchemaFunc(ctx, s)
}
func (f *Fake) Repository(ctx context.Context, o, n string) (Repository, error) {
	if f.RepositoryFunc == nil {
		return Repository{}, failure(Permanent)
	}
	return f.RepositoryFunc(ctx, o, n)
}
func (f *Fake) Issue(ctx context.Context, r Ref) (Issue, error) {
	if f.IssueFunc == nil {
		return Issue{}, failure(Permanent)
	}
	return f.IssueFunc(ctx, r)
}
func (f *Fake) PullRequest(ctx context.Context, r Ref) (PullRequest, error) {
	if f.PullRequestFunc == nil {
		return PullRequest{}, failure(Permanent)
	}
	return f.PullRequestFunc(ctx, r)
}
func (f *Fake) ProjectItems(ctx context.Context, c, p string, fields map[string]config.FieldKind) ([]ProjectItem, error) {
	if f.ProjectItemsFunc == nil {
		return nil, failure(Permanent)
	}
	return f.ProjectItemsFunc(ctx, c, p, fields)
}

var _ Client = (*Fake)(nil)
var _ Client = (*Transport)(nil)
