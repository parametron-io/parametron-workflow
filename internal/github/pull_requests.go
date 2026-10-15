package github

import (
	"context"
	"errors"
	"sort"
)

// Enumeration bounds apply per repository, independently of server ordering.
const MaxPullRequestPages = 100
const MaxPullRequestRecords = 10000

var ErrPullRequestLimit = errors.New("github: pull request enumeration limit")
var ErrPullRequestIncomplete = errors.New("github: incomplete pull request enumeration")
var ErrPullRequestChanged = errors.New("github: pull request enumeration changed")

// ListedPullRequest is observation only, never binding or lifecycle authority.
type ListedPullRequest struct {
	Identity
	State string
}

// PullRequestLister returns every OPEN, CLOSED and MERGED PR or a nil list and
// error. Implementations must bound pagination and reject duplicate identities.
type PullRequestLister interface {
	ListPullRequests(context.Context, Repository) ([]ListedPullRequest, error)
}

type PullRequestListerFake struct {
	ListPullRequestsFunc func(context.Context, Repository) ([]ListedPullRequest, error)
}

func (f *PullRequestListerFake) ListPullRequests(ctx context.Context, r Repository) ([]ListedPullRequest, error) {
	if f == nil || f.ListPullRequestsFunc == nil {
		return nil, failure(Permanent)
	}
	return f.ListPullRequestsFunc(ctx, r)
}

// ListPullRequests uses the native repository connection with explicit all-state
// selection. Stable totals and unique records are required; no truncated success
// or pagination-order inference is permitted. Repeated reads are not atomic.
func (t *Transport) ListPullRequests(ctx context.Context, expected Repository) ([]ListedPullRequest, error) {
	if !ValidBranchRepository(expected) {
		return nil, failure(Permanent)
	}
	actual, err := t.Repository(ctx, expected.Owner, expected.Name)
	if err != nil {
		return nil, err
	}
	if actual != expected {
		return nil, failure(Malformed)
	}
	var cursor any
	cursors := map[string]bool{}
	ids := map[string]bool{}
	numbers := map[int]bool{}
	out := []ListedPullRequest{}
	total := -1
	for count := 0; count < MaxPullRequestPages; count++ {
		var data struct {
			Node *struct {
				Typename string `json:"__typename"`
				wireRepository
				Result *struct {
					TotalCount *int
					Nodes      *[]*struct {
						wireIdentity
						Typename string `json:"__typename"`
						State    *string
					}
					PageInfo *page
				}
			}
		}
		q := `query($id:ID!,$cursor:String){node(id:$id){__typename ... on Repository{` + repositorySelection + ` result:pullRequests(first:100,after:$cursor,states:[OPEN,CLOSED,MERGED]){totalCount nodes{__typename ` + identitySelection + ` state} pageInfo{hasNextPage endCursor}}}}}`
		if err := t.query(ctx, q, map[string]any{"id": expected.ID, "cursor": cursor}, &data); err != nil {
			return nil, err
		}
		n := data.Node
		if n == nil {
			return nil, failure(NotFound)
		}
		r, err := n.wireRepository.normalized()
		if err != nil || n.Typename != "Repository" || r != expected {
			return nil, failure(Malformed)
		}
		c := n.Result
		if c == nil || c.TotalCount == nil || *c.TotalCount < 0 || c.Nodes == nil || *c.Nodes == nil || c.PageInfo == nil || c.PageInfo.HasNextPage == nil {
			return nil, ErrPullRequestIncomplete
		}
		if *c.TotalCount > MaxPullRequestRecords {
			return nil, ErrPullRequestLimit
		}
		if total < 0 {
			total = *c.TotalCount
		} else if total != *c.TotalCount {
			return nil, ErrPullRequestChanged
		}
		if len(*c.Nodes) > 100 {
			return nil, failure(Malformed)
		}
		for _, w := range *c.Nodes {
			if w == nil || w.Typename != "PullRequest" || w.State == nil || (*w.State != "OPEN" && *w.State != "CLOSED" && *w.State != "MERGED") {
				return nil, failure(Malformed)
			}
			i, err := w.wireIdentity.normalized()
			if err != nil || !ValidDevelopmentIdentity(i) || i.ID == expected.ID || i.Repository != expected || ids[i.ID] || numbers[i.Number] {
				return nil, failure(Malformed)
			}
			ids[i.ID], numbers[i.Number] = true, true
			out = append(out, ListedPullRequest{Identity: i, State: *w.State})
		}
		if len(out) > MaxPullRequestRecords {
			return nil, ErrPullRequestLimit
		}
		if len(out) > total {
			return nil, ErrPullRequestIncomplete
		}
		p := c.PageInfo
		if !*p.HasNextPage {
			if len(out) != total {
				return nil, ErrPullRequestIncomplete
			}
			final, err := t.Repository(ctx, expected.Owner, expected.Name)
			if err != nil {
				return nil, err
			}
			if final != expected {
				return nil, ErrPullRequestChanged
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
			return out, nil
		}
		if len(*c.Nodes) == 0 || len(out) >= total || p.EndCursor == "" || cursors[p.EndCursor] {
			return nil, ErrPullRequestIncomplete
		}
		cursors[p.EndCursor] = true
		cursor = p.EndCursor
	}
	return nil, ErrPullRequestLimit
}

var _ PullRequestLister = (*Transport)(nil)
