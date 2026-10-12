// Package pronboarding owns accepted PR ingress, not review or terminal policy.
package pronboarding

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid    = errors.New("pr onboarding: invalid composition or identity")
	ErrRejected   = errors.New("pr onboarding: rejected binding")
	ErrChanged    = errors.New("pr onboarding: stale authority")
	ErrProject    = errors.New("pr onboarding: conflicting project evidence")
	ErrProvenance = errors.New("pr onboarding: corrupt provenance")
	ErrAmbiguous  = errors.New("pr onboarding: indeterminate one-shot outcome")
	ErrStorage    = errors.New("pr onboarding: storage failure")
)

type Binding interface {
	Evaluate(context.Context, storage.Resource) (prbinding.Decision, error)
}
type Reader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
}
type Projects interface {
	AddProjectItem(context.Context, string, string) (string, error)
	SetProjectOption(context.Context, string, string, string, string) error
}
type Config struct {
	Deployment config.ResolvedConfig
	Binding    Binding
	Reader     Reader
	Projects   Projects
	Items      github.PullRequestProjectItemReader
	Draft      github.DraftConverter
	Store      eb.Store
	Clock      func() time.Time
}
type Controller struct {
	cfg  Config
	lane chan struct{}
}

func absent(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice:
		return r.IsNil()
	}
	return false
}
func validID(s string) bool { return s != "" && !strings.ContainsAny(s, " \t\r\n") }
func New(c Config) (*Controller, error) {
	if absent(c.Binding) || absent(c.Reader) || absent(c.Projects) || absent(c.Items) || absent(c.Draft) || absent(c.Store) || !validID(c.Deployment.Organization.ID) || c.Deployment.Organization.Login != "parametron-io" || len(c.Deployment.Repositories) == 0 || !validID(c.Deployment.Engineering.ID) || !validID(c.Deployment.BugTracker.ID) || c.Deployment.Engineering.ID == c.Deployment.BugTracker.ID || !validID(c.Deployment.Engineering.Fields[config.Status]) || !validID(c.Deployment.Engineering.StatusOptions[config.InProgress]) {
		return nil, ErrInvalid
	}
	c.Deployment.Repositories = append([]config.Repository(nil), c.Deployment.Repositories...)
	seen, ids := map[string]bool{}, map[string]bool{}
	for _, r := range c.Deployment.Repositories {
		if !github.ValidBranchRepository(r) || !strings.EqualFold(r.Owner, c.Deployment.Organization.Login) || seen[strings.ToLower(r.Name)] || ids[r.ID] {
			return nil, ErrInvalid
		}
		seen[strings.ToLower(r.Name)], ids[r.ID] = true, true
	}
	c.Deployment.Engineering.Fields = maps.Clone(c.Deployment.Engineering.Fields)
	c.Deployment.Engineering.StatusOptions = maps.Clone(c.Deployment.Engineering.StatusOptions)
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return &Controller{c, make(chan struct{}, 1)}, nil
}

