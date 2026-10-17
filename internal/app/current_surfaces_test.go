package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/github"
)

var runtimeBot = github.Actor{ID: "SYNTHETIC_CONTROLLER", Login: "synthetic-controller[bot]", Type: "Bot"}

// Stateful external surfaces, not workflow authority. Unspecified closes are
// ordinary native closes. Tests supply command comments explicitly when needed.
type runtimeSurfaces struct {
	commandDatabase                int64
	mu                             sync.Mutex
	client                         *github.Fake
	clock                          func() time.Time
	author                         github.Actor
	timelines                      map[string]github.IssueClosure
	links                          map[github.Identity][]github.Identity
	comments                       map[github.CommentResource][]github.IssueComment
	commentCreates, commentUpdates int
	commentFailure                 error
	lostCreate, lostUpdate         bool
}

func newRuntimeSurfaces(client *github.Fake) *runtimeSurfaces {
	return &runtimeSurfaces{client: client, clock: func() time.Time { return time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC) }, author: runtimeBot, timelines: map[string]github.IssueClosure{}, links: map[github.Identity][]github.Identity{}, comments: map[github.CommentResource][]github.IssueComment{}}
}
func (f *runtimeSurfaces) IssueClosure(ctx context.Context, id github.Identity) (github.IssueClosure, error) {
	i, err := f.client.Issue(ctx, github.Ref{Owner: id.Repository.Owner, Repository: id.Repository.Name, Number: id.Number})
	if err != nil {
		return github.IssueClosure{}, err
	}
	if i.Identity != id {
		return github.IssueClosure{}, github.ErrClosureChanged
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	facts, exists := f.timelines[id.ID]
	if !exists {
		facts = github.IssueClosure{Issue: id, State: "OPEN", Order: github.ClosureCursorOrder, Complete: true, CreatedAt: f.clock().Add(-24 * time.Hour), UpdatedAt: f.clock()}
	}
	if facts.Issue != id {
		return github.IssueClosure{}, github.ErrClosureChanged
	}
	if facts.State != i.State {
		kind := "ClosedEvent"
		if i.State == "OPEN" {
			kind = "ReopenedEvent"
		}
		n := len(facts.Items) + 1
		facts.Items = append(facts.Items, github.ClosureItem{Issue: id, ID: fmt.Sprintf("%s-EVENT-%d", id.ID, n), Kind: kind, Cursor: fmt.Sprintf("%s-CURSOR-%d", id.ID, n), CreatedAt: f.clock(), Actor: &github.Actor{ID: "SYNTHETIC_OPERATOR", Login: "operator", Type: "User"}})
		facts.State = i.State
		facts.UpdatedAt = f.clock()
	}
	if err := facts.Validate(id, i.State); err != nil {
		return github.IssueClosure{}, err
	}
	f.timelines[id.ID] = facts
	facts.Items = append([]github.ClosureItem(nil), facts.Items...)
	return facts, ctx.Err()
}
func (f *runtimeSurfaces) AuthenticatedCommentAuthor(ctx context.Context) (github.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.author, ctx.Err()
}
func (f *runtimeSurfaces) IssueComments(ctx context.Context, r github.CommentResource) (github.CommentList, error) {
	if !r.Valid() {
		return github.CommentList{}, errors.New("invalid comment resource")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return github.CommentList{Resource: r, Comments: append([]github.IssueComment{}, f.comments[r]...), Complete: true}, ctx.Err()
}
func (f *runtimeSurfaces) IssueComment(ctx context.Context, r github.CommentResource, id string) (github.IssueComment, error) {
	list, err := f.IssueComments(ctx, r)
	if err != nil {
		return github.IssueComment{}, err
	}
	for _, c := range list.Comments {
		if c.ID == id {
			return c, ctx.Err()
		}
	}
	return github.IssueComment{}, &github.Error{Category: github.NotFound}
}
func (f *runtimeSurfaces) CreateIssueComment(ctx context.Context, r github.CommentResource, body string) (github.IssueComment, error) {
	if err := ctx.Err(); err != nil {
		return github.IssueComment{}, err
	}
	if !r.Valid() {
		return github.IssueComment{}, errors.New("invalid comment resource")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentFailure != nil {
		return github.IssueComment{}, f.commentFailure
	}
	f.commentCreates++
	c := github.IssueComment{ID: fmt.Sprintf("COMMENT-%d", f.commentCreates), DatabaseID: int64(1000 + f.commentCreates), Resource: r, Body: body, Author: f.author}
	f.comments[r] = append(f.comments[r], c)
	if f.lostCreate {
		f.lostCreate = false
		return github.IssueComment{}, &github.Error{Category: github.Transient}
	}
	return c, nil
}
func (f *runtimeSurfaces) UpdateIssueComment(ctx context.Context, old github.IssueComment, body string) (github.IssueComment, error) {
	if err := ctx.Err(); err != nil {
		return github.IssueComment{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentFailure != nil {
		return github.IssueComment{}, f.commentFailure
	}
	for n, c := range f.comments[old.Resource] {
		if c.ID != old.ID {
			continue
		}
		if c != old || !github.SameCommentAuthor(c.Author, f.author) {
			return github.IssueComment{}, errors.New("unowned comment update")
		}
		c.Body = body
		f.comments[old.Resource][n] = c
		f.commentUpdates++
		if f.lostUpdate {
			f.lostUpdate = false
			return github.IssueComment{}, &github.Error{Category: github.Transient}
		}
		return c, nil
	}
	return github.IssueComment{}, &github.Error{Category: github.NotFound}
}
func (f *runtimeSurfaces) DevelopmentReferences(ctx context.Context, pr github.Identity) (github.DevelopmentReferences, error) {
	p, err := f.client.PullRequest(ctx, github.Ref{Owner: pr.Repository.Owner, Repository: pr.Repository.Name, Number: pr.Number})
	if err != nil {
		return github.DevelopmentReferences{}, err
	}
	if p.Identity != pr {
		return github.DevelopmentReferences{}, errors.New("wrong PR identity")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return github.DevelopmentReferences{PR: pr, Manual: append([]github.Identity{}, f.links[pr]...), Complete: true}, ctx.Err()
}
func (f *runtimeSurfaces) AddDevelopmentReference(ctx context.Context, pr, id github.Identity) error {
	p, err := f.client.PullRequest(ctx, github.Ref{Owner: pr.Repository.Owner, Repository: pr.Repository.Name, Number: pr.Number})
	if err != nil {
		return err
	}
	i, err := f.client.Issue(ctx, github.Ref{Owner: id.Repository.Owner, Repository: id.Repository.Name, Number: id.Number})
	if err != nil {
		return err
	}
	if p.Identity != pr || p.State != "OPEN" || i.Identity != id {
		return errors.New("invalid Development mutation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, old := range f.links[pr] {
		if old == id {
			return ctx.Err()
		}
	}
	f.links[pr] = append(f.links[pr], id)
	return ctx.Err()
}
func (f *runtimeSurfaces) RemoveDevelopmentReference(ctx context.Context, pr, id github.Identity) error {
	p, err := f.client.PullRequest(ctx, github.Ref{Owner: pr.Repository.Owner, Repository: pr.Repository.Name, Number: pr.Number})
	if err != nil {
		return err
	}
	if p.Identity != pr || p.State != "OPEN" {
		return errors.New("invalid Development removal")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, old := range f.links[pr] {
		if old == id {
			f.links[pr] = append(f.links[pr][:n], f.links[pr][n+1:]...)
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (f *runtimeSurfaces) closureComment(id github.Identity, body string) error {
	facts, err := f.IssueClosure(context.Background(), id)
	if err != nil {
		return err
	}
	closed, _, index, ok := facts.LatestClose()
	if !ok {
		return errors.New("comment fixture requires an explicit native close")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Each fixture command has its own database identity and remains immediately
	// before this specific close; it never changes the native Issue state.
	f.commandDatabase++
	db := int64(10000) + f.commandDatabase
	c := &github.ClosureComment{ID: fmt.Sprintf("%s-COMMAND-%d", id.ID, db), DatabaseID: db, Issue: id, Body: body, Author: closed.Actor, CreatedAt: closed.CreatedAt, UpdatedAt: closed.CreatedAt}
	items := append([]github.ClosureItem{}, facts.Items[:index]...)
	items = append(items, github.ClosureItem{Issue: id, ID: c.ID, Kind: "IssueComment", Cursor: c.ID + "-CURSOR", CreatedAt: c.CreatedAt, Comment: c})
	facts.Items = append(items, facts.Items[index:]...)
	if err := facts.Validate(id, "CLOSED"); err != nil {
		return err
	}
	f.timelines[id.ID] = facts
	r := github.CommentResource{Identity: id, Kind: "issue"}
	f.comments[r] = append(f.comments[r], github.IssueComment{ID: c.ID, DatabaseID: db, Resource: r, Body: body, Author: *c.Author})
	return nil
}
