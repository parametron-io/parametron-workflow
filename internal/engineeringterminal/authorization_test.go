package engineeringterminal

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type closureReaderFunc func(context.Context, github.Identity) (github.IssueClosure, error)

func (f closureReaderFunc) IssueClosure(ctx context.Context, i github.Identity) (github.IssueClosure, error) {
	return f(ctx, i)
}

func forceFacts() github.IssueClosure {
	i := github.Identity{ID: "SYNTHETIC_ISSUE", Number: 1, Repository: github.Repository{ID: "SYNTHETIC_REPO", Owner: "synthetic-org", Name: "repo"}}
	at := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	actor := &github.Actor{ID: "SYNTHETIC_USER", Login: "operator", Type: "User"}
	comment := &github.ClosureComment{ID: "COMMENT1", DatabaseID: 11, Issue: i, Body: "Force-Close: abandoned work", Author: actor, CreatedAt: at, UpdatedAt: at}
	return github.IssueClosure{Issue: i, State: "CLOSED", Order: github.ClosureCursorOrder, CreatedAt: at.Add(-time.Hour), UpdatedAt: at, Complete: true, Items: []github.ClosureItem{
		{Issue: i, ID: comment.ID, Kind: "IssueComment", Cursor: "CURSOR1", CreatedAt: at, Comment: comment},
		{Issue: i, ID: "CLOSE1", Kind: "ClosedEvent", Cursor: "CURSOR2", CreatedAt: at, Actor: actor},
	}}
}
func TestForceCandidateIndependentCorrelation(t *testing.T) {
	for _, name := range []string{"same-second", "ordinary-comment", "empty-reason", "embedded-command", "edited", "updated", "deleted-comment", "different-actor", "same-login-other-id", "intervening-event", "incomplete", "unordered", "missing-author", "missing-closer", "wrong-issue", "wrong-comment-node", "missing-database", "keyword-close", "open-only", "reopened-cycle"} {
		t.Run(name, func(t *testing.T) {
			f := forceFacts()
			switch name {
			case "ordinary-comment":
				f.Items[0].Comment.Body = "closing this issue"
			case "empty-reason":
				f.Items[0].Comment.Body = "Force-Close:  "
			case "embedded-command":
				f.Items[0].Comment.Body = "Please use Force-Close: reason"
			case "edited":
				at := f.Items[0].CreatedAt
				f.Items[0].Comment.LastEditedAt = &at
			case "updated":
				f.Items[0].Comment.UpdatedAt = f.Items[0].CreatedAt.Add(time.Second)
			case "deleted-comment":
				f.Items = f.Items[1:]
			case "different-actor":
				f.Items[1].Actor = &github.Actor{ID: "OTHER", Login: "other", Type: "User"}
			case "same-login-other-id":
				a := *f.Items[1].Actor
				a.ID = "OTHER"
				f.Items[1].Actor = &a
			case "intervening-event":
				f.Items = append(f.Items[:1], append([]github.ClosureItem{{Issue: f.Issue, ID: "LABEL", Kind: "LabeledEvent", Cursor: "INTERVENING"}}, f.Items[1:]...)...)
			case "incomplete":
				f.Complete = false
			case "unordered":
				f.Order = "timestamp-sort"
			case "missing-author":
				f.Items[0].Comment.Author = nil
			case "missing-closer":
				f.Items[1].Actor = nil
			case "wrong-issue":
				f.Items[0].Comment.Issue.ID = "OTHER"
			case "wrong-comment-node":
				f.Items[0].Comment.ID = "OTHER"
			case "missing-database":
				f.Items[0].Comment.DatabaseID = 0
			case "keyword-close":
				f.Items[1].CloserID = "PR"
				f.Items[1].CloserKind = "PullRequest"
			case "open-only":
				f.Items = f.Items[:1]
				f.State = "OPEN"
			case "reopened-cycle":
				f.Items = append(f.Items, github.ClosureItem{Issue: f.Issue, ID: "REOPEN", Kind: "ReopenedEvent", Cursor: "C3", CreatedAt: f.UpdatedAt, Actor: f.Items[1].Actor}, github.ClosureItem{Issue: f.Issue, ID: "CLOSE2", Kind: "ClosedEvent", Cursor: "C4", CreatedAt: f.UpdatedAt, Actor: f.Items[1].Actor})
			}
			accepted, ok, err := forceCandidate(f)
			if name == "same-second" {
				if err != nil || !ok || accepted.Comment.DatabaseID != 11 || accepted.Closed.ID != "CLOSE1" {
					t.Fatal("same-second ordered evidence rejected", err)
				}
			} else if ok {
				t.Fatal("invalid correlation authorized")
			}
		})
	}
}
func forceStoreController(t *testing.T, path string, facts *github.IssueClosure) (*Controller, *storage.Store) {
	t.Helper()
	ctx := context.Background()
	s, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := storage.Resource{Owner: facts.Issue.Repository.Owner, Repository: facts.Issue.Repository.Name, Kind: "issue", Number: int64(facts.Issue.Number), NodeID: facts.Issue.ID}
	_, err = s.InsertDelivery(ctx, storage.Delivery{ID: "synthetic-delivery", EventName: "issues", Payload: []byte(`{"action":"closed"}`), ReceivedAt: facts.CreatedAt, Resource: &r})
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{cfg: Config{Project: config.ResolvedProject{ID: "ENG"}, Store: s, Clock: func() time.Time { return facts.UpdatedAt.Add(time.Hour) }, Timeline: closureReaderFunc(func(ctx context.Context, i github.Identity) (github.IssueClosure, error) {
		if i != facts.Issue {
			t.Error("wrong issue reread")
		}
		return *facts, ctx.Err()
	})}}, s
}
func TestForceAcceptanceSQLiteRestartAndChangedEvidence(t *testing.T) {
	f := forceFacts()
	path := filepath.Join(t.TempDir(), "force.sqlite")
	c, s := forceStoreController(t, path, &f)
	ctx := context.Background()
	if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); err != nil || !ok {
		t.Fatal("acceptance failed", err)
	}
	before, err := s.Provenance(ctx, forceCycleNS, forceKey(f.Issue, "CLOSE1"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	c, s = forceStoreController(t, path, &f)
	for range 3 {
		if ok, err := c.acceptedForce(ctx, f, "", false); err != nil || !ok {
			t.Fatal("durable retry failed", err)
		}
	}
	after, err := s.Provenance(ctx, forceCycleNS, forceKey(f.Issue, "CLOSE1"))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("retry changed acceptance")
	}
	f.Items[0].Comment.Body = "Force-Close: changed reason"
	if ok, err := c.acceptedForce(ctx, f, "", false); ok || !errors.Is(err, ErrAmbiguous) {
		t.Fatal("changed evidence authorized", err)
	}
}
func nextForceCycle(f github.IssueClosure, commentID string, db int64) github.IssueClosure {
	actor := f.Items[len(f.Items)-1].Actor
	at := f.UpdatedAt.Add(time.Minute)
	f.Items = append(append([]github.ClosureItem{}, f.Items...), github.ClosureItem{Issue: f.Issue, ID: "REOPEN1", Kind: "ReopenedEvent", Cursor: "C3", CreatedAt: at, Actor: actor})
	at = at.Add(time.Minute)
	c := &github.ClosureComment{ID: commentID, DatabaseID: db, Issue: f.Issue, Body: "Force-Close: second cycle", Author: actor, CreatedAt: at, UpdatedAt: at}
	f.Items = append(f.Items, github.ClosureItem{Issue: f.Issue, ID: commentID, Kind: "IssueComment", Cursor: "C4", CreatedAt: at, Comment: c}, github.ClosureItem{Issue: f.Issue, ID: "CLOSE2", Kind: "ClosedEvent", Cursor: "C5", CreatedAt: at, Actor: actor})
	f.UpdatedAt = at
	return f
}
func TestForceAcceptanceRequiresNewCycleComment(t *testing.T) {
	f := forceFacts()
	c, s := forceStoreController(t, filepath.Join(t.TempDir(), "force.sqlite"), &f)
	ctx := context.Background()
	if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); !ok || err != nil {
		t.Fatal(err)
	}
	first, _ := s.Provenance(ctx, forceCycleNS, forceKey(f.Issue, "CLOSE1"))
	f = nextForceCycle(f, "COMMENT2", 12)
	if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); !ok || err != nil {
		t.Fatal("new cycle rejected", err)
	}
	if old, err := s.Provenance(ctx, forceCycleNS, forceKey(f.Issue, "CLOSE1")); err != nil || !reflect.DeepEqual(first, old) {
		t.Fatal("historical authorization changed")
	}
}

