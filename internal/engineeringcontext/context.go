// Package engineeringcontext resolves current declared graphs, never lifecycle
// decisions or mutation authorization. Native relationships are not graph input.
package engineeringcontext

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var (
	ErrInvalid         = errors.New("engineering context: invalid context")
	ErrMissing         = errors.New("engineering context: missing resource")
	ErrIncomplete      = errors.New("engineering context: incomplete semantic state")
	ErrParentCycle     = errors.New("engineering context: parent cycle")
	ErrDependencyCycle = errors.New("engineering context: dependency cycle")
	ErrDeployment      = errors.New("engineering context: out-of-deployment reference")
	ErrChanged         = errors.New("engineering context: discovery changed during refetch")
)

// Error carries a stable local category without bodies or provider diagnostics.
type Error struct{ Kind error }

func (e *Error) Error() string { return e.Kind.Error() }
func (e *Error) Unwrap() error { return e.Kind }
func fail(kind error) error    { return &Error{kind} }

// IssueState is the current repository fact, not a workflow Status.
type IssueState string

const (
	Open   IssueState = "OPEN"
	Closed IssueState = "CLOSED"
)

type TypeSource string

const (
	AcceptedSemantic TypeSource = "accepted_semantic"
	ManualNative     TypeSource = "manual_native"
)

