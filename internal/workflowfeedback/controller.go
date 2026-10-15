package workflowfeedback

import (
	"context"
	"reflect"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/github"
)

// Refresh must reevaluate all supported authorities for this exact resource,
// including withdrawal/resolution of prior corrections. Complete is mandatory.
// It must not replay lifecycle effects. No webhook or raw observation adapter is
// provided: producer orchestration must establish rejected-intent authority.
type Refresh interface {
	CurrentFeedback(context.Context, Resource) (Current, error)
}

// AuthorIdentity verifies a trusted configured/current stable account identity.
// App and personal-token deployments must supply their own correct identity.
type AuthorIdentity interface {
	FeedbackAuthor(context.Context) (github.Actor, error)
}
type Config struct {
	Refresh Refresh
	Author  AuthorIdentity
	Reader  github.CommentReader
	Writer  github.CommentWriter
}
type Controller struct {
	cfg   Config
	lanes [64]chan struct{}
}

func nilValue(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan:
		return r.IsNil()
	}
	return false
}
func New(c Config) (*Controller, error) {
	for _, v := range []any{c.Refresh, c.Author, c.Reader, c.Writer} {
		if nilValue(v) {
			return nil, ErrInvalid
		}
	}
	out := &Controller{cfg: c}
	for i := range out.lanes {
		out.lanes[i] = make(chan struct{}, 1)
	}
	return out, nil
}
func (c *Controller) current(ctx context.Context, r Resource) (Content, error) {
	d, err := c.cfg.Refresh.CurrentFeedback(ctx, r)
	if err != nil {
		return Content{}, err
	}
	if d.Resource != r {
		return Content{}, ErrInvalid
	}
	return Render(d)
}
func (c *Controller) lookup(ctx context.Context, r Resource, a github.Actor) (*github.IssueComment, error) {
	list, err := c.cfg.Reader.IssueComments(ctx, r)
	if err != nil {
		return nil, err
	}
	if !list.Complete || list.Resource != r {
		return nil, ErrAmbiguous
	}
	seen := map[string]bool{}
	seenDatabase := map[int64]bool{}
	var managed *github.IssueComment
	for _, x := range list.Comments {
		if x.Resource != r || x.ID == "" || x.DatabaseID <= 0 || seen[x.ID] || seenDatabase[x.DatabaseID] || !github.ValidCommentAuthor(x.Author) {
			return nil, ErrAmbiguous
		}
		seen[x.ID] = true
		seenDatabase[x.DatabaseID] = true
		if x.Author.ID == a.ID && !github.SameCommentAuthor(a, x.Author) {
			return nil, ErrOwnership
		}
		if !github.SameCommentAuthor(a, x.Author) {
			continue
		}
		// Controller comments with malformed/unknown managed versions cannot establish
		// absence. Human marker copies are never adopted.
		if strings.Contains(x.Body, "parametron-workflow:managed-feedback:") {
			if !strings.HasPrefix(x.Body, Marker+"\n") || strings.Count(x.Body, Marker) != 1 {
				return nil, ErrOwnership
			}
			if managed != nil {
				return nil, ErrAmbiguous
			}
			copy := x
			managed = &copy
		}
	}
	if managed != nil {
		exact, err := c.cfg.Reader.IssueComment(ctx, r, managed.ID)
		if err != nil {
			return nil, err
		}
		if exact != *managed {
			return nil, ErrChanged
		}
	}
	return managed, ctx.Err()
}

// Reconcile uses bounded striped process-local serialization. Every retry starts
// from live evidence, including after an indeterminate write. Independent
// controllers/processes require external coordination; no distributed guarantee
// or atomic policy/comment snapshot is claimed.
func (c *Controller) Reconcile(ctx context.Context, r Resource) error {
	if !valid(r) {
		return ErrInvalid
	}
	hash := uint64(1469598103934665603)
	for _, b := range []byte(r.Identity.ID) {
		hash = (hash ^ uint64(b)) * 1099511628211
	}
	lane := c.lanes[hash%uint64(len(c.lanes))]
	select {
	case lane <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-lane }()
	initial, err := c.current(ctx, r)
	if err != nil {
		return err
	}
	author, err := c.cfg.Author.FeedbackAuthor(ctx)
	if err != nil {
		return err
	}
	if !github.ValidCommentAuthor(author) {
		return ErrOwnership
	}
	existing, err := c.lookup(ctx, r, author)
	if err != nil {
		return err
	}
	next, err := c.current(ctx, r)
	if err != nil {
		return err
	}
	if next != initial {
		return ErrChanged
	}
	if existing == nil && !next.Active {
		return ctx.Err()
	}
	if existing != nil && existing.Body == next.Body {
		return ctx.Err()
	}
	// Repeat complete lookup after the policy refresh. A lost prior response,
	// concurrent comment, deletion or edit cannot authorize a speculative create.
	latest, err := c.lookup(ctx, r, author)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(existing, latest) {
		return ErrChanged
	}
	verifiedAuthor, err := c.cfg.Author.FeedbackAuthor(ctx)
	if err != nil {
		return err
	}
	if !github.SameCommentAuthor(author, verifiedAuthor) {
		return ErrOwnership
	}
	final, err := c.current(ctx, r)
	if err != nil {
		return err
	}
	if final != next {
		return ErrChanged
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	var receipt github.IssueComment
	if latest == nil {
		receipt, err = c.cfg.Writer.CreateIssueComment(ctx, r, final.Body)
	} else {
		receipt, err = c.cfg.Writer.UpdateIssueComment(ctx, *latest, final.Body)
	}
	if err != nil {
		return err
	}
	if receipt.Resource != r || receipt.ID == "" || receipt.Body != final.Body || !github.SameCommentAuthor(author, receipt.Author) || latest != nil && receipt.ID != latest.ID {
		return ErrOwnership
	}
	observed, err := c.lookup(ctx, r, author)
	if err != nil {
		return err
	}
	if observed == nil || *observed != receipt {
		return ErrAmbiguous
	}
	// A concurrent policy withdrawal after a successful write is retryable; the
	// next call resolves the same verified comment without lifecycle replay.
	current, err := c.current(ctx, r)
	if err != nil {
		return err
	}
	if current != final {
		return ErrChanged
	}
	return ctx.Err()
}
