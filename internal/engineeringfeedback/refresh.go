// Package engineeringfeedback assembles current typed producer facts. It has no
// lifecycle writer, and comment content never supplies policy evidence.
package engineeringfeedback

import (
	"context"
	"errors"
	"reflect"
	"strings"

	et "github.com/parametron-io/parametron-workflow/internal/engineeringterminal"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	pc "github.com/parametron-io/parametron-workflow/internal/prcompanion"
	"github.com/parametron-io/parametron-workflow/internal/prdevelopment"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	wf "github.com/parametron-io/parametron-workflow/internal/workflowfeedback"
)

var (
	ErrIncomplete           = errors.New("engineering feedback: incomplete current producer evidence")
	ErrUnsupportedBugTarget = errors.New("engineering feedback: Bug Tracker target is outside Engineering scope")
)

type Reader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
}
type Binding interface {
	Evaluate(context.Context, storage.Resource) (prbinding.Decision, error)
}
type Capacity interface {
	CurrentInput(context.Context) (pc.Input, error)
}

type Config struct {
	Repositories []github.Repository
	Reader       Reader
	Binding      Binding
	Capacity     Capacity
	Development  *prdevelopment.Controller
	Terminal     *et.Controller
}
type Refresh struct{ cfg Config }

func absent(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return r.IsNil()
	}
	return false
}

func New(c Config) (*Refresh, error) {
	if len(c.Repositories) == 0 || absent(c.Reader) || absent(c.Binding) || absent(c.Capacity) || c.Development == nil || c.Terminal == nil {
		return nil, wf.ErrInvalid
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for _, r := range c.Repositories {
		name := strings.ToLower(r.Owner + "/" + r.Name)
		if !github.ValidBranchRepository(r) || names[name] || ids[r.ID] {
			return nil, wf.ErrInvalid
		}
		names[name], ids[r.ID] = true, true
	}
	c.Repositories = append([]github.Repository(nil), c.Repositories...)
	return &Refresh{c}, nil
}
func Resource(i github.Identity, kind string) storage.Resource {
	return storage.Resource{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Kind: kind, Number: int64(i.Number), NodeID: i.ID}
}
func (f *Refresh) CurrentFeedback(ctx context.Context, r wf.Resource) (wf.Current, error) {
	out := wf.Current{Resource: r}
	if !r.Valid() {
		return out, wf.ErrInvalid
	}
	configured := false
	for _, repo := range f.cfg.Repositories {
		if repo == r.Identity.Repository {
			configured = true
		}
	}
	if !configured {
		return out, wf.ErrInvalid
	}
	resource := Resource(r.Identity, r.Kind)
	current, err := f.cfg.Reader.ReadCurrent(ctx, resource)
	if err != nil {
		return out, err
	}
	if current.Presence != observe.Present || current.Resource != resource {
		return out, wf.ErrChanged
	}
	for _, project := range current.Projects {
		if project.Profile == "bug_tracker" && len(project.Items) != 0 {
			return out, ErrUnsupportedBugTarget
		}
	}
	add := func(v wf.Violation, err error) error {
		if err != nil {
			return err
		}
		out.Violations = append(out.Violations, v)
		return nil
	}
	if r.Kind == "issue" {
		if current.Issue == nil || current.PullRequest != nil || current.Issue.Identity != r.Identity {
			return out, wf.ErrChanged
		}
		correction, err := f.cfg.Terminal.CurrentCorrection(ctx, resource)
		if err != nil {
			return out, err
		}
		if correction != nil {
			if err := add(wf.TerminalCorrection(r, *correction)); err != nil {
				return out, err
			}
		}
		// This production producer owns terminal-state correction only. The
		// standalone nonterminal ManualRejection adapter is not an applicable
		// production category and no human-action completeness is asserted here.
	} else {
		if current.PullRequest == nil || current.Issue != nil || current.PullRequest.Identity != r.Identity {
			return out, wf.ErrChanged
		}
		p := current.PullRequest
		if p.State != "OPEN" && p.State != "CLOSED" && p.State != "MERGED" {
			return out, wf.ErrInvalid
		}
		if p.State == "OPEN" {
			decision, err := f.cfg.Binding.Evaluate(ctx, resource)
			if err != nil {
				return out, err
			}
			if decision.Outcome == prbinding.Rejected {
				if err := add(wf.BindingRejection(r, decision)); err != nil {
					return out, err
				}
			} else if decision.Outcome != prbinding.Accepted || decision.Evidence.PR != r.Identity {
				return out, wf.ErrInvalid
			}
			input, err := f.cfg.Capacity.CurrentInput(ctx)
			if err != nil {
				return out, err
			}
			_, policyErr := pc.Evaluate(input)
			if policyErr != nil && !errors.Is(policyErr, pc.ErrCapacity) {
				return out, policyErr
			}
			foundCurrent := false
			for _, pr := range input.PullRequests {
				if pr.Resource == resource && pr.Identity == r.Identity && pr.Binding == decision && pr.State == pc.Open {
					foundCurrent = true
				}
			}
			if !foundCurrent {
				return out, wf.ErrChanged
			}
			if errors.Is(policyErr, pc.ErrCapacity) && decision.Outcome == prbinding.Accepted {
				// With a validated complete input and an exact eligible PR, ErrInvalid
				// from this adapter means overflow belongs to another Target, not this PR.
				v, e := wf.CapacityRejection(r, input)
				if e == nil {
					out.Violations = append(out.Violations, v)
				} else if !errors.Is(e, wf.ErrInvalid) {
					return out, e
				}
			}
		}
		correction, err := f.cfg.Development.CurrentCorrection(ctx, resource)
		if err != nil {
			return out, err
		}
		if correction != nil {
			if err := add(wf.DevelopmentCorrection(r, correction.Input, correction.Action)); err != nil {
				return out, err
			}
		}
	}
	// Comment reconciliation repeats this lifecycle-read-only assembly before and after
	// writes. Complete never means a fabricated empty result after producer error.
	out.Complete = true
	return out, ctx.Err()
}

// TrustedAuthor checks the configured stable identity against the actual shared
// comment credential. User tokens and App-owned verified installation tokens
// are supported; opaque installation tokens cannot establish Bot ownership.
type TrustedAuthor struct {
	Expected github.Actor
	Reader   github.CommentAuthorReader
}

func (a TrustedAuthor) FeedbackAuthor(ctx context.Context) (github.Actor, error) {
	if !github.ValidCommentAuthor(a.Expected) || absent(a.Reader) {
		return github.Actor{}, wf.ErrOwnership
	}
	actual, err := a.Reader.AuthenticatedCommentAuthor(ctx)
	if err != nil {
		return github.Actor{}, err
	}
	if !github.SameCommentAuthor(actual, a.Expected) || actual.Login != a.Expected.Login {
		return github.Actor{}, wf.ErrOwnership
	}
	return actual, ctx.Err()
}
