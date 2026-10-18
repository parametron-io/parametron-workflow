package prdevelopment

import (
	"context"
	"reflect"
	"sort"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type Binding interface {
	Evaluate(context.Context, storage.Resource) (prbinding.Decision, error)
}
type Onboarding interface {
	Completed(context.Context, storage.Resource) (pronboarding.Completion, error)
}
type Reader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
}
type Config struct {
	Repositories  []github.Repository
	Project       string
	BugProject    string
	Binding       Binding
	Onboarding    Onboarding
	Reader        Reader
	Relationships github.DevelopmentReader
	Writer        github.DevelopmentWriter
	Corrections   CorrectionStore
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
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return r.IsNil()
	}
	return false
}
func New(c Config) (*Controller, error) {
	if c.Corrections != nil && absent(c.Corrections) {
		return nil, ErrInvalid
	}
	for _, v := range []any{c.Binding, c.Onboarding, c.Reader, c.Relationships, c.Writer} {
		if absent(v) {
			return nil, ErrInvalid
		}
	}
	if c.Project == "" || strings.ContainsAny(c.Project, " \t\r\n") || c.BugProject == "" || strings.ContainsAny(c.BugProject, " \t\r\n") || c.Project == c.BugProject || len(c.Repositories) == 0 {
		return nil, ErrInvalid
	}
	// Repository bindings are supplied by deployment composition. Keep this
	// boundary single-organization without introducing another organization setting.
	names, ids := map[string]bool{}, map[string]bool{}
	for _, r := range c.Repositories {
		name := strings.ToLower(r.Owner + "/" + r.Name)
		if !github.ValidBranchRepository(r) || !intent.ValidOrganizationLogin(r.Owner) || !strings.EqualFold(r.Owner, c.Repositories[0].Owner) || names[name] || ids[r.ID] {
			return nil, ErrInvalid
		}
		names[name], ids[r.ID] = true, true
	}
	c.Repositories = append([]github.Repository(nil), c.Repositories...)
	return &Controller{cfg: c, lane: make(chan struct{}, 1)}, nil
}
func (c *Controller) configured(i github.Identity) bool {
	for _, r := range c.cfg.Repositories {
		if i.Repository == r && github.ValidDevelopmentIdentity(i) {
			return true
		}
	}
	return false
}
func (c *Controller) fresh(ctx context.Context, r storage.Resource) (Input, error) {
	var in Input
	if err := ctx.Err(); err != nil {
		return in, err
	}
	d, err := c.cfg.Binding.Evaluate(ctx, r)
	if err != nil {
		return in, err
	}
	if d.Outcome == prbinding.Rejected {
		if d.Reason == prbinding.Changed || d.Reason == prbinding.MissingPR {
			return in, ErrChanged
		}
		if d.Reason == prbinding.UnresolvedTarget {
			return in, ErrIncomplete
		}
		return in, ErrRejected
	}
	b := d.Evidence
	if d.Outcome != prbinding.Accepted || d.Reason != "" || !validBinding(b) || !c.configured(b.PR) || !c.configured(b.Target) || resource(b.PR, "pull_request") != r {
		return in, ErrInvalid
	}
	o, err := c.cfg.Onboarding.Completed(ctx, r)
	if err != nil {
		return in, err
	}
	p, err := c.cfg.Reader.ReadCurrent(ctx, r)
	if err != nil {
		return in, err
	}
	tr := resource(b.Target, "issue")
	t, err := c.cfg.Reader.ReadCurrent(ctx, tr)
	if err != nil {
		return in, err
	}
	if p.Presence != observe.Present || p.Resource != r || p.PullRequest == nil || p.Issue != nil || t.Presence != observe.Present || t.Resource != tr || t.Issue == nil || t.PullRequest != nil {
		return in, ErrChanged
	}
	// Current managed membership is required independently of completion history.
	membership := func(s observe.ObservedState) bool {
		eng, bug := false, false
		for _, project := range s.Projects {
			switch project.Profile {
			case "engineering":
				if eng || project.ID != c.cfg.Project || len(project.Items) != 1 || project.Items[0].Archived || project.Items[0].ID == "" {
					return false
				}
				eng = true
			case "bug_tracker":
				if bug || project.ID != c.cfg.BugProject || len(project.Items) != 0 {
					return false
				}
				bug = true
			default:
				return false
			}
		}
		return eng && bug
	}
	if !membership(p) || !membership(t) {
		return in, ErrRejected
	}
	refs, err := c.cfg.Relationships.DevelopmentReferences(ctx, b.PR)
	if err != nil {
		return in, err
	}
	if err := refs.Validate(b.PR); err != nil {
		return in, err
	}
	refs.Manual = append([]github.Identity(nil), refs.Manual...)
	refs.Keyword = append([]github.Identity(nil), refs.Keyword...)
	for _, list := range [][]github.Identity{refs.Manual, refs.Keyword} {
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	}
	in = Input{Binding: d, Onboarding: o, PR: *p.PullRequest, Target: *t.Issue, References: refs, Project: c.cfg.Project}
	_, err = Evaluate(in)
	if err != nil {
		return Input{}, err
	}
	return in, ctx.Err()
}

// Reconcile serializes this controller's callers. Each effect requires two fresh
// barriers and is followed by fresh convergence observation. Transport failures
// return immediately; callers retry from scratch, never from a cached plan.
func (c *Controller) Reconcile(ctx context.Context, r storage.Resource) error {
	if r.Kind != "pull_request" || r.Number <= 0 || r.NodeID == "" || r.Owner != strings.ToLower(r.Owner) || r.Repository != strings.ToLower(r.Repository) {
		return ErrInvalid
	}
	found := false
	for _, repo := range c.cfg.Repositories {
		if strings.EqualFold(repo.Owner, r.Owner) && strings.EqualFold(repo.Name, r.Repository) {
			found = true
		}
	}
	if !found {
		return ErrInvalid
	}
	select {
	case c.lane <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.lane }()
	var previous *Action
	for count := 0; count < 256; count++ {
		in, err := c.fresh(ctx, r)
		if err != nil {
			return err
		}
		actions, err := Evaluate(in)
		if err != nil {
			return err
		}
		if len(actions) == 0 {
			return ctx.Err()
		}
		if previous != nil {
			for _, a := range actions {
				if a == *previous {
					return ErrChanged
				}
			}
		}
		next, err := c.fresh(ctx, r)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(in, next) {
			return ErrChanged
		}
		actions, err = Evaluate(next)
		if err != nil {
			return err
		}
		if len(actions) == 0 {
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		a := actions[0]
		if !a.Add && c.cfg.Corrections != nil {
			if err := c.selectCorrection(ctx, next, a); err != nil {
				return err
			}
			// Persisted explanation evidence grants no relationship authority.
			// Recheck the complete input after the local commit as well.
			barrier, err := c.fresh(ctx, r)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(next, barrier) {
				return ErrChanged
			}
		}
		if a.Add {
			err = c.cfg.Writer.AddDevelopmentReference(ctx, next.Binding.Evidence.PR, a.Target)
		} else {
			err = c.cfg.Writer.RemoveDevelopmentReference(ctx, next.Binding.Evidence.PR, a.Target)
		}
		if err != nil {
			return err
		}
		if !a.Add && c.cfg.Corrections != nil {
			if err := c.appliedCorrection(ctx, r, a); err != nil {
				return err
			}
		}
		previous = &a
	}
	return ErrChanged
}
