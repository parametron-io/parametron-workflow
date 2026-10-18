package prcompletion

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type Reader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
}
type Snapshots interface {
	ResolveSnapshot(context.Context) (ec.Snapshot, error)
}
type CompletionReader interface {
	Completed(context.Context, storage.Resource) (pronboarding.Completion, error)
	Completions(context.Context) ([]pronboarding.Completion, error)
}
type Binding interface {
	Evaluate(context.Context, storage.Resource) (prbinding.Decision, error)
}
type Review interface {
	PRStatus(context.Context, storage.Resource) (config.StatusRole, error)
	CanonicalTargetStatus(context.Context, storage.Resource) (config.StatusRole, bool, error)
}
type Status interface {
	SetProjectOption(context.Context, string, string, string, string) error
}
type Store interface {
	Event(context.Context, string) (storage.Event, error)
	Provenance(context.Context, string, string) (storage.Provenance, error)
	RecordProvenance(context.Context, storage.Provenance) (bool, error)
}
type Config struct {
	Deployment config.ResolvedConfig
	Reader     Reader
	Snapshots  Snapshots
	Onboarding CompletionReader
	Binding    Binding
	Review     Review
	Native     github.NativeLifecycleReader
	Issues     github.IssueStates
	Developers github.DeveloperReads
	Items      github.ProjectItemReader
	PRItems    github.PullRequestProjectItemReader
	Fields     Status
	Store      Store
	Clock      func() time.Time
}
type Controller struct{ cfg Config }
type Result struct {
	Handled bool
	Outcome Outcome
	Target  storage.Resource
}

