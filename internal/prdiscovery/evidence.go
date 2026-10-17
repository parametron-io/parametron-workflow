package prdiscovery

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// identities detects contradictions across PRs, Targets and related resources,
// including repository aliases and node reuse between different resource kinds.
type identities struct {
	nodes     map[string]storage.Resource
	resources map[storage.Resource]string
	names     map[string]string
	repos     map[string]string
	items     map[string]storage.Resource
	reserved  map[string]bool
}

func newIdentities(repositories []github.Repository, projects ...string) *identities {
	v := &identities{nodes: map[string]storage.Resource{}, resources: map[storage.Resource]string{}, names: map[string]string{}, repos: map[string]string{}, items: map[string]storage.Resource{}, reserved: map[string]bool{}}
	for _, id := range projects {
		v.reserved[id] = true
	}
	for _, r := range repositories {
		v.names[strings.ToLower(r.Owner+"/"+r.Name)] = r.ID
		v.repos[r.ID] = strings.ToLower(r.Owner + "/" + r.Name)
	}
	return v
}
func (v *identities) add(i github.Identity, kind string, unique bool) bool {
	if !github.ValidDevelopmentIdentity(i) {
		return false
	}
	if v.reserved[i.ID] || v.reserved[i.Repository.ID] {
		return false
	}
	if _, ok := v.items[i.Repository.ID]; ok {
		return false
	}
	name := strings.ToLower(i.Repository.Owner + "/" + i.Repository.Name)
	if id, ok := v.names[name]; ok && id != i.Repository.ID {
		return false
	}
	if n, ok := v.repos[i.Repository.ID]; ok && n != name {
		return false
	}
	v.names[name], v.repos[i.Repository.ID] = i.Repository.ID, name
	r := resource(i, kind)
	r.NodeID = ""
	key := r
	key.Kind = "" // Issues and PRs share the repository's numeric namespace.
	if old, ok := v.nodes[i.ID]; ok && (unique || old != r) {
		return false
	}
	if id, ok := v.resources[key]; ok && (unique || id != i.ID) {
		return false
	}
	if _, ok := v.repos[i.ID]; ok {
		return false
	}
	if _, ok := v.nodes[i.Repository.ID]; ok {
		return false
	}
	if _, ok := v.items[i.ID]; ok {
		return false
	}
	v.nodes[i.ID], v.resources[key] = r, i.ID
	return true
}
func identitiesCanonical(in []github.Identity, kind string, v *identities) ([]github.Identity, error) {
	out := append([]github.Identity{}, in...)
	seen := map[storage.Resource]bool{}
	for n, i := range out {
		r := resource(i, kind)
		if seen[r] || !v.add(i, kind, false) {
			return nil, fail(ErrInvalid)
		}
		seen[r] = true
		i.Repository.Owner = strings.ToLower(i.Repository.Owner)
		i.Repository.Name = strings.ToLower(i.Repository.Name)
		out[n] = i
	}
	sort.Slice(out, func(i, j int) bool { return less(resource(out[i], kind), resource(out[j], kind)) })
	return out, nil
}
func (c *Coordinator) read(ctx context.Context, r storage.Resource, registry *identities) (observe.ObservedState, error) {
	o, err := c.cfg.Reader.ReadCurrent(ctx, r)
	if err != nil {
		return observe.ObservedState{}, err
	}
	if o.Presence == observe.Missing {
		return observe.ObservedState{}, fail(ErrChanged)
	}
	if r.NodeID == "" {
		r.NodeID = o.Resource.NodeID
	}
	if o.Presence != observe.Present || o.Resource != r || (r.Kind == "pull_request" && (o.PullRequest == nil || o.Issue != nil)) || (r.Kind == "issue" && (o.Issue == nil || o.PullRequest != nil)) {
		return observe.ObservedState{}, fail(ErrInvalid)
	}
	o, err = clone(o)
	if err != nil {
		return observe.ObservedState{}, err
	}
	var i github.Identity
	if o.PullRequest != nil {
		p := o.PullRequest
		i = p.Identity
		if !validState(p.State) {
			return observe.ObservedState{}, fail(ErrInvalid)
		}
		p.Labels = append([]string{}, p.Labels...)
		sort.Strings(p.Labels)
		p.ClosingIssues, err = identitiesCanonical(p.ClosingIssues, "issue", registry)
	} else {
		p := o.Issue
		i = p.Identity
		if p.State != "OPEN" && p.State != "CLOSED" {
			return observe.ObservedState{}, fail(ErrInvalid)
		}
		p.Labels = append([]string{}, p.Labels...)
		sort.Strings(p.Labels)
		p.Assignees = append([]github.Actor{}, p.Assignees...)
		sort.Slice(p.Assignees, func(i, j int) bool {
			a, _ := json.Marshal(p.Assignees[i])
			b, _ := json.Marshal(p.Assignees[j])
			return string(a) < string(b)
		})
		assignees := map[string]bool{}
		for _, a := range p.Assignees {
			if strings.TrimSpace(a.Login) == "" || a.ID != "" && assignees[a.ID] {
				return observe.ObservedState{}, fail(ErrInvalid)
			}
			assignees[a.ID] = true
		}
		if p.Parent != nil && !registry.add(*p.Parent, "issue", false) {
			return observe.ObservedState{}, fail(ErrInvalid)
		}
		for _, list := range []*[]github.Identity{&p.SubIssues, &p.BlockedBy, &p.Blocking} {
			*list, err = identitiesCanonical(*list, "issue", registry)
			if err != nil {
				return observe.ObservedState{}, err
			}
		}
		p.LinkedPullRequests, err = identitiesCanonical(p.LinkedPullRequests, "pull_request", registry)
	}
	if err != nil {
		return observe.ObservedState{}, err
	}
	if !c.configured(i) || resource(i, r.Kind) != r || !registry.add(i, r.Kind, false) {
		return observe.ObservedState{}, fail(ErrInvalid)
	}
	// Known absence is retained. Both Project observations are required even when
	// neither contains an item; absence never changes accepted binding eligibility.
	if len(o.Projects) != 2 {
		return observe.ObservedState{}, fail(ErrIncomplete)
	}
	profiles := map[config.Profile]bool{}
	itemIDs := map[string]bool{}
	for n := range o.Projects {
		p := &o.Projects[n]
		expected := ""
		switch p.Profile {
		case config.Engineering:
			expected = c.cfg.Project
		case config.BugTracker:
			expected = c.cfg.BugProject
		default:
			return observe.ObservedState{}, fail(ErrInvalid)
		}
		if profiles[p.Profile] || p.ID != expected || len(p.Items) > 1 {
			return observe.ObservedState{}, fail(ErrInvalid)
		}
		profiles[p.Profile] = true
		p.Items = append([]observe.ProjectItem{}, p.Items...)
		for n := range p.Items {
			item := &p.Items[n]
			if !validID(item.ID) || itemIDs[item.ID] {
				return observe.ObservedState{}, fail(ErrInvalid)
			}
			itemIDs[item.ID] = true
			if old, ok := registry.items[item.ID]; ok && old != r {
				return observe.ObservedState{}, fail(ErrInvalid)
			}
			if _, ok := registry.nodes[item.ID]; ok {
				return observe.ObservedState{}, fail(ErrInvalid)
			}
			if _, ok := registry.repos[item.ID]; ok {
				return observe.ObservedState{}, fail(ErrInvalid)
			}
			if item.ID == c.cfg.Project || item.ID == c.cfg.BugProject {
				return observe.ObservedState{}, fail(ErrInvalid)
			}
			registry.items[item.ID] = r
			if item.Fields == nil {
				item.Fields = map[config.FieldRole]observe.FieldValue{}
			}
			for role, f := range item.Fields {
				kind := config.SingleSelect
				switch role {
				case config.Estimate, config.PriorityScore, config.RoadmapOrder:
					kind = config.Number
				case config.StartDate:
					kind = config.Date
				case config.Status, config.Priority, config.Effort:
				default:
					return observe.ObservedState{}, fail(ErrInvalid)
				}
				if f.Kind != kind {
					return observe.ObservedState{}, fail(ErrInvalid)
				}
			}
		}
	}
	sort.Slice(o.Projects, func(i, j int) bool { return o.Projects[i].Profile < o.Projects[j].Profile })
	return o, ctx.Err()
}
