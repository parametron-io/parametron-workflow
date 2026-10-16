package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// CommentResource selects the conversation surface, never a review surface.
type CommentResource struct {
	Identity Identity
	Kind     string
}

func (r CommentResource) Valid() bool {
	return ValidDevelopmentIdentity(r.Identity) && (r.Kind == "issue" || r.Kind == "pull_request")
}

type IssueComment struct {
	ID, Body   string
	DatabaseID int64
	Resource   CommentResource
	Author     Actor
}
type CommentList struct {
	Resource CommentResource
	Comments []IssueComment
	Complete bool
}
type CommentReader interface {
	IssueComments(context.Context, CommentResource) (CommentList, error)
	IssueComment(context.Context, CommentResource, string) (IssueComment, error)
}
type CommentWriter interface {
	CreateIssueComment(context.Context, CommentResource, string) (IssueComment, error)
	UpdateIssueComment(context.Context, IssueComment, string) (IssueComment, error)
}

// CommentAuthorReader verifies the actual shared writer credential before any
// comment mutation. An installation account is never treated as its Bot actor.
type CommentAuthorReader interface {
	AuthenticatedCommentAuthor(context.Context) (Actor, error)
}

var ErrCommentCredential = errors.New("github: comment credential author is not independently verifiable")

// CommentCredentialSource explicitly selects the supported identity contract.
// Installation tokens do not support GET /user. Only the App-owned provider
// below can establish their provenance; a token-kind flag grants no authority.
type CommentCredentialSource interface{ CommentCredentialKind() string }

func (t *Transport) AuthenticatedCommentAuthor(ctx context.Context) (Actor, error) {
	if err := ctx.Err(); err != nil {
		return Actor{}, err
	}
	if app, ok := t.tokens.(*AppCredentials); ok && app != nil {
		return app.commentAuthor(ctx)
	}
	credential, ok := t.tokens.(CommentCredentialSource)
	if !ok || credential.CommentCredentialKind() != "user" {
		return Actor{}, ErrCommentCredential
	}
	var u restUser
	if err := t.developerRead(ctx, "/user", &u); err != nil {
		return Actor{}, err
	}
	a := Actor{ID: u.NodeID, Login: u.Login, Type: u.Type}
	if u.ID <= 0 || !ValidCommentAuthor(a) {
		return Actor{}, failure(Malformed)
	}
	return a, ctx.Err()
}

// Expected authors come from trusted deployment configuration, including account
// type and stable node ID; no inference about the credential's author is made.
func ValidCommentAuthor(a Actor) bool {
	login := strings.TrimSuffix(a.Login, "[bot]")
	return validLogin(login) && validIDs(a.ID) && (a.Type == "User" || a.Type == "Bot") && (a.Type == "Bot" || login == a.Login)
}
func SameCommentAuthor(a, b Actor) bool {
	return ValidCommentAuthor(a) && ValidCommentAuthor(b) && a.ID == b.ID && a.Type == b.Type && strings.EqualFold(a.Login, b.Login)
}

const commentSelection = `__typename id body url databaseId author{__typename login ... on User{id} ... on Bot{id}}`

type wireComment struct {
	Type       string `json:"__typename"`
	ID         string
	Body       *string
	URL        string
	DatabaseID int64
	Author     *struct {
		Type      string `json:"__typename"`
		ID, Login string
	}
}