// A database identity already consumed under another node identity must never
// be accepted again, even after the original comment disappears from GitHub.
func TestForceAcceptanceRejectsReusedDatabaseIdentityAfterRestart(t *testing.T) {
	f := forceFacts()
	path := filepath.Join(t.TempDir(), "force.sqlite")
	c, s := forceStoreController(t, path, &f)
	ctx := context.Background()
	if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); !ok || err != nil {
		t.Fatal(err)
	}
	s.Close()
	c, _ = forceStoreController(t, path, &f)
	f = nextForceCycle(f, "LOOKALIKE_NODE", 11)
	f.Items = f.Items[1:] // deleted original comment; historical close remains
	if err := f.Validate(f.Issue, "CLOSED"); err != nil {
		t.Fatal("test timeline is structurally invalid", err)
	}
	if ok, err := c.acceptedForce(ctx, f, "synthetic-delivery", true); ok || err == nil {
		t.Fatal("consumed comment database identity accepted under a different node in a later closure cycle")
	}
}

type terminalMergeFunc func(context.Context, storage.Resource, github.IssueClosure) (bool, error)

func (f terminalMergeFunc) AuthorizesClosure(ctx context.Context, r storage.Resource, n github.IssueClosure) (bool, error) {
	return f(ctx, r, n)
}
