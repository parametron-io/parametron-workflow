package engineeringactivity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

var repo = github.Repository{ID: "R", Owner: "parametron-io", Name: "workflow"}
var acceptedAt = time.Date(2026, 10, 11, 0, 30, 0, 0, time.FixedZone("acceptance", 3*3600))
var base = strings.Repeat("a", 40)
var head = strings.Repeat("b", 40)

func resource(n int64) storage.Resource {
	return storage.Resource{Owner: repo.Owner, Repository: repo.Name, Kind: "issue", Number: n, NodeID: fmt.Sprintf("I%d", n)}
}
func node(n int64, typ sp.IssueType, body string) ec.IssueNode {
	parsed, err := intent.Parse("Classification: false\n"+body, intent.Context{Organization: repo.Owner, Repository: repo.Name})
	if err != nil {
		panic(err)
	}
	return ec.IssueNode{Resource: resource(n), State: ec.Open, Type: typ, TypeSource: ec.ManualNative, Intent: parsed}
}
func input(typ sp.IssueType, body string) ep.Input {
	n := node(38, typ, body)
	in := ep.Input{Context: ec.Context{Root: n.Resource, Nodes: []ec.IssueNode{n}}}
	if typ == sp.Phase {
		in.PhaseOrder = []storage.Resource{n.Resource}
	}
	return in
}
func childInput(body string) ep.Input {
	in := input(sp.Task, "Parent: #4\n"+body)
	in.Context.Nodes = append(in.Context.Nodes, node(4, sp.Phase, ""))
	in.Context.Parents = []ec.ParentEdge{{Child: resource(38), Parent: resource(4)}}
	in.PhaseOrder = []storage.Resource{resource(4)}
	return in
}

type compareFunc func(context.Context, github.Repository, string, string) (github.Comparison, error)

func (f compareFunc) CompareCommits(ctx context.Context, r github.Repository, b, h string) (github.Comparison, error) {
	return f(ctx, r, b, h)
}

type projectFunc func(context.Context, storage.Resource) (observe.ObservedState, error)

func (f projectFunc) ReadCurrent(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	return f(ctx, r)
}

type evidenceFunc func(context.Context, storage.Resource) (eb.Completion, error)

func (f evidenceFunc) Completed(ctx context.Context, r storage.Resource) (eb.Completion, error) {
	return f(ctx, r)
}

type fixture struct {
	t                  *testing.T
	path               string
	store              *storage.Store
	branch             *eb.Controller
	controller         *Controller
	cfg                Config
	remote             *github.Branch
	items              map[int64]observe.ProjectItem
	writes             []string
	failure            string
	cancel             func()
	cancelAt           string
	reads, comparisons int
	status             string
	assignees          []github.Actor
	expectedDate       string
}

