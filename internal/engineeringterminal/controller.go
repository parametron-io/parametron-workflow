package engineeringterminal

import (
	"context"
	"maps"
	"reflect"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type Snapshots interface {
	ResolveSnapshot(context.Context) (ec.Snapshot, error)
}
type Reader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
}
type Canonical interface {
	CanonicalStatus(context.Context, ec.Context, storage.Resource) (config.StatusRole, error)
}
type Fields interface {
	SetProjectOption(context.Context, string, string, string, string) error
}
type Config struct {
	Project    config.ResolvedProject
	BugTracker string
	Snapshots  Snapshots
	Reader     Reader
	Canonical  Canonical
	Issues     github.IssueStates
	Items      github.ProjectItemReader
	Fields     Fields
}
type Controller struct{ cfg Config }

func missing(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func:
		return r.IsNil()
	}
	return false
}
func New(c Config) (*Controller, error) {
	for _, v := range []any{c.Snapshots, c.Reader, c.Canonical, c.Issues, c.Items, c.Fields} {
		if missing(v) {
			return nil, ErrInvalid
		}
	}
	if c.Project.ID == "" || c.Project.Fields[config.Status] == "" || c.BugTracker == "" || c.BugTracker == c.Project.ID {
		return nil, ErrInvalid
	}
	seen := map[string]bool{}
	for _, role := range []config.StatusRole{config.Backlog, config.Blocked, config.Ready, config.InProgress, config.InReview, config.Done} {
		id := c.Project.StatusOptions[role]
		if id == "" || seen[id] {
			return nil, ErrInvalid
		}
		seen[id] = true
	}
	c.Project.Fields = maps.Clone(c.Project.Fields)
	c.Project.StatusOptions = maps.Clone(c.Project.StatusOptions)
	return &Controller{c}, nil
}
func (c *Controller) item(ctx context.Context, s ec.Snapshot, r storage.Resource) (observe.ObservedState, observe.ProjectItem, bool, error) {
	o, err := c.cfg.Reader.ReadCurrent(ctx, r)
	if err != nil {
		return o, observe.ProjectItem{}, false, err
	}
	if o.Presence != observe.Present || o.Issue == nil || o.PullRequest != nil || o.Resource != r {
		return o, observe.ProjectItem{}, false, ec.ErrChanged
	}
	if err := s.CheckCurrent(r, o); err != nil {
		return o, observe.ProjectItem{}, false, err
	}
	var eng *observe.ProjectState
	bug := false
	for idx := range o.Projects {
		p := &o.Projects[idx]
		switch p.Profile {
		case config.Engineering:
			if eng != nil || p.ID != c.cfg.Project.ID {
				return o, observe.ProjectItem{}, false, ErrInvalid
			}
			eng = p
		case config.BugTracker:
			if bug || p.ID != c.cfg.BugTracker {
				return o, observe.ProjectItem{}, false, ErrInvalid
			}
			bug = true
			if len(p.Items) > 0 {
				return o, observe.ProjectItem{}, false, ErrInvalid
			}
		default:
			return o, observe.ProjectItem{}, false, ErrInvalid
		}
	}
	if eng == nil || !bug {
		return o, observe.ProjectItem{}, false, ErrInvalid
	}
	if len(eng.Items) == 0 {
		return o, observe.ProjectItem{}, false, nil
	}
	if len(eng.Items) != 1 || eng.Items[0].Archived || eng.Items[0].ID == "" {
		return o, observe.ProjectItem{}, false, ErrInvalid
	}
	item := eng.Items[0]
	if v, ok := item.Fields[config.Status]; ok && (v.Kind != config.SingleSelect || v.OptionID == "" || v.Number != 0 || v.Date != "" || c.cfg.Project.StatusOptions[v.StatusRole] != v.OptionID) {
		return o, item, false, ErrInvalid
	}
	current, err := c.cfg.Items.CurrentProjectItem(ctx, item.ID)
	if err != nil {
		return o, item, false, err
	}
	if current.ID != item.ID || current.ProjectID != c.cfg.Project.ID || current.ContentKind != "Issue" || current.Issue == nil || *current.Issue != o.Issue.Identity {
		return o, item, false, ErrInvalid
	}
	return o, item, true, ctx.Err()
}

