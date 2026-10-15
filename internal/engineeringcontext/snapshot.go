package engineeringcontext

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// Snapshot owns a complete deployment graph and exact primary evidence. It has
// no workflow root. Projection never removes unrelated nodes or edges.
type Snapshot struct {
	nodes        []IssueNode
	parents      []ParentEdge
	dependencies []DependencyEdge
	facts        map[storage.Resource]github.ListedIssue
}

// CanonicalContext projects the complete Issue-only graph using its first
// canonical Issue as the API root. It never selects an affected subset.
func (s Snapshot) CanonicalContext() (Context, error) {
	if len(s.nodes) == 0 {
		return Context{}, fail(ErrIncomplete)
	}
	return s.Context(s.nodes[0].Resource)
}

func (s Snapshot) Context(root storage.Resource) (Context, error) {
	root = identity(root)
	if root.Kind != "issue" || !validIssueNumber(root.Number) || !github.ValidBranchRepository(github.Repository{ID: "validation", Owner: root.Owner, Name: root.Repository}) {
		return Context{}, fail(ErrInvalid)
	}
	for _, n := range s.nodes {
		if identity(n.Resource) == root {
			// All nested intent pointers, collections and classification labels belong
			// to the caller. The graph's private representation remains immutable.
			out := Context{Root: n.Resource, Nodes: slices.Clone(s.nodes), Parents: slices.Clone(s.parents), Dependencies: slices.Clone(s.dependencies)}
			for i := range out.Nodes {
				node := &out.Nodes[i]
				node.Intent.BlockedBy = slices.Clone(node.Intent.BlockedBy)
				node.Intent.Blocks = slices.Clone(node.Intent.Blocks)
				node.Intent.Refs = slices.Clone(node.Intent.Refs)
				if node.Intent.Parent != nil {
					p := *node.Intent.Parent
					node.Intent.Parent = &p
				}
				if node.Intent.Target != nil {
					p := *node.Intent.Target
					node.Intent.Target = &p
				}
				if node.AcceptedClassification != nil {
					c := *node.AcceptedClassification
					c.Labels = slices.Clone(c.Labels)
					node.AcceptedClassification = &c
				}
			}
			return out, nil
		}
	}
	return Context{}, fail(ErrMissing)
}

// CheckCurrent rejects primary drift before any Project mutation. Native Type
// participates only when it supplied authority; accepted semantics remain fixed.
func (s Snapshot) CheckCurrent(resource storage.Resource, o observe.ObservedState) error {
	f, ok := s.facts[identity(resource)]
	if !ok {
		return fail(ErrMissing)
	}
	if o.Presence == observe.Missing {
		return fail(ErrChanged)
	}
	if o.Presence != observe.Present || o.Issue == nil || o.PullRequest != nil {
		return fail(ErrInvalid)
	}
	i := o.Issue
	if identity(o.Resource) != identity(resource) || o.Resource.NodeID != f.ID || i.ID != f.ID || i.Number != f.Number || i.Repository.ID != f.Repository.ID || !strings.EqualFold(i.Repository.Owner, f.Repository.Owner) || !strings.EqualFold(i.Repository.Name, f.Repository.Name) || i.Body != f.Body || i.State != f.State {
		return fail(ErrChanged)
	}
	for _, n := range s.nodes {
		if identity(n.Resource) == identity(resource) && n.TypeSource == ManualNative && !reflect.DeepEqual(i.Type, f.Type) {
			return fail(ErrChanged)
		}
	}
	return nil
}

func (r *Resolver) enumerate(ctx context.Context) (map[storage.Resource]github.ListedIssue, []storage.Resource, error) {
	listed := map[storage.Resource]github.ListedIssue{}
	ids := map[string]bool{}
	for _, repo := range r.repositories {
		issues, err := r.cfg.Lister.ListIssues(ctx, repo)
		if err != nil {
			return nil, nil, err
		}
		for _, i := range issues {
			k := identity(storage.Resource{Owner: i.Repository.Owner, Repository: i.Repository.Name, Kind: "issue", Number: int64(i.Number)})
			if i.Repository.ID != repo.ID || !strings.EqualFold(i.Repository.Owner, repo.Owner) || !strings.EqualFold(i.Repository.Name, repo.Name) || strings.TrimSpace(i.ID) == "" || !validIssueNumber(k.Number) || (i.State != "OPEN" && i.State != "CLOSED") {
				return nil, nil, fail(ErrInvalid)
			}
			if _, ok := listed[k]; ok || ids[i.ID] {
				return nil, nil, fail(ErrInvalid)
			}
			if i.Type != nil {
				copy := *i.Type
				i.Type = &copy
			}
			listed[k], ids[i.ID] = i, true
		}
	}
	keys := make([]storage.Resource, 0, len(listed))
	for k := range listed {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return less(keys[i], keys[j]) })
	return listed, keys, ctx.Err()
}

