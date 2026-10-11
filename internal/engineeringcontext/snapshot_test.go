package engineeringcontext

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type snapshotAcceptedFunc func(context.Context, storage.Resource) (sp.AcceptedIssue, bool, error)

func (f snapshotAcceptedFunc) AcceptedIssue(ctx context.Context, r storage.Resource) (sp.AcceptedIssue, bool, error) {
	return f(ctx, r)
}

func TestSnapshotNativeFallbackAuthorityAndAcceptedAppearance(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		native     *github.IssueType
		want       error
	}{
		{"enabled unaccepted unrelated Task", "", &github.IssueType{ID: "type-task", Name: "Task"}, ErrIncomplete},
		{"enabled unaccepted unrelated Bug", "", &github.IssueType{ID: "type-bug", Name: "Bug"}, ErrIncomplete},
		{"disabled exact native", "Classification: false", &github.IssueType{ID: "type-task", Name: "Task"}, nil},
		{"disabled missing native", "Classification: false", nil, ErrInvalid},
		{"disabled wrong native", "Classification: false", &github.IssueType{ID: "wrong", Name: "Task"}, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := issue(engine, 1, "")
			unrelated := issue(freecad, 99, tc.body)
			unrelated.Type = tc.native
			f := setup(t, root, unrelated)
			f.accept(resource(engine, 1), sp.Task)
			f.resolver.cfg.Lister = snapshotLister(f, false, nil)
			s, err := f.resolver.ResolveSnapshot(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatal(s, err, tc.want)
			}
			if tc.want != nil && !reflect.DeepEqual(s, Snapshot{}) {
				t.Fatal("partial Snapshot", s)
			}
		})
	}
	f := setup(t, issue(engine, 1, "Classification: false"))
	i := f.issues[resource(engine, 1)]
	i.Type = &github.IssueType{ID: "type-task", Name: "Task"}
	f.issues[resource(engine, 1)] = i
	f.resolver.cfg.Lister = snapshotLister(f, false, nil)
	calls := 0
	f.resolver.cfg.Accepted = snapshotAcceptedFunc(func(context.Context, storage.Resource) (sp.AcceptedIssue, bool, error) {
		calls++
		return sp.AcceptedIssue{}, calls == 2, nil
	})
	if s, err := f.resolver.ResolveSnapshot(context.Background()); !errors.Is(err, ErrChanged) || !reflect.DeepEqual(s, Snapshot{}) || calls != 2 {
		t.Fatal(s, err, calls)
	}
}

func TestSnapshotCompleteGraphRejectsDeclaredInvalidity(t *testing.T) {
	for _, tc := range []struct {
		name, a, b string
		want       error
	}{
		{"self Parent", "Parent: #1", "", ErrInvalid}, {"self dependency", "Blocked-By: #1", "", ErrInvalid},
		{"missing Parent", "Parent: #999", "", ErrMissing}, {"missing blocker", "Blocked-By: #999", "", ErrMissing},
		{"outside Parent", "Parent: parametron-io/outside#1", "", ErrDeployment}, {"outside Blocks", "Blocks: parametron-io/outside#1", "", ErrDeployment},
		{"conflicting Parent", "Parent: #2\nParent: #3", "", ErrInvalid}, {"Parent cycle", "Parent: #2", "Parent: #1", ErrParentCycle},
		{"dependency cycle", "Blocked-By: #2", "Blocked-By: #1", ErrDependencyCycle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, issue(engine, 1, ""), issue(engine, 2, ""))
			f.acceptAll()
			for index, body := range []string{tc.a, tc.b} {
				k := resource(engine, int64(index+1))
				i := f.issues[k]
				i.Body = body
				f.issues[k] = i
			}
			f.resolver.cfg.Lister = snapshotLister(f, false, nil)
			s, err := f.resolver.ResolveSnapshot(context.Background())
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(s, Snapshot{}) {
				t.Fatal(s, err)
			}
		})
	}
}

type snapshotPrimaryFunc func(context.Context, storage.Resource) (observe.ObservedState, error)

func (f snapshotPrimaryFunc) ReadPrimary(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	return f(ctx, r)
}

