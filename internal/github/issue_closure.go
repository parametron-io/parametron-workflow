package github

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

var (
	ErrClosureChanged    = errors.New("github: Issue closure evidence changed")
	ErrClosureIncomplete = errors.New("github: incomplete Issue closure timeline")
	ErrClosureLimit      = errors.New("github: Issue closure observation limit")
)

const MaxClosureItems = 4096
const MaxClosureEvidenceBytes = 16 << 20
const ClosureCursorOrder = "github_issue_timeline_cursor_sequence_v1"

// ClosureComment includes current editing evidence, separately from conversation
// comment ownership. A missing author/editor never becomes an inferred identity.
type ClosureComment struct {
	ID                   string
	DatabaseID           int64
	Issue                Identity
	Body                 string
	Author               *Actor
	CreatedAt, UpdatedAt time.Time
	LastEditedAt         *time.Time
	Editor               *Actor
}
type ClosureItem struct {
	Issue            Identity
	ID, Kind, Cursor string
	CreatedAt        time.Time
	Actor            *Actor
	Comment          *ClosureComment
	// Nonempty for keyword/PR/commit closure. Such a closure is not an explicit
	// normal Phase close, nor a Close-with-comment exception.
	CloserID, CloserKind string
}

// IssueClosure preserves the unfiltered server timeline connection, including
// unrelated nodes. Order is the API's cursor sequence, never a local timestamp,
// event-ID, REST page-order or webhook-order sort. Equal timestamps are allowed.
// The provider traverses after/endCursor, validates totalCount/updatedAt on every
// page, then repeats the entire connection. This establishes a stable native
// timeline sequence, not an atomic snapshot or a shared UI transaction ID.
// API contract: https://docs.github.com/en/graphql/reference/issues#issue
// and https://docs.github.com/en/graphql/guides/using-pagination-in-the-graphql-api
// Unknown nodes occupy their exact positions and cannot be skipped for adjacency.
type IssueClosure struct {
	Issue                Identity
	State, Order         string
	CreatedAt, UpdatedAt time.Time
	Complete             bool
	Items                []ClosureItem
}
type IssueClosureReader interface {
	IssueClosure(context.Context, Identity) (IssueClosure, error)
}

