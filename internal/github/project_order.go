package github

import (
	"context"
	"encoding/json"
	"strings"
)

// OrderedProjectItem retains connection order and uncontrolled content. Kind is
// the GraphQL content typename; empty means null/deleted/redacted content. Type
// retains ProjectV2Item.type even when content cannot be read.
type OrderedProjectItem struct {
	ID, ProjectID          string
	Archived               bool
	Type                   string
	ContentKind, ContentID string
	Issue                  *Identity
}

// ProjectOrderReader is separate from unordered resource membership and writes.
type ProjectOrderReader interface {
	ListProjectItemsInOrder(context.Context, string) ([]OrderedProjectItem, error)
}
type ProjectOrderFake struct {
	ListProjectItemsInOrderFunc func(context.Context, string) ([]OrderedProjectItem, error)
}

func (f *ProjectOrderFake) ListProjectItemsInOrder(ctx context.Context, id string) ([]OrderedProjectItem, error) {
	if f == nil || f.ListProjectItemsInOrderFunc == nil {
		return nil, failure(Permanent)
	}
	return f.ListProjectItemsInOrderFunc(ctx, id)
}

// ListProjectItemsInOrder explicitly requests POSITION ASC and both archive
// states. Order is data: pages are appended verbatim, never normalized/sorted.
// Reads across pages are not an atomic snapshot; duplicates fail the attempt.
func (t *Transport) ListProjectItemsInOrder(ctx context.Context, id string) ([]OrderedProjectItem, error) {
	if blank(id) {
		return nil, failure(Permanent)
	}
	out := []OrderedProjectItem{}
	var cursor any
	cursors, items, contents, issues := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[Ref]bool{}
	for {
		var data struct {
			Node *struct {
				Typename string `json:"__typename"`
				ID       string
				Items    *connection
			}
		}
		q := `query($id:ID!,$cursor:String){node(id:$id){__typename ... on ProjectV2{id items(first:100,after:$cursor,orderBy:{field:POSITION,direction:ASC},archivedStates:[ARCHIVED,NOT_ARCHIVED]){nodes{id type isArchived project{id} content{__typename ... on Issue{` + identitySelection + `} ... on PullRequest{id} ... on DraftIssue{id}}} pageInfo{hasNextPage endCursor}}}}}`
		if err := t.query(ctx, q, map[string]any{"id": id, "cursor": cursor}, &data); err != nil {
			return nil, err
		}
		if data.Node == nil {
			return nil, failure(NotFound)
		}
		n := data.Node
		if n.Typename != "ProjectV2" || n.ID != id || n.Items == nil || n.Items.Nodes == nil || n.Items.PageInfo == nil || n.Items.PageInfo.HasNextPage == nil {
			return nil, failure(Malformed)
		}
		for _, raw := range n.Items.Nodes {
			var w struct {
				ID, Type   string
				IsArchived *bool
				Project    *struct{ ID string }
				Content    *struct {
					wireIdentity
					Typename string `json:"__typename"`
				}
			}
			if json.Unmarshal(raw, &w) != nil || blank(w.ID) || items[w.ID] || w.IsArchived == nil || w.Project == nil || w.Project.ID != id {
				return nil, failure(Malformed)
			}
			switch w.Type {
			case "ISSUE", "PULL_REQUEST", "DRAFT_ISSUE", "REDACTED":
			default:
				return nil, failure(Malformed)
			}
			item := OrderedProjectItem{ID: w.ID, ProjectID: id, Archived: *w.IsArchived, Type: w.Type}
			items[w.ID] = true
			if c := w.Content; c != nil {
				if blank(c.Typename) || blank(c.ID) || contents[c.ID] {
					return nil, failure(Malformed)
				}
				contents[c.ID] = true
				item.ContentKind, item.ContentID = c.Typename, c.ID
				switch c.Typename {
				case "Issue":
					if w.Type != "ISSUE" {
						return nil, failure(Malformed)
					}
					i, err := c.wireIdentity.normalized()
					if err != nil {
						return nil, err
					}
					key := Ref{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Number: i.Number}
					if issues[key] {
						return nil, failure(Malformed)
					}
					issues[key] = true
					item.Issue = &i
				case "PullRequest":
					if w.Type != "PULL_REQUEST" {
						return nil, failure(Malformed)
					}
				case "DraftIssue":
					if w.Type != "DRAFT_ISSUE" {
						return nil, failure(Malformed)
					}
				default: // Preserve future opaque content without treating it as an Issue.
				}
			}
			out = append(out, item)
		}
		p := n.Items.PageInfo
		if !*p.HasNextPage {
			return out, nil
		}
		if blank(p.EndCursor) || cursors[p.EndCursor] {
			return nil, failure(Malformed)
		}
		cursors[p.EndCursor] = true
		cursor = p.EndCursor
	}
}

var _ ProjectOrderReader = (*Transport)(nil)
var _ ProjectOrderReader = (*ProjectOrderFake)(nil)
