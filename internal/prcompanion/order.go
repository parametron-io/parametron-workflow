// Package prcompanion computes desired Engineering PR Roadmap Order values.
// Callers supply current authoritative facts; this package performs no reads,
// writes, lifecycle transitions, or persistence. Binding decisions are current
// evidence, never durable authorization grants.
package prcompanion

import (
	"errors"
	"sort"
	"strings"

	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid   = errors.New("pr companion: invalid input or identity")
	ErrBinding   = errors.New("pr companion: conflicting binding evidence")
	ErrPrimary   = errors.New("pr companion: contradictory primary assignments")
	ErrTarget    = errors.New("pr companion: unavailable Target assignment")
	ErrCapacity  = errors.New("pr companion: companion capacity exceeded")
	ErrOwnership = errors.New("pr companion: unsafe ownership evidence")
)

type Error struct{ Kind error }

func (e *Error) Error() string { return e.Kind.Error() }
func (e *Error) Unwrap() error { return e.Kind }
func fail(kind error) error    { return &Error{Kind: kind} }

type State string

const (
	Open   State = "OPEN"
	Closed State = "CLOSED"
	Merged State = "MERGED"
)

// PullRequest pairs an exact canonical identity with authoritative current
// lifecycle evidence and the current #46 decision. Zero Decision means unknown,
// not rejected. Review Status and Draft are deliberately absent.
type PullRequest struct {
	Resource storage.Resource
	Identity github.Identity
	State    State
	Binding  prbinding.Decision
}

// OwnedField is explicit caller-validated provenance of this PR's previously
// controller-owned Engineering Roadmap Order field. Neither an observed value
// nor a historical binding alone may be used to construct this evidence.
// Target identifies the historical exact Target, not a current grant. No numeric
// value is needed or trusted. This type authorizes no other field or Project.
type OwnedField struct {
	PR     github.Identity
	Target github.Identity
}

type Input struct {
	// Primary must be a current authoritative #36 result. Topology is not rebuilt.
	Primary      eo.Plan
	PullRequests []PullRequest
	Owned        []OwnedField
}
type Assignment struct {
	PR           storage.Resource
	Target       storage.Resource
	RoadmapOrder int
}
type Plan struct {
	Assignments []Assignment
	// Clear means unset only the PR's owned Engineering Roadmap Order field.
	Clear []storage.Resource
}

func validID(s string) bool { return s != "" && !strings.ContainsAny(s, " \t\r\n") }
func canonical(r storage.Resource, kind string) (storage.Resource, bool) {
	r.Owner = strings.ToLower(r.Owner)
	r.Repository = strings.ToLower(r.Repository)
	return r, r.Kind == kind && r.Number > 0 && validID(r.NodeID) && github.ValidBranchRepository(github.Repository{Owner: r.Owner, Name: r.Repository, ID: "validation"})
}
func resource(i github.Identity, kind string) (storage.Resource, bool) {
	r, ok := canonical(storage.Resource{Owner: i.Repository.Owner, Repository: i.Repository.Name, Kind: kind, Number: int64(i.Number), NodeID: i.ID}, kind)
	return r, ok && github.ValidBranchRepository(i.Repository)
}
func key(r storage.Resource) storage.Resource { r.NodeID = ""; return r }
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

// identities rejects node reuse, conflicting node evidence and inconsistent
// repository IDs, including across PR, Target and ownership evidence.
type identities struct {
	nodes     map[storage.Resource]string
	keys      map[string]storage.Resource
	repos     map[string]string
	repoNames map[string]string
}