func (f IssueClosure) Validate(i Identity, state string) error {
	if !ValidDevelopmentIdentity(i) || f.Issue != i || f.State != state || state != "OPEN" && state != "CLOSED" || f.CreatedAt.IsZero() || f.UpdatedAt.IsZero() || !f.Complete || f.Order != ClosureCursorOrder || len(f.Items) > MaxClosureItems {
		return ErrClosureIncomplete
	}
	ids, cursors := map[string]bool{}, map[string]bool{}
	comments := map[int64]bool{}
	native := "OPEN"
	var at time.Time
	for _, e := range f.Items {
		if e.Issue != i || !validIDs(e.ID, e.Cursor) || e.Kind == "" || ids[e.ID] || cursors[e.Cursor] {
			return ErrClosureIncomplete
		}
		ids[e.ID], cursors[e.Cursor] = true, true
		switch e.Kind {
		case "IssueComment":
			c := e.Comment
			if c == nil || c.ID != e.ID || c.Issue != i || c.DatabaseID <= 0 || comments[c.DatabaseID] || c.CreatedAt.IsZero() || c.CreatedAt.Before(f.CreatedAt) || c.UpdatedAt.Before(c.CreatedAt) || c.Author != nil && !ValidCommentAuthor(*c.Author) || c.Editor != nil && !ValidCommentAuthor(*c.Editor) || !e.CreatedAt.Equal(c.CreatedAt) {
				return ErrClosureIncomplete
			}
			comments[c.DatabaseID] = true
		case "ClosedEvent", "ReopenedEvent":
			if e.CreatedAt.IsZero() || e.CreatedAt.Before(f.CreatedAt) || !at.IsZero() && e.CreatedAt.Before(at) || e.Comment != nil || e.Actor != nil && !ValidCommentAuthor(*e.Actor) {
				return ErrClosureIncomplete
			}
			if (e.CloserID == "") != (e.CloserKind == "") || e.CloserID != "" && (!validIDs(e.CloserID) || e.Kind != "ClosedEvent" || e.CloserKind != "PullRequest" && e.CloserKind != "Commit") {
				return ErrClosureIncomplete
			}
			if e.Kind == "ClosedEvent" {
				if native != "OPEN" {
					return ErrClosureIncomplete
				}
				native = "CLOSED"
			} else {
				if native != "CLOSED" {
					return ErrClosureIncomplete
				}
				native = "OPEN"
			}
			at = e.CreatedAt
		default:
			if e.Comment != nil || e.Actor != nil || e.CloserID != "" || e.CloserKind != "" {
				return ErrClosureIncomplete
			}
		}
	}
	if native != state {
		return ErrClosureChanged
	}
	return nil
}
func (f IssueClosure) LatestClose() (ClosureItem, ClosureItem, int, bool) {
	var close, boundary ClosureItem
	index := -1
	for n, e := range f.Items {
		if e.Kind == "ReopenedEvent" {
			boundary = e
			index = -1
		}
		if e.Kind == "ClosedEvent" {
			close = e
			index = n
		}
	}
	return close, boundary, index, f.State == "CLOSED" && index >= 0
}
func (t *Transport) closureRead(ctx context.Context, i Identity) (IssueClosure, error) {
	var out IssueClosure
	state, err := t.nativeIdentity(ctx, i, "issue")
	if err != nil {
		return out, err
	}
	out = IssueClosure{Issue: i, State: state, Order: ClosureCursorOrder, Items: []ClosureItem{}}
	var cursor *string
	seen := map[string]bool{}
	total := -1
	bytesRead := 0
	selection := `__typename ... on Node{id} ... on IssueComment{databaseId body createdAt updatedAt lastEditedAt author{__typename login ... on User{id} ... on Bot{id}} editor{__typename login ... on User{id} ... on Bot{id}} issue{` + identitySelection + `}} ... on ClosedEvent{createdAt actor{__typename login ... on User{id} ... on Bot{id}} closer{__typename ... on Node{id}} closable{__typename ... on Issue{` + identitySelection + `}}} ... on ReopenedEvent{createdAt actor{__typename login ... on User{id} ... on Bot{id}} closable{__typename ... on Issue{` + identitySelection + `}}}`
	for pages := 0; pages < 64; pages++ {
		var data struct {
			Node *struct {
				wireIdentity
				State     string
				CreatedAt time.Time
				Timeline  *struct {
					TotalCount *int
					UpdatedAt  time.Time
					PageInfo   struct {
						HasNextPage            *bool
						EndCursor, StartCursor string
					}
					Edges []struct {
						Cursor string
						Node   json.RawMessage
					}
				} `json:"timelineItems"`
			}
		}
		q := `query($id:ID!,$cursor:String){node(id:$id){__typename ... on Issue{` + identitySelection + ` state createdAt timelineItems(first:100,after:$cursor){totalCount updatedAt pageInfo{hasNextPage endCursor startCursor} edges{cursor node{` + selection + `}}}}}}`
		if err := t.query(ctx, q, map[string]any{"id": i.ID, "cursor": cursor}, &data); err != nil {
			return IssueClosure{}, err
		}
		if data.Node == nil || data.Node.Timeline == nil {
			return IssueClosure{}, ErrClosureIncomplete
		}
		actual, err := data.Node.wireIdentity.normalized()
		p := data.Node.Timeline
		if err != nil || actual != i || data.Node.State != state || data.Node.CreatedAt.IsZero() || p.TotalCount == nil || *p.TotalCount < 0 || *p.TotalCount > MaxClosureItems || p.UpdatedAt.IsZero() || p.PageInfo.HasNextPage == nil {
			return IssueClosure{}, ErrClosureIncomplete
		}
		if total < 0 {
			total = *p.TotalCount
			out.CreatedAt = data.Node.CreatedAt
			out.UpdatedAt = p.UpdatedAt
		} else if total != *p.TotalCount || !out.CreatedAt.Equal(data.Node.CreatedAt) || !out.UpdatedAt.Equal(p.UpdatedAt) {
			return IssueClosure{}, ErrClosureChanged
		}
		if len(p.Edges) > 100 || len(out.Items)+len(p.Edges) > MaxClosureItems {
			return IssueClosure{}, ErrClosureLimit
		}
		if len(p.Edges) > 0 && (p.PageInfo.StartCursor != p.Edges[0].Cursor || p.PageInfo.EndCursor != p.Edges[len(p.Edges)-1].Cursor) {
			return IssueClosure{}, ErrClosureIncomplete
		}
		for _, edge := range p.Edges {
			bytesRead += len(edge.Node) + len(edge.Cursor)
			if bytesRead > MaxClosureEvidenceBytes {
				return IssueClosure{}, ErrClosureLimit
			}
			var w struct {
				ID                    string
				Kind                  string `json:"__typename"`
				CreatedAt, UpdatedAt  time.Time
				LastEditedAt          *time.Time
				DatabaseID            int64
				Body                  *string
				Actor, Author, Editor *reviewActor
				Closer                *struct {
					ID   string
					Kind string `json:"__typename"`
				}
				Issue, Closable *wireIdentity
			}
			if json.Unmarshal(edge.Node, &w) != nil || !validIDs(w.ID, edge.Cursor) || seen[edge.Cursor] {
				return IssueClosure{}, ErrClosureIncomplete
			}
			seen[edge.Cursor] = true
			e := ClosureItem{Issue: i, ID: w.ID, Kind: w.Kind, Cursor: edge.Cursor, CreatedAt: w.CreatedAt}
			if w.Kind == "IssueComment" {
				if w.Issue == nil || w.Body == nil {
					return IssueClosure{}, ErrClosureIncomplete
				}
				target, err := w.Issue.normalized()
				if err != nil || target != i {
					return IssueClosure{}, ErrClosureIncomplete
				}
				author, err := w.Author.normalized()
				if err != nil {
					return IssueClosure{}, err
				}
				editor, err := w.Editor.normalized()
				if err != nil {
					return IssueClosure{}, err
				}
				e.Comment = &ClosureComment{ID: w.ID, DatabaseID: w.DatabaseID, Issue: i, Body: *w.Body, Author: author, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, LastEditedAt: w.LastEditedAt, Editor: editor}
			}
			if w.Kind == "ClosedEvent" || w.Kind == "ReopenedEvent" {
				if w.Closable == nil {
					return IssueClosure{}, ErrClosureIncomplete
				}
				target, err := w.Closable.normalized()
				if err != nil || target != i {
					return IssueClosure{}, ErrClosureIncomplete
				}
				actor, err := w.Actor.normalized()
				if err != nil {
					return IssueClosure{}, err
				}
				e.Actor = actor
				if w.Closer != nil {
					if !validIDs(w.Closer.ID) || w.Closer.Kind != "PullRequest" && w.Closer.Kind != "Commit" {
						return IssueClosure{}, ErrClosureIncomplete
					}
					e.CloserID, e.CloserKind = w.Closer.ID, w.Closer.Kind
				}
			}
			out.Items = append(out.Items, e)
		}
		if !*p.PageInfo.HasNextPage {
			if len(out.Items) != total {
				return IssueClosure{}, ErrClosureIncomplete
			}
			final, err := t.nativeIdentity(ctx, i, "issue")
			if err != nil {
				return IssueClosure{}, err
			}
			if final != state {
				return IssueClosure{}, ErrClosureChanged
			}
			out.Complete = true
			return out, out.Validate(i, state)
		}
		if len(p.Edges) == 0 || !validIDs(p.PageInfo.EndCursor) || cursor != nil && *cursor == p.PageInfo.EndCursor {
			return IssueClosure{}, ErrClosureIncomplete
		}
		next := p.PageInfo.EndCursor
		cursor = &next
	}
	return IssueClosure{}, ErrClosureLimit
}
func (t *Transport) IssueClosure(ctx context.Context, i Identity) (IssueClosure, error) {
	ctx = WithRequestLimit(ctx, 140)
	first, err := t.closureRead(ctx, i)
	if err != nil {
		return IssueClosure{}, err
	}
	second, err := t.closureRead(ctx, i)
	if err != nil {
		return IssueClosure{}, err
	}
	if !reflect.DeepEqual(first, second) {
		return IssueClosure{}, ErrClosureChanged
	}
	return second, ctx.Err()
}
