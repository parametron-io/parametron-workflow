package github

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// ListedIssue is current discovery input, with no relationship or policy authority.
type ListedIssue struct {
	Identity
	Title, Body, State string
}

// IssueLister is deliberately separate from Client and all mutation capabilities.
type IssueLister interface {
	ListIssues(context.Context, Repository) ([]ListedIssue, error)
}

type ListerFake struct {
	ListIssuesFunc func(context.Context, Repository) ([]ListedIssue, error)
}

func (f *ListerFake) ListIssues(ctx context.Context, r Repository) ([]ListedIssue, error) {
	if f == nil || f.ListIssuesFunc == nil {
		return nil, failure(Permanent)
	}
	return f.ListIssuesFunc(ctx, r)
}

// ListIssues enumerates the supported Repository.issues IssueConnection, not
// issueOrPullRequest or search. With no states filter both OPEN and CLOSED remain.
func (t *Transport) ListIssues(ctx context.Context, expected Repository) ([]ListedIssue, error) {
	if blank(expected.ID) || blank(expected.Owner) || blank(expected.Name) {
		return nil, failure(Permanent)
	}
	actual, err := t.Repository(ctx, expected.Owner, expected.Name)
	if err != nil {
		return nil, err
	}
	if actual != expected {
		return nil, failure(Malformed)
	}
	nodes, err := t.connection(ctx, expected.ID, "Repository", "issues", "__typename "+identitySelection+" title body state")
	if err != nil {
		return nil, err
	}
	out := make([]ListedIssue, 0, len(nodes))
	numbers, ids := map[int]bool{}, map[string]bool{}
	for _, raw := range nodes {
		var w struct {
			wireIdentity
			Typename           string `json:"__typename"`
			Title, Body, State *string
		}
		if json.Unmarshal(raw, &w) != nil || w.Typename != "Issue" || w.Title == nil || w.Body == nil || w.State == nil || (*w.State != "OPEN" && *w.State != "CLOSED") {
			return nil, failure(Malformed)
		}
		i, err := w.wireIdentity.normalized()
		if err != nil {
			return nil, err
		}
		if i.Repository.ID != expected.ID || !strings.EqualFold(i.Repository.Owner, expected.Owner) || !strings.EqualFold(i.Repository.Name, expected.Name) || numbers[i.Number] || ids[i.ID] {
			return nil, failure(Malformed)
		}
		numbers[i.Number], ids[i.ID] = true, true
		i.Repository = expected
		out = append(out, ListedIssue{i, *w.Title, *w.Body, *w.State})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

var _ IssueLister = (*Transport)(nil)
