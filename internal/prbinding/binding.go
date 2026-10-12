// Package prbinding evaluates current implementation binding evidence without
// mutations or persistent grants. Consumers must Evaluate again immediately
// before consequential actions; acceptance provides no snapshot isolation.
package prbinding

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var ErrInvalid = errors.New("pr binding: invalid dependency or identity")

type Reason string

const (
	MissingPR              Reason = "missing_pr"
	InvalidTarget          Reason = "invalid_target_directive"
	MissingTarget          Reason = "missing_target_directive"
	OutsideDeployment      Reason = "target_outside_deployment"
	UnresolvedTarget       Reason = "unresolved_target"
	UnsupportedTarget      Reason = "unsupported_target"
	IneligibleTarget       Reason = "ineligible_target"
	MissingProvenance      Reason = "missing_branch_completion"
	InvalidProvenance      Reason = "invalid_branch_provenance"
	HeadMismatch           Reason = "head_mismatch"
	MissingHead            Reason = "missing_head"
	InvalidDeveloper       Reason = "invalid_developer_identity"
	InactiveMembership     Reason = "inactive_membership"
	InsufficientPermission Reason = "insufficient_repository_permission"
	AssigneeMismatch       Reason = "assignee_mismatch"
	Changed                Reason = "current_state_changed"
)

type Outcome string

const (
	Accepted Outcome = "accepted"
	Rejected Outcome = "rejected"
)

// Evidence contains value-only bounded current facts; it is never a grant.
type Evidence struct {
	PR        github.Identity
	Target    github.Identity
	Kind      sp.IssueType
	Branch    github.Branch
	Developer github.Actor
	Policy    string
}
type Decision struct {
	Outcome  Outcome
	Reason   Reason
	Evidence Evidence
}

func reject(r Reason) Decision { return Decision{Outcome: Rejected, Reason: r} }

type ContextReader interface {
	ResolveSnapshot(context.Context) (ec.Snapshot, error)
}
type CompletionReader interface {
	Completed(context.Context, storage.Resource) (eb.Completion, error)
}
type BranchReader interface {
	Branch(context.Context, github.Repository, string) (github.Branch, error)
}
type Config struct {
	Deployment config.ResolvedConfig
	Primary    ec.PrimaryReader
	Context    ContextReader
	Completion CompletionReader
	Branches   BranchReader
	Developers github.DeveloperReads
}
type Validator struct{ cfg Config }

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
func New(c Config) (*Validator, error) {
	if absent(c.Primary) || absent(c.Context) || absent(c.Completion) || absent(c.Branches) || absent(c.Developers) || c.Deployment.Organization.Login != "parametron-io" || strings.TrimSpace(c.Deployment.Organization.ID) == "" || strings.ContainsAny(c.Deployment.Organization.ID, " \t\r\n") || len(c.Deployment.Repositories) == 0 {
		return nil, ErrInvalid
	}
	c.Deployment.Repositories = append([]config.Repository(nil), c.Deployment.Repositories...)
	names, ids := map[string]bool{}, map[string]bool{}
	for _, r := range c.Deployment.Repositories {
		name := strings.ToLower(r.Name)
		if !github.ValidBranchRepository(r) || !strings.EqualFold(r.Owner, c.Deployment.Organization.Login) || names[name] || ids[r.ID] {
			return nil, ErrInvalid
		}
		names[name], ids[r.ID] = true, true
	}
	// Retain only configuration actually owned by this boundary.
	c.Deployment = config.ResolvedConfig{Organization: c.Deployment.Organization, Repositories: c.Deployment.Repositories}
	return &Validator{c}, nil
}
func (v *Validator) repository(owner, name string) (github.Repository, bool) {
	for _, r := range v.cfg.Deployment.Repositories {
		if strings.EqualFold(owner, r.Owner) && strings.EqualFold(name, r.Name) {
			return r, true
		}
	}
	return github.Repository{}, false
}
func validIdentity(i github.Identity, r storage.Resource, repo github.Repository) bool {
	return strings.TrimSpace(i.ID) != "" && i.Number > 0 && int64(i.Number) == r.Number && i.Repository.ID == repo.ID && strings.EqualFold(i.Repository.Owner, repo.Owner) && strings.EqualFold(i.Repository.Name, repo.Name) && (r.NodeID == "" || i.ID == r.NodeID)
}
func (v *Validator) primary(ctx context.Context, r storage.Resource, repo github.Repository) (observe.ObservedState, error) {
	o, err := v.cfg.Primary.ReadPrimary(ctx, r)
	if err != nil {
		return observe.ObservedState{}, err
	}
	if o.Presence == observe.Missing {
		return o, nil
	}
	if o.Presence != observe.Present {
		return observe.ObservedState{}, ErrInvalid
	}
	var i github.Identity
	if r.Kind == "pull_request" && o.PullRequest != nil && o.Issue == nil {
		i = o.PullRequest.Identity
	} else if r.Kind == "issue" && o.Issue != nil && o.PullRequest == nil {
		i = o.Issue.Identity
	} else {
		return observe.ObservedState{}, ErrInvalid
	}
	if !validIdentity(i, r, repo) || o.Resource.NodeID != i.ID || !strings.EqualFold(o.Resource.Owner, r.Owner) || !strings.EqualFold(o.Resource.Repository, r.Repository) || o.Resource.Kind != r.Kind || o.Resource.Number != r.Number {
		return observe.ObservedState{}, ErrInvalid
	}
	return o, nil
}

