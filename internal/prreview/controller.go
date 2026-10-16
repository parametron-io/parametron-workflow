package prreview

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
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
type CompletionReader interface {
	Completed(context.Context, storage.Resource) (pronboarding.Completion, error)
}
type Reader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
}
type StatusWriter interface {
	SetProjectOption(context.Context, string, string, string, string) error
}
type Store interface {
	Event(context.Context, string) (storage.Event, error)
	ProvenanceRecords(context.Context, string) ([]storage.Provenance, error)
	RecordProvenance(context.Context, storage.Provenance) (bool, error)
}
type Config struct {
	Native     github.NativeLifecycleReader
	Deployment config.ResolvedConfig
	Binding    Binding
	Onboarding CompletionReader
	Reader     Reader
	Reviews    github.ReviewFactsReader
	PRItems    github.PullRequestProjectItemReader
	Items      github.ProjectItemReader
	Status     StatusWriter
	Draft      github.DraftConverter
	Store      Store
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
func New(c Config) (*Controller, error) {
	if c.Native != nil && absent(c.Native) {
		return nil, ErrInvalid
	}
	for _, v := range []any{c.Binding, c.Onboarding, c.Reader, c.Reviews, c.PRItems, c.Items, c.Status, c.Draft, c.Store} {
		if absent(v) {
			return nil, ErrInvalid
		}
	}
	if c.Deployment.Organization.Login != "parametron-io" || !validID(c.Deployment.Organization.ID) || len(c.Deployment.Repositories) == 0 || !validID(c.Deployment.Engineering.ID) || !validID(c.Deployment.BugTracker.ID) || c.Deployment.Engineering.ID == c.Deployment.BugTracker.ID || !validID(c.Deployment.Engineering.Fields[config.Status]) || !validID(c.Deployment.Engineering.StatusOptions[config.InReview]) || !validID(c.Deployment.Engineering.StatusOptions[config.InProgress]) || c.Deployment.Engineering.StatusOptions[config.InProgress] == c.Deployment.Engineering.StatusOptions[config.InReview] {
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
	options := map[string]bool{}
	for _, id := range c.Deployment.Engineering.StatusOptions {
		if !validID(id) || options[id] {
			return nil, ErrInvalid
		}
		options[id] = true
	}
	c.Deployment.Engineering.Fields = maps.Clone(c.Deployment.Engineering.Fields)
	c.Deployment.Engineering.StatusOptions = maps.Clone(c.Deployment.Engineering.StatusOptions)
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return &Controller{cfg: c, lane: make(chan struct{}, 1)}, nil
}

type authority struct {
	binding              prbinding.Decision
	onboarding           pronboarding.Completion
	pr, target           observe.ObservedState
	facts                github.ReviewFacts
	prItem, targetItem   observe.ProjectItem
	prOwned, targetOwned bool
}

func ownership(body string, i github.Identity) (bool, error) {
	in, err := intent.Parse(body, intent.Context{Organization: i.Repository.Owner, Repository: i.Repository.Name})
	if err != nil {
		return false, ErrChanged
	}
	return in.Effective().SetStatus, nil
}
func (c *Controller) membership(ctx context.Context, o observe.ObservedState, i github.Identity, kind string) (observe.ProjectItem, error) {
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
	if v, ok := item.Fields[config.Status]; ok && (v.Kind != config.SingleSelect || !validID(v.OptionID) || v.Date != "" || v.Number != 0) {
		return item, ErrProject
	}
	if kind == "pull_request" {
		p, err := c.cfg.PRItems.CurrentPullRequestProjectItem(ctx, item.ID)
		if err != nil {
			return item, err
		}
		if p.ID != item.ID || p.ProjectID != eng.ID || p.ContentKind != "PullRequest" || p.PullRequest == nil || *p.PullRequest != i || p.Issue != nil {
			return item, ErrProject
		}
	} else {
		p, err := c.cfg.Items.CurrentProjectItem(ctx, item.ID)
		if err != nil {
			return item, err
		}
		if p.ID != item.ID || p.ProjectID != eng.ID || p.ContentKind != "Issue" || p.Issue == nil || *p.Issue != i {
			return item, ErrProject
		}
	}
	return item, ctx.Err()
}
func (c *Controller) fresh(ctx context.Context, r storage.Resource, want *prbinding.Evidence) (authority, error) {
	var a authority
	d, err := c.cfg.Binding.Evaluate(ctx, r)
	if err != nil {
		return a, err
	}
	if d.Outcome == prbinding.Rejected {
		if d.Reason == prbinding.Changed {
			return a, ErrChanged
		}
		return a, ErrRejected
	}
	if d.Outcome != prbinding.Accepted || d.Reason != "" || !c.validBinding(d.Evidence) || (r.NodeID != "" && r.NodeID != d.Evidence.PR.ID) || key(r) != key(resource(d.Evidence.PR, "pull_request")) {
		return a, ErrInvalid
	}
	b := d.Evidence
	if want != nil && !sameBinding(*want, b, true) {
		return a, ErrChanged
	}
	a.binding = d
	a.onboarding, err = c.cfg.Onboarding.Completed(ctx, resource(b.PR, "pull_request"))
	if err != nil {
		return a, err
	}
	if !a.onboarding.Completed || a.onboarding.Resource != resource(b.PR, "pull_request") || a.onboarding.Project != c.cfg.Deployment.Engineering.ID || !sameBinding(a.onboarding.Binding, b, false) || a.onboarding.CompletedAt.IsZero() {
		return a, ErrRejected
	}
	a.pr, err = c.cfg.Reader.ReadCurrent(ctx, resource(b.PR, "pull_request"))
	if err != nil {
		return a, err
	}
	p := a.pr.PullRequest
	if a.pr.Presence != observe.Present || a.pr.Resource != resource(b.PR, "pull_request") || p == nil || a.pr.Issue != nil || p.Identity != b.PR || p.Author == nil || !github.SameUser(*p.Author, b.Developer) || p.HeadRepository != b.Branch.Repository || p.HeadRef != b.Branch.Name || p.HeadSHA != b.Branch.SHA {
		return a, ErrChanged
	}
	if p.State != "OPEN" {
		return a, ErrRejected
	}
	a.target, err = c.cfg.Reader.ReadCurrent(ctx, resource(b.Target, "issue"))
	if err != nil {
		return a, err
	}
	i := a.target.Issue
	if a.target.Presence != observe.Present || a.target.Resource != resource(b.Target, "issue") || i == nil || a.target.PullRequest != nil || i.Identity != b.Target || i.State != "OPEN" {
		return a, ErrChanged
	}
	a.prOwned, err = ownership(p.Body, p.Identity)
	if err != nil {
		return a, err
	}
	a.targetOwned, err = ownership(i.Body, i.Identity)
	if err != nil {
		return a, err
	}
	a.prItem, err = c.membership(ctx, a.pr, b.PR, "pull_request")
	if err != nil {
		return a, err
	}
	a.targetItem, err = c.membership(ctx, a.target, b.Target, "issue")
	if err != nil {
		return a, err
	}
	if a.prItem.ID == a.targetItem.ID {
		return a, ErrProject
	}
	a.facts, err = c.cfg.Reviews.PullRequestReviewFacts(ctx, b.PR)
	if err != nil {
		return a, err
	}
	if err := NativeOrder(a.facts, b.PR); err != nil {
		return a, err
	}
	last, err := c.cfg.Binding.Evaluate(ctx, resource(b.PR, "pull_request"))
	if err != nil {
		return a, err
	}
	if last.Outcome != prbinding.Accepted || last.Reason != "" || !sameBinding(b, last.Evidence, true) {
		return a, ErrChanged
	}
	pr, err := c.cfg.Reader.ReadCurrent(ctx, resource(b.PR, "pull_request"))
	if err != nil {
		return a, err
	}
	target, err := c.cfg.Reader.ReadCurrent(ctx, resource(b.Target, "issue"))
	if err != nil {
		return a, err
	}
	facts, err := c.cfg.Reviews.PullRequestReviewFacts(ctx, b.PR)
	if err != nil {
		return a, err
	}
	if !reflect.DeepEqual(a.pr, pr) || !reflect.DeepEqual(a.target, target) || !reflect.DeepEqual(a.facts, facts) {
		return a, ErrChanged
	}
	return a, ctx.Err()
}
func (a authority) input(candidate Candidate, current *Cycle) Input {
	return Input{Candidate: candidate, Binding: a.binding, Onboarding: a.onboarding, PR: *a.pr.PullRequest, Facts: a.facts, Current: current, PROwned: a.prOwned, TargetOwned: a.targetOwned}
}

func (c *Controller) withLane(ctx context.Context, fn func() error) error {
	select {
	case c.lane <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.lane }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// Handle runs within the production Engineering lane and durable worker boundary.
// Draft intent commits before the one-shot call; unknown outcomes fail closed.
func (c *Controller) Handle(ctx context.Context, e storage.Event) error {
	return c.withLane(ctx, func() error {
		candidate, ok, err := Notification(e.Delivery)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if e.Resource == nil || e.Resource.Kind != "pull_request" {
			return ErrInvalid
		}
		if err := c.delivery(ctx, e, *e.Resource); err != nil {
			return err
		}
		a, err := c.fresh(ctx, *e.Resource, nil)
		if err != nil {
			return err
		}
		if c.cfg.Native != nil {
			facts, err := c.cfg.Native.NativeLifecycle(ctx, a.binding.Evidence.PR, "pull_request")
			if err != nil {
				return err
			}
			if err := facts.Validate(a.binding.Evidence.PR, "pull_request", "OPEN"); err != nil {
				return err
			}
			for _, t := range facts.Transitions {
				if t.Kind == "ReopenedEvent" && !candidate.At.After(t.CreatedAt) {
					return nil
				}
			}
		}
		records, err := c.load(ctx)
		if err != nil {
			return err
		}
		for _, state := range records {
			if key(resource(state.start.Binding.Target, "issue")) == key(resource(a.binding.Evidence.Target, "issue")) && state.start.Binding.PR != a.binding.Evidence.PR {
				if c.cfg.Native == nil {
					return ErrRejected
				}
				applicable, err := c.applicable(ctx, state)
				if err != nil {
					return err
				}
				if applicable {
					return ErrRejected
				}
			}
		}
		current, err := latest(records, a.binding.Evidence.PR)
		if err != nil {
			return err
		}
		var cycle *Cycle
		if current != nil {
			applicable, err := c.applicable(ctx, current)
			if err != nil {
				return err
			}
			// A new readiness notification may establish a current interval's
			// cycle. Evaluate still requires the completed rework chain; older
			// review submissions cannot resume superseded authority.
			if !applicable && (candidate.Kind != "ready" || current.attempt == nil || current.doneAt.IsZero()) {
				return ErrRejected
			}
		}
		if current != nil {
			cy := current.cycle()
			cycle = &cy
			if !sameBinding(current.start.Binding, a.binding.Evidence, false) {
				return ErrProvenance
			}
		}
		d, err := Evaluate(a.input(candidate, cycle))
		if err != nil {
			return err
		}
		if d.Outcome == NoTransition {
			// Comment and approval cannot initiate, complete or rework a cycle.
			// Duplicate readiness may retry only the already accepted cycle.
			if d.Reason == Duplicate && (candidate.Kind == "ready" || current != nil && current.attempt != nil && candidate.ReviewID == current.attempt.Review.ID) {
				return c.converge(ctx, current)
			}
			return nil
		}
		if d.Outcome == StartReview {
			previous := ""
			if current != nil {
				previous = current.start.Ready.ID
			}
			s := startEvidence{Version: 1, Resource: resource(a.binding.Evidence.PR, "pull_request"), Binding: a.binding.Evidence, Project: c.cfg.Deployment.Engineering.ID, Field: c.cfg.Deployment.Engineering.Fields[config.Status], Onboarding: a.onboarding, Ready: d.Ready, Previous: previous}
			// Re-evaluate immediately before persisting authorization, then again
			// before each external effect. Durable facts are never permanent grants.
			a, err = c.fresh(ctx, s.Resource, &s.Binding)
			if err != nil {
				return err
			}
			check, err := Evaluate(a.input(candidate, cycle))
			if err != nil {
				return err
			}
			if check.Outcome != StartReview || !equalTransition(check.Ready, s.Ready) {
				return ErrChanged
			}
			if err := c.record(ctx, startNS, cycleKey(s), e.Delivery.ID, s); err != nil {
				return err
			}
			records, err = c.load(ctx)
			if err != nil {
				return err
			}
			current, err = latest(records, s.Binding.PR)
			if err != nil {
				return err
			}
			return c.converge(ctx, current)
		}
		if d.Outcome != EnterRework || current == nil {
			return ErrInvalid
		}
		effect := effectEvidence{Version: 1, Start: current.start, Review: d.Review}
		a, err = c.fresh(ctx, effect.Start.Resource, &a.binding.Evidence)
		if err != nil {
			return err
		}
		check, err := Evaluate(a.input(candidate, cycle))
		if err != nil {
			return err
		}
		if check.Outcome != EnterRework || check.EffectID != d.EffectID {
			return ErrChanged
		}
		if err := c.record(ctx, attemptNS, cycleKey(current.start), e.Delivery.ID, effect); err != nil {
			return err
		}
		// Once intent exists, any interruption before a durable successful receipt
		// is indeterminate. Matching Draft on recovery cannot attribute success.
		a, err = c.fresh(ctx, effect.Start.Resource, &a.binding.Evidence)
		if err != nil {
			return err
		}
		check, err = Evaluate(a.input(candidate, cycle))
		if err != nil {
			return err
		}
		if check.Outcome != EnterRework || check.EffectID != d.EffectID {
			return ErrAmbiguous
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.cfg.Draft.ConvertPullRequestToDraft(ctx, effect.Start.Binding.PR.ID); err != nil {
			return err
		}
		a, err = c.fresh(ctx, effect.Start.Resource, &a.binding.Evidence)
		if err != nil {
			return err
		}
		if !a.pr.PullRequest.Draft {
			return ErrAmbiguous
		}
		draft, err := validateReworkObservation(a.facts, current.start.Ready, d.Review)
		if err != nil {
			return err
		}
		receipt := receiptEvidence{Version: 1, Effect: effect, Draft: draft}
		if err := c.record(ctx, doneNS, cycleKey(current.start), e.Delivery.ID, receipt); err != nil {
			return err
		}
		records, err = c.load(ctx)
		if err != nil {
			return err
		}
		current, err = latest(records, effect.Start.Binding.PR)
		if err != nil {
			return err
		}
		return c.converge(ctx, current)
	})
}

func validateReworkObservation(f github.ReviewFacts, ready github.ReviewTransition, review github.ReviewSubmission) (github.ReviewTransition, error) {
	if len(f.Transitions) < 2 {
		return github.ReviewTransition{}, ErrAmbiguous
	}
	last := f.Transitions[len(f.Transitions)-1]
	before := f.Transitions[len(f.Transitions)-2]
	if !equalTransition(before, ready) || last.Kind != "ConvertToDraftEvent" || last.CreatedAt.Before(review.SubmittedAt) {
		return github.ReviewTransition{}, ErrAmbiguous
	}
	for _, r := range f.LatestOpinionated {
		if r.ID == review.ID && reflect.DeepEqual(r, review) {
			return last, nil
		}
	}
	return github.ReviewTransition{}, ErrChanged
}

func (c *Controller) active(ctx context.Context, state *record) (authority, config.StatusRole, error) {
	var zero authority
	if state == nil {
		return zero, "", ErrInvalid
	}
	applicable, err := c.applicable(ctx, state)
	if err != nil {
		return zero, "", err
	}
	if !applicable {
		return zero, "", errInactive
	}
	a, err := c.fresh(ctx, state.start.Resource, nil)
	if err != nil {
		return a, "", err
	}
	if !sameBinding(state.start.Binding, a.binding.Evidence, false) || !reflect.DeepEqual(state.start.Onboarding, a.onboarding) {
		return a, "", ErrProvenance
	}
	if state.attempt != nil && state.doneAt.IsZero() {
		return a, "", ErrAmbiguous
	}
	if len(a.facts.Transitions) == 0 {
		return a, "", ErrChanged
	}
	last := a.facts.Transitions[len(a.facts.Transitions)-1]
	if last.Kind == "ReadyForReviewEvent" && last.CreatedAt.After(state.start.Ready.CreatedAt) {
		// Do not project an unaccepted newer cycle, or keep an older PR delivery
		// retrying ahead of the explicit readiness delivery in the worker FIFO.
		return a, "", ErrSuperseded
	}
	if state.attempt == nil {
		if !equalTransition(last, state.start.Ready) || a.pr.PullRequest.Draft {
			return a, "", ErrChanged
		}
		return a, config.InReview, nil
	}
	if !a.pr.PullRequest.Draft {
		return a, "", ErrChanged
	}
	// Historical successful rework remains authoritative after review dismissal;
	// dismissal itself supplies no new transition or reversal policy.
	if len(a.facts.Transitions) < 2 || state.draft == nil || !equalTransition(last, *state.draft) || !equalTransition(a.facts.Transitions[len(a.facts.Transitions)-2], state.start.Ready) {
		return a, "", ErrChanged
	}
	return a, config.InProgress, nil
}
func (c *Controller) converge(ctx context.Context, state *record) error {
	for _, pr := range []bool{true, false} {
		a, status, err := c.active(ctx, state)
		if err != nil {
			return err
		}
		item, own := a.targetItem, a.targetOwned
		if pr {
			item, own = a.prItem, a.prOwned
		}
		if !own {
			continue
		}
		option := c.cfg.Deployment.Engineering.StatusOptions[status]
		if item.Fields[config.Status].OptionID == option {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.cfg.Status.SetProjectOption(ctx, c.cfg.Deployment.Engineering.ID, item.ID, c.cfg.Deployment.Engineering.Fields[config.Status], option); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Converge repairs normal PR/Target Status only; it never initiates a cycle or
// retries Draft conversion. Callers hold the deployment Engineering lane.
func (c *Controller) Converge(ctx context.Context, r storage.Resource) error {
	return c.withLane(ctx, func() error {
		records, err := c.load(ctx)
		if err != nil {
			return err
		}
		var identity github.Identity
		for _, state := range records {
			if key(state.start.Resource) == key(r) {
				identity = state.start.Binding.PR
				if r.NodeID != "" && r.NodeID != identity.ID {
					return ErrChanged
				}
			}
		}
		if identity.ID == "" {
			return c.convergePreReview(ctx, r)
		}
		state, err := latest(records, identity)
		if err != nil {
			return err
		}
		err = c.converge(ctx, state)
		if errors.Is(err, ErrSuperseded) || errors.Is(err, errInactive) {
			return nil
		}
		return err
	})
}

// convergePreReview repairs only the PR's owned Status. Initial effects remain
// onboarding-owned, and Target Status remains under its existing authorities.
// An unaccepted native readiness transition is a barrier, never authorization
// either to advance review or to overwrite a newer review interval.
func (c *Controller) convergePreReview(ctx context.Context, r storage.Resource) error {
	// This newly continuous effect must not turn an ordinary unbound or
	// not-yet-onboarded PR into a failed lifecycle action. Invalid completion
	// provenance and operational failures still propagate unchanged.
	d, err := c.cfg.Binding.Evaluate(ctx, r)
	if err != nil {
		return err
	}
	if d.Outcome == prbinding.Rejected {
		if d.Reason == prbinding.Changed || d.Reason == prbinding.UnresolvedTarget || d.Reason == prbinding.MissingPR {
			return ErrChanged
		}
		return nil
	}
	if d.Outcome != prbinding.Accepted || d.Reason != "" || !c.validBinding(d.Evidence) || (r.NodeID != "" && r.NodeID != d.Evidence.PR.ID) || key(r) != key(resource(d.Evidence.PR, "pull_request")) {
		return ErrInvalid
	}
	// The notification is only a locator. Exact-identity consumers use the PR
	// independently verified by the accepted binding, never a payload node ID.
	r = resource(d.Evidence.PR, "pull_request")
	completed, err := c.cfg.Onboarding.Completed(ctx, r)
	if err != nil {
		return err
	}
	if !completed.Completed {
		return nil
	}
	observed, err := c.cfg.Reader.ReadCurrent(ctx, r)
	if err != nil {
		return err
	}
	if observed.PullRequest == nil || observed.Resource != r || observed.PullRequest.Identity != d.Evidence.PR {
		return ErrChanged
	}
	if observed.PullRequest.State == "CLOSED" || observed.PullRequest.State == "MERGED" {
		return nil
	}
	a, err := c.fresh(ctx, r, &d.Evidence)
	if err != nil {
		return err
	}
	if !a.prOwned {
		return nil
	}
	barrier := a.onboarding.CompletedAt
	if c.cfg.Native == nil {
		return ErrInvalid
	}
	native, err := c.cfg.Native.NativeLifecycle(ctx, a.binding.Evidence.PR, "pull_request")
	if err != nil {
		return err
	}
	if err := native.Validate(a.binding.Evidence.PR, "pull_request", "OPEN"); err != nil {
		return err
	}
	for _, t := range native.Transitions {
		if t.Kind == "ReopenedEvent" && t.CreatedAt.After(barrier) {
			barrier = t.CreatedAt
		}
	}
	for _, t := range a.facts.Transitions {
		if t.Kind == "ReadyForReviewEvent" && !t.CreatedAt.Before(barrier) {
			return nil
		}
	}
	option := c.cfg.Deployment.Engineering.StatusOptions[config.InProgress]
	if a.prItem.Fields[config.Status].OptionID == option {
		return ctx.Err()
	}
	check, err := c.fresh(ctx, r, &a.binding.Evidence)
	if err != nil {
		return err
	}
	latest, err := c.cfg.Native.NativeLifecycle(ctx, a.binding.Evidence.PR, "pull_request")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, check) || !reflect.DeepEqual(native, latest) {
		return ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.cfg.Status.SetProjectOption(ctx, c.cfg.Deployment.Engineering.ID, check.prItem.ID, c.cfg.Deployment.Engineering.Fields[config.Status], option); err != nil {
		return err
	}
	after, err := c.fresh(ctx, r, &check.binding.Evidence)
	if err != nil {
		return err
	}
	if after.prItem.ID != check.prItem.ID || after.prItem.Fields[config.Status].OptionID != option || !reflect.DeepEqual(after.facts, check.facts) {
		return ErrChanged
	}
	return ctx.Err()
}

// TargetStatus supplies only an exact Target overlay. Parent Phase policy is
// untouched. Conflicting implementation PRs cannot compete for Status ownership.
func (c *Controller) TargetStatus(ctx context.Context, target storage.Resource) (config.StatusRole, bool, error) {
	status, active, err := c.CanonicalTargetStatus(ctx, target)
	if err != nil || !active {
		return status, false, err
	}
	o, err := c.cfg.Reader.ReadCurrent(ctx, target)
	if err != nil {
		return "", false, err
	}
	if o.Issue == nil || o.PullRequest != nil || resource(o.Issue.Identity, "issue") != target || o.Resource != target {
		return "", false, ErrChanged
	}
	if o.Issue.State != "OPEN" {
		return "", false, nil
	}
	own, err := ownership(o.Issue.Body, o.Issue.Identity)
	return status, own, err
}

// CanonicalTargetStatus returns validated review authority independently of
// ordinary field ownership, for the narrow native-open/Done consistency repair.
func (c *Controller) CanonicalTargetStatus(ctx context.Context, target storage.Resource) (config.StatusRole, bool, error) {
	records, err := c.load(ctx)
	if err != nil {
		return "", false, err
	}
	prs := map[github.Identity]bool{}
	for _, state := range records {
		if key(resource(state.start.Binding.Target, "issue")) == key(target) {
			current, err := latest(records, state.start.Binding.PR)
			if err != nil {
				return "", false, err
			}
			applicable, err := c.applicable(ctx, current)
			if err != nil {
				return "", false, err
			}
			if applicable {
				prs[state.start.Binding.PR] = true
			}
		}
	}
	if len(prs) > 1 {
		return "", false, ErrRejected
	}
	for pr := range prs {
		state, err := latest(records, pr)
		if err != nil {
			return "", false, err
		}
		if resource(state.start.Binding.Target, "issue") != target {
			return "", false, ErrChanged
		}
		_, status, err := c.active(ctx, state)
		if errors.Is(err, errInactive) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return status, true, nil
	}
	return "", false, ctx.Err()
}

// PRStatus is a read-only canonical Status accessor. It never normalizes Draft
// or creates a review cycle. The caller separately validates current binding.
func (c *Controller) PRStatus(ctx context.Context, r storage.Resource) (config.StatusRole, error) {
	records, err := c.load(ctx)
	if err != nil {
		return "", err
	}
	var identity github.Identity
	for _, state := range records {
		if key(state.start.Resource) == key(r) {
			identity = state.start.Binding.PR
		}
	}
	if identity.ID == "" {
		return config.InProgress, nil
	}
	state, err := latest(records, identity)
	if err != nil {
		return "", err
	}
	_, status, err := c.active(ctx, state)
	if errors.Is(err, errInactive) {
		return config.InProgress, nil
	}
	return status, err
}

var errInactive = errors.New("pr review: native terminal or superseded by reopening")

func (c *Controller) applicable(ctx context.Context, state *record) (bool, error) {
	if state == nil {
		return false, ErrInvalid
	}
	o, err := c.cfg.Reader.ReadCurrent(ctx, state.start.Resource)
	if err != nil {
		return false, err
	}
	if o.PullRequest == nil || o.PullRequest.Identity != state.start.Binding.PR {
		return false, ErrChanged
	}
	if o.PullRequest.State == "CLOSED" || o.PullRequest.State == "MERGED" {
		return false, nil
	}
	if o.PullRequest.State != "OPEN" {
		return false, ErrChanged
	}
	target, err := c.cfg.Reader.ReadCurrent(ctx, resource(state.start.Binding.Target, "issue"))
	if err != nil {
		return false, err
	}
	if target.Issue == nil || target.Issue.Identity != state.start.Binding.Target {
		return false, ErrChanged
	}
	if target.Issue.State == "CLOSED" {
		return false, nil
	}
	if c.cfg.Native != nil {
		facts, err := c.cfg.Native.NativeLifecycle(ctx, o.PullRequest.Identity, "pull_request")
		if err != nil {
			return false, err
		}
		if err := facts.Validate(o.PullRequest.Identity, "pull_request", "OPEN"); err != nil {
			return false, err
		}
		for _, t := range facts.Transitions {
			if t.Kind == "ReopenedEvent" && !state.start.Ready.CreatedAt.After(t.CreatedAt) {
				return false, nil
			}
		}
	}
	return true, ctx.Err()
}

func (c *Controller) storageError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) {
		return ErrProvenance
	}
	return ErrStorage
}
