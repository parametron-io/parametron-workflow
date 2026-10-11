package github

import (
	"context"
	"encoding/json"
)

// CurrentProjectItem exposes current content identity only, with no field values.
type CurrentProjectItem struct {
	ID, ProjectID, ContentKind string
	Issue                      *Identity
}
type ProjectItemReader interface {
	CurrentProjectItem(context.Context, string) (CurrentProjectItem, error)
}

func (t *Transport) CurrentProjectItem(ctx context.Context, item string) (CurrentProjectItem, error) {
	if !validIDs(item) {
		return CurrentProjectItem{}, failure(Permanent)
	}
	var data struct {
		Node *struct {
			Typename string `json:"__typename"`
			ID       string
			Project  *struct{ ID string }
			Content  json.RawMessage
		}
	}
	q := `query($id:ID!){node(id:$id){__typename ... on ProjectV2Item{id project{id} content{__typename ... on Issue{` + identitySelection + `} ... on PullRequest{id} ... on DraftIssue{id}}}}}`
	if err := t.query(ctx, q, map[string]any{"id": item}, &data); err != nil {
		return CurrentProjectItem{}, err
	}
	n := data.Node
	if n == nil {
		return CurrentProjectItem{}, failure(NotFound)
	}
	if n.Typename != "ProjectV2Item" || n.ID != item || n.Project == nil || !validIDs(n.Project.ID) {
		return CurrentProjectItem{}, failure(Malformed)
	}
	out := CurrentProjectItem{ID: item, ProjectID: n.Project.ID}
	if string(n.Content) == "null" || len(n.Content) == 0 {
		return out, nil
	}
	var c struct {
		wireIdentity
		Typename string `json:"__typename"`
	}
	if json.Unmarshal(n.Content, &c) != nil {
		return CurrentProjectItem{}, failure(Malformed)
	}
	out.ContentKind = c.Typename
	switch c.Typename {
	case "Issue":
		i, err := c.wireIdentity.normalized()
		if err != nil {
			return CurrentProjectItem{}, err
		}
		out.Issue = &i
	case "PullRequest", "DraftIssue":
	default:
		return CurrentProjectItem{}, failure(Malformed)
	}
	return out, ctx.Err()
}

var _ ProjectItemReader = (*Transport)(nil)