// fresh is the only authorization path. Observation is checked against this
// current decision, including author/head and the exact canonical PR resource.
func (c *Controller) fresh(ctx context.Context, r storage.Resource, want *prbinding.Evidence) (prbinding.Evidence, observe.ObservedState, error) {
	d, err := c.cfg.Binding.Evaluate(ctx, r)
	if err != nil {
		return prbinding.Evidence{}, observe.ObservedState{}, err
	}
	if d.Outcome == prbinding.Rejected {
		if d.Reason == "" || d.Evidence != (prbinding.Evidence{}) {
			return d.Evidence, observe.ObservedState{}, ErrInvalid
		}
		if d.Reason == prbinding.Changed {
			return d.Evidence, observe.ObservedState{}, ErrChanged
		}
		return d.Evidence, observe.ObservedState{}, ErrRejected
	}
	if d.Outcome != prbinding.Accepted || d.Reason != "" || !c.validBinding(d.Evidence, canonical(r)) {
		return d.Evidence, observe.ObservedState{}, ErrInvalid
	}
	if r.NodeID != "" && r.NodeID != d.Evidence.PR.ID || want != nil && !sameBinding(*want, d.Evidence, true) {
		return d.Evidence, observe.ObservedState{}, ErrChanged
	}
	o, err := c.cfg.Reader.ReadCurrent(ctx, r)
	if err != nil {
		return d.Evidence, o, err
	}
	p := o.PullRequest
	if o.Presence != observe.Present || p == nil || o.Issue != nil || canonical(o.Resource) != canonical(r) || o.Resource.NodeID != d.Evidence.PR.ID || !sameIdentity(p.Identity, d.Evidence.PR) || p.Author == nil || !github.SameUser(*p.Author, d.Evidence.Developer) || p.HeadRepository != d.Evidence.Branch.Repository || p.HeadRef != d.Evidence.Branch.Name || p.HeadSHA != d.Evidence.Branch.SHA {
		return d.Evidence, o, ErrChanged
	}
	if p.State != "OPEN" {
		return d.Evidence, o, ErrRejected
	}
	last, err := c.cfg.Binding.Evaluate(ctx, r)
	if err != nil {
		return d.Evidence, o, err
	}
	if last.Outcome == prbinding.Rejected {
		if last.Reason == "" || last.Evidence != (prbinding.Evidence{}) {
			return d.Evidence, o, ErrInvalid
		}
		if last.Reason == prbinding.Changed {
			return d.Evidence, o, ErrChanged
		}
		return d.Evidence, o, ErrRejected
	}
	if last.Outcome != prbinding.Accepted || last.Reason != "" || !c.validBinding(last.Evidence, canonical(r)) {
		return d.Evidence, o, ErrInvalid
	}
	if !sameBinding(d.Evidence, last.Evidence, true) {
		return d.Evidence, o, ErrChanged
	}
	after, err := c.cfg.Reader.ReadCurrent(ctx, r)
	if err != nil {
		return d.Evidence, o, err
	}
	if !reflect.DeepEqual(o, after) {
		return d.Evidence, o, ErrChanged
	}
	return d.Evidence, after, ctx.Err()
}
func (c *Controller) membership(ctx context.Context, o observe.ObservedState) (*observe.ProjectItem, error) {
	var eng *observe.ProjectState
	bug := false
	for i := range o.Projects {
		p := &o.Projects[i]
		switch p.Profile {
		case config.Engineering:
			if eng != nil || p.ID != c.cfg.Deployment.Engineering.ID {
				return nil, ErrProject
			}
			eng = p
		case config.BugTracker:
			if bug || p.ID != c.cfg.Deployment.BugTracker.ID || len(p.Items) > 0 {
				return nil, ErrProject
			}
			bug = true
		default:
			return nil, ErrProject
		}
	}
	if eng == nil || !bug || len(eng.Items) > 1 {
		return nil, ErrProject
	}
	if len(eng.Items) == 0 {
		return nil, nil
	}
	item := eng.Items[0]
	if !validID(item.ID) || item.Archived {
		return nil, ErrProject
	}
	current, err := c.cfg.Items.CurrentPullRequestProjectItem(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	if current.ID != item.ID || current.ProjectID != eng.ID || current.ContentKind != "PullRequest" || current.PullRequest == nil || current.Issue != nil || !sameIdentity(*current.PullRequest, o.PullRequest.Identity) {
		return nil, ErrProject
	}
	if s, ok := item.Fields[config.Status]; ok && (s.Kind != config.SingleSelect || !validID(s.OptionID) || s.Date != "" || s.Number != 0) {
		return nil, ErrProject
	}
	return &item, ctx.Err()
}

// Handle runs under the worker's exclusive runtime ownership and Engineering
// lane. Its own context-aware lane also protects standalone calls. No SQLite
// transaction spans GitHub I/O. Each immutable attempt commits BEFORE the one-shot
// write; a validated response commits a done fact. An attempt without done is
// ALWAYS ambiguous, even if the current flag/option matches: observation cannot
// attribute historical success or distinguish a subsequent review transition.
func (c *Controller) Handle(ctx context.Context, event storage.Event) error {
	select {
	case c.lane <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.lane }()
	if event.Resource == nil {
		return ErrInvalid
	}
	r := *event.Resource
	binding, o, err := c.fresh(ctx, r, nil)
	if err != nil {
		return err
	}
	r = o.Resource
	if err := c.delivery(ctx, event.Delivery.ID, r); err != nil {
		return err
	}
	f, err := c.load(ctx, r)
	if err != nil {
		return err
	}
	if (f.present["prepared"] || f.present["membership_intent"]) && !sameBinding(f.e.Binding, binding, false) {
		return ErrProvenance
	}
	// Fail closed before even membership repair when initialization attribution is
	// indeterminate. A durable successful receipt permits later readiness to stand.
	if f.present["status_attempt"] && !f.present["status_done"] || f.present["draft_attempt"] && !f.present["draft_done"] {
		return ErrAmbiguous
	}
	binding, o, err = c.fresh(ctx, r, &binding)
	if err != nil {
		return err
	}
	item, err := c.membership(ctx, o)
	if err != nil {
		return err
	}
	if item == nil {
		// Last read barrier before a reconcilable membership write.
		binding, o, err = c.fresh(ctx, r, &binding)
		if err != nil {
			return err
		}
		item, err = c.membership(ctx, o)
		if err != nil {
			return err
		}
		if item == nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !f.present["prepared"] && !f.present["membership_intent"] {
				own, err := owned(*o.PullRequest)
				if err != nil {
					return err
				}
				f.e = evidence{Version: 1, Resource: r, Binding: binding, Organization: c.cfg.Deployment.Organization, Project: c.cfg.Deployment.Engineering.ID, Field: c.cfg.Deployment.Engineering.Fields[config.Status], Option: c.cfg.Deployment.Engineering.StatusOptions[config.InProgress], InitiallyDraft: o.PullRequest.Draft, StatusOwned: own}
				f.delivery = event.Delivery.ID
				if err := c.record(ctx, &f, "membership_intent"); err != nil {
					return err
				}
			}
			binding, o, err = c.fresh(ctx, r, &binding)
			if err != nil {
				return err
			}
			item, err = c.membership(ctx, o)
			if err != nil {
				return err
			}
			if item != nil {
				return ErrChanged
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			id, err := c.cfg.Projects.AddProjectItem(ctx, c.cfg.Deployment.Engineering.ID, binding.PR.ID)
			if err != nil {
				return err
			}
			if !validID(id) {
				return ErrProject
			}
			binding, o, err = c.fresh(ctx, r, &binding)
			if err != nil {
				return err
			}
			item, err = c.membership(ctx, o)
			if err != nil {
				return err
			}
			if item == nil || item.ID != id {
				return ErrChanged
			}
		}
	}
	if f.present["completed"] {
		return ctx.Err()
	}
	if !f.present["prepared"] {
		statusOwned, err := owned(*o.PullRequest)
		if err != nil {
			return err
		}
		s, present := item.Fields[config.Status]
		// An observed review/terminal status is not ingress authority.
		if statusOwned && present && (s.StatusRole == config.InReview || s.StatusRole == config.Done) {
			return ErrAmbiguous
		}
		initiallyDraft := o.PullRequest.Draft
		original := event.Delivery.ID
		if f.present["membership_intent"] {
			initiallyDraft = f.e.InitiallyDraft
			statusOwned = f.e.StatusOwned
			original = f.delivery
		}
		f.e = evidence{Version: 1, Resource: r, Binding: binding, Organization: c.cfg.Deployment.Organization, Project: c.cfg.Deployment.Engineering.ID, Field: c.cfg.Deployment.Engineering.Fields[config.Status], Option: c.cfg.Deployment.Engineering.StatusOptions[config.InProgress], Item: item.ID, InitiallyDraft: initiallyDraft, StatusOwned: statusOwned, StatusPresent: present, Status: s}
		f.delivery = original
		if err := c.record(ctx, &f, "prepared"); err != nil {
			return err
		}
	}
	if !f.present["status_done"] {
		binding, o, err = c.fresh(ctx, r, &binding)
		if err != nil {
			return err
		}
		item, err = c.membership(ctx, o)
		if err != nil {
			return err
		}
		if item == nil || item.ID != f.e.Item {
			return ErrAmbiguous
		}
		own, err := owned(*o.PullRequest)
		if err != nil {
			return err
		}
		s, present := item.Fields[config.Status]
		if f.e.StatusOwned && own {
			if s.OptionID != f.e.Option {
				if present != f.e.StatusPresent || s != f.e.Status {
					return ErrAmbiguous
				}
				if err := c.record(ctx, &f, "status_attempt"); err != nil {
					return err
				}
				// Persistence is not authority: revalidate after committing intent.
				binding, o, err = c.fresh(ctx, r, &binding)
				if err != nil {
					return err
				}
				item, err = c.membership(ctx, o)
				if err != nil {
					return err
				}
				if item == nil || item.ID != f.e.Item {
					return ErrAmbiguous
				}
				own, err = owned(*o.PullRequest)
				if err != nil {
					return err
				}
				current, exists := item.Fields[config.Status]
				if !own || exists != present || current != s {
					return ErrAmbiguous
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := c.cfg.Projects.SetProjectOption(ctx, f.e.Project, item.ID, f.e.Field, f.e.Option); err != nil {
					return err
				}
				binding, o, err = c.fresh(ctx, r, &binding)
				if err != nil {
					return err
				}
				item, err = c.membership(ctx, o)
				if err != nil {
					return err
				}
				if item == nil || item.ID != f.e.Item || item.Fields[config.Status].OptionID != f.e.Option {
					return ErrAmbiguous
				}
			}
		}
		if err := c.record(ctx, &f, "status_done"); err != nil {
			return err
		}
	}
	if !f.present["draft_done"] {
		binding, o, err = c.fresh(ctx, r, &binding)
		if err != nil {
			return err
		}
		if !f.e.InitiallyDraft && !o.PullRequest.Draft {
			if err := c.record(ctx, &f, "draft_attempt"); err != nil {
				return err
			}
			binding, o, err = c.fresh(ctx, r, &binding)
			if err != nil {
				return err
			}
			if o.PullRequest.Draft {
				return ErrAmbiguous
			}
			item, err = c.membership(ctx, o)
			if err != nil {
				return err
			}
			if item == nil {
				return ErrChanged
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := c.cfg.Draft.ConvertPullRequestToDraft(ctx, binding.PR.ID); err != nil {
				return err
			}
			binding, o, err = c.fresh(ctx, r, &binding)
			if err != nil {
				return err
			}
			if !o.PullRequest.Draft {
				return ErrAmbiguous
			}
		}
		// Initially Draft was captured in durable preparation; later readiness must
		// never cause conversion. A successful mutation response is controller evidence,
		// unlike a bare observed Draft flag on recovery from an attempted write.
		if err := c.record(ctx, &f, "draft_done"); err != nil {
			return err
		}
	}
	binding, o, err = c.fresh(ctx, r, &binding)
	if err != nil {
		return err
	}
	item, err = c.membership(ctx, o)
	if err != nil {
		return err
	}
	if item == nil {
		return ErrChanged
	}
	return c.record(ctx, &f, "completed")
}
