// Package prcompletion owns accepted implementation terminal effects, separately
// from OPEN-only binding, onboarding initialization and review authorization.
package prcompletion

import (
	"errors"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid    = errors.New("pr completion: invalid composition or identity")
	ErrRejected   = errors.New("pr completion: rejected terminal authority")
	ErrChanged    = errors.New("pr completion: current authority changed")
	ErrProject    = errors.New("pr completion: conflicting Project identity")
	ErrProvenance = errors.New("pr completion: corrupt close provenance")
	ErrAmbiguous  = errors.New("pr completion: indeterminate native close outcome")
	ErrStorage    = errors.New("pr completion: storage failure")
	ErrReopened   = errors.New("pr completion: target reopened after merge")
)

type Outcome string

const (
	Open      Outcome = "open"
	Merged    Outcome = "merged"
	Abandoned Outcome = "abandoned"
)

type Input struct {
	Onboarding pronboarding.Completion
	PR         github.PullRequest
	Target     github.Issue
	Node       ec.IssueNode
	Native     github.NativeLifecycle
}
type Decision struct {
	Outcome                Outcome
	PRStatus, TargetStatus config.StatusRole
	CloseTarget            bool
	Merge                  github.NativeTransition
}

func resource(i github.Identity, k string) storage.Resource {
	return storage.Resource{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Kind: k, Number: int64(i.Number), NodeID: i.ID}
}
func validID(s string) bool { return s != "" && !strings.ContainsAny(s, " \t\r\n") }

// ValidateNative rejects unsupported, inconsistent or unordered facts, including
// fake/provider output. Equal timestamps cannot establish terminal ordering.
func ValidateNative(f github.NativeLifecycle, i github.Identity, k, state string) error {
	err := f.Validate(i, k, state)
	if errors.Is(err, github.ErrNativeLifecycleChanged) {
		return ErrChanged
	}
	if err != nil {
		return ErrRejected
	}
	return nil
}

// Evaluate consumes validated onboarding evidence and current exact facts. Its
// terminal path intentionally does not invoke the OPEN-only #46 validator.
func Evaluate(in Input) (Decision, error) {
	o, p, t, n := in.Onboarding, in.PR, in.Target, in.Node
	b := o.Binding
	name, err := eb.Name(resource(b.Target, "issue"))
	if err != nil || !validID(b.PR.ID) || !validID(b.Target.ID) || b.PR.Number <= 0 || !github.ValidBranchRepository(b.PR.Repository) || !github.ValidBranchRepository(b.Target.Repository) || b.Policy != "active_org_member_repository_write_assignee_v1" || !github.ValidHuman(b.Developer) || b.Branch.Repository != b.Target.Repository || b.Branch.Name != name || !github.ValidCommitSHA(b.Branch.SHA) || o.DeliveryID == "" {
		return Decision{}, ErrRejected
	}
	if !o.Completed || o.Resource != resource(p.Identity, "pull_request") || o.Project == "" || o.CompletedAt.IsZero() || b.PR != p.Identity || b.Target != t.Identity || n.Resource != resource(t.Identity, "issue") || n.Type != b.Kind || string(n.State) != t.State {
		return Decision{}, ErrRejected
	}
	if n.Type != sp.Task && n.Type != sp.Feature && n.Type != sp.Phase || t.State != "OPEN" && t.State != "CLOSED" || p.State != "OPEN" && p.State != "CLOSED" && p.State != "MERGED" {
		return Decision{}, ErrRejected
	}
	if p.Author == nil || !github.SameUser(*p.Author, b.Developer) || p.HeadRepository != b.Branch.Repository || p.HeadRef != b.Branch.Name || !github.ValidCommitSHA(p.HeadSHA) {
		return Decision{}, ErrChanged
	}
	parsed, err := intent.Parse(p.Body, intent.Context{Organization: strings.ToLower(p.Repository.Owner), Repository: strings.ToLower(p.Repository.Name)})
	if err != nil || parsed.Target == nil || parsed.Target.Number != int64(t.Number) || !strings.EqualFold(parsed.Target.Repository, t.Repository.Name) {
		return Decision{}, ErrRejected
	}
	if n.Type == sp.Phase && (!n.Intent.CreateBranch.Explicit || !n.Intent.CreateBranch.Value) {
		return Decision{}, ErrRejected
	}
	if p.State == "OPEN" {
		return Decision{Outcome: Open}, nil
	}
	if err := ValidateNative(in.Native, p.Identity, "pull_request", p.State); err != nil {
		return Decision{}, err
	}
	last := in.Native.Transitions[len(in.Native.Transitions)-1]
	if !last.CreatedAt.After(o.CompletedAt) {
		return Decision{}, ErrRejected
	}
	if p.State == "CLOSED" {
		return Decision{Outcome: Abandoned, PRStatus: config.Done, TargetStatus: config.InProgress}, nil
	}
	return Decision{Outcome: Merged, PRStatus: config.Done, CloseTarget: n.Type != sp.Phase, Merge: last}, nil
}
