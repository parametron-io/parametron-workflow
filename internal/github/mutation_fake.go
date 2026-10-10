package github

import "context"

// MutationFake exposes explicit hooks; unconfigured writes fail loudly.
// Callers own recording and synchronize shared fixtures.
type MutationFake struct {
	SetIssueTypeFunc      func(context.Context, string, string) error
	ResolveLabelsFunc     func(context.Context, Repository, []string) ([]string, error)
	AddLabelsFunc         func(context.Context, string, []string) error
	RemoveLabelsFunc      func(context.Context, string, []string) error
	AddProjectItemFunc    func(context.Context, string, string) (string, error)
	RemoveProjectItemFunc func(context.Context, string, string) error
	SetProjectOptionFunc  func(context.Context, string, string, string, string) error
}

func (f MutationFake) SetIssueType(c context.Context, a, b string) error {
	if f.SetIssueTypeFunc == nil {
		return failure(Permanent)
	}
	return f.SetIssueTypeFunc(c, a, b)
}
func (f MutationFake) ResolveLabels(c context.Context, a Repository, b []string) ([]string, error) {
	if f.ResolveLabelsFunc == nil {
		return nil, failure(Permanent)
	}
	return f.ResolveLabelsFunc(c, a, b)
}
func (f MutationFake) AddLabels(c context.Context, a string, b []string) error {
	if f.AddLabelsFunc == nil {
		return failure(Permanent)
	}
	return f.AddLabelsFunc(c, a, b)
}
func (f MutationFake) RemoveLabels(c context.Context, a string, b []string) error {
	if f.RemoveLabelsFunc == nil {
		return failure(Permanent)
	}
	return f.RemoveLabelsFunc(c, a, b)
}
func (f MutationFake) AddProjectItem(c context.Context, a, b string) (string, error) {
	if f.AddProjectItemFunc == nil {
		return "", failure(Permanent)
	}
	return f.AddProjectItemFunc(c, a, b)
}
func (f MutationFake) RemoveProjectItem(c context.Context, a, b string) error {
	if f.RemoveProjectItemFunc == nil {
		return failure(Permanent)
	}
	return f.RemoveProjectItemFunc(c, a, b)
}
func (f MutationFake) SetProjectOption(c context.Context, a, b, d, e string) error {
	if f.SetProjectOptionFunc == nil {
		return failure(Permanent)
	}
	return f.SetProjectOptionFunc(c, a, b, d, e)
}