func (f *fixture) SetProjectOption(ctx context.Context, p, item, field, option string) error {
	if p != "P" || field != "status" || option != "progress" {
		f.t.Fatal(p, item, field, option)
	}
	return f.effect(ctx, item, "status", option)
}
func (f *fixture) SetProjectDate(ctx context.Context, p, item, field, date string) error {
	if p != "P" || field != "date" || date != f.expectedDate {
		f.t.Fatal(p, item, field, date)
	}
	return f.effect(ctx, item, "date", date)
}
func (f *fixture) effect(ctx context.Context, item, kind, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	label := item + ":" + kind
	if f.failure == label {
		return &github.Error{Category: github.Transient}
	}
	f.writes = append(f.writes, label)
	for n, i := range f.items {
		if i.ID == item {
			if kind == "status" {
				i.Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: value}, StatusRole: config.InProgress}
			} else {
				i.Fields[config.StartDate] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Date, Date: value}}
			}
			f.items[n] = i
		}
	}
	if f.cancelAt == label {
		f.cancel()
	}
	return nil
}
func setup(t *testing.T, complete bool) *fixture {
	t.Helper()
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "activity.sqlite"), items: map[int64]observe.ProjectItem{}, status: "ahead", expectedDate: "2026-10-10"}
	var err error
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	for _, n := range []int64{38, 4, 5} {
		f.items[n] = observe.ProjectItem{ID: fmt.Sprintf("item%d", n), Fields: map[config.FieldRole]observe.FieldValue{config.Status: {FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "ready"}, StatusRole: config.Ready}}}
	}
	f.delivery("branch", false, acceptedAt.Add(-96*time.Hour))
	f.delivery("push", true, acceptedAt)
	branches := github.BranchFake{DefaultBranchFunc: func(context.Context, github.Repository) (github.Branch, error) {
		return github.Branch{Repository: repo, Name: "main", SHA: base}, nil
	}, BranchFunc: func(ctx context.Context, r github.Repository, n string) (github.Branch, error) {
		f.reads++
		if f.remote == nil {
			return github.Branch{}, &github.MissingBranch{Repository: r, Name: n}
		}
		return *f.remote, nil
	}, CreateBranchFunc: func(ctx context.Context, r github.Repository, n, s string) (github.Branch, error) {
		b := github.Branch{Repository: r, Name: n, SHA: s}
		f.remote = &b
		return b, nil
	}}
	f.branch, err = eb.New(eb.Config{Repositories: []github.Repository{repo}, Branches: branches, Store: f.store, Clock: func() time.Time { return acceptedAt.Add(-72 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		r, err := f.branch.Ensure(context.Background(), "branch", input(sp.Task, ""))
		if err != nil || !r.Completed {
			t.Fatal(r, err)
		}
	}
	f.remote = &github.Branch{Repository: repo, Name: "development/issue-38", SHA: head}
	f.reads = 0
	f.cfg = Config{Repositories: []github.Repository{repo}, Project: config.ResolvedProject{ID: "P", Fields: map[config.FieldRole]string{config.Status: "status", config.StartDate: "date"}, StatusOptions: map[config.StatusRole]string{config.InProgress: "progress"}}, Provenance: f.branch, Branches: branches, Comparison: compareFunc(func(ctx context.Context, r github.Repository, b, h string) (github.Comparison, error) {
		f.comparisons++
		c := github.Comparison{Repository: r, BaseSHA: b, HeadSHA: h, Status: f.status}
		switch f.status {
		case "ahead":
			c.AheadBy = 1
		case "behind":
			c.BehindBy = 1
		case "diverged":
			c.AheadBy = 1
			c.BehindBy = 1
		}
		return c, nil
	}), Projects: projectFunc(func(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
		i := f.items[r.Number]
		i.Fields = maps.Clone(i.Fields)
		return observe.ObservedState{Resource: r, Presence: observe.Present, Issue: &github.Issue{Identity: github.Identity{ID: r.NodeID, Repository: repo, Number: int(r.Number)}, State: "OPEN", Assignees: append([]github.Actor(nil), f.assignees...)}, Projects: []observe.ProjectState{{Profile: config.Engineering, ID: "P", Items: []observe.ProjectItem{i}}}}, nil
	}), Fields: f, Store: f.store}
	f.controller, err = New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) delivery(id string, push bool, at time.Time) {
	f.t.Helper()
	r := resource(38)
	event := "issues"
	payload := []byte(`{"issue":{"number":38}}`)
	if push {
		event = "push"
		payload = []byte(`{"repository":{"owner":{"login":"PARAMETRON-IO"},"name":"WORKFLOW"},"ref":"refs/heads/development/issue-38","before":"stale","after":"untrusted","sender":{"login":"untrusted"}}`)
	}
	if _, err := f.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: event, Payload: payload, ReceivedAt: at, Resource: &r}); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) handle(in ep.Input) (Result, error) {
	return f.controller.Handle(context.Background(), "push", in)
}
func (f *fixture) restart() {
	f.t.Helper()
	if err := f.store.Close(); err != nil {
		f.t.Fatal(err)
	}
	var err error
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.branch, err = eb.New(eb.Config{Repositories: []github.Repository{repo}, Store: f.store, Branches: github.BranchFake{}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.cfg.Store = f.store
	f.cfg.Provenance = f.branch
	f.controller, err = New(f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
}
func TestStandaloneActivityIntegration(t *testing.T) {
	f := setup(t, true)
	in := childInput("")
	event, err := f.store.Event(context.Background(), "push")
	if err != nil {
		t.Fatal(err)
	}
	candidate, ok, err := ResolvePush(context.Background(), f.cfg.Repositories, event.Delivery)
	if err != nil || !ok || candidate != identity(in.Context.Root) {
		t.Fatal(candidate, ok, err)
	}
	got, err := f.handle(in)
	if err != nil || !got.Accepted || !got.Completed || got.Evidence.HeadSHA != head || got.Evidence.StartDate != "2026-10-10" || f.reads != 1 || f.comparisons != 1 {
		t.Fatal(got, err, f.writes)
	}
	if !reflect.DeepEqual(f.writes, []string{"item38:status", "item38:date", "item4:status"}) {
		t.Fatal(f.writes)
	}
	p, _ := f.store.Provenance(context.Background(), firstNamespace, key(identity(resource(38))))
	q, _ := f.store.Provenance(context.Background(), completionNamespace, p.Key)
	if !bytes.Equal(p.Metadata, q.Metadata) || p.DeliveryID != "push" || q.DeliveryID != p.DeliveryID {
		t.Fatal(p, q)
	}
	f.restart()
	f.writes = nil
	f.delivery("later", true, acceptedAt.Add(72*time.Hour))
	f.remote.SHA = strings.Repeat("c", 40)
	again, err := f.controller.Handle(context.Background(), "later", in)
	if err != nil || again != got || len(f.writes) != 0 {
		t.Fatal(again, err, f.writes)
	}
	// Returned evidence is a value, independent from durable and caller input.
	again.Evidence.Branch.WorkItem.Number = 99
	again.Evidence.StartDate = "2000-01-01"
	again, err = f.handle(in)
	if err != nil || again != got {
		t.Fatal(again, err)
	}
}
func TestQualificationAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		name     string
		complete bool
		change   func(*fixture, *ep.Input)
		accepted bool
		writes   []string
	}{
		{"Task", true, func(f *fixture, in *ep.Input) {}, true, []string{"item38:status", "item38:date"}},
		{"Feature", true, func(f *fixture, in *ep.Input) { *in = input(sp.Feature, "") }, true, []string{"item38:status", "item38:date"}},
		{"explicit Phase", true, func(f *fixture, in *ep.Input) { *in = input(sp.Phase, "Create-Branch: true") }, true, []string{"item38:status", "item38:date"}},
		{"manual lookalike observed Ready", false, func(f *fixture, in *ep.Input) {}, false, nil},
		{"manual In Progress", false, func(f *fixture, in *ep.Input) {
			i := f.items[38]
			i.Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "progress"}}
		}, false, nil},
		{"base only", true, func(f *fixture, in *ep.Input) { f.remote.SHA = base }, false, nil},
		{"behind", true, func(f *fixture, in *ep.Input) { f.status = "behind" }, false, nil},
		{"diverged", true, func(f *fixture, in *ep.Input) { f.status = "diverged" }, false, nil},
		{"missing branch", true, func(f *fixture, in *ep.Input) { f.remote = nil }, false, nil},
		{"Create-Branch false", true, func(f *fixture, in *ep.Input) { *in = input(sp.Task, "Create-Branch: false") }, false, nil},
		{"blocked", true, func(f *fixture, in *ep.Input) {
			in.Context.Nodes = append(in.Context.Nodes, node(39, sp.Task, ""))
			in.Context.Dependencies = []ec.DependencyEdge{{Blocker: resource(39), Blocked: resource(38)}}
		}, false, nil},
		{"inactive parent", true, func(f *fixture, in *ep.Input) {
			*in = childInput("")
			in.Context.Nodes = append(in.Context.Nodes, node(5, sp.Phase, ""))
			in.PhaseOrder = []storage.Resource{resource(5), resource(4)}
		}, false, nil},
		{"Set-Status false", true, func(f *fixture, in *ep.Input) { *in = input(sp.Task, "Set-Status: false") }, true, []string{"item38:date"}},
		{"parent Set-Status false", true, func(f *fixture, in *ep.Input) {
			*in = childInput("")
			in.Context.Nodes[1] = node(4, sp.Phase, "Set-Status: false")
		}, true, []string{"item38:status", "item38:date"}},
		{"target false parent owned", true, func(f *fixture, in *ep.Input) { *in = childInput("Set-Status: false") }, true, []string{"item38:date", "item4:status"}},
		{"already In Progress", true, func(f *fixture, in *ep.Input) {
			i := f.items[38]
			i.Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: "progress"}}
		}, true, []string{"item38:date"}},
		{"existing Start Date", true, func(f *fixture, in *ep.Input) {
			i := f.items[38]
			i.Fields[config.StartDate] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Date, Date: "2020-01-01"}}
		}, true, []string{"item38:status"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, tc.complete)
			in := input(sp.Task, "")
			tc.change(f, &in)
			got, err := f.handle(in)
			if err != nil || got.Accepted != tc.accepted || got.Completed != tc.accepted || !reflect.DeepEqual(f.writes, tc.writes) {
				t.Fatal(got, err, f.writes)
			}
			if !tc.accepted {
				_, err = f.store.Provenance(context.Background(), firstNamespace, key(identity(resource(38))))
				if !errors.Is(err, storage.ErrNotFound) {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestPartialFailuresAndRestart(t *testing.T) {
	for _, at := range []string{"item38:status", "item38:date", "item4:status"} {
		t.Run(at, func(t *testing.T) {
			f := setup(t, true)
			f.failure = at
			in := childInput("")
			r, err := f.handle(in)
			if err == nil || !r.Accepted || r.Completed {
				t.Fatal(r, err)
			}
			before := append([]string(nil), f.writes...)
			f.restart()
			f.failure = ""
			f.delivery("later", true, acceptedAt.Add(7*24*time.Hour))
			r, err = f.controller.Handle(context.Background(), "later", in)
			if err != nil || !r.Completed || r.Evidence.StartDate != "2026-10-10" {
				t.Fatal(r, err)
			}
			if !reflect.DeepEqual(f.writes, []string{"item38:status", "item38:date", "item4:status"}) {
				t.Fatal(before, f.writes)
			}
			f.writes = nil
			r, err = f.handle(in)
			if err != nil || !r.Completed || len(f.writes) != 0 {
				t.Fatal(r, err, f.writes)
			}
		})
	}
}

type faultyStore struct {
	eb.Store
	record func(context.Context, storage.Provenance) (bool, error)
	read   func(context.Context, string, string) (storage.Provenance, error)
}

func (s faultyStore) RecordProvenance(ctx context.Context, p storage.Provenance) (bool, error) {
	if s.record != nil {
		return s.record(ctx, p)
	}
	return s.Store.RecordProvenance(ctx, p)
}
func (s faultyStore) Provenance(ctx context.Context, ns, k string) (storage.Provenance, error) {
	if s.read != nil {
		return s.read(ctx, ns, k)
	}
	return s.Store.Provenance(ctx, ns, k)
}
func TestStorageAndCancellationRecovery(t *testing.T) {
	for _, at := range []string{"first storage failure", "completion storage failure", "first cancellation", "target cancellation", "date cancellation", "parent cancellation"} {
		t.Run(at, func(t *testing.T) {
			f := setup(t, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.cancel = cancel
			f.controller.cfg.Store = faultyStore{Store: f.store, record: func(ctx context.Context, p storage.Provenance) (bool, error) {
				if at == "first storage failure" && p.Namespace == firstNamespace || at == "completion storage failure" && p.Namespace == completionNamespace {
					return false, errors.New("private error")
				}
				ok, err := f.store.RecordProvenance(ctx, p)
				if at == "first cancellation" && p.Namespace == firstNamespace {
					cancel()
				}
				return ok, err
			}}
			switch at {
			case "target cancellation":
				f.cancelAt = "item38:status"
			case "date cancellation":
				f.cancelAt = "item38:date"
			case "parent cancellation":
				f.cancelAt = "item4:status"
			}
			r, err := f.controller.Handle(ctx, "push", childInput(""))
			if err == nil || r.Completed {
				t.Fatal(r, err)
			}
			if strings.Contains(at, "cancellation") && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if strings.Contains(at, "storage failure") && !errors.Is(err, ErrStorage) {
				t.Fatal(err)
			}
			_, err = f.store.Provenance(context.Background(), completionNamespace, key(identity(resource(38))))
			if !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("fabricated completion", err)
			}
			if (at == "first storage failure" || at == "first cancellation") && len(f.writes) != 0 {
				t.Fatal(f.writes)
			}
			f.restart()
			f.cancelAt = ""
			r, err = f.handle(childInput(""))
			if err != nil || !r.Completed || !reflect.DeepEqual(f.writes, []string{"item38:status", "item38:date", "item4:status"}) {
				t.Fatal(r, err, f.writes)
			}
		})
	}
}
func TestUnfinishedRevalidation(t *testing.T) {
	for _, body := range []string{"Create-Branch: false", "blocked", "inactive"} {
		t.Run(body, func(t *testing.T) {
			f := setup(t, true)
			f.failure = "item38:date"
			r, err := f.handle(childInput(""))
			if err == nil || !r.Accepted {
				t.Fatal(r, err)
			}
			first := r.Evidence
			f.restart()
			f.failure = ""
			f.writes = nil
			in := childInput(body)
			if body == "blocked" {
				in = childInput("")
				in.Context.Nodes = append(in.Context.Nodes, node(39, sp.Task, ""))
				in.Context.Dependencies = []ec.DependencyEdge{{Blocker: resource(39), Blocked: resource(38)}}
			}
			if body == "inactive" {
				in = childInput("")
				in.Context.Nodes = append(in.Context.Nodes, node(5, sp.Phase, ""))
				in.PhaseOrder = []storage.Resource{resource(5), resource(4)}
			}
			r, err = f.handle(in)
			if err != nil || r.Completed || r.Evidence != first || len(f.writes) != 0 {
				t.Fatal(r, err, f.writes)
			}
		})
	}
}
func TestIdentityAndProjectFailures(t *testing.T) {
	for _, at := range []string{"repository", "name", "sha", "comparison repository", "comparison base", "comparison head", "malformed comparison", "missing item", "duplicate item", "wrong project", "wrong resource", "wrong content", "wrong repository", "invalid date", "archived", "missing root decision", "wrong delivery"} {
		t.Run(at, func(t *testing.T) {
			f := setup(t, true)
			in := input(sp.Task, "")
			switch at {
			case "repository":
				f.remote.Repository.ID = "other"
			case "name":
				f.remote.Name = "other"
			case "sha":
				f.remote.SHA = "payload"
			case "missing root decision":
				in.Context.Nodes[0].State = ec.Closed
			case "wrong delivery":
				f.controller.cfg.Store = faultyStore{Store: f.store}
				f.delivery("other", false, acceptedAt)
				_, err := f.controller.Handle(context.Background(), "other", in)
				if err == nil || len(f.writes) != 0 {
					t.Fatal(err)
				}
				return
			default:
				if strings.Contains(at, "comparison") {
					f.controller.cfg.Comparison = compareFunc(func(ctx context.Context, r github.Repository, b, h string) (github.Comparison, error) {
						c := github.Comparison{Repository: r, BaseSHA: b, HeadSHA: h, Status: "ahead", AheadBy: 1}
						switch at {
						case "comparison repository":
							c.Repository.ID = "wrong"
						case "comparison base":
							c.BaseSHA = head
						case "comparison head":
							c.HeadSHA = base
						case "malformed comparison":
							c.BehindBy = 1
						}
						return c, nil
					})
				} else {
					original := f.cfg.Projects
					f.controller.cfg.Projects = projectFunc(func(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
						o, err := original.ReadCurrent(ctx, r)
						switch at {
						case "missing item":
							o.Projects[0].Items = nil
						case "duplicate item":
							o.Projects[0].Items = append(o.Projects[0].Items, o.Projects[0].Items[0])
						case "wrong project":
							o.Projects[0].ID = "other"
						case "wrong resource":
							o.Resource.NodeID = "other"
						case "wrong content":
							o.Issue.ID = "other"
						case "wrong repository":
							o.Issue.Repository.ID = "other"
						case "invalid date":
							o.Projects[0].Items[0].Fields[config.StartDate] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Date, Date: "yesterday"}}
						case "archived":
							o.Projects[0].Items[0].Archived = true
						}
						return o, err
					})
				}
			}
			r, err := f.handle(in)
			if err == nil || r.Accepted || len(f.writes) != 0 {
				t.Fatal(r, err, f.writes)
			}
		})
	}
}
func TestAssigneesAlwaysPreserved(t *testing.T) {
	for _, actors := range [][]github.Actor{nil, {{Login: "human"}}, {{Login: "bot[bot]"}}, {{Login: "ambiguous"}, {Login: "other"}}} {
		f := setup(t, true)
		f.assignees = actors
		r, err := f.handle(input(sp.Task, ""))
		if err != nil || !r.Completed || !reflect.DeepEqual(f.assignees, actors) {
			t.Fatal(r, err, f.assignees)
		}
	}
}
func TestCanonicalEvidenceAndInputOrdering(t *testing.T) {
	var baseline []byte
	for i := 0; i < 10; i++ {
		f := setup(t, true)
		in := childInput("")
		in.Context.Nodes = append(in.Context.Nodes, node(5, sp.Feature, ""))
		rand.New(rand.NewSource(int64(i))).Shuffle(len(in.Context.Nodes), func(a, b int) { in.Context.Nodes[a], in.Context.Nodes[b] = in.Context.Nodes[b], in.Context.Nodes[a] })
		before, _ := json.Marshal(in)
		r, err := f.handle(in)
		after, _ := json.Marshal(in)
		if err != nil || !r.Completed || !bytes.Equal(before, after) {
			t.Fatal(r, err)
		}
		p, _ := f.store.Provenance(context.Background(), firstNamespace, key(identity(resource(38))))
		if i == 0 {
			baseline = p.Metadata
		} else if !bytes.Equal(p.Metadata, baseline) {
			t.Fatal("unstable JSON")
		}
	}
}