func (v *identities) add(r storage.Resource) bool {
	k := key(r)
	if n, ok := v.nodes[k]; ok && n != r.NodeID {
		return false
	}
	if prior, ok := v.keys[r.NodeID]; ok && prior != k {
		return false
	}
	v.nodes[k], v.keys[r.NodeID] = r.NodeID, k
	return true
}
func (v *identities) identity(i github.Identity, kind string) (storage.Resource, bool) {
	r, ok := resource(i, kind)
	if !ok || !v.add(r) {
		return storage.Resource{}, false
	}
	name := r.Owner + "/" + r.Repository
	if id, ok := v.repos[name]; ok && id != i.Repository.ID {
		return storage.Resource{}, false
	}
	if prior, ok := v.repoNames[i.Repository.ID]; ok && prior != name {
		return storage.Resource{}, false
	}
	v.repos[name], v.repoNames[i.Repository.ID] = i.Repository.ID, name
	return r, true
}
func accepted(d prbinding.Decision) bool {
	e := d.Evidence
	return d.Reason == "" && (e.Kind == sp.Phase || e.Kind == sp.Task || e.Kind == sp.Feature) && github.ValidHuman(e.Developer) && e.Policy == "active_org_member_repository_write_assignee_v1" && github.ValidBranchRepository(e.Branch.Repository) && strings.EqualFold(e.Branch.Repository.Owner, e.Target.Repository.Owner) && strings.EqualFold(e.Branch.Repository.Name, e.Target.Repository.Name) && e.Branch.Repository.ID == e.Target.Repository.ID && github.ValidBranchName(e.Branch.Name) && github.ValidCommitSHA(e.Branch.SHA)
}
func validReason(r prbinding.Reason) bool {
	switch r {
	case prbinding.MissingPR, prbinding.InvalidTarget, prbinding.MissingTarget, prbinding.OutsideDeployment, prbinding.UnresolvedTarget, prbinding.UnsupportedTarget, prbinding.IneligibleTarget, prbinding.MissingProvenance, prbinding.InvalidProvenance, prbinding.HeadMismatch, prbinding.MissingHead, prbinding.InvalidDeveloper, prbinding.InactiveMembership, prbinding.InsufficientPermission, prbinding.AssigneeMismatch, prbinding.Changed:
		return true
	}
	return false
}

// These current rejections deny eligibility rather than describing unavailable
// or changing evidence. They permit clearing only with separate field ownership.
func denied(d prbinding.Decision) bool {
	return d.Outcome == prbinding.Rejected && d.Reason != prbinding.Changed && d.Reason != prbinding.UnresolvedTarget && d.Reason != prbinding.MissingPR
}

