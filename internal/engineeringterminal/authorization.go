package engineeringterminal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrAuthority  = errors.New("engineering terminal: missing terminal authority capability")
	ErrProvenance = errors.New("engineering terminal: invalid Force-Close provenance")
	ErrAmbiguous  = errors.New("engineering terminal: accepted closure evidence unavailable or changed")
)

type MergeAuthority interface {
	AuthorizesClosure(context.Context, storage.Resource, github.IssueClosure) (bool, error)
}
type AuthorizationStore interface {
	Event(context.Context, string) (storage.Event, error)
	Provenance(context.Context, string, string) (storage.Provenance, error)
	RecordForceClose(context.Context, storage.Provenance, storage.Provenance) error
}

const forceCommentNS = "force_close_comment_v1"
const forceCycleNS = "force_close_cycle_v1"

type ForceAcceptance struct {
	Version          int
	Project          string
	Issue            github.Identity
	CreatedAt        time.Time
	Comment          github.ClosureComment
	Closed, Boundary github.ClosureItem
	PrefixDigest     string
	Origin           storage.Resource
}

func forceKey(i github.Identity, id string) string { return i.Repository.ID + "/" + i.ID + "/" + id }
func forceCommand(body string) bool {
	const prefix = "Force-Close: "
	if !strings.HasPrefix(body, prefix) || len(body) > 4096 || strings.ContainsAny(body, "\r\n\x00") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(body, prefix)) != ""
}