func TestAuthorizationOnlyCannotStart(t *testing.T) {
	f := setup(t, false)
	f.remote = nil
	// Use #37 itself to reserve; fail its create response before completion.
	branches := github.BranchFake{DefaultBranchFunc: func(context.Context, github.Repository) (github.Branch, error) {
		return github.Branch{Repository: repo, Name: "main", SHA: base}, nil
	}, BranchFunc: func(_ context.Context, r github.Repository, n string) (github.Branch, error) {
		return github.Branch{}, &github.MissingBranch{Repository: r, Name: n}
	}, CreateBranchFunc: func(context.Context, github.Repository, string, string) (github.Branch, error) {
		return github.Branch{}, &github.Error{Category: github.Transient}
	}}
	branch, err := eb.New(eb.Config{Repositories: []github.Repository{repo}, Branches: branches, Store: f.store})
	if err != nil {
		t.Fatal(err)
	}
	_, err = branch.Ensure(context.Background(), "branch", input(sp.Task, ""))
	if err == nil {
		t.Fatal("expected create failure")
	}
	f.remote = &github.Branch{Repository: repo, Name: "development/issue-38", SHA: head}
	f.controller.cfg.Provenance = branch
	r, err := f.handle(input(sp.Task, ""))
	if err != nil || r.Accepted || len(f.writes) != 0 || f.reads != 0 {
		t.Fatal(r, err, f.writes, f.reads)
	}
}
func TestDirectPhaseWithParentDoesNotProgressParent(t *testing.T) {
	f := setup(t, true)
	in := input(sp.Phase, "Create-Branch: true\nParent: #4")
	in.Context.Nodes = append(in.Context.Nodes, node(4, sp.Phase, ""))
	in.Context.Parents = []ec.ParentEdge{{Child: resource(38), Parent: resource(4)}}
	in.PhaseOrder = []storage.Resource{resource(38), resource(4)}
	r, err := f.handle(in)
	if err != nil || !r.Completed || !reflect.DeepEqual(f.writes, []string{"item38:status", "item38:date"}) {
		t.Fatal(r, err, f.writes)
	}
}
func TestActivityProvenanceCorruption(t *testing.T) {
	for _, bad := range []string{"unknown", "duplicate", "version", "head", "date", "timestamp", "branch", "delivery", "completion-only", "different-completion"} {
		t.Run(bad, func(t *testing.T) {
			f := setup(t, true)
			r, err := f.handle(input(sp.Task, ""))
			if err != nil || !r.Completed {
				t.Fatal(r, err)
			}
			f.writes = nil
			f.controller.cfg.Store = faultyStore{Store: f.store, read: func(ctx context.Context, ns, k string) (storage.Provenance, error) {
				if bad == "completion-only" && ns == firstNamespace {
					return storage.Provenance{}, storage.ErrNotFound
				}
				p, err := f.store.Provenance(ctx, ns, k)
				if err != nil {
					return p, err
				}
				if bad == "different-completion" && ns != completionNamespace {
					return p, nil
				}
				var e Evidence
				json.Unmarshal(p.Metadata, &e)
				switch bad {
				case "unknown":
					p.Metadata = append(p.Metadata[:len(p.Metadata)-1], []byte(`,"extra":true}`)...)
					return p, nil
				case "duplicate":
					p.Metadata = append(p.Metadata[:len(p.Metadata)-1], []byte(`,"version":1}`)...)
					return p, nil
				case "version":
					e.Version = 2
				case "head":
					e.HeadSHA = base
				case "date":
					e.StartDate = "2026-10-11"
				case "timestamp":
					e.ReceivedAt = e.ReceivedAt.Add(time.Hour)
				case "branch":
					e.Branch.BaseSHA = head
				case "delivery":
					p.DeliveryID = "branch"
				case "different-completion":
					e.HeadSHA = strings.Repeat("c", 40)
				}
				p.Metadata, _ = json.Marshal(e)
				return p, nil
			}}
			r, err = f.handle(input(sp.Task, ""))
			if err == nil || r.Completed || len(f.writes) != 0 {
				t.Fatal(r, err, f.writes)
			}
		})
	}
}
func TestCurrentBranchAndComparisonCancellation(t *testing.T) {
	for _, at := range []string{"entry", "branch", "comparison", "projects"} {
		t.Run(at, func(t *testing.T) {
			f := setup(t, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch at {
			case "entry":
				cancel()
			case "branch":
				f.controller.cfg.Branches = github.BranchFake{BranchFunc: func(context.Context, github.Repository, string) (github.Branch, error) {
					cancel()
					return *f.remote, nil
				}}
			case "comparison":
				original := f.cfg.Comparison
				f.controller.cfg.Comparison = compareFunc(func(ctx context.Context, r github.Repository, b, h string) (github.Comparison, error) {
					c, e := original.CompareCommits(ctx, r, b, h)
					cancel()
					return c, e
				})
			case "projects":
				original := f.cfg.Projects
				f.controller.cfg.Projects = projectFunc(func(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
					o, e := original.ReadCurrent(ctx, r)
					cancel()
					return o, e
				})
			}
			r, err := f.controller.Handle(ctx, "push", input(sp.Task, ""))
			if !errors.Is(err, context.Canceled) || r.Completed || len(f.writes) != 0 {
				t.Fatal(r, err, f.writes)
			}
		})
	}
}
func TestDelayedPushNeverReplacesFirstAcceptance(t *testing.T) {
	f := setup(t, true)
	f.failure = "item38:date"
	first, err := f.handle(input(sp.Task, ""))
	if err == nil || !first.Accepted {
		t.Fatal(first, err)
	}
	f.delivery("older", true, acceptedAt.Add(-48*time.Hour))
	f.restart()
	f.failure = ""
	f.remote.SHA = strings.Repeat("c", 40)
	again, err := f.controller.Handle(context.Background(), "older", input(sp.Feature, ""))
	if err != nil || !again.Completed || again.Evidence != first.Evidence {
		t.Fatal(again, err)
	}
	p, err := f.store.Provenance(context.Background(), firstNamespace, key(identity(resource(38))))
	if err != nil || p.DeliveryID != "push" {
		t.Fatal(p, err)
	}
}
func TestConfigurationCopiedAndInvalid(t *testing.T) {
	f := setup(t, true)
	f.cfg.Repositories[0].ID = "caller"
	f.cfg.Project.Fields[config.StartDate] = "caller"
	f.cfg.Project.StatusOptions[config.InProgress] = "caller"
	r, err := f.handle(input(sp.Task, ""))
	if err != nil || !r.Completed {
		t.Fatal(r, err)
	}
	for _, cfg := range []Config{{}, {Repositories: []github.Repository{repo}, Store: (*storage.Store)(nil)}} {
		if _, err := New(cfg); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}

// This test-only assembly uses explicit complete fixtures. It is not #39's
// production repository-wide assembler or routing integration.
type workerProcessor func(context.Context, storage.Event) error

func (f workerProcessor) Process(ctx context.Context, e storage.Event) error { return f(ctx, e) }

type workerResolver struct{}

func (workerResolver) Resolve(context.Context, storage.Event) (storage.Resource, error) {
	return storage.Resource{}, ErrInvalid
}
func TestDurableWorkerRetryAcrossRestart(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	now := acceptedAt.UTC()
	e, err := f.store.Claim(ctx, "branch", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Settle(ctx, "branch", e.State.Attempts, storage.Completed, nil, ""); err != nil {
		t.Fatal(err)
	}
	newWorker := func() *worker.Worker {
		w, err := worker.New(worker.Config{Store: f.store, Resolver: workerResolver{}, Processor: workerProcessor(func(ctx context.Context, e storage.Event) error {
			_, err := f.controller.Handle(ctx, e.Delivery.ID, childInput(""))
			return err
		}), Classifier: func(error) worker.Classification { return worker.Classification{Category: "activity_local"} }, RetrySchedule: func(_ worker.Classification, _ int64, at time.Time) time.Time { return at.Add(time.Hour) }, Clock: func() time.Time { return now }, Concurrency: 1})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	f.failure = "item38:date"
	w := newWorker()
	if err := w.Step(ctx); err != nil {
		t.Fatal(err)
	}
	e, err = f.store.Event(ctx, "push")
	if err != nil || e.State.Status != storage.Retryable || e.State.Attempts != 1 {
		t.Fatal(e, err)
	}
	f.restart()
	now = now.Add(48 * time.Hour)
	f.failure = ""
	w = newWorker()
	if err := w.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(ctx); err != nil {
		t.Fatal(err)
	}
	e, err = f.store.Event(ctx, "push")
	if err != nil || e.State.Status != storage.Completed || e.State.Attempts != 2 {
		t.Fatal(e, err)
	}
	if !reflect.DeepEqual(f.writes, []string{"item38:status", "item38:date", "item4:status"}) {
		t.Fatal(f.writes)
	}
	f.writes = nil
	f.delivery("duplicate-event", true, now)
	if err := w.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatal(f.writes)
	}
}
func TestUnfinishedBranchRevalidation(t *testing.T) {
	for _, state := range []string{"base", "missing", "diverged"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t, true)
			f.failure = "item38:date"
			first, err := f.handle(input(sp.Task, ""))
			if err == nil || !first.Accepted {
				t.Fatal(first, err)
			}
			f.restart()
			f.failure = ""
			f.writes = nil
			switch state {
			case "base":
				f.remote.SHA = base
			case "missing":
				f.remote = nil
			case "diverged":
				f.status = "diverged"
			}
			r, err := f.handle(input(sp.Task, ""))
			if err != nil || r.Completed || r.Evidence != first.Evidence || len(f.writes) != 0 {
				t.Fatal(r, err, f.writes)
			}
		})
	}
}

