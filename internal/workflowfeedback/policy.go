// Package workflowfeedback consumes authoritative rejections; it owns no workflow mutations.
package workflowfeedback

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/engineeringterminal"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/prcompanion"
	"github.com/parametron-io/parametron-workflow/internal/prdevelopment"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid   = errors.New("workflow feedback: invalid authority or identity")
	ErrStale     = errors.New("workflow feedback: stale authority")
	ErrChanged   = errors.New("workflow feedback: current facts changed")
	ErrAmbiguous = errors.New("workflow feedback: ambiguous comment evidence")
	ErrOwnership = errors.New("workflow feedback: unverified ownership")
)

type Resource = github.CommentResource
type Category uint8

const (
	BindingTarget Category = iota + 1
	BindingBranch
	BindingDeveloper
	CompanionCapacity
	Lifecycle
	PhaseClosure
	Development
)

// Violation has private fields so raw observations cannot be supplied as decisions.
type Violation struct {
	resource Resource
	category Category
	detail   string
	related  github.Identity
}

func valid(r Resource) bool { return r.Valid() }
func res(i github.Identity, k string) storage.Resource {
	return storage.Resource{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Number: int64(i.Number), NodeID: i.ID, Kind: k}
}
func BindingRejection(r Resource, d prbinding.Decision) (Violation, error) {
	if !valid(r) || r.Kind != "pull_request" || d.Outcome != prbinding.Rejected || d.Evidence != (prbinding.Evidence{}) {
		return Violation{}, ErrInvalid
	}
	var c Category
	switch d.Reason {
	case prbinding.InvalidTarget, prbinding.MissingTarget, prbinding.OutsideDeployment, prbinding.UnsupportedTarget, prbinding.IneligibleTarget:
		c = BindingTarget
	case prbinding.MissingProvenance, prbinding.InvalidProvenance, prbinding.MissingHead, prbinding.HeadMismatch:
		c = BindingBranch
	case prbinding.InvalidDeveloper, prbinding.InactiveMembership, prbinding.InsufficientPermission, prbinding.AssigneeMismatch:
		c = BindingDeveloper
	case prbinding.Changed, prbinding.UnresolvedTarget, prbinding.MissingPR:
		return Violation{}, ErrStale
	default:
		return Violation{}, ErrInvalid
	}
	return Violation{r, c, string(d.Reason), github.Identity{}}, nil
}

// CapacityRejection requires the complete current producer input, not ErrCapacity
// alone. Evaluate retains all companion eligibility and allocation authority.
func CapacityRejection(r Resource, in prcompanion.Input) (Violation, error) {
	if !valid(r) || r.Kind != "pull_request" {
		return Violation{}, ErrInvalid
	}
	if _, err := prcompanion.Evaluate(in); !errors.Is(err, prcompanion.ErrCapacity) {
		return Violation{}, ErrInvalid
	}
	var target github.Identity
	for _, p := range in.PullRequests {
		if p.Identity == r.Identity && p.Resource == res(r.Identity, r.Kind) && p.State == prcompanion.Open && p.Binding.Outcome == prbinding.Accepted {
			target = p.Binding.Evidence.Target
		}
	}
	count := 0
	for _, p := range in.PullRequests {
		if p.State == prcompanion.Open && p.Binding.Outcome == prbinding.Accepted && p.Binding.Evidence.Target == target {
			count++
		}
	}
	if !github.ValidDevelopmentIdentity(target) || count <= 9 || count > 10000 {
		return Violation{}, ErrInvalid
	}
	return Violation{r, CompanionCapacity, fmt.Sprint(count), target}, nil
}
func ClosureRejection(r Resource, d engineeringterminal.Rejection) (Violation, error) {
	if !valid(r) || r.Kind != "issue" || d.Reason != engineeringterminal.OpenChildren || d.Resource != res(r.Identity, r.Kind) {
		return Violation{}, ErrInvalid
	}
	return Violation{resource: r, category: PhaseClosure}, nil
}

// DevelopmentCorrection accepts only an actual typed contradictory removal from
// a validated producer plan; canonical missing-link additions remain silent.
func DevelopmentCorrection(r Resource, in prdevelopment.Input, a prdevelopment.Action) (Violation, error) {
	if !valid(r) || r.Kind != "pull_request" || in.PR.Identity != r.Identity || a.Add {
		return Violation{}, ErrInvalid
	}
	actions, err := prdevelopment.Evaluate(in)
	if err != nil {
		return Violation{}, err
	}
	for _, x := range actions {
		if x == a && !x.Add {
			return Violation{r, Development, "", in.Binding.Evidence.Target}, nil
		}
	}
	return Violation{}, ErrInvalid
}

type LifecycleReason string

const (
	ActiveBlocker                 LifecycleReason = "active_blocker"
	InactiveParent                LifecycleReason = "inactive_parent_phase"
	MissingExecutionAuthorization LifecycleReason = "missing_execution_authorization"
)

// ManualRejection is an integration contract for an owning lifecycle producer.
// Its caller must verify explicit rejected human intent, current policy, and
// correction ownership. Observed Status drift alone cannot construct authority.
// No existing lifecycle producer is implicitly adapted to this contract.
type ManualRejection struct {
	Resource Resource
	Reason   LifecycleReason
}

func LifecycleRejection(d ManualRejection) (Violation, error) {
	if !valid(d.Resource) || d.Resource.Kind != "issue" {
		return Violation{}, ErrInvalid
	}
	switch d.Reason {
	case ActiveBlocker, InactiveParent, MissingExecutionAuthorization:
	default:
		return Violation{}, ErrInvalid
	}
	return Violation{resource: d.Resource, category: Lifecycle, detail: string(d.Reason)}, nil
}

type Current struct {
	Resource   Resource
	Complete   bool
	Violations []Violation
}

func canonical(c Current) ([]Violation, error) {
	if !valid(c.Resource) || !c.Complete || len(c.Violations) > 256 {
		return nil, ErrInvalid
	}
	out := append([]Violation(nil), c.Violations...)
	for _, v := range out {
		if v.resource != c.Resource || v.category < BindingTarget || v.category > Development {
			return nil, ErrInvalid
		}
	}
	key := func(v Violation) string {
		return fmt.Sprintf("%02d/%s/%s/%s/%020d/%s/%s", v.category, v.detail, strings.ToLower(v.related.Repository.Owner), strings.ToLower(v.related.Repository.Name), v.related.Number, v.related.Repository.ID, v.related.ID)
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	unique := out[:0]
	for _, v := range out {
		if len(unique) == 0 || unique[len(unique)-1] != v {
			unique = append(unique, v)
		}
	}
	return unique, nil
}