// DeveloperEligibility is pure policy over verified current evidence. Assignees
// require stable human identity and never substitute for membership/permission.
func DeveloperEligibility(author, user github.Actor, org config.Organization, repo github.Repository, m github.Membership, p github.Permission, assignees []github.Actor) Reason {
	if !github.SameUser(author, user) || !github.SameUser(user, m.User) || !github.SameUser(user, p.User) || m.Organization != org || p.Repository != repo {
		return InvalidDeveloper
	}
	if m.State != "active" {
		return InactiveMembership
	}
	if p.Base != "write" && p.Base != "admin" {
		return InsufficientPermission
	}
	if len(assignees) > 0 {
		match := false
		for _, a := range assignees {
			if !github.ValidHuman(a) {
				return InvalidDeveloper
			}
			if github.SameUser(user, a) {
				match = true
			}
		}
		if !match {
			return AssigneeMismatch
		}
	}
	return ""
}
func contextRejection(err error) Reason {
	switch {
	case errors.Is(err, ec.ErrMissing):
		return UnresolvedTarget
	case errors.Is(err, ec.ErrDeployment):
		return OutsideDeployment
	case errors.Is(err, ec.ErrChanged):
		return Changed
	case errors.Is(err, ec.ErrInvalid), errors.Is(err, intent.ErrDirective), errors.Is(err, intent.ErrConflict), errors.Is(err, intent.ErrReference), errors.Is(err, intent.ErrNoncanonical):
		return UnresolvedTarget
	}
	return ""
}