// Evaluate allocates dense canonical PR slots within shared generic capacity.
// No other companion occupants are represented or authorized by this boundary;
// future generic allocation must explicitly arbitrate those occupants before
// consuming this plan. Every failure returns a zero Plan.
func Evaluate(in Input) (Plan, error) {
	ids := identities{map[storage.Resource]string{}, map[string]storage.Resource{}, map[string]string{}, map[string]string{}}
	primary := map[storage.Resource]int{}
	seenPrimary := map[storage.Resource]bool{}
	values := map[int]bool{}
	for _, a := range in.Primary.Assignments {
		r, ok := canonical(a.Resource, "issue")
		max := eo.FirstRoadmapBase + eo.MaxRoadmapSegments*eo.RoadmapStride - eo.DirectChildStride
		if !ok || !ids.add(r) || seenPrimary[key(r)] || values[a.RoadmapOrder] || a.RoadmapOrder < eo.FirstRoadmapBase || a.RoadmapOrder > max || a.RoadmapOrder%eo.DirectChildStride != 0 {
			return Plan{}, fail(ErrPrimary)
		}
		if a.AnchorPhase != nil {
			anchor, ok := canonical(*a.AnchorPhase, "issue")
			if !ok || !ids.add(anchor) || key(anchor) == key(r) || a.RoadmapOrder%eo.RoadmapStride == 0 {
				return Plan{}, fail(ErrPrimary)
			}
		} else if a.RoadmapOrder%eo.RoadmapStride != 0 {
			return Plan{}, fail(ErrPrimary)
		}
		primary[r], seenPrimary[key(r)], values[a.RoadmapOrder] = a.RoadmapOrder, true, true
	}
	for _, item := range in.Primary.Clear {
		r, ok := canonical(item, "issue")
		if !ok || !ids.add(r) || seenPrimary[key(r)] {
			return Plan{}, fail(ErrPrimary)
		}
		seenPrimary[key(r)] = true
	}
	anchorBases := map[storage.Resource]int{}
	for _, a := range in.Primary.Assignments {
		if a.AnchorPhase == nil {
			continue
		}
		anchor, _ := canonical(*a.AnchorPhase, "issue")
		base := a.RoadmapOrder - a.RoadmapOrder%eo.RoadmapStride
		if !seenPrimary[key(anchor)] {
			return Plan{}, fail(ErrPrimary)
		}
		if value, ok := primary[anchor]; ok && value != base {
			return Plan{}, fail(ErrPrimary)
		}
		if value, ok := anchorBases[anchor]; ok && value != base {
			return Plan{}, fail(ErrPrimary)
		}
		anchorBases[anchor] = base
	}
	prs := map[storage.Resource]PullRequest{}
	groups := map[storage.Resource][]storage.Resource{}
	for _, p := range in.PullRequests {
		r, ok := canonical(p.Resource, "pull_request")
		ir, iok := ids.identity(p.Identity, "pull_request")
		if !ok || !iok || r != ir || (p.State != Open && p.State != Closed && p.State != Merged) {
			return Plan{}, fail(ErrInvalid)
		}
		if _, dup := prs[key(r)]; dup {
			return Plan{}, fail(ErrBinding)
		}
		prs[key(r)] = p
		d := p.Binding
		switch d.Outcome {
		case prbinding.Accepted:
			if !accepted(d) {
				return Plan{}, fail(ErrBinding)
			}
			br, bok := ids.identity(d.Evidence.PR, "pull_request")
			target, tok := ids.identity(d.Evidence.Target, "issue")
			if !bok || !tok || br != r {
				return Plan{}, fail(ErrBinding)
			}
			if p.State == Open {
				if _, ok := primary[target]; !ok {
					return Plan{}, fail(ErrTarget)
				}
				groups[target] = append(groups[target], r)
			}
		case prbinding.Rejected:
			if !validReason(d.Reason) || d.Evidence != (prbinding.Evidence{}) {
				return Plan{}, fail(ErrBinding)
			}
		case "":
			if d != (prbinding.Decision{}) {
				return Plan{}, fail(ErrBinding)
			}
		default:
			return Plan{}, fail(ErrBinding)
		}
	}
	out := Plan{Assignments: []Assignment{}, Clear: []storage.Resource{}}
	owned := map[storage.Resource]bool{}
	for _, o := range in.Owned {
		r, ok := ids.identity(o.PR, "pull_request")
		_, tok := ids.identity(o.Target, "issue")
		if !ok || !tok || owned[key(r)] {
			return Plan{}, fail(ErrOwnership)
		}
		owned[key(r)] = true
		p, exists := prs[key(r)]
		if !exists || p.Resource.NodeID != r.NodeID {
			return Plan{}, fail(ErrOwnership)
		}
		// Unknown, unresolved, or changing binding evidence supplies no clear
		// authority. Explicit ownership plus terminal state or a definitive
		// current denial establishes that this owned companion is ineligible.
		if p.State == Closed || p.State == Merged || denied(p.Binding) {
			out.Clear = append(out.Clear, r)
		}
	}
	for target, group := range groups {
		if len(group) > eo.MaxCompanionsPerIssue {
			return Plan{}, fail(ErrCapacity)
		}
		sort.Slice(group, func(i, j int) bool { return less(group[i], group[j]) })
		for i, r := range group {
			out.Assignments = append(out.Assignments, Assignment{PR: r, Target: target, RoadmapOrder: primary[target] + i + 1})
		}
	}
	sort.Slice(out.Assignments, func(i, j int) bool {
		a, b := out.Assignments[i], out.Assignments[j]
		if a.RoadmapOrder != b.RoadmapOrder {
			return a.RoadmapOrder < b.RoadmapOrder
		}
		return less(a.PR, b.PR)
	})
	sort.Slice(out.Clear, func(i, j int) bool { return less(out.Clear[i], out.Clear[j]) })
	return out, nil
}
