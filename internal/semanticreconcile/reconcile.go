// Package semanticreconcile owns accepted semantic metadata convergence only.
package semanticreconcile

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var ErrReconcile = errors.New("semantic reconciliation: invalid authority or state")

type Error struct{ Reason string }

func (e *Error) Error() string        { return "semantic reconciliation: " + e.Reason }
func (e *Error) Is(target error) bool { return target == ErrReconcile }
func fail(reason string) error        { return &Error{reason} }

type DesiredIssue struct {
	Type           semanticpolicy.IssueType
	ManagedLabels  []semanticpolicy.Label
	Priority       semanticpolicy.Priority
	Effort         semanticpolicy.Effort
	ProjectProfile config.Profile
}
type DesiredPR struct{ ManagedLabels []semanticpolicy.Label }

func DesiredIssueState(c semanticpolicy.IssueClassification) (DesiredIssue, error) {
	if !slices.Contains(semanticpolicy.Types(), c.Type) || !slices.Contains(semanticpolicy.Priorities(), c.Priority) || !slices.Contains(semanticpolicy.Efforts(), c.Effort) {
		return DesiredIssue{}, fail("classification")
	}
	labels, err := ordered(c.Labels)
	if err != nil {
		return DesiredIssue{}, err
	}
	profile := config.Engineering
	if c.Type == semanticpolicy.Bug {
		profile = config.BugTracker
	}
	return DesiredIssue{c.Type, labels, c.Priority, c.Effort, profile}, nil
}
func DesiredPRState(c semanticpolicy.PRClassification) (DesiredPR, error) {
	labels, err := ordered(c.Labels)
	return DesiredPR{labels}, err
}
func ordered(labels []semanticpolicy.Label) ([]semanticpolicy.Label, error) {
	taxonomy := semanticpolicy.ManagedLabels()
	out := []semanticpolicy.Label{}
	for i, l := range labels {
		if !slices.Contains(taxonomy, l) || slices.Contains(labels[:i], l) {
			return nil, fail("labels")
		}
	}
	for _, l := range taxonomy {
		if slices.Contains(labels, l) {
			out = append(out, l)
		}
	}
	return out, nil
}