func (w wireComment) normalized(r CommentResource) (IssueComment, error) {
	u, e := url.Parse(w.URL)
	if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || w.DatabaseID <= 0 || u.Fragment != "issuecomment-"+strconv.FormatInt(w.DatabaseID, 10) || w.Type != "IssueComment" || !validIDs(w.ID) || w.Body == nil || w.Author == nil {
		return IssueComment{}, failure(Malformed)
	}
	path := "/" + r.Identity.Repository.Owner + "/" + r.Identity.Repository.Name
	suffix := "/issues/"
	if r.Kind == "pull_request" {
		suffix = "/pull/"
	}
	if u.Path != path+suffix+strconv.Itoa(r.Identity.Number) {
		return IssueComment{}, failure(Malformed)
	}
	a := Actor{ID: w.Author.ID, Login: w.Author.Login, Type: w.Author.Type}
	// Deleted authors cannot establish complete ownership evidence.
	if !ValidCommentAuthor(a) {
		return IssueComment{}, failure(Malformed)
	}
	return IssueComment{ID: w.ID, Body: *w.Body, DatabaseID: w.DatabaseID, Resource: r, Author: a}, nil
}
func (t *Transport) IssueComments(ctx context.Context, r CommentResource) (CommentList, error) {
	if !r.Valid() {
		return CommentList{}, failure(Permanent)
	}
	if _, err := t.nativeIdentity(ctx, r.Identity, r.Kind); err != nil {
		return CommentList{}, err
	}
	typ := "Issue"
	if r.Kind == "pull_request" {
		typ = "PullRequest"
	}
	nodes, err := t.commentPages(ctx, r, typ)
	if err != nil {
		return CommentList{}, err
	}
	out := CommentList{Resource: r, Complete: true, Comments: []IssueComment{}}
	seen := map[string]bool{}
	seenDatabase := map[int64]bool{}
	for _, raw := range nodes {
		var w wireComment
		if json.Unmarshal(raw, &w) != nil {
			return CommentList{}, failure(Malformed)
		}
		c, e := w.normalized(r)
		if e != nil {
			return CommentList{}, e
		}
		if seen[c.ID] || seenDatabase[c.DatabaseID] {
			return CommentList{}, failure(Malformed)
		}
		seen[c.ID] = true
		seenDatabase[c.DatabaseID] = true
		out.Comments = append(out.Comments, c)
	}
	if _, err = t.nativeIdentity(ctx, r.Identity, r.Kind); err != nil {
		return CommentList{}, err
	}
	return out, ctx.Err()
}
func (t *Transport) IssueComment(ctx context.Context, r CommentResource, id string) (IssueComment, error) {
	if !r.Valid() || !validIDs(id) {
		return IssueComment{}, failure(Permanent)
	}
	if _, err := t.nativeIdentity(ctx, r.Identity, r.Kind); err != nil {
		return IssueComment{}, err
	}
	var d struct{ Node *wireComment }
	if err := t.query(ctx, `query($id:ID!){node(id:$id){... on IssueComment{`+commentSelection+`}}}`, map[string]any{"id": id}, &d); err != nil {
		return IssueComment{}, err
	}
	if d.Node == nil {
		return IssueComment{}, failure(NotFound)
	}
	c, err := d.Node.normalized(r)
	if err == nil && c.ID != id {
		err = failure(Malformed)
	}
	return c, err
}
func (t *Transport) commentWrite(ctx context.Context, r CommentResource, id, body string) (IssueComment, error) {
	if !r.Valid() || len(body) == 0 || len(body) > 60000 {
		return IssueComment{}, failure(Permanent)
	}
	if _, err := t.nativeIdentity(ctx, r.Identity, r.Kind); err != nil {
		return IssueComment{}, err
	}
	op, input, field := "addComment", "AddCommentInput", "commentEdge{node{"
	vars := map[string]any{"subjectId": r.Identity.ID, "body": body}
	end := "}}"
	if id != "" {
		op, input, field = "updateIssueComment", "UpdateIssueCommentInput", "issueComment{"
		vars = map[string]any{"id": id, "body": body}
		end = "}"
	}
	var d struct {
		Result *struct {
			CommentEdge  *struct{ Node *wireComment }
			IssueComment *wireComment
		}
	}
	if err := t.query(ctx, `mutation($input:`+input+`!){result:`+op+`(input:$input){`+field+commentSelection+end+`}}`, map[string]any{"input": vars}, &d); err != nil {
		return IssueComment{}, err
	}
	if d.Result == nil {
		return IssueComment{}, failure(Malformed)
	}
	w := d.Result.IssueComment
	if id == "" && d.Result.CommentEdge != nil {
		w = d.Result.CommentEdge.Node
	}
	if w == nil {
		return IssueComment{}, failure(Malformed)
	}
	c, err := w.normalized(r)
	if err == nil && (c.Body != body || id != "" && c.ID != id) {
		err = failure(Malformed)
	}
	return c, err
}
func (t *Transport) CreateIssueComment(ctx context.Context, r CommentResource, body string) (IssueComment, error) {
	return t.commentWrite(ctx, r, "", body)
}
func (t *Transport) UpdateIssueComment(ctx context.Context, c IssueComment, body string) (IssueComment, error) {
	current, err := t.IssueComment(ctx, c.Resource, c.ID)
	if err != nil {
		return IssueComment{}, err
	}
	if current != c {
		return IssueComment{}, failure(Conflict)
	}
	return t.commentWrite(ctx, c.Resource, c.ID, body)
}

// commentPages requires stable total counts and exact parent identity on every
// page. Bounds are safe failures, never partial evidence of absence.
func (t *Transport) commentPages(ctx context.Context, r CommentResource, typ string) ([]json.RawMessage, error) {
	var result []json.RawMessage
	var cursor any
	seen := map[string]bool{}
	total := -1
	for {
		var d struct {
			Node *struct {
				wireIdentity
				Type     string `json:"__typename"`
				Comments *struct {
					Nodes      []json.RawMessage
					TotalCount *int
					PageInfo   *page
				}
			}
		}
		q := `query($id:ID!,$cursor:String){node(id:$id){__typename ... on ` + typ + `{` + identitySelection + ` comments(first:100,after:$cursor){totalCount nodes{` + commentSelection + `} pageInfo{hasNextPage endCursor}}}}}`
		if err := t.query(ctx, q, map[string]any{"id": r.Identity.ID, "cursor": cursor}, &d); err != nil {
			return nil, err
		}
		if d.Node == nil {
			return nil, failure(NotFound)
		}
		parent, err := d.Node.wireIdentity.normalized()
		p := d.Node.Comments
		if err != nil || parent != r.Identity || d.Node.Type != typ || p == nil || p.TotalCount == nil || *p.TotalCount < 0 || *p.TotalCount > 100000 || p.Nodes == nil || len(p.Nodes) > 100 || p.PageInfo == nil || p.PageInfo.HasNextPage == nil {
			return nil, failure(Malformed)
		}
		if total < 0 {
			total = *p.TotalCount
		} else if total != *p.TotalCount {
			return nil, failure(Conflict)
		}
		result = append(result, p.Nodes...)
		if len(result) > total {
			return nil, failure(Malformed)
		}
		if !*p.PageInfo.HasNextPage {
			if len(result) != total {
				return nil, failure(Malformed)
			}
			return result, nil
		}
		if len(p.Nodes) == 0 || len(result) >= total || p.PageInfo.EndCursor == "" || seen[p.PageInfo.EndCursor] {
			return nil, failure(Malformed)
		}
		seen[p.PageInfo.EndCursor] = true
		cursor = p.PageInfo.EndCursor
	}
}