// forceCandidate uses the stable unfiltered native cursor sequence. In
// particular a comment/close within one second is valid; timestamps do not
// supply adjacency or break ties. Any intervening node breaks the correlation.
func forceCandidate(f github.IssueClosure) (ForceAcceptance, bool, error) {
	var out ForceAcceptance
	if err := f.Validate(f.Issue, "CLOSED"); err != nil {
		return out, false, err
	}
	closed, boundary, index, ok := f.LatestClose()
	if !ok || index == 0 {
		return out, false, nil
	}
	previous := f.Items[index-1]
	c := previous.Comment
	if previous.Kind != "IssueComment" || c == nil || !forceCommand(c.Body) {
		return out, false, nil
	}
	if c.Author == nil || closed.Actor == nil {
		return out, false, github.ErrClosureIncomplete
	}
	if c.Author.ID == closed.Actor.ID && !github.SameCommentAuthor(*c.Author, *closed.Actor) || c.CreatedAt.After(closed.CreatedAt) {
		return out, false, github.ErrClosureIncomplete
	}
	if !github.SameCommentAuthor(*c.Author, *closed.Actor) || c.LastEditedAt != nil || c.Editor != nil || !c.CreatedAt.Equal(c.UpdatedAt) || c.CreatedAt.After(closed.CreatedAt) || c.CreatedAt.Before(f.CreatedAt) || closed.CloserID != "" {
		return out, false, nil
	}
	if boundary.ID != "" {
		boundaryIndex := -1
		for n, e := range f.Items {
			if e.ID == boundary.ID {
				boundaryIndex = n
			}
		}
		if boundaryIndex < 0 || index-1 <= boundaryIndex || c.CreatedAt.Before(boundary.CreatedAt) {
			return out, false, nil
		}
	}
	prefix, err := json.Marshal(f.Items[:index+1])
	if err != nil {
		return out, false, ErrProvenance
	}
	digest := sha256.Sum256(prefix)
	out = ForceAcceptance{Version: 1, Issue: f.Issue, CreatedAt: f.CreatedAt, Comment: *c, Closed: closed, Boundary: boundary, PrefixDigest: hex.EncodeToString(digest[:])}
	return out, true, nil
}
func (c *Controller) acceptedForce(ctx context.Context, f github.IssueClosure, delivery string, accept bool) (bool, error) {
	if missing(c.cfg.Store) {
		return false, ErrAuthority
	}
	closed, _, _, ok := f.LatestClose()
	if !ok {
		return false, github.ErrClosureChanged
	}
	cycleKey := forceKey(f.Issue, closed.ID)
	row, err := c.cfg.Store.Provenance(ctx, forceCycleNS, cycleKey)
	stored := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	current, candidate, err := forceCandidate(f)
	if err != nil {
		return false, err
	}
	current.Project = c.cfg.Project.ID
	if stored {
		var original ForceAcceptance
		if json.Unmarshal(row.Metadata, &original) != nil {
			return false, ErrProvenance
		}
		canonical, _ := json.Marshal(original)
		if original.Version != 1 || original.Project != c.cfg.Project.ID || original.Issue != f.Issue || original.Closed.ID != closed.ID || !bytes.Equal(canonical, row.Metadata) || row.Namespace != forceCycleNS || row.Key != cycleKey || row.CreatedAt.IsZero() {
			return false, ErrProvenance
		}
		comment, err := c.cfg.Store.Provenance(ctx, forceCommentNS, forceKey(f.Issue, original.Comment.ID))
		if err != nil {
			return false, ErrProvenance
		}
		if comment.Namespace != forceCommentNS || comment.Key != forceKey(f.Issue, original.Comment.ID) || comment.DeliveryID != row.DeliveryID || !comment.CreatedAt.Equal(row.CreatedAt) || !bytes.Equal(comment.Metadata, row.Metadata) {
			return false, ErrProvenance
		}
		origin, err := c.cfg.Store.Event(ctx, row.DeliveryID)
		if err != nil {
			return false, err
		}
		if origin.Resource == nil || origin.Delivery.ID != row.DeliveryID || origin.Delivery.ReceivedAt.IsZero() || *origin.Resource != original.Origin || row.CreatedAt.Before(origin.Delivery.ReceivedAt) || row.CreatedAt.Before(original.Closed.CreatedAt) {
			return false, ErrProvenance
		}
		// Accepted history cannot turn changed/deleted evidence into a second
		// acceptance. Fail closed without attributing an edit or guessing a repair.
		current.Origin = original.Origin
		if !candidate || !reflect.DeepEqual(current, original) {
			return false, ErrAmbiguous
		}
		return true, ctx.Err()
	}
	if !candidate {
		return false, nil
	}
	// A comment already consumed in any prior cycle cannot authorize a new one.
	if _, err := c.cfg.Store.Provenance(ctx, forceCommentNS, forceKey(f.Issue, current.Comment.ID)); err == nil {
		return false, ErrProvenance
	} else if !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	if !accept {
		return false, ErrAmbiguous
	}
	if delivery == "" {
		return false, ErrAuthority
	}
	origin, err := c.cfg.Store.Event(ctx, delivery)
	if err != nil {
		return false, err
	}
	if origin.Resource == nil || origin.Delivery.ID != delivery || origin.Delivery.ReceivedAt.IsZero() {
		return false, ErrProvenance
	}
	current.Origin = *origin.Resource
	at := c.cfg.Clock().UTC()
	if at.IsZero() || at.Before(closed.CreatedAt) || at.Before(origin.Delivery.ReceivedAt) {
		return false, ErrProvenance
	}
	// Reobserve current native identity, complete timeline and current comment
	// before committing acceptance. No remote mutation is performed here.
	again, err := c.cfg.Timeline.IssueClosure(ctx, f.Issue)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(f, again) {
		return false, github.ErrClosureChanged
	}
	data, err := json.Marshal(current)
	if err != nil {
		return false, ErrProvenance
	}
	comment := storage.Provenance{Namespace: forceCommentNS, Key: forceKey(f.Issue, current.Comment.ID), DeliveryID: delivery, Metadata: data, CreatedAt: at}
	cycle := storage.Provenance{Namespace: forceCycleNS, Key: cycleKey, DeliveryID: delivery, Metadata: data, CreatedAt: at}
	if err := c.cfg.Store.RecordForceClose(ctx, comment, cycle); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			return false, ErrProvenance
		}
		return false, err
	}
	return true, ctx.Err()
}
func (c *Controller) authorize(ctx context.Context, graph ec.Context, r storage.Resource, i github.Identity, delivery string, accept bool) (Decision, github.IssueClosure, error) {
	decision, err := Evaluate(graph, r)
	if err != nil {
		return decision, github.IssueClosure{}, err
	}
	if missing(c.cfg.Timeline) || missing(c.cfg.Merges) {
		return decision, github.IssueClosure{}, ErrAuthority
	}
	facts, err := c.cfg.Timeline.IssueClosure(ctx, i)
	if err != nil {
		return decision, facts, err
	}
	state := ""
	var kind sp.IssueType
	for _, node := range graph.Nodes {
		if node.Resource == r {
			state = string(node.State)
			kind = node.Type
		}
	}
	if err := facts.Validate(i, state); err != nil {
		return decision, facts, err
	}
	if state != "CLOSED" {
		return decision, facts, nil
	}
	if kind == sp.Phase {
		if decision.Reopen {
			return decision, facts, nil
		}
		closed, _, _, ok := facts.LatestClose()
		if !ok || closed.Actor == nil {
			return decision, facts, github.ErrClosureIncomplete
		}
		if closed.CloserID != "" {
			return Decision{Reopen: true, Rejection: &Rejection{Resource: r, Reason: UnauthorizedClosure}}, facts, nil
		}
		return decision, facts, nil
	}
	merged, err := c.cfg.Merges.AuthorizesClosure(ctx, r, facts)
	if err != nil {
		return decision, facts, err
	}
	if merged {
		return Decision{Status: config.Done}, facts, nil
	}
	forced, err := c.acceptedForce(ctx, facts, delivery, accept)
	if err != nil {
		return decision, facts, err
	}
	if forced {
		return Decision{Status: config.Done}, facts, nil
	}
	return decision, facts, nil
}