type Reader interface {
	ReadCurrent(context.Context, storage.Resource) (observe.ObservedState, error)
	ReadPrimary(context.Context, storage.Resource) (observe.ObservedState, error)
}
type Reconciler struct {
	deployment config.ResolvedConfig
	reader     Reader
	mutator    github.Mutator
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
func New(d config.ResolvedConfig, reader Reader, mutator github.Mutator) (*Reconciler, error) {
	if absent(reader) || absent(mutator) {
		return nil, fail("configuration")
	}
	d.IssueTypes = maps.Clone(d.IssueTypes)
	ids := map[string]bool{}
	for _, typ := range semanticpolicy.Types() {
		id := d.IssueTypes[string(typ)]
		if strings.TrimSpace(id) == "" || ids[id] {
			return nil, fail("issue type configuration")
		}
		ids[id] = true
	}
	clone := func(p config.ResolvedProject) (config.ResolvedProject, error) {
		p.Fields = maps.Clone(p.Fields)
		p.StatusOptions = maps.Clone(p.StatusOptions)
		options := map[config.FieldRole]map[string]string{}
		for role, values := range p.FieldOptions {
			options[role] = maps.Clone(values)
		}
		p.FieldOptions = options
		if strings.TrimSpace(p.ID) == "" || p.StatusOptions[config.Backlog] == "" {
			return p, fail("project configuration")
		}
		for _, role := range []config.FieldRole{config.Status, config.Priority, config.Effort} {
			if p.Fields[role] == "" {
				return p, fail("field configuration")
			}
		}
		required := map[config.FieldRole][]string{config.Priority: {}, config.Effort: {}}
		for _, v := range semanticpolicy.Priorities() {
			required[config.Priority] = append(required[config.Priority], string(v))
		}
		for _, v := range semanticpolicy.Efforts() {
			required[config.Effort] = append(required[config.Effort], string(v))
		}
		for _, role := range []config.FieldRole{config.Priority, config.Effort} {
			seen := map[string]bool{}
			for _, name := range required[role] {
				id := p.FieldOptions[role][name]
				if strings.TrimSpace(id) == "" || seen[id] {
					return p, fail("option configuration")
				}
				seen[id] = true
			}
		}
		return p, nil
	}
	var err error
	d.Engineering, err = clone(d.Engineering)
	if err != nil {
		return nil, err
	}
	d.BugTracker, err = clone(d.BugTracker)
	if err != nil {
		return nil, err
	}
	if d.Engineering.ID == d.BugTracker.ID {
		return nil, fail("project configuration")
	}
	return &Reconciler{d, reader, mutator}, nil
}
func (r *Reconciler) Evaluate(ctx context.Context, in semanticflow.PolicyInput) error {
	switch in.Classification.State() {
	case semanticflow.NotRequired:
		return nil
	case semanticflow.Accepted:
	default:
		return fail("classification state")
	}
	resource := in.Current.Observed.Resource
	var desired DesiredIssue
	var labels []semanticpolicy.Label
	switch resource.Kind {
	case "issue":
		accepted, ok := in.Classification.Issue()
		if !ok {
			return fail("classification kind")
		}
		var err error
		desired, err = DesiredIssueState(accepted.Classification)
		if err != nil {
			return err
		}
		labels = desired.ManagedLabels
	case "pull_request":
		accepted, ok := in.Classification.PR()
		if !ok {
			return fail("classification kind")
		}
		d, err := DesiredPRState(accepted.Classification)
		if err != nil {
			return err
		}
		labels = d.ManagedLabels
	default:
		return fail("resource kind")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var fresh observe.ObservedState
	var err error
	if resource.Kind == "issue" {
		fresh, err = r.reader.ReadCurrent(ctx, resource)
	} else {
		fresh, err = r.reader.ReadPrimary(ctx, resource)
	}
	if err != nil {
		return err
	}
	if fresh.Presence == observe.Missing {
		return nil
	}
	if fresh.Presence != observe.Present || fresh.Resource.Kind != resource.Kind || fresh.Resource.Number != resource.Number || !strings.EqualFold(fresh.Resource.Owner, resource.Owner) || !strings.EqualFold(fresh.Resource.Repository, resource.Repository) || fresh.Resource.NodeID == "" {
		return fail("observation")
	}
	var body string
	var currentLabels []string
	var repo github.Repository
	if resource.Kind == "issue" {
		if fresh.Issue == nil || fresh.PullRequest != nil {
			return fail("observation")
		}
		body, currentLabels, repo = fresh.Issue.Body, fresh.Issue.Labels, fresh.Issue.Repository
	} else {
		if fresh.PullRequest == nil || fresh.Issue != nil {
			return fail("observation")
		}
		body, currentLabels, repo = fresh.PullRequest.Body, fresh.PullRequest.Labels, fresh.PullRequest.Repository
	}
	explicit, err := intent.Parse(body, intent.Context{Organization: strings.ToLower(fresh.Resource.Owner), Repository: strings.ToLower(fresh.Resource.Repository)})
	if err != nil {
		return fail("current intent")
	}
	if !explicit.Effective().Classification {
		return nil
	}
	var target, other observe.ProjectState
	var binding config.ResolvedProject
	if resource.Kind == "issue" {
		seen := map[config.Profile]bool{}
		for _, p := range fresh.Projects {
			expected := r.deployment.Engineering
			if p.Profile == config.BugTracker {
				expected = r.deployment.BugTracker
			} else if p.Profile != config.Engineering {
				return fail("project observation")
			}
			if seen[p.Profile] || p.ID != expected.ID || len(p.Items) > 1 {
				return fail("ambiguous membership")
			}
			seen[p.Profile] = true
			if p.Profile == desired.ProjectProfile {
				target = p
				binding = expected
			} else {
				other = p
			}
		}
		if len(seen) != 2 {
			return fail("incomplete projects")
		}
	}
	additions, removals := []string{}, []string{}
	for _, l := range semanticpolicy.ManagedLabels() {
		have, want := slices.Contains(currentLabels, string(l)), slices.Contains(labels, l)
		if want && !have {
			additions = append(additions, string(l))
		}
		if have && !want {
			removals = append(removals, string(l))
		}
	}
	// Resolve every label delta before starting writes; missing definitions fail safely.
	addIDs, removeIDs := []string{}, []string{}
	if len(additions) > 0 {
		addIDs, err = r.mutator.ResolveLabels(ctx, repo, additions)
		if err != nil {
			return err
		}
		if len(addIDs) != len(additions) {
			return fail("label resolution")
		}
	}
	if len(removals) > 0 {
		removeIDs, err = r.mutator.ResolveLabels(ctx, repo, removals)
		if err != nil {
			return err
		}
		if len(removeIDs) != len(removals) {
			return fail("label resolution")
		}
	}
	write := func(fn func() error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	}
	id := fresh.Resource.NodeID
	if resource.Kind == "issue" && (fresh.Issue.Type == nil || fresh.Issue.Type.ID != r.deployment.IssueTypes[string(desired.Type)]) {
		if err = write(func() error { return r.mutator.SetIssueType(ctx, id, r.deployment.IssueTypes[string(desired.Type)]) }); err != nil {
			return err
		}
	}
	if len(addIDs) > 0 {
		if err = write(func() error { return r.mutator.AddLabels(ctx, id, addIDs) }); err != nil {
			return err
		}
	}
	if len(removeIDs) > 0 {
		if err = write(func() error { return r.mutator.RemoveLabels(ctx, id, removeIDs) }); err != nil {
			return err
		}
	}
	if resource.Kind == "pull_request" {
		return nil
	}
	item := observe.ProjectItem{Fields: map[config.FieldRole]observe.FieldValue{}}
	if len(target.Items) == 1 {
		item = target.Items[0]
	} else {
		if err = ctx.Err(); err != nil {
			return err
		}
		item.ID, err = r.mutator.AddProjectItem(ctx, binding.ID, id)
		if err != nil {
			return err
		}
		if strings.TrimSpace(item.ID) == "" {
			return fail("added item")
		}
	}
	for _, v := range []struct {
		role   config.FieldRole
		option string
	}{{config.Priority, binding.FieldOptions[config.Priority][string(desired.Priority)]}, {config.Effort, binding.FieldOptions[config.Effort][string(desired.Effort)]}} {
		if item.Fields[v.role].OptionID != v.option {
			if err = write(func() error {
				return r.mutator.SetProjectOption(ctx, binding.ID, item.ID, binding.Fields[v.role], v.option)
			}); err != nil {
				return err
			}
		}
	}
	if explicit.Effective().SetStatus && item.Fields[config.Status].OptionID == "" {
		if err = write(func() error {
			return r.mutator.SetProjectOption(ctx, binding.ID, item.ID, binding.Fields[config.Status], binding.StatusOptions[config.Backlog])
		}); err != nil {
			return err
		}
	}
	if len(other.Items) == 1 {
		return write(func() error { return r.mutator.RemoveProjectItem(ctx, other.ID, other.Items[0].ID) })
	}
	return nil
}

var _ semanticflow.Consumer = (*Reconciler)(nil)