type IssueNode struct {
	Resource               storage.Resource
	State                  IssueState
	Type                   semanticpolicy.IssueType
	TypeSource             TypeSource
	AcceptedClassification *semanticpolicy.IssueClassification
	Intent                 intent.Intent
}
type ParentEdge struct{ Child, Parent storage.Resource }
type DependencyEdge struct{ Blocker, Blocked storage.Resource }
type Context struct {
	Root         storage.Resource
	Nodes        []IssueNode
	Parents      []ParentEdge
	Dependencies []DependencyEdge
}
type PrimaryReader interface {
	ReadPrimary(context.Context, storage.Resource) (observe.ObservedState, error)
}
type Config struct {
	Deployment config.ResolvedConfig
	Primary    PrimaryReader
	Lister     github.IssueLister
	Accepted   semanticflow.AcceptedReader
}
type Resolver struct {
	cfg          Config
	repositories []config.Repository
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
func New(c Config) (*Resolver, error) {
	if absent(c.Primary) || absent(c.Lister) || absent(c.Accepted) || !strings.EqualFold(c.Deployment.Organization.Login, "parametron-io") || len(c.Deployment.Repositories) == 0 {
		return nil, fail(ErrInvalid)
	}
	repos := append([]config.Repository(nil), c.Deployment.Repositories...)
	names, ids := map[string]bool{}, map[string]bool{}
	for _, repo := range repos {
		name := strings.ToLower(repo.Name)
		if strings.TrimSpace(repo.ID) == "" || strings.TrimSpace(name) == "" || !strings.EqualFold(repo.Owner, c.Deployment.Organization.Login) || names[name] || ids[repo.ID] {
			return nil, fail(ErrInvalid)
		}
		names[name], ids[repo.ID] = true, true
	}
	sort.Slice(repos, func(i, j int) bool { return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name) })
	types := make(map[string]string)
	typeIDs := make(map[string]bool)
	for _, typ := range semanticpolicy.Types() {
		id := c.Deployment.IssueTypes[string(typ)]
		if strings.TrimSpace(id) == "" || typeIDs[id] {
			return nil, fail(ErrInvalid)
		}
		types[string(typ)] = id
		typeIDs[id] = true
	}
	// Retain only the deployment identities used here, never caller-owned maps.
	c.Deployment = config.ResolvedConfig{Organization: c.Deployment.Organization, IssueTypes: types}
	return &Resolver{c, repos}, nil
}
func identity(r storage.Resource) storage.Resource {
	r.Owner = strings.ToLower(r.Owner)
	r.Repository = strings.ToLower(r.Repository)
	r.NodeID = ""
	return r
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
func (r *Resolver) authorize(v storage.Resource) (storage.Resource, error) {
	v = identity(v)
	if v.Kind != "issue" || v.Number <= 0 || int64(int(v.Number)) != v.Number {
		return v, fail(ErrInvalid)
	}
	for _, repo := range r.repositories {
		if strings.EqualFold(repo.Owner, v.Owner) && strings.EqualFold(repo.Name, v.Repository) {
			return v, nil
		}
	}
	return v, fail(ErrDeployment)
}
func (r *Resolver) reference(ref intent.IssueRef) (storage.Resource, error) {
	return r.authorize(storage.Resource{Owner: strings.ToLower(r.cfg.Deployment.Organization.Login), Repository: ref.Repository, Kind: "issue", Number: ref.Number})
}

type discovery struct {
	issue  github.ListedIssue
	parsed intent.Intent
}

// Resolve reads primary facts only: Project fields are unnecessary for #34.
// Discovery is complete but not atomic; changed participant bodies fail this
// attempt rather than mixing enumerated membership with newer directives.
func (r *Resolver) Resolve(ctx context.Context, root storage.Resource) (Context, error) {
	if err := ctx.Err(); err != nil {
		return Context{}, err
	}
	root, err := r.authorize(root)
	if err != nil {
		return Context{}, err
	}
	listed := map[storage.Resource]discovery{}
	nodeIDs := map[string]bool{}
	for _, repo := range r.repositories {
		issues, err := r.cfg.Lister.ListIssues(ctx, repo)
		if err != nil {
			return Context{}, err
		}
		issues = append([]github.ListedIssue(nil), issues...)
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
		for _, i := range issues {
			key := identity(storage.Resource{Owner: i.Repository.Owner, Repository: i.Repository.Name, Kind: "issue", Number: int64(i.Number)})
			if i.Repository.ID != repo.ID || !strings.EqualFold(i.Repository.Owner, repo.Owner) || !strings.EqualFold(i.Repository.Name, repo.Name) || strings.TrimSpace(i.ID) == "" || i.Number <= 0 || (i.State != "OPEN" && i.State != "CLOSED") {
				return Context{}, fail(ErrInvalid)
			}
			if _, exists := listed[key]; exists || nodeIDs[i.ID] {
				return Context{}, fail(ErrInvalid)
			}
			parsed, err := intent.Parse(i.Body, intent.Context{Organization: key.Owner, Repository: key.Repository})
			if err != nil {
				return Context{}, fail(ErrInvalid)
			}
			listed[key] = discovery{i, parsed}
			nodeIDs[i.ID] = true
		}
	}
	keys := make([]storage.Resource, 0, len(listed))
	for k := range listed {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return less(keys[i], keys[j]) })
	pending := map[storage.Resource]bool{root: true}
	nodes := map[storage.Resource]IssueNode{}
	currentIDs := map[string]storage.Resource{}
	parents := map[[2]storage.Resource]bool{}
	dependencies := map[[2]storage.Resource]bool{}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return Context{}, err
		}
		queue := make([]storage.Resource, 0, len(pending))
		for k := range pending {
			queue = append(queue, k)
		}
		sort.Slice(queue, func(i, j int) bool { return less(queue[i], queue[j]) })
		key := queue[0]
		delete(pending, key)
		if _, done := nodes[key]; done {
			continue
		}
		o, err := r.cfg.Primary.ReadPrimary(ctx, key)
		if err != nil {
			return Context{}, err
		}
		if o.Presence == observe.Missing {
			return Context{}, fail(ErrMissing)
		}
		if o.Presence != observe.Present || o.Issue == nil || o.PullRequest != nil || identity(o.Resource) != key || strings.TrimSpace(o.Resource.NodeID) == "" || (o.Issue.State != "OPEN" && o.Issue.State != "CLOSED") {
			return Context{}, fail(ErrInvalid)
		}
		if d, ok := listed[key]; ok && (d.issue.ID != o.Resource.NodeID || d.issue.Body != o.Issue.Body) {
			return Context{}, fail(ErrChanged)
		}
		if other, exists := currentIDs[o.Resource.NodeID]; exists && other != key {
			return Context{}, fail(ErrInvalid)
		}
		currentIDs[o.Resource.NodeID] = key
		parsed, err := intent.Parse(o.Issue.Body, intent.Context{Organization: key.Owner, Repository: key.Repository})
		if err != nil {
			return Context{}, fail(ErrInvalid)
		}
		accepted, ok, err := r.cfg.Accepted.AcceptedIssue(ctx, key)
		if err != nil {
			return Context{}, err
		}
		node := IssueNode{Resource: o.Resource, State: IssueState(o.Issue.State), Intent: parsed}
		if ok {
			classification, err := validateClassification(accepted.Classification)
			if err != nil {
				return Context{}, fail(ErrInvalid)
			}
			node.Type, node.TypeSource = classification.Type, AcceptedSemantic
			node.AcceptedClassification = &classification
		} else if parsed.Effective().Classification {
			return Context{}, fail(ErrIncomplete)
		} else {
			typ := o.Issue.Type
			if typ == nil || r.cfg.Deployment.IssueTypes[typ.Name] == "" || typ.ID != r.cfg.Deployment.IssueTypes[typ.Name] {
				return Context{}, fail(ErrInvalid)
			}
			node.Type, node.TypeSource = semanticpolicy.IssueType(typ.Name), ManualNative
		}
		nodes[key] = node
		add := func(ref intent.IssueRef, kind int) error {
			target, err := r.reference(ref)
			if err != nil {
				return err
			}
			if target == key {
				return fail(ErrInvalid)
			}
			if _, done := nodes[target]; !done {
				pending[target] = true
			}
			switch kind {
			case 0:
				parents[[2]storage.Resource{key, target}] = true
			case 1:
				dependencies[[2]storage.Resource{target, key}] = true
			case 2:
				dependencies[[2]storage.Resource{key, target}] = true
			}
			return nil
		}
		if parsed.Parent != nil {
			if err := add(*parsed.Parent, 0); err != nil {
				return Context{}, err
			}
		}
		for _, ref := range parsed.BlockedBy {
			if err := add(ref, 1); err != nil {
				return Context{}, err
			}
		}
		for _, ref := range parsed.Blocks {
			if err := add(ref, 2); err != nil {
				return Context{}, err
			}
		}
		// Discover incoming one-sided dependencies and children of every reached
		// validated Phase. Native SubIssues/Parent/BlockedBy never enter this test.
		for _, candidate := range keys {
			if _, done := nodes[candidate]; done {
				continue
			}
			p := listed[candidate].parsed
			matches := func(ref intent.IssueRef) bool { return ref.Repository == key.Repository && ref.Number == key.Number }
			relevant := node.Type == semanticpolicy.Phase && p.Parent != nil && matches(*p.Parent)
			for _, ref := range p.BlockedBy {
				relevant = relevant || matches(ref)
			}
			for _, ref := range p.Blocks {
				relevant = relevant || matches(ref)
			}
			if relevant {
				pending[candidate] = true
			}
		}
	}
	if cyclic(nodes, parents) {
		return Context{}, fail(ErrParentCycle)
	}
	if cyclic(nodes, dependencies) {
		return Context{}, fail(ErrDependencyCycle)
	}
	out := Context{Root: nodes[root].Resource, Nodes: []IssueNode{}, Parents: []ParentEdge{}, Dependencies: []DependencyEdge{}}
	for _, node := range nodes {
		out.Nodes = append(out.Nodes, node)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return less(out.Nodes[i].Resource, out.Nodes[j].Resource) })
	for pair := range parents {
		out.Parents = append(out.Parents, ParentEdge{nodes[pair[0]].Resource, nodes[pair[1]].Resource})
	}
	for pair := range dependencies {
		out.Dependencies = append(out.Dependencies, DependencyEdge{nodes[pair[0]].Resource, nodes[pair[1]].Resource})
	}
	sort.Slice(out.Parents, func(i, j int) bool {
		a, b := out.Parents[i], out.Parents[j]
		if a.Child != b.Child {
			return less(a.Child, b.Child)
		}
		return less(a.Parent, b.Parent)
	})
	sort.Slice(out.Dependencies, func(i, j int) bool {
		a, b := out.Dependencies[i], out.Dependencies[j]
		if a.Blocker != b.Blocker {
			return less(a.Blocker, b.Blocker)
		}
		return less(a.Blocked, b.Blocked)
	})
	return out, nil
}
