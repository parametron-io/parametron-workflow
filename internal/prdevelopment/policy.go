// Package prdevelopment projects accepted Target authority without lifecycle effects.
package prdevelopment

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/pronboarding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid   = errors.New("pr development: invalid identity or composition")
	ErrRejected  = errors.New("pr development: current authority rejected")
	ErrChanged   = errors.New("pr development: current evidence changed")
	ErrAmbiguous = errors.New("pr development: ambiguous relationship evidence")
)

type Action struct {
	Add    bool
	Target github.Identity
}
type Input struct {
	Binding    prbinding.Decision
	Onboarding pronboarding.Completion
	PR         github.PullRequest
	Target     github.Issue
	References github.DevelopmentReferences
	Project    string
}

func resource(i github.Identity, k string) storage.Resource {
	return storage.Resource{Owner: strings.ToLower(i.Repository.Owner), Repository: strings.ToLower(i.Repository.Name), Kind: k, Number: int64(i.Number), NodeID: i.ID}
}
func validBinding(b prbinding.Evidence) bool {
	name, err := eb.Name(resource(b.Target, "issue"))
	return err == nil && github.ValidDevelopmentIdentity(b.PR) && github.ValidDevelopmentIdentity(b.Target) && b.PR.ID != b.Target.ID && (b.Kind == sp.Task || b.Kind == sp.Feature || b.Kind == sp.Phase) && b.Policy == "active_org_member_repository_write_assignee_v1" && github.ValidHuman(b.Developer) && b.Branch.Repository == b.Target.Repository && b.Branch.Name == name && github.ValidCommitSHA(b.Branch.SHA)
}
func sameBinding(a, b prbinding.Evidence) bool { a.Branch.SHA = b.Branch.SHA; return a == b }

// Evaluate never derives authority from native links. Closed PRs preserve existing
// traceability but receive no effects, even when a historical link is absent.
func Evaluate(in Input) ([]Action, error) {
	b, o := in.Binding.Evidence, in.Onboarding
	if in.Binding.Outcome != prbinding.Accepted || in.Binding.Reason != "" || !validBinding(b) {
		return nil, ErrRejected
	}
	if !o.Completed || o.Resource != resource(b.PR, "pull_request") || o.Project != in.Project || in.Project == "" || o.CompletedAt.IsZero() || o.DeliveryID == "" || !sameBinding(o.Binding, b) {
		return nil, ErrRejected
	}
	if in.PR.Identity != b.PR || in.Target.Identity != b.Target || in.PR.Author == nil || !github.SameUser(*in.PR.Author, b.Developer) || in.PR.HeadRepository != b.Branch.Repository || in.PR.HeadRef != b.Branch.Name || in.PR.HeadSHA != b.Branch.SHA {
		return nil, ErrChanged
	}
	if in.PR.State != "OPEN" && in.PR.State != "CLOSED" && in.PR.State != "MERGED" || in.Target.State != "OPEN" && in.Target.State != "CLOSED" {
		return nil, ErrInvalid
	}
	parsed, err := intent.Parse(in.PR.Body, intent.Context{Organization: strings.ToLower(b.PR.Repository.Owner), Repository: strings.ToLower(b.PR.Repository.Name)})
	if err != nil || parsed.Target == nil || parsed.Target.Number != int64(b.Target.Number) || !strings.EqualFold(parsed.Target.Repository, b.Target.Repository.Name) {
		return nil, ErrChanged
	}
	if err := in.References.Validate(b.PR); err != nil {
		return nil, err
	}
	for _, list := range [][]github.Identity{in.References.Manual, in.References.Keyword} {
		for _, i := range list {
			if i.Repository.ID == b.Target.Repository.ID && i.Repository != b.Target.Repository || strings.EqualFold(i.Repository.Owner, b.Target.Repository.Owner) && strings.EqualFold(i.Repository.Name, b.Target.Repository.Name) && i.Repository.ID != b.Target.Repository.ID {
				return nil, ErrAmbiguous
			}
			if i != b.Target && (i.ID == b.Target.ID || i.Number == b.Target.Number && strings.EqualFold(i.Repository.Owner, b.Target.Repository.Owner) && strings.EqualFold(i.Repository.Name, b.Target.Repository.Name)) {
				return nil, ErrAmbiguous
			}
		}
	}
	if in.PR.State != "OPEN" {
		return nil, nil
	}
	if in.Target.State != "OPEN" {
		return nil, ErrRejected
	}
	canonical := false
	for _, i := range in.References.Keyword {
		if i != b.Target {
			return nil, ErrAmbiguous
		}
		canonical = true
	}
	actions := []Action{}
	for _, i := range in.References.Manual {
		if i == b.Target {
			canonical = true
		} else {
			actions = append(actions, Action{Target: i})
		}
	}
	sort.Slice(actions, func(i, j int) bool {
		a, b := actions[i].Target, actions[j].Target
		return fmt.Sprintf("%s/%s/%020d/%s/%s", strings.ToLower(a.Repository.Owner), strings.ToLower(a.Repository.Name), a.Number, a.Repository.ID, a.ID) < fmt.Sprintf("%s/%s/%020d/%s/%s", strings.ToLower(b.Repository.Owner), strings.ToLower(b.Repository.Name), b.Number, b.Repository.ID, b.ID)
	})
	if !canonical {
		actions = append(actions, Action{Add: true, Target: b.Target})
	}
	return actions, nil
}
