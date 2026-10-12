package github

import (
	"context"
	"encoding/json"
)

// PullRequestProjectItem exposes current content identity only, with no field values.
type PullRequestProjectItem struct {
	ID, ProjectID, ContentKind string
	Issue                      *Identity
	PullRequest                *Identity
}
type PullRequestProjectItemReader interface {
	CurrentPullRequestProjectItem(context.Context, string) (PullRequestProjectItem, error)
}

func (t *Transport) CurrentPullRequestProjectItem(ctx context.Context, item string) (PullRequestProjectItem, error) {
	if !validIDs(item) {
		return PullRequestProjectItem{}, failure(Permanent)
	}
	var data struct {
		Node *struct {
			Typename string `json:"__typename"`
			ID       string
			Project  *struct{ ID string }
			Content  json.RawMessage
		}
	}
	q := `query($id:ID!){node(id:$id){__typename ... on ProjectV2Item{id project{id} content{__typename ... on Issue{` + identitySelection + `} ... on PullRequest{` + identitySelection + `} ... on DraftIssue{id}}}}}`
	if err := t.query(ctx, q, map[string]any{"id": item}, &data); err != nil {
		return PullRequestProjectItem{}, err
	}
	n := data.Node
	if n == nil {
		return PullRequestProjectItem{}, failure(NotFound)
	}
	if n.Typename != "ProjectV2Item" || n.ID != item || n.Project == nil || !validIDs(n.Project.ID) {
		return PullRequestProjectItem{}, failure(Malformed)
	}
	out := PullRequestProjectItem{ID: item, ProjectID: n.Project.ID}
	if string(n.Content) == "null" || len(n.Content) == 0 {
		return out, nil
	}
	var c struct {
		wireIdentity
		Typename string `json:"__typename"`
	}
	if json.Unmarshal(n.Content, &c) != nil {
		return PullRequestProjectItem{}, failure(Malformed)
	}
	out.ContentKind = c.Typename
	switch c.Typename {
	case "Issue":
		i, err := c.wireIdentity.normalized()
		if err != nil {
			return PullRequestProjectItem{}, err
		}
		out.Issue = &i
	case "PullRequest":
		i, err := c.wireIdentity.normalized()
		if err != nil {
			return PullRequestProjectItem{}, err
		}
		out.PullRequest = &i
	case "DraftIssue":
	default:
		return PullRequestProjectItem{}, failure(Malformed)
	}
	return out, ctx.Err()
}

var _ PullRequestProjectItemReader = (*Transport)(nil)