// Synchronize runs inside the Engineering lane. It returns bounded feedback
// outcomes without comments. Every consequential effect uses a new complete
// graph barrier; no pre-write Snapshot survives a native state change.
func (c *Controller) Synchronize(ctx context.Context, root storage.Resource) ([]Rejection, error) {
	s, err := c.cfg.Snapshots.ResolveSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	graph, err := s.Context(root)
	if err != nil {
		return nil, err
	}
	var rejections []Rejection
	for _, node := range graph.Nodes {
		if node.Type != sp.Task && node.Type != sp.Feature && node.Type != sp.Phase {
			continue
		}
		s, err = c.cfg.Snapshots.ResolveSnapshot(ctx)
		if err != nil {
			return rejections, err
		}
		current, err := s.Context(node.Resource)
		if err != nil {
			return rejections, err
		}
		decision, err := Evaluate(current, node.Resource)
		if err != nil {
			return rejections, err
		}
		// Ordinary nonterminal state is outside this exception boundary.
		observed, err := c.cfg.Reader.ReadCurrent(ctx, node.Resource)
		if err != nil {
			return rejections, err
		}
		if err := s.CheckCurrent(node.Resource, observed); err != nil {
			return rejections, err
		}
		terminalDrift := false
		for _, p := range observed.Projects {
			if p.ID == c.cfg.Project.ID {
				for _, i := range p.Items {
					terminalDrift = terminalDrift || i.Fields[config.Status].OptionID == c.cfg.Project.StatusOptions[config.Done]
				}
			}
		}
		if observed.Issue != nil && observed.Issue.State == "OPEN" && !terminalDrift {
			continue
		}
		o, item, managed, err := c.item(ctx, s, node.Resource)
		if err != nil {
			return rejections, err
		}
		if !managed {
			continue
		}
		force := false
		if decision.Reopen {
			// CanonicalStatus invokes the existing #35/#36 validation as well. This
			// hypothetical OPEN projection grants no close and changes no stored graph.
			open := current
			open.Nodes = append([]ec.IssueNode(nil), current.Nodes...)
			for idx := range open.Nodes {
				if open.Nodes[idx].Resource == node.Resource {
					open.Nodes[idx].State = ec.Open
				}
			}
			_, err = c.cfg.Canonical.CanonicalStatus(ctx, open, node.Resource)
			if err != nil {
				return rejections, err
			}
			fresh, err := c.cfg.Snapshots.ResolveSnapshot(ctx)
			if err != nil {
				return rejections, err
			}
			check, err := fresh.Context(node.Resource)
			if err != nil {
				return rejections, err
			}
			if !reflect.DeepEqual(current, check) {
				return rejections, ec.ErrChanged
			}
			latest, latestItem, ok, err := c.item(ctx, fresh, node.Resource)
			if err != nil {
				return rejections, err
			}
			if !ok || !reflect.DeepEqual(o, latest) || item.ID != latestItem.ID {
				return rejections, ec.ErrChanged
			}
			// Native reopen precedes the single canonical Status write. Retry uses
			// current OPEN plus remaining Done drift if interrupted after reopening.
			if err := ctx.Err(); err != nil {
				return rejections, err
			}
			receipt, err := c.cfg.Issues.ReopenIssue(ctx, latest.Issue.Identity)
			if err != nil {
				return rejections, err
			}
			if receipt.Issue != latest.Issue.Identity || receipt.State != "OPEN" {
				return rejections, ErrInvalid
			}
			rejections = append(rejections, *decision.Rejection)
			// Refresh after mutation; changed blockers/Phase selection determine the
			// retained open Status, never the earlier hypothetical projection.
			s, err = c.cfg.Snapshots.ResolveSnapshot(ctx)
			if err != nil {
				return rejections, err
			}
			current, err = s.Context(node.Resource)
			if err != nil {
				return rejections, err
			}
			o, item, managed, err = c.item(ctx, s, node.Resource)
			if err != nil {
				return rejections, err
			}
			if !managed || o.Issue.State != "OPEN" {
				return rejections, ec.ErrChanged
			}
			force = true
		}
		desired := decision.Status
		if o.Issue.State == "OPEN" {
			if !force && item.Fields[config.Status].OptionID != c.cfg.Project.StatusOptions[config.Done] {
				continue
			}
			desired, err = c.cfg.Canonical.CanonicalStatus(ctx, current, node.Resource)
			if err != nil {
				return rejections, err
			}
		}
		fresh, err := c.cfg.Snapshots.ResolveSnapshot(ctx)
		if err != nil {
			return rejections, err
		}
		check, err := fresh.Context(node.Resource)
		if err != nil {
			return rejections, err
		}
		if !reflect.DeepEqual(current, check) {
			return rejections, ec.ErrChanged
		}
		latest, latestItem, ok, err := c.item(ctx, fresh, node.Resource)
		if err != nil {
			return rejections, err
		}
		if !ok || !reflect.DeepEqual(latest, o) || latestItem.ID != item.ID {
			return rejections, ec.ErrChanged
		}
		if err := c.set(ctx, latestItem, desired); err != nil {
			return rejections, err
		}
	}
	return rejections, ctx.Err()
}
func (c *Controller) set(ctx context.Context, item observe.ProjectItem, status config.StatusRole) error {
	option := c.cfg.Project.StatusOptions[status]
	if option == "" {
		return ErrInvalid
	}
	if item.Fields[config.Status].OptionID == option {
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.cfg.Fields.SetProjectOption(ctx, c.cfg.Project.ID, item.ID, c.cfg.Project.Fields[config.Status], option)
}
