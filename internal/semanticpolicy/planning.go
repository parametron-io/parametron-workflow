package semanticpolicy

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
)

type Repository struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}
type ParentPhase struct {
	Identity intent.IssueRef `json:"identity"`
	Title    string          `json:"title"`
}

// PlanningContext owns copies of normalized semantic values and explicit relationships.
// Construction is required; the private eligibility bit cannot be supplied by model output.
type PlanningContext struct {
	Repository   Repository        `json:"repository"`
	Type         IssueType         `json:"type"`
	Title        string            `json:"title"`
	Body         string            `json:"body"`
	Labels       []Label           `json:"labels"`
	Priority     Priority          `json:"priority"`
	Effort       Effort            `json:"effort"`
	Parent       *intent.IssueRef  `json:"parent"`
	BlockedBy    []intent.IssueRef `json:"blocked_by"`
	Blocks       []intent.IssueRef `json:"blocks"`
	Refs         []intent.IssueRef `json:"refs"`
	ParentPhase  *ParentPhase      `json:"parent_phase,omitempty"`
	eligible     bool
	createBranch intent.Boolean
}

// CanEstimate describes ownership only, never lifecycle timing or Automation gating.
// PRs have no IssueType and are rejected. Bug timing belongs to Bug triage policy.
func CanEstimate(t IssueType, branch intent.Boolean) bool {
	if t == Phase {
		return branch.Explicit && branch.Value
	}
	return t == Task || t == Feature || t == Bug
}
func validName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func validRef(r intent.IssueRef) bool { return validName(r.Repository) && r.Number > 0 }
func references(in []intent.IssueRef) ([]intent.IssueRef, error) {
	out := make([]intent.IssueRef, 0, len(in))
	for _, r := range in {
		if !validRef(r) {
			return nil, ErrPlanning
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out, nil
}
func NewPlanningContext(repo Repository, title, body string, c IssueClassification, i intent.Intent, parent *ParentPhase) (PlanningContext, error) {
	if !CanEstimate(c.Type, i.CreateBranch) {
		return PlanningContext{}, ErrInapplicable
	}
	c, err := validateClassification(c)
	if err != nil {
		return PlanningContext{}, ErrPlanning
	}
	repo.Owner = strings.ToLower(repo.Owner)
	repo.Name = strings.ToLower(repo.Name)
	if repo.Owner != "parametron-io" || !validName(repo.Name) {
		return PlanningContext{}, ErrPlanning
	}
	p := PlanningContext{Repository: repo, Type: c.Type, Title: title, Body: body, Labels: c.Labels, Priority: c.Priority, Effort: c.Effort, eligible: true, createBranch: i.CreateBranch}
	if i.Parent != nil {
		if !validRef(*i.Parent) {
			return PlanningContext{}, ErrPlanning
		}
		copy := *i.Parent
		p.Parent = &copy
	}
	if p.BlockedBy, err = references(i.BlockedBy); err != nil {
		return PlanningContext{}, err
	}
	if p.Blocks, err = references(i.Blocks); err != nil {
		return PlanningContext{}, err
	}
	if p.Refs, err = references(i.Refs); err != nil {
		return PlanningContext{}, err
	}
	if parent != nil {
		if p.Parent == nil || parent.Identity != *p.Parent || !validRef(parent.Identity) {
			return PlanningContext{}, ErrPlanning
		}
		copy := *parent
		p.ParentPhase = &copy
	}
	return p, nil
}

// Serialize revalidates caller-editable values and emits byte-stable structured JSON.
func (p PlanningContext) Serialize() (semantic.ContextJSON, error) {
	if !p.eligible {
		return "", ErrInapplicable
	}
	normalized, err := NewPlanningContext(p.Repository, p.Title, p.Body, IssueClassification{p.Type, p.Labels, p.Priority, p.Effort}, intent.Intent{CreateBranch: p.createBranch, Parent: p.Parent, BlockedBy: p.BlockedBy, Blocks: p.Blocks, Refs: p.Refs}, p.ParentPhase)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", ErrPlanning
	}
	return semantic.ContextJSON(data), nil
}
func (p PlanningContext) input() (semantic.Input, error) {
	data, err := p.Serialize()
	if err != nil {
		return semantic.Input{}, err
	}
	return semantic.Input{Title: p.Title, Body: p.Body, Context: data}, nil
}
