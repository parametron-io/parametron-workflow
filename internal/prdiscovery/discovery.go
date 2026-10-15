// Package prdiscovery observes complete configured PR sets. It owns neither
// authorization nor reconciliation. Stable reads are not atomic snapshots and
// never replace a consumer's pre-write authority and freshness barriers.
package prdiscovery

import (
	"context"
	"encoding/json"
	"errors"
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

// Bounds cover the whole invocation, not each Target. Existing binding readers
// may resolve the complete Issue graph repeatedly; the shared transport request
// budget also bounds their otherwise unbounded nested pagination. Alternate
// adapters must provide bounded, complete methods honoring caller cancellation.
const MaxRepositories = 64
const MaxPullRequests = 4096
const MaxRequests = 50000
const MaxEvidenceBytes = 64 << 20

var (
	ErrInvalid    = errors.New("pr discovery: invalid configuration or evidence")
	ErrIncomplete = errors.New("pr discovery: incomplete observation")
	ErrChanged    = errors.New("pr discovery: changed observation")
	ErrLimit      = errors.New("pr discovery: observation limit")
)

type Error struct{ Kind error }

func (e *Error) Error() string { return e.Kind.Error() }
func (e *Error) Unwrap() error { return e.Kind }
func fail(e error) error       { return &Error{Kind: e} }

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
	Repositories        []github.Repository
	Project, BugProject string
	Lister              github.PullRequestLister
	Binding             Binding
	Onboarding          Onboarding
	Reader              Reader
}
type Coordinator struct{ cfg Config }

type Entry struct {
	Resource storage.Resource
	PR       observe.ObservedState
	Binding  prbinding.Decision
	// Target observes a valid configured declarative Target, even for a rejected
	// binding. Its presence is not authorization. Native links never select it.
	Target *observe.ObservedState
	// Completion is independent historical evidence, never an eligibility filter.
	Onboarding pronboarding.Completion
}
type Completeness string
type Freshness string

const Complete Completeness = "complete"
const StableReads Freshness = "stable_reads"

