package observe

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"sort"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

type Presence string

const (
	Present Presence = "present"
	Missing Presence = "missing"
)

// ObservedState contains exactly one Issue/PR when present and neither when
// missing. Projects are complete only for present resources.
// GitHub models are normalized read-only domain data, not transport payloads.
type ObservedState struct {
	Resource    storage.Resource
	Presence    Presence
	Issue       *github.Issue
	PullRequest *github.PullRequest
	Projects    []ProjectState
}
type ProjectState struct {
	Profile config.Profile
	ID      string
	// Empty Items means known absent membership, not unknown/unread membership.
	Items []ProjectItem
}
type ProjectItem struct {
	ID       string
	Archived bool
	// Missing roles mean unset; a present numeric zero remains explicit.
	Fields map[config.FieldRole]FieldValue
}
type FieldValue struct {
	github.FieldValue
	// Empty for an unmapped option. OptionID is always preserved.
	StatusRole config.StatusRole
}

// PolicyInput is the decision-record handoff: durable trigger, bound identity,
// and current observation. It is not itself a decision or a persisted record.
type PolicyInput struct {
	DeliveryID string
	Sequence   int64
	Observed   ObservedState
}
type Consumer interface {
	Evaluate(context.Context, PolicyInput) error
}

type Processor struct {
	deployment config.ResolvedConfig
	client     github.Client
	consumer   Consumer
}

func nilInterface(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Interface:
		return r.IsNil()
	}
	return false
}
func NewProcessor(d config.ResolvedConfig, client github.Client, consumer Consumer) (*Processor, error) {
	if nilInterface(client) || nilInterface(consumer) {
		return nil, ErrConfiguration
	}
	copy, err := snapshot(d)
	if err != nil {
		return nil, err
	}
	return &Processor{copy, client, consumer}, nil
}

func (p *Processor) Process(ctx context.Context, e storage.Event) error {
	if e.Resource == nil {
		return ErrBinding
	}
	o, err := p.ReadCurrent(ctx, *e.Resource)
	if err != nil {
		return err
	}
	return p.consumer.Evaluate(ctx, PolicyInput{e.Delivery.ID, e.Sequence, o})
}

// ReadCurrent performs complete read-only observation without a policy handoff.
func (p *Processor) ReadCurrent(ctx context.Context, resource storage.Resource) (ObservedState, error) {
	o, err := p.ReadPrimary(ctx, resource)
	if err != nil {
		return ObservedState{}, err
	}
	if o.Presence == Missing {
		return o, nil
	}
	r := o.Resource
	for _, project := range []struct {
		profile config.Profile
		binding config.ResolvedProject
	}{{config.Engineering, p.deployment.Engineering}, {config.BugTracker, p.deployment.BugTracker}} {
		fields := map[string]config.FieldKind{}
		for role, id := range project.binding.Fields {
			fields[id] = fieldKind(role)
		}
		items, err := p.client.ProjectItems(ctx, r.NodeID, project.binding.ID, fields)
		if err != nil {
			return ObservedState{}, err
		}
		state, err := normalizeProject(project.profile, project.binding, r.NodeID, r.Kind, items)
		if err != nil {
			return ObservedState{}, err
		}
		o.Projects = append(o.Projects, state)
	}
	return o, nil
}

// ReadPrimary reuses observation identity verification without fetching Projects.
// Projects is nil: this is a primary read, not a complete policy observation.
func (p *Processor) ReadPrimary(ctx context.Context, r storage.Resource) (ObservedState, error) {
	r.Owner, r.Repository, r.NodeID = strings.ToLower(r.Owner), strings.ToLower(r.Repository), ""
	if r.Number <= 0 || int64(int(r.Number)) != r.Number || (r.Kind != "issue" && r.Kind != "pull_request") {
		return ObservedState{}, ErrBinding
	}
	repo, err := repository(p.deployment, r)
	if err != nil {
		return ObservedState{}, err
	}
	// Use discovered spelling for the GitHub client, lowercase for durable identity.
	ref := github.Ref{Owner: repo.Owner, Repository: repo.Name, Number: int(r.Number)}
	o := ObservedState{Resource: r, Presence: Present}
	var identity github.Identity
	if r.Kind == "issue" {
		value, readErr := p.client.Issue(ctx, ref)
		err = readErr
		if err == nil {
			value = normalizeIssue(value)
			identity = value.Identity
			o.Issue = &value
		}
	} else {
		value, readErr := p.client.PullRequest(ctx, ref)
		err = readErr
		if err == nil {
			value = normalizePR(value)
			identity = value.Identity
			o.PullRequest = &value
		}
	}
	if err != nil {
		var ge *github.Error
		if !errors.As(err, &ge) || ge.Category != github.NotFound {
			return ObservedState{}, err
		}
		o = ObservedState{Resource: r, Presence: Missing}
		return o, nil
	}
	if strings.TrimSpace(identity.ID) == "" || identity.Number != ref.Number || !strings.EqualFold(identity.Repository.Owner, repo.Owner) || !strings.EqualFold(identity.Repository.Name, repo.Name) || (repo.ID != "" && identity.Repository.ID != repo.ID) {
		return ObservedState{}, ErrObservation
	}
	o.Resource.NodeID = identity.ID
	return o, nil
}

