package workflowfeedback

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
)

type feedbackRemote struct {
	current                                    Current
	author                                     github.Actor
	comments                                   []github.IssueComment
	writes, refreshes, lists, lookups, authors int
	incomplete                                 bool
	exactChange                                bool
	failWrite                                  error
	refreshError                               error
	malformedReceipt                           bool
	refreshHook                                func(*feedbackRemote)
	listHook                                   func(*feedbackRemote)
	authorHook                                 func(*feedbackRemote)
	writeHook                                  func(*feedbackRemote)
}

func remote(kind string) *feedbackRemote {
	r := feedbackResource(kind)
	var v Violation
	if kind == "issue" {
		v, _ = LifecycleRejection(ManualRejection{r, ActiveBlocker})
	} else {
		v, _ = BindingRejection(r, prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingTarget})
	}
	return &feedbackRemote{current: Current{r, true, []Violation{v}}, author: github.Actor{ID: "BOT", Login: "controller[bot]", Type: "Bot"}}
}
func (f *feedbackRemote) CurrentFeedback(ctx context.Context, r Resource) (Current, error) {
	f.refreshes++
	if f.refreshHook != nil {
		f.refreshHook(f)
	}
	if f.refreshError != nil {
		return Current{}, f.refreshError
	}
	return f.current, ctx.Err()
}
func (f *feedbackRemote) FeedbackAuthor(ctx context.Context) (github.Actor, error) {
	f.authors++
	if f.authorHook != nil {
		f.authorHook(f)
	}
	return f.author, ctx.Err()
}
func (f *feedbackRemote) IssueComments(ctx context.Context, r Resource) (github.CommentList, error) {
	f.lists++
	if f.listHook != nil {
		f.listHook(f)
	}
	return github.CommentList{Resource: r, Complete: !f.incomplete, Comments: append([]github.IssueComment(nil), f.comments...)}, ctx.Err()
}
func (f *feedbackRemote) IssueComment(ctx context.Context, r Resource, id string) (github.IssueComment, error) {
	f.lookups++
	for _, c := range f.comments {
		if c.ID == id {
			if f.exactChange {
				c.Body += " edit"
			}
			return c, ctx.Err()
		}
	}
	return github.IssueComment{}, &github.Error{Category: github.NotFound}
}
func (f *feedbackRemote) CreateIssueComment(ctx context.Context, r Resource, body string) (github.IssueComment, error) {
	c := github.IssueComment{ID: fmt.Sprintf("C%d", len(f.comments)+1), DatabaseID: int64(len(f.comments) + 1), Body: body, Resource: r, Author: f.author}
	f.comments = append(f.comments, c)
	return f.wrote(c)
}
func (f *feedbackRemote) UpdateIssueComment(ctx context.Context, c github.IssueComment, body string) (github.IssueComment, error) {
	for i := range f.comments {
		if f.comments[i] == c {
			c.Body = body
			f.comments[i] = c
			return f.wrote(c)
		}
	}
	return github.IssueComment{}, &github.Error{Category: github.Conflict}
}
func (f *feedbackRemote) wrote(c github.IssueComment) (github.IssueComment, error) {
	f.writes++
	if f.writeHook != nil {
		f.writeHook(f)
	}
	if f.malformedReceipt {
		c.Body = "malformed"
	}
	return c, f.failWrite
}
func (f *feedbackRemote) controller(t *testing.T) *Controller {
	t.Helper()
	c, e := New(Config{Refresh: f, Author: f, Reader: f, Writer: f})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func (f *feedbackRemote) managed() github.IssueComment {
	body, _ := Render(f.current)
	return github.IssueComment{ID: "C1", DatabaseID: 1, Resource: f.current.Resource, Author: f.author, Body: body.Body}
}
func TestReconciliationLiveStateTransitions(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		t.Run(kind, func(t *testing.T) {
			f := remote(kind)
			c := f.controller(t)
			r := f.current.Resource
			run := func(want int) {
				t.Helper()
				if e := c.Reconcile(context.Background(), r); e != nil {
					t.Fatal(e)
				}
				if f.writes != want {
					t.Fatalf("writes=%d want=%d", f.writes, want)
				}
			}
			original := f.current.Violations
			f.current.Violations = nil
			run(0)
			f.current.Violations = original
			run(1)
			run(1)
			var extra Violation
			if kind == "issue" {
				extra, _ = LifecycleRejection(ManualRejection{r, InactiveParent})
			} else {
				extra, _ = BindingRejection(r, prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.HeadMismatch})
			}
			f.current.Violations = append(append([]Violation(nil), original...), extra)
			run(2)
			f.current.Violations = []Violation{extra, original[0], extra}
			run(2)
			f.current.Violations = []Violation{extra}
			run(3)
			f.current.Violations = nil
			run(4)
			run(4)
			if len(f.comments) != 1 {
				t.Fatal(f.comments)
			}
		})
	}
}
func TestOwnershipFailsClosed(t *testing.T) {
	for _, variant := range []string{"human marker", "other bot", "author node", "truncated marker", "malformed marker", "unknown version", "duplicate managed", "duplicate id", "duplicate database", "wrong resource", "author login", "author type", "missing author", "incomplete", "exact differs", "deleted author"} {
		t.Run(variant, func(t *testing.T) {
			f := remote("pull_request")
			comment := f.managed()
			f.comments = []github.IssueComment{comment}
			wantError := true
			switch variant {
			case "human marker":
				f.comments[0].Author = github.Actor{ID: "H", Login: "human", Type: "User"}
				wantError = false
			case "other bot":
				f.comments[0].Author.ID = "OTHER"
				f.comments[0].Author.Login = "unrelated[bot]"
				wantError = false
			case "author node":
				f.comments[0].Author.ID = "OTHER"
			case "truncated marker":
				f.comments[0].Body = "<!-- parametron-workflow:managed-feedback -->\nold"
			case "malformed marker":
				f.comments[0].Body = "prefix " + comment.Body
			case "unknown version":
				f.comments[0].Body = "<!-- parametron-workflow:managed-feedback:v2 -->\nold"
			case "duplicate managed":
				x := comment
				x.ID = "C2"
				x.DatabaseID = 2
				f.comments = append(f.comments, x)
			case "duplicate id":
				x := comment
				x.DatabaseID = 2
				f.comments = append(f.comments, x)
			case "duplicate database":
				x := comment
				x.ID = "C2"
				f.comments = append(f.comments, x)
			case "wrong resource":
				f.comments[0].Resource.Identity.Number++
			case "author login":
				f.comments[0].Author.Login = "impostor"
			case "author type":
				f.comments[0].Author.Type = "User"
				f.comments[0].Author.Login = "controller"
			case "missing author":
				f.author = github.Actor{}
			case "incomplete":
				f.incomplete = true
			case "exact differs":
				f.exactChange = true
			case "deleted author":
				f.comments[0].Author = github.Actor{}
			}
			before := f.comments[0]
			e := f.controller(t).Reconcile(context.Background(), f.current.Resource)
			if wantError {
				if e == nil || f.writes != 0 {
					t.Fatal(e, f.writes)
				}
			} else if e != nil || f.writes != 1 || f.comments[0] != before {
				t.Fatal("unrelated discussion changed", e, f.comments)
			}
		})
	}
}
func TestFreshnessBarriersPreventWrites(t *testing.T) {
	for _, boundary := range []string{"second policy", "final policy", "second list", "author", "cancel", "update detail"} {
		t.Run(boundary, func(t *testing.T) {
			f := remote("pull_request")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch boundary {
			case "second policy", "final policy":
				n := 2
				if boundary == "final policy" {
					n = 3
				}
				f.refreshHook = func(f *feedbackRemote) {
					if f.refreshes == n {
						f.current.Violations = nil
					}
				}
			case "second list":
				f.listHook = func(f *feedbackRemote) {
					if f.lists == 2 {
						f.comments = []github.IssueComment{f.managed()}
					}
				}
			case "author":
				f.authorHook = func(f *feedbackRemote) {
					if f.authors == 2 {
						f.author.ID = "OTHER"
					}
				}
			case "cancel":
				f.refreshHook = func(f *feedbackRemote) {
					if f.refreshes == 3 {
						cancel()
					}
				}
			case "update detail":
				old := f.managed()
				old.Body += " old"
				f.comments = []github.IssueComment{old}
				f.refreshHook = func(f *feedbackRemote) {
					if f.refreshes == 3 {
						v, _ := BindingRejection(f.current.Resource, prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.HeadMismatch})
						f.current.Violations = []Violation{v}
					}
				}
			}
			e := f.controller(t).Reconcile(ctx, f.current.Resource)
			if e == nil || f.writes != 0 {
				t.Fatal(e, f.writes)
			}
		})
	}
}
func TestUncertainWritesRecoverFromLiveState(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		for _, update := range []bool{false, true} {
			for _, fault := range []string{"transient", "rate limited", "canceled", "malformed receipt", "policy withdrawn"} {
				t.Run(fmt.Sprintf("%s/update=%t/%s", kind, update, fault), func(t *testing.T) {
					f := remote(kind)
					if update {
						x := f.managed()
						x.Body += " old"
						f.comments = []github.IssueComment{x}
					}
					switch fault {
					case "transient":
						f.failWrite = &github.Error{Category: github.Transient}
					case "rate limited":
						f.failWrite = &github.Error{Category: github.RateLimited}
					case "canceled":
						f.failWrite = context.Canceled
					case "malformed receipt":
						f.malformedReceipt = true
					case "policy withdrawn":
						f.writeHook = func(f *feedbackRemote) { f.current.Violations = nil }
					}
					e := f.controller(t).Reconcile(context.Background(), f.current.Resource)
					if e == nil || f.writes != 1 {
						t.Fatal(e, f.writes)
					}
					if fault == "canceled" && !errors.Is(e, context.Canceled) {
						t.Fatal(e)
					}
					f.failWrite = nil
					f.malformedReceipt = false
					f.writeHook = nil
					// A new controller represents loss of all process-local mutation planning.
					c := f.controller(t)
					if e = c.Reconcile(context.Background(), f.current.Resource); e != nil {
						t.Fatal(e)
					}
					want := 1
					if fault == "policy withdrawn" {
						want = 2
					}
					if f.writes != want || len(f.comments) != 1 {
						t.Fatal(f.writes, f.comments)
					}
				})
			}
		}
	}
}
func TestSameControllerSerializesDuplicateDeliveries(t *testing.T) {
	f := remote("pull_request")
	c := f.controller(t)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- c.Reconcile(context.Background(), f.current.Resource) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if f.writes != 1 || len(f.comments) != 1 {
		t.Fatal(f.writes, f.comments)
	}
}
func TestRefreshFailureIsNotARejection(t *testing.T) {
	f := remote("issue")
	f.refreshError = &github.Error{Category: github.Transient}
	if e := f.controller(t).Reconcile(context.Background(), f.current.Resource); !errors.Is(e, f.refreshError) || f.writes != 0 {
		t.Fatal(e, f.writes)
	}
	f.refreshError = nil
	f.current.Complete = false
	if e := f.controller(t).Reconcile(context.Background(), f.current.Resource); !errors.Is(e, ErrInvalid) || f.writes != 0 {
		t.Fatal(e, f.writes)
	}
}