type Result struct {
	Repositories        []github.Repository
	Project, BugProject string
	Completeness        Completeness
	Freshness           Freshness
	Entries             []Entry
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
func validID(s string) bool { return s != "" && !strings.ContainsAny(s, " \t\r\n") }
func New(c Config) (*Coordinator, error) {
	if len(c.Repositories) == 0 || len(c.Repositories) > MaxRepositories || !validID(c.Project) || !validID(c.BugProject) || c.Project == c.BugProject {
		return nil, fail(ErrInvalid)
	}
	for _, d := range []any{c.Lister, c.Binding, c.Onboarding, c.Reader} {
		if absent(d) {
			return nil, fail(ErrInvalid)
		}
	}
	c.Repositories = append([]github.Repository(nil), c.Repositories...)
	names, ids := map[string]bool{}, map[string]bool{}
	for _, r := range c.Repositories {
		name := strings.ToLower(r.Owner + "/" + r.Name)
		if !github.ValidBranchRepository(r) || r.Owner != "parametron-io" || names[name] || ids[r.ID] || r.ID == c.Project || r.ID == c.BugProject {
			return nil, fail(ErrInvalid)
		}
		names[name], ids[r.ID] = true, true
	}
	sort.Slice(c.Repositories, func(i, j int) bool {
		return strings.ToLower(c.Repositories[i].Owner+"/"+c.Repositories[i].Name) < strings.ToLower(c.Repositories[j].Owner+"/"+c.Repositories[j].Name)
	})
	return &Coordinator{cfg: c}, nil
}
func resource(i github.Identity, kind string) storage.Resource {
	return storage.Resource{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Kind: kind, Number: int64(i.Number), NodeID: i.ID}
}
func (c *Coordinator) configured(i github.Identity) bool {
	if !github.ValidDevelopmentIdentity(i) {
		return false
	}
	for _, r := range c.cfg.Repositories {
		if i.Repository.ID == r.ID && strings.EqualFold(i.Repository.Owner, r.Owner) && strings.EqualFold(i.Repository.Name, r.Name) {
			return true
		}
	}
	return false
}
func identityEqual(a, b github.Identity) bool {
	return resource(a, "") == resource(b, "") && a.Repository.ID == b.Repository.ID
}
func less(a, b storage.Resource) bool {
	if a.Owner != b.Owner {
		return a.Owner < b.Owner
	}
	if a.Repository != b.Repository {
		return a.Repository < b.Repository
	}
	if a.Number != b.Number {
		return a.Number < b.Number
	}
	return a.NodeID < b.NodeID
}
func translate(err error) error {
	switch {
	case errors.Is(err, github.ErrRequestLimit), errors.Is(err, github.ErrPullRequestLimit):
		return fail(ErrLimit)
	case errors.Is(err, github.ErrPullRequestChanged):
		return fail(ErrChanged)
	case errors.Is(err, github.ErrPullRequestIncomplete):
		return fail(ErrIncomplete)
	}
	return err
}

// Discover performs two complete enumeration/materialization passes followed by
// a final complete membership/lifecycle barrier. Every error returns zero Result;
// no rejected/accepted subset or cached plan escapes. There are no internal retries.
func (c *Coordinator) Discover(ctx context.Context) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	ctx = github.WithRequestLimit(ctx, MaxRequests)
	first, err := c.pass(ctx)
	if err != nil {
		return Result{}, translate(err)
	}
	second, err := c.pass(ctx)
	if err != nil {
		return Result{}, translate(err)
	}
	if !reflect.DeepEqual(first, second) {
		return Result{}, fail(ErrChanged)
	}
	last, err := c.enumerate(ctx)
	if err != nil {
		return Result{}, translate(err)
	}
	if len(last) != len(second) {
		return Result{}, fail(ErrChanged)
	}
	for n, p := range last {
		if !identityEqual(p.Identity, second[n].PR.PullRequest.Identity) || p.State != second[n].PR.PullRequest.State {
			return Result{}, fail(ErrChanged)
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Repositories: append([]github.Repository(nil), c.cfg.Repositories...), Project: c.cfg.Project, BugProject: c.cfg.BugProject, Completeness: Complete, Freshness: StableReads, Entries: second}, nil
}
func (c *Coordinator) enumerate(ctx context.Context) ([]github.ListedPullRequest, error) {
	out := []github.ListedPullRequest{}
	registry := newIdentities(c.cfg.Repositories, c.cfg.Project, c.cfg.BugProject)
	for _, repo := range c.cfg.Repositories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		listed, err := c.cfg.Lister.ListPullRequests(ctx, repo)
		if err != nil {
			return nil, err
		}
		if len(listed) > github.MaxPullRequestRecords || len(listed) > MaxPullRequests-len(out) {
			return nil, fail(ErrLimit)
		}
		for _, p := range listed {
			if p.Repository != repo || !validState(p.State) || !registry.add(p.Identity, "pull_request", true) {
				return nil, fail(ErrInvalid)
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return less(resource(out[i].Identity, "pull_request"), resource(out[j].Identity, "pull_request"))
	})
	return out, nil
}
func validState(s string) bool { return s == "OPEN" || s == "CLOSED" || s == "MERGED" }
func (c *Coordinator) pass(ctx context.Context) ([]Entry, error) {
	listed, err := c.enumerate(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(listed))
	evidenceBytes := 0
	targets := map[storage.Resource]observe.ObservedState{}
	registry := newIdentities(c.cfg.Repositories, c.cfg.Project, c.cfg.BugProject)
	for _, p := range listed {
		if !registry.add(p.Identity, "pull_request", true) {
			return nil, fail(ErrInvalid)
		}
	}
	for _, p := range listed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r := resource(p.Identity, "pull_request")
		observed, err := c.read(ctx, r, registry)
		if err != nil {
			return nil, err
		}
		if observed.PullRequest.State != p.State {
			return nil, fail(ErrChanged)
		}
		d, err := c.cfg.Binding.Evaluate(ctx, r)
		if err != nil {
			return nil, err
		}
		if err := c.decision(d, p.Identity); err != nil {
			return nil, err
		}
		e := Entry{Resource: r, PR: observed, Binding: d}
		parsed, parseErr := intent.Parse(observed.PullRequest.Body, intent.Context{Organization: r.Owner, Repository: r.Repository})
		if parseErr == nil && parsed.Target != nil {
			for _, repo := range c.cfg.Repositories {
				if strings.EqualFold(repo.Name, parsed.Target.Repository) {
					t, err := c.read(ctx, storage.Resource{Owner: strings.ToLower(repo.Owner), Repository: strings.ToLower(repo.Name), Kind: "issue", Number: parsed.Target.Number}, registry)
					if err != nil {
						return nil, err
					}
					e.Target = &t
					if old, ok := targets[t.Resource]; ok && !reflect.DeepEqual(old, t) {
						return nil, fail(ErrChanged)
					}
					targets[t.Resource] = t
				}
			}
		}
		if d.Outcome == prbinding.Accepted {
			if e.Target == nil {
				return nil, fail(ErrChanged)
			}
			t := *e.Target
			pr := observed.PullRequest
			b := d.Evidence
			if !identityEqual(t.Issue.Identity, b.Target) || t.Issue.State != "OPEN" || pr.HeadRepository != b.Branch.Repository || pr.HeadRef != b.Branch.Name || pr.HeadSHA != b.Branch.SHA || pr.Author == nil || !github.SameUser(*pr.Author, b.Developer) {
				return nil, fail(ErrChanged)
			}
			e.Target = &t
		}
		e.Onboarding, err = c.cfg.Onboarding.Completed(ctx, r)
		if err != nil {
			return nil, err
		}
		o := e.Onboarding
		if o.Completed {
			if o.Resource != r || !identityEqual(o.Binding.PR, p.Identity) || !c.bindingEvidence(o.Binding) || o.Project != c.cfg.Project || !validID(o.DeliveryID) || o.CompletedAt.IsZero() || !registry.add(o.Binding.Target, "issue", false) {
				return nil, fail(ErrInvalid)
			}
		} else if o != (pronboarding.Completion{}) {
			return nil, fail(ErrInvalid)
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, fail(ErrInvalid)
		}
		if len(raw) > MaxEvidenceBytes-evidenceBytes {
			return nil, fail(ErrLimit)
		}
		evidenceBytes += len(raw)
		out = append(out, e)
	}
	return out, ctx.Err()
}
func (c *Coordinator) bindingEvidence(b prbinding.Evidence) bool {
	return c.configured(b.PR) && c.configured(b.Target) && b.PR.ID != b.Target.ID && (b.Kind == "Task" || b.Kind == "Feature" || b.Kind == "Phase") && b.Policy == "active_org_member_repository_write_assignee_v1" && b.Branch.Repository == b.Target.Repository && github.ValidBranchName(b.Branch.Name) && github.ValidCommitSHA(b.Branch.SHA) && github.ValidHuman(b.Developer)
}
func (c *Coordinator) decision(d prbinding.Decision, pr github.Identity) error {
	if d.Outcome == prbinding.Accepted {
		if d.Reason != "" || !c.bindingEvidence(d.Evidence) || !identityEqual(d.Evidence.PR, pr) {
			return fail(ErrInvalid)
		}
		return nil
	}
	if d.Outcome != prbinding.Rejected || d.Evidence != (prbinding.Evidence{}) {
		return fail(ErrInvalid)
	}
	switch d.Reason {
	case prbinding.Changed, prbinding.MissingPR:
		return fail(ErrChanged)
	case prbinding.UnresolvedTarget:
		return fail(ErrIncomplete)
	case prbinding.InvalidTarget, prbinding.MissingTarget, prbinding.OutsideDeployment, prbinding.UnsupportedTarget, prbinding.IneligibleTarget, prbinding.MissingProvenance, prbinding.InvalidProvenance, prbinding.HeadMismatch, prbinding.MissingHead, prbinding.InvalidDeveloper, prbinding.InactiveMembership, prbinding.InsufficientPermission, prbinding.AssigneeMismatch:
		return nil
	}
	return fail(ErrInvalid)
}

// JSON cloning detaches mutable reader-owned slices/maps/pointers before
// canonicalization. It also rejects non-finite numeric Project evidence.
func clone(in observe.ObservedState) (observe.ObservedState, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return observe.ObservedState{}, fail(ErrInvalid)
	}
	if len(raw) > MaxEvidenceBytes {
		return observe.ObservedState{}, fail(ErrLimit)
	}
	var out observe.ObservedState
	if json.Unmarshal(raw, &out) != nil {
		return observe.ObservedState{}, fail(ErrInvalid)
	}
	return out, nil
}