func fieldKind(role config.FieldRole) config.FieldKind {
	switch role {
	case config.Estimate, config.PriorityScore, config.RoadmapOrder:
		return config.Number
	case config.StartDate:
		return config.Date
	default:
		return config.SingleSelect
	}
}

// Snapshot validates the resolved boundary and owns its maps. Normalized name
// collisions are rejected because storage uses case-insensitive resource keys.
func snapshot(d config.ResolvedConfig) (config.ResolvedConfig, error) {
	if strings.TrimSpace(d.Organization.Login) == "" || len(d.Repositories) == 0 {
		return config.ResolvedConfig{}, ErrConfiguration
	}
	d.Repositories = append([]config.Repository(nil), d.Repositories...)
	d.IssueTypes = maps.Clone(d.IssueTypes)
	seen := map[string]bool{}
	for _, r := range d.Repositories {
		key := strings.ToLower(r.Name)
		if strings.TrimSpace(r.Name) == "" || !strings.EqualFold(r.Owner, d.Organization.Login) || seen[key] {
			return config.ResolvedConfig{}, ErrConfiguration
		}
		seen[key] = true
	}
	clone := func(p config.ResolvedProject, bug bool) (config.ResolvedProject, error) {
		if strings.TrimSpace(p.ID) == "" {
			return p, ErrConfiguration
		}
		roles := []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate}
		statuses := []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done}
		if bug {
			roles = append(roles, config.PriorityScore)
			statuses = append(statuses, config.ToTriage)
		} else {
			roles = append(roles, config.RoadmapOrder)
			statuses = append(statuses, config.Blocked)
		}
		if len(p.Fields) != len(roles) || len(p.StatusOptions) != len(statuses) {
			return p, ErrConfiguration
		}
		f := map[config.FieldRole]string{}
		s := map[config.StatusRole]string{}
		ids := map[string]bool{}
		for _, role := range roles {
			id := p.Fields[role]
			if strings.TrimSpace(id) == "" || ids[id] {
				return p, ErrConfiguration
			}
			ids[id] = true
			f[role] = id
		}
		ids = map[string]bool{}
		for _, role := range statuses {
			id := p.StatusOptions[role]
			if strings.TrimSpace(id) == "" || ids[id] {
				return p, ErrConfiguration
			}
			ids[id] = true
			s[role] = id
		}
		p.Fields, p.StatusOptions = f, s
		options := map[config.FieldRole]map[string]string{}
		for role, values := range p.FieldOptions {
			options[role] = maps.Clone(values)
		}
		p.FieldOptions = options
		return p, nil
	}
	var err error
	d.Engineering, err = clone(d.Engineering, false)
	if err != nil {
		return config.ResolvedConfig{}, err
	}
	d.BugTracker, err = clone(d.BugTracker, true)
	if err != nil || d.Engineering.ID == d.BugTracker.ID {
		return config.ResolvedConfig{}, ErrConfiguration
	}
	return d, nil
}

func normalizedIdentity(i github.Identity) github.Identity {
	i.Repository.Owner = strings.ToLower(i.Repository.Owner)
	i.Repository.Name = strings.ToLower(i.Repository.Name)
	return i
}
func identities(in []github.Identity) []github.Identity {
	out := make([]github.Identity, len(in))
	for i, v := range in {
		out[i] = normalizedIdentity(v)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Repository.Owner != b.Repository.Owner {
			return a.Repository.Owner < b.Repository.Owner
		}
		if a.Repository.Name != b.Repository.Name {
			return a.Repository.Name < b.Repository.Name
		}
		if a.Number != b.Number {
			return a.Number < b.Number
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Repository.ID < b.Repository.ID
	})
	return out
}
func labels(in []string) []string { out := append([]string{}, in...); sort.Strings(out); return out }
func actor(in *github.Actor) *github.Actor {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
func normalizeIssue(v github.Issue) github.Issue {
	v.Identity = normalizedIdentity(v.Identity)
	v.Labels = labels(v.Labels)
	v.Assignees = append([]github.Actor{}, v.Assignees...)
	sort.Slice(v.Assignees, func(i, j int) bool { return v.Assignees[i].Login < v.Assignees[j].Login })
	v.Author = actor(v.Author)
	if v.Type != nil {
		copy := *v.Type
		v.Type = &copy
	}
	if v.Parent != nil {
		copy := normalizedIdentity(*v.Parent)
		v.Parent = &copy
	}
	v.SubIssues, v.BlockedBy, v.Blocking, v.LinkedPullRequests = identities(v.SubIssues), identities(v.BlockedBy), identities(v.Blocking), identities(v.LinkedPullRequests)
	return v
}
func normalizePR(v github.PullRequest) github.PullRequest {
	v.Identity = normalizedIdentity(v.Identity)
	v.Labels = labels(v.Labels)
	v.Author = actor(v.Author)
	v.ClosingIssues = identities(v.ClosingIssues)
	return v
}

var _ worker.Processor = (*Processor)(nil)