// A notification accepted while a manual lookalike existed cannot gain authority
// from a controller creation that completed after that notification was accepted.
func TestManualPushBeforeControllerCreationNeverStartsActivity(t *testing.T) {
	f := setup(t, false)
	ctx := context.Background()
	// The authorization event is already accepted and a manual lookalike now
	// exists. Accept its push before successful controller creation.
	f.delivery("manual-push", true, acceptedAt.Add(time.Second))
	// The manual branch is removed before #37 performs its exact lookup.
	branches := f.cfg.Branches.(github.BranchFake)
	lookup := branches.BranchFunc
	branches.BranchFunc = func(ctx context.Context, r github.Repository, n string) (github.Branch, error) {
		f.remote = nil
		return lookup(ctx, r, n)
	}
	creationTime := acceptedAt.Add(2 * time.Second)
	var err error
	f.branch, err = eb.New(eb.Config{Repositories: []github.Repository{repo}, Branches: branches, Store: f.store, Clock: func() time.Time { return creationTime }})
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.branch.Ensure(ctx, "branch", input(sp.Task, ""))
	if err != nil || !created.Completed || f.remote == nil || f.remote.SHA != base {
		t.Fatal(created, err)
	}
	f.remote.SHA = head // Real controller-created branch subsequently advances.
	f.cfg.Provenance = f.branch
	f.controller, err = New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertRejected := func() {
		t.Helper()
		f.reads = 0
		f.comparisons = 0
		before := append([]string(nil), f.writes...)
		r, err := f.controller.Handle(ctx, "manual-push", childInput(""))
		if err != nil || r.Accepted || r.Completed || len(f.writes) != len(before) || f.reads != 0 || f.comparisons != 0 {
			t.Fatal(r, err, f.writes)
		}
	}
	assertRejected()
	for _, ns := range []string{firstNamespace, completionNamespace} {
		if _, err := f.store.Provenance(ctx, ns, key(identity(resource(38)))); !errors.Is(err, storage.ErrNotFound) {
			t.Fatal("temporal rejection persisted provenance", ns, err)
		}
	}
	f.restart()
	assertRejected()
	// Even a later qualifying notification arriving/processing out of order cannot
	// authorize the original manual push or change its rejection across restart.
	f.delivery("authorized-push", true, creationTime.Add(time.Nanosecond))
	r, err := f.controller.Handle(ctx, "authorized-push", childInput(""))
	if err != nil || !r.Completed || !reflect.DeepEqual(f.writes, []string{"item38:status", "item38:date", "item4:status"}) {
		t.Fatal(r, err, f.writes)
	}
	first, err := f.store.Provenance(ctx, firstNamespace, key(identity(resource(38))))
	if err != nil || first.DeliveryID != "authorized-push" {
		t.Fatal(first, err)
	}
	assertRejected()
	f.restart()
	assertRejected()
	unchanged, err := f.store.Provenance(ctx, firstNamespace, first.Key)
	if err != nil || !bytes.Equal(first.Metadata, unchanged.Metadata) || unchanged.DeliveryID != first.DeliveryID {
		t.Fatal(unchanged, err)
	}
}
func TestPushMustBeStrictlyAfterCreationCompletion(t *testing.T) {
	for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(delta.String(), func(t *testing.T) {
			f := setup(t, true)
			creation, err := f.branch.Completed(context.Background(), resource(38))
			if err != nil || !creation.Completed {
				t.Fatal(creation, err)
			}
			f.expectedDate = creation.CreatedAt.Add(delta).UTC().Format(time.DateOnly)
			f.delivery("candidate", true, creation.CreatedAt.Add(delta))
			r, err := f.controller.Handle(context.Background(), "candidate", childInput(""))
			if err != nil || r.Completed != (delta > 0) || r.Accepted != (delta > 0) {
				t.Fatal(r, err)
			}
			if delta <= 0 {
				if len(f.writes) != 0 {
					t.Fatal(f.writes)
				}
				for _, ns := range []string{firstNamespace, completionNamespace} {
					if _, err := f.store.Provenance(context.Background(), ns, key(identity(resource(38)))); !errors.Is(err, storage.ErrNotFound) {
						t.Fatal(ns, err)
					}
				}
				f.restart()
				r, err = f.controller.Handle(context.Background(), "candidate", childInput(""))
				if err != nil || r.Accepted || len(f.writes) != 0 {
					t.Fatal(r, err, f.writes)
				}
			}
		})
	}
}

func TestReloadedFirstActivityMustAlsoPostdateCreation(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	creation, err := f.branch.Completed(ctx, resource(38))
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.handle(childInput(""))
	if err != nil || !first.Completed {
		t.Fatal(first, err)
	}
	staleTime := creation.CreatedAt.Add(-time.Nanosecond)
	f.delivery("stale-origin", true, staleTime)
	f.writes = nil
	f.controller.cfg.Store = faultyStore{Store: f.store, read: func(ctx context.Context, ns, k string) (storage.Provenance, error) {
		if ns == completionNamespace {
			return storage.Provenance{}, storage.ErrNotFound
		}
		p, err := f.store.Provenance(ctx, ns, k)
		if err != nil {
			return p, err
		}
		e := first.Evidence
		e.ReceivedAt = staleTime
		e.StartDate = staleTime.UTC().Format(time.DateOnly)
		p.DeliveryID = "stale-origin"
		p.Metadata, _ = json.Marshal(e)
		return p, nil
	}}
	r, err := f.handle(childInput(""))
	if !errors.Is(err, ErrProvenance) || r.Completed || len(f.writes) != 0 {
		t.Fatal(r, err, f.writes)
	}
}