func TestPostWriteObservationFailsSafely(t *testing.T) {
	for _, fault := range []string{"missing", "ambiguous", "changed body", "policy error"} {
		t.Run(fault, func(t *testing.T) {
			f := remote("issue")
			f.writeHook = func(f *feedbackRemote) {
				switch fault {
				case "missing":
					f.comments = nil
				case "ambiguous":
					copy := f.comments[0]
					copy.ID = "OTHER"
					copy.DatabaseID = 2
					f.comments = append(f.comments, copy)
				case "changed body":
					f.comments[0].Body += " edited"
				case "policy error":
					f.current.Complete = false
				}
			}
			e := f.controller(t).Reconcile(context.Background(), f.current.Resource)
			if e == nil || f.writes != 1 {
				t.Fatal(e, f.writes)
			}
		})
	}
}
func TestConsumerCapabilitiesAreOnlyFeedback(t *testing.T) {
	for _, tc := range []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeOf((*Refresh)(nil)).Elem(), []string{"CurrentFeedback"}},
		{reflect.TypeOf((*AuthorIdentity)(nil)).Elem(), []string{"FeedbackAuthor"}},
		{reflect.TypeOf((*github.CommentReader)(nil)).Elem(), []string{"IssueComment", "IssueComments"}},
		{reflect.TypeOf((*github.CommentWriter)(nil)).Elem(), []string{"CreateIssueComment", "UpdateIssueComment"}},
	} {
		if tc.typ.NumMethod() != len(tc.want) {
			t.Fatal("unexpected mutation capability", tc.typ)
		}
		for i, name := range tc.want {
			if tc.typ.Method(i).Name != name {
				t.Fatal(tc.typ.Method(i))
			}
		}
	}
}
func TestControllerRejectsMissingDependenciesAndCanceledWork(t *testing.T) {
	f := remote("issue")
	for _, cfg := range []Config{{}, {Refresh: f, Author: f, Reader: f}, {Refresh: f, Author: f, Writer: f}, {Refresh: f, Reader: f, Writer: f}, {Author: f, Reader: f, Writer: f}} {
		if _, e := New(cfg); !errors.Is(e, ErrInvalid) {
			t.Fatal(e)
		}
	}
	var absent *feedbackRemote
	if _, e := New(Config{Refresh: absent, Author: f, Reader: f, Writer: f}); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := f.controller(t).Reconcile(ctx, f.current.Resource); !errors.Is(e, context.Canceled) || f.writes != 0 {
		t.Fatal(e, f.writes)
	}
}