func absent(v any) bool {
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
	for _, v := range []any{c.Reader, c.Snapshots, c.Onboarding, c.Binding, c.Review, c.Native, c.Issues, c.Developers, c.Items, c.PRItems, c.Fields, c.Store} {
		if absent(v) {
			return nil, ErrInvalid
		}
	}
	if !intent.ValidOrganizationLogin(c.Deployment.Organization.Login) || !validID(c.Deployment.Organization.ID) || !validID(c.Deployment.Engineering.ID) || !validID(c.Deployment.BugTracker.ID) || c.Deployment.Engineering.ID == c.Deployment.BugTracker.ID || !validID(c.Deployment.Engineering.Fields[config.Status]) || len(c.Deployment.Repositories) == 0 {
		return nil, ErrInvalid
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for _, r := range c.Deployment.Repositories {
		n := strings.ToLower(r.Owner + "/" + r.Name)
		if !github.ValidBranchRepository(r) || !strings.EqualFold(r.Owner, c.Deployment.Organization.Login) || names[n] || ids[r.ID] {
			return nil, ErrInvalid
		}
		names[n], ids[r.ID] = true, true
	}
	options := map[string]bool{}
	for _, role := range []config.StatusRole{config.Backlog, config.Blocked, config.Ready, config.InProgress, config.InReview, config.Done} {
		id := c.Deployment.Engineering.StatusOptions[role]
		if !validID(id) || options[id] {
			return nil, ErrInvalid
		}
		options[id] = true
	}
	c.Deployment.Repositories = append([]config.Repository(nil), c.Deployment.Repositories...)
	c.Deployment.Engineering.Fields = maps.Clone(c.Deployment.Engineering.Fields)
	c.Deployment.Engineering.StatusOptions = maps.Clone(c.Deployment.Engineering.StatusOptions)
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return &Controller{c}, nil
}
func (c *Controller) configured(i github.Identity) bool {
	if !validID(i.ID) || i.Number <= 0 {
		return false
	}
	for _, r := range c.cfg.Deployment.Repositories {
		if i.Repository == r {
			return true
		}
	}
	return false
}
func (c *Controller) read(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	o, err := c.cfg.Reader.ReadCurrent(ctx, r)
	if err != nil {
		return o, err
	}
	if o.Presence != observe.Present || o.Resource.Kind != r.Kind || o.Resource.Owner != strings.ToLower(r.Owner) || o.Resource.Repository != strings.ToLower(r.Repository) || o.Resource.Number != r.Number || r.NodeID != "" && r.NodeID != o.Resource.NodeID {
		return o, ErrChanged
	}
	var i github.Identity
	if r.Kind == "pull_request" && o.PullRequest != nil && o.Issue == nil {
		i = o.PullRequest.Identity
	} else if r.Kind == "issue" && o.Issue != nil && o.PullRequest == nil {
		i = o.Issue.Identity
	} else {
		return o, ErrInvalid
	}
	if !c.configured(i) || o.Resource != resource(i, r.Kind) {
		return o, ErrChanged
	}
	return o, ctx.Err()
}
func (c *Controller) membership(ctx context.Context, o observe.ObservedState) (observe.ProjectItem, error) {
	var eng *observe.ProjectState
	bug := false
	for idx := range o.Projects {
		p := &o.Projects[idx]
		switch p.Profile {
		case config.Engineering:
			if eng != nil || p.ID != c.cfg.Deployment.Engineering.ID {
				return observe.ProjectItem{}, ErrProject
			}
			eng = p
		case config.BugTracker:
			if bug || p.ID != c.cfg.Deployment.BugTracker.ID || len(p.Items) != 0 {
				return observe.ProjectItem{}, ErrProject
			}
			bug = true
		default:
			return observe.ProjectItem{}, ErrProject
		}
	}
	if eng == nil || !bug || len(eng.Items) != 1 || eng.Items[0].Archived || !validID(eng.Items[0].ID) {
		return observe.ProjectItem{}, ErrProject
	}
	item := eng.Items[0]
	if v, ok := item.Fields[config.Status]; ok && (v.Kind != config.SingleSelect || !validID(v.OptionID) || v.Number != 0 || v.Date != "" || c.cfg.Deployment.Engineering.StatusOptions[v.StatusRole] != v.OptionID) {
		return item, ErrProject
	}
	if o.PullRequest != nil {
		p, err := c.cfg.PRItems.CurrentPullRequestProjectItem(ctx, item.ID)
		if err != nil {
			return item, err
		}
		if p.ID != item.ID || p.ProjectID != eng.ID || p.ContentKind != "PullRequest" || p.PullRequest == nil || *p.PullRequest != o.PullRequest.Identity || p.Issue != nil {
			return item, ErrProject
		}
	} else {
		p, err := c.cfg.Items.CurrentProjectItem(ctx, item.ID)
		if err != nil {
			return item, err
		}
		if p.ID != item.ID || p.ProjectID != eng.ID || p.ContentKind != "Issue" || p.Issue == nil || *p.Issue != o.Issue.Identity {
			return item, ErrProject
		}
	}
	return item, ctx.Err()
}

type authority struct {
	input              Input
	pr, target         observe.ObservedState
	prItem, targetItem observe.ProjectItem
	decision           Decision
}

func (c *Controller) fresh(ctx context.Context, r storage.Resource) (authority, error) {
	var a authority
	var err error
	a.pr, err = c.read(ctx, r)
	if err != nil {
		return a, err
	}
	p := a.pr.PullRequest
	if p == nil {
		return a, ErrInvalid
	}
	o, err := c.cfg.Onboarding.Completed(ctx, a.pr.Resource)
	if err != nil {
		return a, err
	}
	if !o.Completed || o.Project != c.cfg.Deployment.Engineering.ID || !c.configured(o.Binding.Target) {
		return a, ErrRejected
	}
	a.target, err = c.read(ctx, resource(o.Binding.Target, "issue"))
	if err != nil {
		return a, err
	}
	snapshot, err := c.cfg.Snapshots.ResolveSnapshot(ctx)
	if err != nil {
		return a, err
	}
	graph, err := snapshot.Context(a.target.Resource)
	if err != nil {
		return a, err
	}
	if err := snapshot.CheckCurrent(a.target.Resource, a.target); err != nil {
		return a, err
	}
	var n ec.IssueNode
	for _, node := range graph.Nodes {
		if node.Resource == a.target.Resource {
			n = node
		}
	}
	var native github.NativeLifecycle
	if p.State != "OPEN" {
		native, err = c.cfg.Native.NativeLifecycle(ctx, p.Identity, "pull_request")
		if err != nil {
			return a, err
		}
	}
	a.input = Input{Onboarding: o, PR: *p, Target: *a.target.Issue, Node: n, Native: native}
	a.decision, err = Evaluate(a.input)
	if err != nil {
		return a, err
	}
	user, err := c.cfg.Developers.User(ctx, o.Binding.Developer.Login)
	if err != nil {
		return a, err
	}
	membership, err := c.cfg.Developers.OrganizationMembership(ctx, c.cfg.Deployment.Organization, user.Login)
	if err != nil {
		return a, err
	}
	permission, err := c.cfg.Developers.RepositoryPermission(ctx, o.Binding.Target.Repository, user.Login)
	if err != nil {
		return a, err
	}
	if prbinding.DeveloperEligibility(*p.Author, user, c.cfg.Deployment.Organization, o.Binding.Target.Repository, membership, permission, a.target.Issue.Assignees) != "" {
		return a, ErrRejected
	}
	a.prItem, err = c.membership(ctx, a.pr)
	if err != nil {
		return a, err
	}
	a.targetItem, err = c.membership(ctx, a.target)
	if err != nil {
		return a, err
	}
	if a.prItem.ID == a.targetItem.ID {
		return a, ErrProject
	}
	last, err := c.read(ctx, a.pr.Resource)
	if err != nil {
		return a, err
	}
	target, err := c.read(ctx, a.target.Resource)
	if err != nil {
		return a, err
	}
	if !reflect.DeepEqual(last, a.pr) || !reflect.DeepEqual(target, a.target) {
		return a, ErrChanged
	}
	finalSnapshot, err := c.cfg.Snapshots.ResolveSnapshot(ctx)
	if err != nil {
		return a, err
	}
	finalGraph, err := finalSnapshot.Context(a.target.Resource)
	if err != nil {
		return a, err
	}
	if !reflect.DeepEqual(graph, finalGraph) {
		return a, ErrChanged
	}
	finalUser, err := c.cfg.Developers.User(ctx, user.Login)
	if err != nil {
		return a, err
	}
	finalMember, err := c.cfg.Developers.OrganizationMembership(ctx, c.cfg.Deployment.Organization, user.Login)
	if err != nil {
		return a, err
	}
	finalPermission, err := c.cfg.Developers.RepositoryPermission(ctx, o.Binding.Target.Repository, user.Login)
	if err != nil {
		return a, err
	}
	if finalUser != user || finalMember != membership || finalPermission != permission {
		return a, ErrChanged
	}
	if p.State != "OPEN" {
		finalNative, err := c.cfg.Native.NativeLifecycle(ctx, p.Identity, "pull_request")
		if err != nil {
			return a, err
		}
		if !reflect.DeepEqual(native, finalNative) {
			return a, ErrChanged
		}
	}

	return a, ctx.Err()
}
func (c *Controller) set(ctx context.Context, item observe.ProjectItem, status config.StatusRole) error {
	option := c.cfg.Deployment.Engineering.StatusOptions[status]
	if !validID(option) {
		return ErrInvalid
	}
	if item.Fields[config.Status].OptionID == option {
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.cfg.Fields.SetProjectOption(ctx, c.cfg.Deployment.Engineering.ID, item.ID, c.cfg.Deployment.Engineering.Fields[config.Status], option)
}

// Handle must run under the deployment Engineering lane. Notifications supply
// only a resource identity; current native state selects the terminal path.
func (c *Controller) Handle(ctx context.Context, e storage.Event) (Result, error) {
	if e.Resource == nil || e.Resource.Kind != "pull_request" {
		return Result{}, ErrInvalid
	}
	pr, err := c.read(ctx, *e.Resource)
	if err != nil {
		return Result{}, err
	}
	state := pr.PullRequest.State
	if state == "OPEN" {
		return c.open(ctx, pr)
	}
	if state != "MERGED" && state != "CLOSED" {
		return Result{}, ErrRejected
	}
	result := Result{Handled: true}
	a, err := c.fresh(ctx, pr.Resource)
	if err != nil {
		return result, err
	}
	result.Outcome, result.Target = a.decision.Outcome, a.target.Resource
	if err := c.set(ctx, a.prItem, config.Done); err != nil {
		return result, err
	}
	a, err = c.fresh(ctx, pr.Resource)
	if err != nil {
		return result, err
	}
	if a.decision.Outcome == Merged && a.decision.CloseTarget {
		if err := c.closeTarget(ctx, e, a); err != nil {
			return result, err
		}
	} else if a.decision.Outcome == Abandoned && a.target.Issue.State == "OPEN" && a.input.Node.Intent.Effective().SetStatus {
		// Current active replacement review wins over abandoned-attempt rollback.
		status, active, err := c.cfg.Review.CanonicalTargetStatus(ctx, a.target.Resource)
		if err != nil {
			return result, err
		}
		if active && status == config.InReview {
			return result, nil
		}
		if err := c.set(ctx, a.targetItem, config.InProgress); err != nil {
			return result, err
		}
	}
	return result, ctx.Err()
}
func (c *Controller) open(ctx context.Context, o observe.ObservedState) (Result, error) {
	// Ordinary onboarding/review retain ownership unless terminal Done drift exists.
	done := false
	for _, p := range o.Projects {
		if p.ID == c.cfg.Deployment.Engineering.ID {
			for _, i := range p.Items {
				done = done || i.Fields[config.Status].OptionID == c.cfg.Deployment.Engineering.StatusOptions[config.Done]
			}
		}
	}
	if !done {
		return Result{}, nil
	}
	result := Result{Handled: true, Outcome: Open}
	completed, err := c.cfg.Onboarding.Completed(ctx, o.Resource)
	if err != nil {
		return result, err
	}
	if !completed.Completed || completed.Project != c.cfg.Deployment.Engineering.ID {
		return result, ErrRejected
	}
	d, err := c.cfg.Binding.Evaluate(ctx, o.Resource)
	if err != nil {
		return result, err
	}
	if d.Outcome != prbinding.Accepted || d.Evidence.PR != completed.Binding.PR || d.Evidence.Target != completed.Binding.Target || d.Evidence.Kind != completed.Binding.Kind || d.Evidence.Policy != completed.Binding.Policy || d.Evidence.Branch.Repository != completed.Binding.Branch.Repository || d.Evidence.Branch.Name != completed.Binding.Branch.Name || !github.SameUser(d.Evidence.Developer, completed.Binding.Developer) {
		return result, ErrRejected
	}
	status, err := c.cfg.Review.PRStatus(ctx, o.Resource)
	if err != nil {
		return result, err
	}
	fresh, err := c.read(ctx, o.Resource)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(fresh, o) || fresh.PullRequest.State != "OPEN" {
		return result, ErrChanged
	}
	item, err := c.membership(ctx, fresh)
	if err != nil {
		return result, err
	}
	final, err := c.cfg.Binding.Evaluate(ctx, o.Resource)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(final, d) {
		return result, ErrChanged
	}
	return result, c.set(ctx, item, status)
}

// TargetStatus supplies only freshly validated accepted implementation activity.
// Latest completed onboarding selects an attempt; terminal review is never replayed.
func (c *Controller) TargetStatus(ctx context.Context, target storage.Resource) (config.StatusRole, bool, error) {
	records, err := c.cfg.Onboarding.Completions(ctx)
	if err != nil {
		return "", false, err
	}
	var latest *pronboarding.Completion
	for idx := range records {
		o := &records[idx]
		if resource(o.Binding.Target, "issue") != target {
			continue
		}
		if latest == nil || o.CompletedAt.After(latest.CompletedAt) {
			latest = o
		} else if o.CompletedAt.Equal(latest.CompletedAt) && o.Binding.PR != latest.Binding.PR {
			return "", false, ErrRejected
		}
	}
	if latest == nil {
		return "", false, nil
	}
	a, err := c.fresh(ctx, latest.Resource)
	if err != nil {
		return "", false, err
	}
	if a.target.Issue.State != "OPEN" {
		return "", false, nil
	}
	switch a.decision.Outcome {
	case Abandoned:
		return config.InProgress, true, nil
	case Merged:
		if a.decision.CloseTarget {
			return "", false, nil
		}
		return config.InProgress, true, nil
	case Open:
		binding, err := c.cfg.Binding.Evaluate(ctx, latest.Resource)
		if err != nil {
			return "", false, err
		}
		if binding.Outcome != prbinding.Accepted || binding.Evidence.Target != latest.Binding.Target || binding.Evidence.PR != latest.Binding.PR {
			return "", false, ErrRejected
		}
		return config.InProgress, true, nil
	}
	return "", false, ErrRejected
}