// Each enumeration has an explicit pass number. Returned order can be reversed
// without depending on map iteration, random choices, or scheduling.
func snapshotLister(f *fixture, reverse bool, edit func(int, github.Repository, []github.ListedIssue) []github.ListedIssue) *github.ListerFake {
	calls := 0
	return &github.ListerFake{ListIssuesFunc: func(ctx context.Context, repo github.Repository) ([]github.ListedIssue, error) {
		pass := calls / len(f.resolver.repositories)
		calls++
		out := []github.ListedIssue{}
		for _, i := range f.issues {
			if i.Repository == repo {
				out = append(out, github.ListedIssue{Identity: i.Identity, Title: i.Title, Body: i.Body, State: i.State, Type: i.Type})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
		if reverse {
			slices.Reverse(out)
		}
		if edit != nil {
			out = edit(pass, repo, out)
		}
		return out, ctx.Err()
	}}
}

func TestSnapshotCompleteProjectionAndIndependentCopies(t *testing.T) {
	phase := issue(engine, 45, "Blocks: parametron-io/parametron-freecad#1")
	child := issue(freecad, 1, "Parent: parametron-io/parametron-engine#45\nBlocked-By: #2 parametron-io/parametron-engine#45\nTarget: #2\nRefs: #2")
	bug := issue(freecad, 2, "Blocks: #1")
	bug.State = "CLOSED"
	child.Parent = &bug.Identity
	child.BlockedBy = []github.Identity{phase.Identity}
	phase.SubIssues = []github.Identity{bug.Identity}
	f := setup(t, phase, child, bug, issue(engine, 46, ""), issue(engine, 99, ""), issue(freecad, 3, ""))
	for k := range f.issues {
		typ := sp.Task
		if k.Number == 45 || k.Number == 46 {
			typ = sp.Phase
		}
		if k.Repository == freecad && k.Number == 2 {
			typ = sp.Bug
		}
		if k.Repository == freecad && k.Number == 3 {
			typ = sp.Feature
		}
		f.accept(k, typ)
	}
	var baseline Context
	for _, reverse := range []bool{false, true} {
		f.resolver.cfg.Lister = snapshotLister(f, reverse, nil)
		s, err := f.resolver.ResolveSnapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		root := resource(engine, 45)
		root.NodeID = "caller is not authoritative"
		got, err := s.Context(root)
		if err != nil || got.Root.NodeID != phase.ID || len(got.Nodes) != 6 || len(got.Parents) != 1 || len(got.Dependencies) != 2 {
			t.Fatal(got, err)
		}
		if !reverse {
			baseline, err = s.Context(root)
			if err != nil {
				t.Fatal(err)
			}
		} else if !reflect.DeepEqual(got, baseline) {
			t.Fatal("enumeration changed canonical graph")
		}
		for _, r := range []storage.Resource{resource(engine, 999), resource("unknown", 45), {Owner: "parametron-io", Repository: engine, Kind: "pull_request", Number: 45}, resource(engine, 0)} {
			if _, err := s.Context(r); err == nil {
				t.Fatal("invalid/missing root accepted", r)
			}
		}
		for i := range got.Nodes {
			n := &got.Nodes[i]
			if n.Intent.Parent != nil {
				n.Intent.Parent.Number = 999
			}
			if n.Intent.Target != nil {
				n.Intent.Target.Number = 999
			}
			for j := range n.Intent.BlockedBy {
				n.Intent.BlockedBy[j].Number = 999
			}
			for j := range n.Intent.Blocks {
				n.Intent.Blocks[j].Number = 999
			}
			for j := range n.Intent.Refs {
				n.Intent.Refs[j].Number = 999
			}
			n.AcceptedClassification.Labels[0] = "docs"
			n.Resource.Number = 999
		}
		got.Parents[0].Child.Number = 999
		got.Dependencies[0].Blocker.Number = 999
		again, err := s.Context(root)
		if err != nil || !reflect.DeepEqual(again, baseline) {
			t.Fatal("projection mutated Snapshot", again, err)
		}
		f.resolver.cfg.Lister = snapshotLister(f, reverse, nil)
		scoped, err := f.resolver.Resolve(context.Background(), root)
		if err != nil || len(scoped.Nodes) != 3 {
			t.Fatal("root-scoped Resolve changed", scoped, err)
		}
	}
}

func TestSnapshotEnumerationBarrier(t *testing.T) {
	for _, change := range []string{"add phase", "add task", "add feature", "add phase child", "remove", "body", "close", "reopen", "node", "native type"} {
		t.Run(change, func(t *testing.T) {
			i := issue(engine, 1, "Classification: false")
			i.Type = &github.IssueType{ID: "type-task", Name: "Task"}
			if change == "reopen" {
				i.State = "CLOSED"
			}
			f := setup(t, i)
			f.resolver.cfg.Lister = snapshotLister(f, false, func(pass int, repo github.Repository, out []github.ListedIssue) []github.ListedIssue {
				if pass != 1 || repo.Name != engine {
					return out
				}
				switch change {
				case "add phase", "add task", "add feature", "add phase child":
					x := out[0]
					x.ID, x.Number = "new", 2
					x.Type = &github.IssueType{ID: "type-task", Name: "Task"}
					if change == "add phase" {
						x.Type = &github.IssueType{ID: "type-phase", Name: "Phase"}
					}
					if change == "add feature" {
						x.Type = &github.IssueType{ID: "type-feature", Name: "Feature"}
					}
					if change == "add phase child" {
						x.Body += "\nParent: #1"
					}
					return append(out, x)
				case "remove":
					return nil
				case "body":
					out[0].Body += "\nCreate-Branch: false"
				case "close":
					out[0].State = "CLOSED"
				case "reopen":
					out[0].State = "OPEN"
				case "node":
					out[0].ID = "replacement"
				case "native type":
					out[0].Type = &github.IssueType{ID: "type-feature", Name: "Feature"}
				}
				return out
			})
			got, err := f.resolver.ResolveSnapshot(context.Background())
			if !errors.Is(err, ErrChanged) || !reflect.DeepEqual(got, Snapshot{}) {
				t.Fatal(got, err)
			}
		})
	}
}

func TestSnapshotPrimaryAndCheckCurrentBarrier(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, change := range []string{"unchanged", "missing", "body", "state", "node", "repository", "kind", "type", "projects"} {
			t.Run(map[bool]string{false: "semantic", true: "native"}[native]+"/"+change, func(t *testing.T) {
				i := issue(engine, 1, "Classification: false")
				if !native {
					i.Body = ""
				}
				i.Type = &github.IssueType{ID: "type-task", Name: "Task"}
				f := setup(t, i)
				if !native {
					f.accept(resource(engine, 1), sp.Task)
				}
				f.resolver.cfg.Lister = snapshotLister(f, false, nil)
				s, err := f.resolver.ResolveSnapshot(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				o, err := f.primary.ReadPrimary(context.Background(), resource(engine, 1))
				if err != nil {
					t.Fatal(err)
				}
				switch change {
				case "missing":
					o.Presence = observe.Missing
				case "body":
					o.Issue.Body += "\nCreate-Branch: false"
				case "state":
					o.Issue.State = "CLOSED"
				case "node":
					o.Resource.NodeID, o.Issue.ID = "replacement", "replacement"
				case "repository":
					o.Issue.Repository.ID = "replacement"
				case "kind":
					o.Issue = nil
				case "type":
					o.Issue.Type = &github.IssueType{ID: "type-feature", Name: "Feature"}
				case "projects":
					o.Projects = []observe.ProjectState{{ID: "unrelated"}}
				}
				want := ErrChanged
				if change == "kind" {
					want = ErrInvalid
				}
				if change == "unchanged" || change == "projects" || change == "type" && !native {
					want = nil
				}
				if err := s.CheckCurrent(resource(engine, 1), o); !errors.Is(err, want) {
					t.Fatal(err, want)
				}
				f.resolver.cfg.Primary = snapshotPrimaryFunc(func(context.Context, storage.Resource) (observe.ObservedState, error) { return o, nil })
				_, err = f.resolver.ResolveSnapshot(context.Background())
				if !errors.Is(err, want) {
					t.Fatal("current materialization", err, want)
				}
			})
		}
	}
}