// Evaluate always fetches new PR, Target, context, provenance, branch and developer
// evidence. A second read barrier rejects material drift. Provider failures
// (including hidden-resource 404s for authorization APIs) remain operational
// errors and never become durable negative policy facts. No partial evidence
// escapes a rejection or error.
func (v *Validator) Evaluate(ctx context.Context, r storage.Resource) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	repo, ok := v.repository(r.Owner, r.Repository)
	if !ok || r.Kind != "pull_request" || r.Number <= 0 {
		return Decision{}, ErrInvalid
	}
	first, err := v.primary(ctx, r, repo)
	if err != nil {
		return Decision{}, err
	}
	if first.Presence == observe.Missing {
		return reject(MissingPR), nil
	}
	pr := *first.PullRequest
	// Pin the current node identity for the final read, even when the caller
	// supplied only repository/number identity.
	r = first.Resource
	parsed, err := intent.Parse(pr.Body, intent.Context{Organization: v.cfg.Deployment.Organization.Login, Repository: strings.ToLower(repo.Name)})
	if err != nil {
		return reject(InvalidTarget), nil
	}
	if parsed.Target == nil {
		return reject(MissingTarget), nil
	}
	tr, ok := v.repository(repo.Owner, parsed.Target.Repository)
	if !ok {
		return reject(OutsideDeployment), nil
	}
	target := storage.Resource{Owner: strings.ToLower(tr.Owner), Repository: strings.ToLower(tr.Name), Kind: "issue", Number: parsed.Target.Number}
	snapshot, err := v.cfg.Context.ResolveSnapshot(ctx)
	if err != nil {
		if reason := contextRejection(err); reason != "" {
			return reject(reason), nil
		}
		return Decision{}, err
	}
	graph, err := snapshot.Context(target)
	if err != nil {
		return reject(UnresolvedTarget), nil
	}
	var node ec.IssueNode
	found := false
	for _, n := range graph.Nodes {
		if n.Resource.Owner == target.Owner && n.Resource.Repository == target.Repository && n.Resource.Kind == target.Kind && n.Resource.Number == target.Number {
			if found {
				return Decision{}, ErrInvalid
			}
			node = n
			found = true
		}
	}
	if !found || node.Resource.NodeID == "" {
		return reject(UnresolvedTarget), nil
	}
	target = node.Resource
	if node.Type != sp.Task && node.Type != sp.Feature && node.Type != sp.Phase {
		return reject(UnsupportedTarget), nil
	}
	if node.State != ec.Open || node.Type == sp.Phase && (!node.Intent.CreateBranch.Explicit || !node.Intent.CreateBranch.Value) {
		return reject(IneligibleTarget), nil
	}
	order, err := eo.BuildRoadmapOrder(graph)
	if err != nil {
		return Decision{}, err
	}
	plan, err := ep.Evaluate(ep.Input{Context: graph, PhaseOrder: order.PhaseOrder})
	if err != nil {
		return Decision{}, err
	}
	eligible := false
	for _, d := range plan.Decisions {
		if d.Resource == target {
			eligible = d.ExecutionEligible
		}
	}
	if !eligible {
		return reject(IneligibleTarget), nil
	}
	observed, err := v.primary(ctx, target, tr)
	if err != nil {
		return Decision{}, err
	}
	if observed.Presence == observe.Missing {
		return reject(UnresolvedTarget), nil
	}
	issue := *observed.Issue
	if err := snapshot.CheckCurrent(target, observed); err != nil {
		return reject(Changed), nil
	}
	targetIntent, err := intent.Parse(issue.Body, intent.Context{Organization: v.cfg.Deployment.Organization.Login, Repository: strings.ToLower(tr.Name)})
	if err != nil || !reflect.DeepEqual(targetIntent, node.Intent) || issue.State != string(node.State) {
		return reject(Changed), nil
	}
	completion, err := v.cfg.Completion.Completed(ctx, target)
	if errors.Is(err, eb.ErrProvenance) {
		return reject(InvalidProvenance), nil
	}
	if err != nil {
		return Decision{}, err
	}
	if !completion.Completed {
		return reject(MissingProvenance), nil
	}
	e := completion.Evidence
	name, err := eb.Name(target)
	work := target
	work.NodeID = ""
	if err != nil || e.WorkItem != work || e.Repository != tr || e.Branch != name || completion.CreatedAt.IsZero() {
		return reject(InvalidProvenance), nil
	}
	branch, err := v.cfg.Branches.Branch(ctx, tr, e.Branch)
	if err != nil {
		var missing *github.MissingBranch
		if errors.As(err, &missing) && missing.Repository == tr && missing.Name == e.Branch {
			return reject(MissingHead), nil
		}
		return Decision{}, err
	}
	if pr.HeadRepository.ID == "" || pr.HeadRef == "" || pr.HeadSHA == "" {
		return reject(MissingHead), nil
	}
	if branch.Repository != tr || branch.Name != e.Branch || !github.ValidCommitSHA(branch.SHA) || pr.HeadRepository != tr || pr.HeadRef != branch.Name || pr.HeadSHA != branch.SHA {
		return reject(HeadMismatch), nil
	}
	if pr.Author == nil || !github.ValidHuman(*pr.Author) {
		return reject(InvalidDeveloper), nil
	}
	user, err := v.cfg.Developers.User(ctx, pr.Author.Login)
	if err != nil {
		return Decision{}, err
	}
	if !github.SameUser(*pr.Author, user) {
		return reject(InvalidDeveloper), nil
	}
	membership, err := v.cfg.Developers.OrganizationMembership(ctx, v.cfg.Deployment.Organization, user.Login)
	if err != nil {
		return Decision{}, err
	}
	permission, err := v.cfg.Developers.RepositoryPermission(ctx, tr, user.Login)
	if err != nil {
		return Decision{}, err
	}
	if reason := DeveloperEligibility(*pr.Author, user, v.cfg.Deployment.Organization, tr, membership, permission, issue.Assignees); reason != "" {
		return reject(reason), nil
	}
	// Resolve Type again through its original authority, including native fallback.
	snapshot2, err := v.cfg.Context.ResolveSnapshot(ctx)
	if err != nil {
		if reason := contextRejection(err); reason != "" {
			return reject(reason), nil
		}
		return Decision{}, err
	}
	graph2, err := snapshot2.Context(target)
	if err != nil {
		return reject(Changed), nil
	}
	if !reflect.DeepEqual(graph, graph2) {
		return reject(Changed), nil
	}
	lastTarget, err := v.primary(ctx, target, tr)
	if err != nil {
		return Decision{}, err
	}
	if lastTarget.Presence != observe.Present || lastTarget.Issue.Body != issue.Body || lastTarget.Issue.State != issue.State || !sameAssignees(lastTarget.Issue.Assignees, issue.Assignees) {
		return reject(Changed), nil
	}
	if err := snapshot2.CheckCurrent(target, lastTarget); err != nil {
		return reject(Changed), nil
	}
	lastCompletion, err := v.cfg.Completion.Completed(ctx, target)
	if err != nil {
		if errors.Is(err, eb.ErrProvenance) {
			return reject(InvalidProvenance), nil
		}
		return Decision{}, err
	}
	if lastCompletion != completion {
		return reject(Changed), nil
	}
	lastBranch, err := v.cfg.Branches.Branch(ctx, tr, e.Branch)
	if err != nil {
		var missing *github.MissingBranch
		if errors.As(err, &missing) {
			return reject(Changed), nil
		}
		return Decision{}, err
	}
	if lastBranch != branch {
		return reject(Changed), nil
	}
	lastUser, err := v.cfg.Developers.User(ctx, user.Login)
	if err != nil {
		return Decision{}, err
	}
	lastMembership, err := v.cfg.Developers.OrganizationMembership(ctx, v.cfg.Deployment.Organization, user.Login)
	if err != nil {
		return Decision{}, err
	}
	lastPermission, err := v.cfg.Developers.RepositoryPermission(ctx, tr, user.Login)
	if err != nil {
		return Decision{}, err
	}
	if lastUser != user || lastMembership != membership || lastPermission != permission {
		return reject(Changed), nil
	}
	last, err := v.primary(ctx, r, repo)
	if err != nil {
		return Decision{}, err
	}
	if last.Presence != observe.Present || last.PullRequest.Body != pr.Body || last.PullRequest.State != pr.State || last.PullRequest.HeadRepository != pr.HeadRepository || last.PullRequest.HeadRef != pr.HeadRef || last.PullRequest.HeadSHA != pr.HeadSHA || !reflect.DeepEqual(last.PullRequest.Author, pr.Author) {
		return reject(Changed), nil
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	return Decision{Outcome: Accepted, Evidence: Evidence{PR: pr.Identity, Target: issue.Identity, Kind: node.Type, Branch: branch, Developer: user, Policy: "active_org_member_repository_write_assignee_v1"}}, nil
}
func sameAssignees(a, b []github.Actor) bool {
	a = append([]github.Actor(nil), a...)
	b = append([]github.Actor(nil), b...)
	less := func(x, y github.Actor) bool {
		if x.ID != y.ID {
			return x.ID < y.ID
		}
		if x.Login != y.Login {
			return x.Login < y.Login
		}
		return x.Type < y.Type
	}
	sort.Slice(a, func(i, j int) bool { return less(a[i], a[j]) })
	sort.Slice(b, func(i, j int) bool { return less(b[i], b[j]) })
	return reflect.DeepEqual(a, b)
}