// ResolveSnapshot uses an E1/current/E2 consistency barrier, not an atomic
// GitHub transaction. Every enumerated Issue requires #34 Type authority.
func (r *Resolver) ResolveSnapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	e1, keys, err := r.enumerate(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	nodes := map[storage.Resource]IssueNode{}
	facts := map[storage.Resource]github.ListedIssue{}
	for _, k := range keys {
		o, err := r.cfg.Primary.ReadPrimary(ctx, k)
		if err != nil {
			return Snapshot{}, err
		}
		if o.Presence == observe.Missing {
			return Snapshot{}, fail(ErrChanged)
		}
		if o.Presence != observe.Present || o.Issue == nil || o.PullRequest != nil || identity(o.Resource) != k || o.Issue.ID != o.Resource.NodeID || int64(o.Issue.Number) != k.Number {
			return Snapshot{}, fail(ErrInvalid)
		}
		i, d := o.Issue, e1[k]
		if i.Repository.ID != d.Repository.ID || !strings.EqualFold(i.Repository.Owner, d.Repository.Owner) || !strings.EqualFold(i.Repository.Name, d.Repository.Name) || i.ID != d.ID || i.Body != d.Body || i.State != d.State {
			return Snapshot{}, fail(ErrChanged)
		}
		parsed, err := intent.Parse(i.Body, intent.Context{Organization: k.Owner, Repository: k.Repository})
		if err != nil {
			return Snapshot{}, fail(ErrInvalid)
		}
		accepted, ok, err := r.cfg.Accepted.AcceptedIssue(ctx, k)
		if err != nil {
			return Snapshot{}, err
		}
		n := IssueNode{Resource: o.Resource, State: IssueState(i.State), Intent: parsed}
		if ok {
			c, err := validateClassification(accepted.Classification)
			if err != nil {
				return Snapshot{}, fail(ErrInvalid)
			}
			n.Type, n.TypeSource, n.AcceptedClassification = c.Type, AcceptedSemantic, &c
		} else if parsed.Effective().Classification {
			return Snapshot{}, fail(ErrIncomplete)
		} else {
			if !reflect.DeepEqual(d.Type, i.Type) {
				return Snapshot{}, fail(ErrChanged)
			}
			if i.Type == nil || r.cfg.Deployment.IssueTypes[i.Type.Name] == "" || i.Type.ID != r.cfg.Deployment.IssueTypes[i.Type.Name] {
				return Snapshot{}, fail(ErrInvalid)
			}
			n.Type, n.TypeSource = semanticpolicy.IssueType(i.Type.Name), ManualNative
		}
		nodes[k], facts[k] = n, d
	}
	// Complete discovery is checked again before graph rejection, so an endpoint
	// newly appearing during this attempt is a retryable change, not a stable
	// missing-reference conclusion.
	e2, _, err := r.enumerate(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if len(e2) != len(e1) {
		return Snapshot{}, fail(ErrChanged)
	}
	for _, k := range keys {
		a, b := e1[k], e2[k]
		if a.ID != b.ID || a.Number != b.Number || a.Repository.ID != b.Repository.ID || !strings.EqualFold(a.Repository.Owner, b.Repository.Owner) || !strings.EqualFold(a.Repository.Name, b.Repository.Name) || a.Body != b.Body || a.State != b.State {
			return Snapshot{}, fail(ErrChanged)
		}
		if nodes[k].TypeSource == ManualNative && !reflect.DeepEqual(a.Type, b.Type) {
			return Snapshot{}, fail(ErrChanged)
		}
	}
	parents, dependencies := map[[2]storage.Resource]bool{}, map[[2]storage.Resource]bool{}
	for _, k := range keys {
		p := nodes[k].Intent
		add := func(ref intent.IssueRef, kind int) error {
			target, err := r.reference(ref)
			if err != nil {
				return err
			}
			if target == k {
				return fail(ErrInvalid)
			}
			if _, ok := nodes[target]; !ok {
				return fail(ErrMissing)
			}
			switch kind {
			case 0:
				parents[[2]storage.Resource{k, target}] = true
			case 1:
				dependencies[[2]storage.Resource{target, k}] = true
			case 2:
				dependencies[[2]storage.Resource{k, target}] = true
			}
			return nil
		}
		if p.Parent != nil {
			if err := add(*p.Parent, 0); err != nil {
				return Snapshot{}, err
			}
		}
		for _, ref := range p.BlockedBy {
			if err := add(ref, 1); err != nil {
				return Snapshot{}, err
			}
		}
		for _, ref := range p.Blocks {
			if err := add(ref, 2); err != nil {
				return Snapshot{}, err
			}
		}
	}
	if cyclic(nodes, parents) {
		return Snapshot{}, fail(ErrParentCycle)
	}
	if cyclic(nodes, dependencies) {
		return Snapshot{}, fail(ErrDependencyCycle)
	}
	out := Context{Nodes: []IssueNode{}, Parents: []ParentEdge{}, Dependencies: []DependencyEdge{}}
	for _, k := range keys {
		out.Nodes = append(out.Nodes, nodes[k])
	}
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
	// Accepted completion is immutable, but its appearance changes the Type
	// authority of a node that initially resolved through native fallback.
	for _, k := range keys {
		if nodes[k].TypeSource != ManualNative {
			continue
		}
		_, ok, err := r.cfg.Accepted.AcceptedIssue(ctx, k)
		if err != nil {
			return Snapshot{}, err
		}
		if ok {
			return Snapshot{}, fail(ErrChanged)
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{nodes: out.Nodes, parents: out.Parents, dependencies: out.Dependencies, facts: facts}, nil
}
