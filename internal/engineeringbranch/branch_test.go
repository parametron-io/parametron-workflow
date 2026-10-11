package engineeringbranch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

var repo = github.Repository{ID: "R", Owner: "parametron-io", Name: "engine"}
var now = time.Unix(1700000000, 0).UTC()

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
	n := node(37, typ, body)
	in := ep.Input{Context: ec.Context{Root: n.Resource, Nodes: []ec.IssueNode{n}}}
	if typ == sp.Phase {
		in.PhaseOrder = []storage.Resource{n.Resource}
	}
	return in
}

type fixture struct {
	t          *testing.T
	path       string
	store      *storage.Store
	controller *Controller
	fake       github.BranchFake
	remote     map[string]github.Branch
	calls      []string
	creates    int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "branches.sqlite"), remote: map[string]github.Branch{}}
	var err error
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	f.fake = github.BranchFake{
		DefaultBranchFunc: func(context.Context, github.Repository) (github.Branch, error) {
			f.calls = append(f.calls, "default")
			return github.Branch{Repository: repo, Name: "trunk", SHA: sha}, nil
		},
		BranchFunc: func(_ context.Context, r github.Repository, name string) (github.Branch, error) {
			f.calls = append(f.calls, "lookup:"+name)
			if r != repo {
				t.Fatal(r)
			}
			if b, ok := f.remote[name]; ok {
				return b, nil
			}
			return github.Branch{}, &github.MissingBranch{Repository: r, Name: name}
		},
		CreateBranchFunc: func(ctx context.Context, r github.Repository, name, base string) (github.Branch, error) {
			f.calls = append(f.calls, "create:"+name)
			f.creates++
			// Mutation requires an already committed, exactly matching reservation.
			p, e, ok, err := f.controller.read(ctx, authorizationNamespace, identity(resource(37)), repo)
			if err != nil || !ok || p.DeliveryID == "" || e.Branch != name || e.BaseSHA != base || r != repo {
				t.Fatal(p, e, ok, err)
			}
			if _, exists := f.remote[name]; exists {
				return github.Branch{}, &github.Error{Category: github.Conflict}
			}
			b := github.Branch{Repository: r, Name: name, SHA: base}
			f.remote[name] = b
			return b, nil
		},
	}
	f.controller, err = New(Config{Repositories: []github.Repository{repo}, Store: f.store, Branches: f.fake, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	f.delivery("first", resource(37))
	return f
}
func (f *fixture) delivery(id string, r storage.Resource) {
	f.t.Helper()
	_, err := f.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: "issues", Payload: []byte(`{"status":"Ready","title":"stale title","body":"Create-Branch: true"}`), ReceivedAt: now, Resource: &r})
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) ensure(id string, in ep.Input) (Result, error) {
	return f.controller.Ensure(context.Background(), id, in)
}
func (f *fixture) evidence(ns string) (storage.Provenance, Evidence, bool) {
	f.t.Helper()
	p, e, ok, err := f.controller.read(context.Background(), ns, identity(resource(37)), repo)
	if err != nil {
		f.t.Fatal(err)
	}
	return p, e, ok
}

func TestEnsureFromCrossTargetOriginAndRestart(t *testing.T) {
	f := setup(t)
	trigger := resource(39)
	f.delivery("trigger", trigger)
	in := input(sp.Task, "")
	in.Context.Nodes = append(in.Context.Nodes, node(39, sp.Bug, ""))
	origin := Origin{DeliveryID: "trigger", Resource: trigger}
	got, err := f.controller.EnsureFrom(context.Background(), origin, in)
	if err != nil || !got.Completed || got.Evidence.WorkItem != identity(resource(37)) || got.Evidence.Repository != repo || got.Evidence.BaseRef != "trunk" || got.Evidence.BaseSHA != sha {
		t.Fatal(got, err)
	}
	auth, e, found := f.evidence(authorizationNamespace)
	done, created, finished := f.evidence(creationNamespace)
	want, err := json.Marshal(originEvidence{Version: 2, Origin: Origin{DeliveryID: "trigger", Resource: identity(trigger)}, Branch: got.Evidence})
	if err != nil || !found || !finished || e != created || !bytes.Equal(auth.Metadata, want) || !bytes.Equal(done.Metadata, want) || auth.DeliveryID != "trigger" || done.DeliveryID != "trigger" {
		t.Fatal(auth, done, err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.controller, err = New(Config{Repositories: []github.Repository{repo}, Store: f.store, Branches: f.fake})
	if err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	completion, err := f.controller.Completed(context.Background(), resource(37))
	if err != nil || !completion.Completed || completion.Evidence != got.Evidence {
		t.Fatal(completion, err)
	}
	again, err := f.controller.EnsureFrom(context.Background(), origin, in)
	if err != nil || again != got || len(f.calls) != 0 || f.creates != 1 {
		t.Fatal(again, err, f.calls)
	}
}

func TestEnsureFromSameTargetRetainsV1(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		f := setup(t)
		var got Result
		var err error
		if explicit {
			got, err = f.controller.EnsureFrom(context.Background(), Origin{DeliveryID: "first", Resource: resource(37)}, input(sp.Task, ""))
		} else {
			got, err = f.ensure("first", input(sp.Task, ""))
		}
		if err != nil || !got.Completed {
			t.Fatal(got, err)
		}
		auth, _, _ := f.evidence(authorizationNamespace)
		want, _ := json.Marshal(got.Evidence)
		if !bytes.Equal(auth.Metadata, want) {
			t.Fatal("same-target provenance is not v1", string(auth.Metadata))
		}
	}
}

func TestEnsureFromRejectsInvalidOriginWithoutBranchIO(t *testing.T) {
	for _, bad := range []string{"fabricated delivery", "different binding", "absent trigger", "outside trigger", "malformed target", "ineligible target"} {
		t.Run(bad, func(t *testing.T) {
			f := setup(t)
			trigger := resource(39)
			f.delivery("trigger", trigger)
			in := input(sp.Task, "")
			in.Context.Nodes = append(in.Context.Nodes, node(39, sp.Bug, ""))
			origin := Origin{DeliveryID: "trigger", Resource: trigger}
			switch bad {
			case "fabricated delivery":
				origin.DeliveryID = "fabricated"
			case "different binding":
				origin.DeliveryID = "first"
			case "absent trigger":
				in.Context.Nodes = in.Context.Nodes[:1]
			case "outside trigger":
				origin.Resource.Repository = "outside"
			case "malformed target":
				in.Context.Root.Kind = "pull_request"
			case "ineligible target":
				in.Context.Nodes[0] = node(37, sp.Task, "Create-Branch: false")
			}
			got, err := f.controller.EnsureFrom(context.Background(), origin, in)
			if bad == "ineligible target" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid origin accepted")
			}
			if got.Completed || len(f.calls) != 0 || f.creates != 0 {
				t.Fatal(got, err, f.calls)
			}
		})
	}
}

func TestEnsureFromV2CorruptionAndEnvelopeMismatch(t *testing.T) {
	for _, bad := range []string{"origin version", "origin resource", "origin delivery", "v1 v2 mismatch", "different completion", "noncanonical"} {
		t.Run(bad, func(t *testing.T) {
			f := setup(t)
			f.delivery("trigger", resource(39))
			in := input(sp.Task, "")
			in.Context.Nodes = append(in.Context.Nodes, node(39, sp.Bug, ""))
			origin := Origin{DeliveryID: "trigger", Resource: resource(39)}
			result, err := f.controller.EnsureFrom(context.Background(), origin, in)
			if err != nil || !result.Completed {
				t.Fatal(result, err)
			}
			f.calls = nil
			f.controller.store = faultyStore{Store: f.store, read: func(ctx context.Context, ns, k string) (storage.Provenance, error) {
				p, err := f.store.Provenance(ctx, ns, k)
				if err != nil {
					return p, err
				}
				if ns != creationNamespace {
					return p, nil
				}
				var wrapped originEvidence
				if err := json.Unmarshal(p.Metadata, &wrapped); err != nil {
					t.Fatal(err)
				}
				switch bad {
				case "origin version":
					wrapped.Version = 3
				case "origin resource":
					wrapped.Origin.Resource.Repository = "outside"
				case "origin delivery":
					wrapped.Origin.DeliveryID = "fabricated"
				case "v1 v2 mismatch":
					p.Metadata, _ = json.Marshal(wrapped.Branch)
					return p, nil
				case "different completion":
					wrapped.Branch.BaseSHA = strings.Repeat("f", 40)
				case "noncanonical":
					p.Metadata = append([]byte(" "), p.Metadata...)
					return p, nil
				}
				p.Metadata, err = json.Marshal(wrapped)
				return p, err
			}}
			got, err := f.controller.Completed(context.Background(), resource(37))
			if !errors.Is(err, ErrProvenance) || got.Completed || len(f.calls) != 0 {
				t.Fatal(got, err, f.calls)
			}
		})
	}
}
func TestAuthorizationUsesCurrentLifecycle(t *testing.T) {
	cases := []struct {
		name string
		in   ep.Input
		want bool
	}{
		{"Task default", input(sp.Task, ""), true}, {"Feature default", input(sp.Feature, ""), true},
		{"parentless Task", input(sp.Task, "Set-Status: false"), true},
		{"normal Phase", input(sp.Phase, ""), false}, {"explicit Phase", input(sp.Phase, "Create-Branch: true"), true},
		{"disabled branch despite observed Ready", input(sp.Task, "Create-Branch: false"), false},
		{"Set-Status false disabled branch", input(sp.Feature, "Set-Status: false\nCreate-Branch: false"), false},
	}
	child := input(sp.Task, "Parent: #45")
	child.Context.Nodes = append(child.Context.Nodes, node(45, sp.Phase, ""))
	child.Context.Parents = []ec.ParentEdge{{Child: resource(37), Parent: resource(45)}}
	child.PhaseOrder = []storage.Resource{resource(45)}
	cases = append(cases, struct {
		name string
		in   ep.Input
		want bool
	}{"active-parent child", child, true})
	blocked := input(sp.Task, "Parent: #45\nBlocked-By: #38\nSet-Status: false")
	blocked.Context.Nodes = append(blocked.Context.Nodes, node(45, sp.Phase, ""), node(38, sp.Task, "Parent: #45"))
	blocked.Context.Parents = []ec.ParentEdge{{resource(37), resource(45)}, {resource(38), resource(45)}}
	blocked.Context.Dependencies = []ec.DependencyEdge{{resource(38), resource(37)}}
	blocked.PhaseOrder = []storage.Resource{resource(45)}
	cases = append(cases, struct {
		name string
		in   ep.Input
		want bool
	}{"blocked Ready child Set-Status false", blocked, false})
	inactive := child
	inactive.Context.Nodes = append([]ec.IssueNode(nil), child.Context.Nodes...)
	inactive.Context.Nodes = append(inactive.Context.Nodes, node(44, sp.Phase, ""))
	inactive.PhaseOrder = []storage.Resource{resource(44), resource(45)}
	cases = append(cases, struct {
		name string
		in   ep.Input
		want bool
	}{"inactive-parent child", inactive, false})
	blockedRoot := input(sp.Feature, "Blocked-By: #38")
	blockedRoot.Context.Nodes = append(blockedRoot.Context.Nodes, node(38, sp.Bug, ""))
	blockedRoot.Context.Dependencies = []ec.DependencyEdge{{resource(38), resource(37)}}
	cases = append(cases, struct {
		name string
		in   ep.Input
		want bool
	}{"blocked parentless", blockedRoot, false})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			r, err := f.ensure("first", tc.in)
			if err != nil || r.Completed != tc.want || f.creates != boolInt(tc.want) {
				t.Fatal(r, err, f.calls)
			}
			if !tc.want && len(f.calls) != 0 {
				t.Fatal("ineligible must not read or mutate branches", f.calls)
			}
			_, _, auth := f.evidence(authorizationNamespace)
			_, _, complete := f.evidence(creationNamespace)
			if auth != tc.want || complete != tc.want {
				t.Fatal(auth, complete)
			}
		})
	}
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func TestDurableCompletionDuplicateRestartAndTitleChanges(t *testing.T) {
	f := setup(t)
	in := input(sp.Task, "")
	first, err := f.ensure("first", in)
	if err != nil || !first.Completed {
		t.Fatal(first, err)
	}
	p, e, ok := f.evidence(authorizationNamespace)
	q, created, done := f.evidence(creationNamespace)
	if !ok || !done || e != created || e != first.Evidence || !bytes.Equal(p.Metadata, q.Metadata) || p.DeliveryID != q.DeliveryID || e.BaseRef != "trunk" || e.BaseSHA != sha {
		t.Fatal(p, q, e)
	}
	f.delivery("duplicate", resource(37))
	f.calls = nil
	// Titles never enter the authorization input or identity. A changed webhook
	// title and Type/intent cannot rename or replace completed authorization.
	in = input(sp.Feature, "Create-Branch: false")
	again, err := f.ensure("duplicate", in)
	if err != nil || again != first || len(f.calls) != 0 {
		t.Fatal(again, err, f.calls)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.controller, err = New(Config{Repositories: []github.Repository{repo}, Branches: f.fake, Store: f.store})
	if err != nil {
		t.Fatal(err)
	}
	again, err = f.ensure("duplicate", input(sp.Task, ""))
	if err != nil || again != first || f.creates != 1 || len(f.calls) != 0 {
		t.Fatal(again, err, f.calls)
	}
	// Returned evidence is a value; caller changes cannot alter durable authority.
	again.Evidence.Branch = "replacement"
	again.Evidence.WorkItem.Number = 100
	_, durable, _ := f.evidence(creationNamespace)
	if durable != first.Evidence {
		t.Fatal(durable)
	}
}
func TestManualLookalikeAndAuthorizationOnlyFailClosed(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		t.Run(fmt.Sprint(authorized), func(t *testing.T) {
			f := setup(t)
			if authorized {
				f.reserve()
			}
			name, _ := Name(resource(37))
			f.remote[name] = github.Branch{Repository: repo, Name: name, SHA: sha}
			r, err := f.ensure("first", input(sp.Task, ""))
			if !errors.Is(err, ErrAmbiguous) || r.Completed || f.creates != 0 {
				t.Fatal(r, err, f.calls)
			}
			_, _, complete := f.evidence(creationNamespace)
			_, _, auth := f.evidence(authorizationNamespace)
			if complete || auth != authorized {
				t.Fatal("manual exact name + SHA was adopted", auth, complete)
			}
		})
	}
}
func (f *fixture) reserve() {
	f.t.Helper()
	e := Evidence{1, identity(resource(37)), repo, "development/issue-37", "trunk", sha}
	if err := f.controller.record(context.Background(), authorizationNamespace, "first", e); err != nil {
		f.t.Fatal(err)
	}
}
func TestAuthorizationOnlyRetriesExactBaseAndRequiresCurrentEligibility(t *testing.T) {
	f := setup(t)
	f.reserve()
	f.controller.branches = github.BranchFake{BranchFunc: f.fake.BranchFunc, CreateBranchFunc: f.fake.CreateBranchFunc}
	// Current ineligibility suppresses creation even with durable reservation.
	r, err := f.ensure("first", input(sp.Task, "Create-Branch: false"))
	if err != nil || r.Completed || len(f.calls) != 0 {
		t.Fatal(r, err, f.calls)
	}
	f.delivery("retry", resource(37))
	r, err = f.ensure("retry", input(sp.Feature, ""))
	if err != nil || !r.Completed || r.Evidence.BaseSHA != sha || !reflect.DeepEqual(f.calls, []string{"lookup:development/issue-37", "create:development/issue-37"}) {
		t.Fatal(r, err, f.calls)
	}
	p, _, _ := f.evidence(creationNamespace)
	if p.DeliveryID != "first" {
		t.Fatal("retry changed authorization event", p)
	}
}

type faultyStore struct {
	Store
	record func(context.Context, storage.Provenance) (bool, error)
	read   func(context.Context, string, string) (storage.Provenance, error)
}

func (s faultyStore) RecordProvenance(ctx context.Context, p storage.Provenance) (bool, error) {
	if s.record != nil {
		return s.record(ctx, p)
	}
	return s.Store.RecordProvenance(ctx, p)
}
func (s faultyStore) Provenance(ctx context.Context, ns, key string) (storage.Provenance, error) {
	if s.read != nil {
		return s.read(ctx, ns, key)
	}
	return s.Store.Provenance(ctx, ns, key)
}
func TestMissingCompletionAfterRemoteSuccessNeverAdopted(t *testing.T) {
	f := setup(t)
	f.controller.store = faultyStore{Store: f.store, record: func(ctx context.Context, p storage.Provenance) (bool, error) {
		if p.Namespace == creationNamespace {
			return false, errors.New("provider private details")
		}
		return f.store.RecordProvenance(ctx, p)
	}}
	r, err := f.ensure("first", input(sp.Task, ""))
	if !errors.Is(err, ErrStorage) || r.Completed || f.creates != 1 {
		t.Fatal(r, err)
	}
	_, _, authorized := f.evidence(authorizationNamespace)
	_, _, complete := f.evidence(creationNamespace)
	if !authorized || complete {
		t.Fatal(authorized, complete)
	}
	f.controller.store = f.store
	r, err = f.ensure("first", input(sp.Task, ""))
	if !errors.Is(err, ErrAmbiguous) || r.Completed || f.creates != 1 {
		t.Fatal(r, err)
	}
}
func TestGitHubPartialFailures(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			f := setup(t)
			fake := f.fake
			fake.CreateBranchFunc = func(_ context.Context, r github.Repository, name, sha string) (github.Branch, error) {
				f.creates++
				if ambiguous {
					f.remote[name] = github.Branch{Repository: r, Name: name, SHA: sha}
				}
				return github.Branch{}, &github.Error{Category: github.Transient}
			}
			f.controller.branches = fake
			_, err := f.ensure("first", input(sp.Task, ""))
			var ge *github.Error
			if !errors.As(err, &ge) || ge.Category != github.Transient {
				t.Fatal(err)
			}
			_, _, auth := f.evidence(authorizationNamespace)
			_, _, done := f.evidence(creationNamespace)
			if !auth || done {
				t.Fatal(auth, done)
			}
			f.controller.branches = f.fake
			f.calls = nil
			r, err := f.ensure("first", input(sp.Task, ""))
			if ambiguous {
				if !errors.Is(err, ErrAmbiguous) || r.Completed || f.creates != 1 {
					t.Fatal(r, err)
				}
			} else {
				if err != nil || !r.Completed || f.creates != 2 || strings.Contains(strings.Join(f.calls, ","), "default") {
					t.Fatal(r, err, f.calls)
				}
			}
		})
	}
}
func TestCancellationBoundaries(t *testing.T) {
	for _, at := range []string{"entry", "default", "lookup", "authorization", "create"} {
		t.Run(at, func(t *testing.T) {
			f := setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := f.fake
			switch at {
			case "entry":
				cancel()
			case "default":
				fake.DefaultBranchFunc = func(context.Context, github.Repository) (github.Branch, error) {
					cancel()
					return github.Branch{Repository: repo, Name: "trunk", SHA: sha}, nil
				}
			case "lookup":
				fake.BranchFunc = func(_ context.Context, r github.Repository, name string) (github.Branch, error) {
					cancel()
					return github.Branch{}, &github.MissingBranch{Repository: r, Name: name}
				}
			case "authorization":
				f.controller.store = faultyStore{Store: f.store, record: func(ctx context.Context, p storage.Provenance) (bool, error) {
					ok, err := f.store.RecordProvenance(ctx, p)
					cancel()
					return ok, err
				}}
			case "create":
				fake.CreateBranchFunc = func(ctx context.Context, r github.Repository, name, sha string) (github.Branch, error) {
					b, err := f.fake.CreateBranchFunc(ctx, r, name, sha)
					cancel()
					return b, err
				}
			}
			f.controller.branches = fake
			r, err := f.controller.Ensure(ctx, "first", input(sp.Task, ""))
			if !errors.Is(err, context.Canceled) || r.Completed {
				t.Fatal(r, err)
			}
			_, _, auth := f.evidence(authorizationNamespace)
			_, _, done := f.evidence(creationNamespace)
			if done || auth != (at == "authorization" || at == "create") || f.creates != boolInt(at == "create") {
				t.Fatal(auth, done, f.creates)
			}
			f.controller.store = f.store
			f.controller.branches = f.fake
			r, err = f.ensure("first", input(sp.Task, ""))
			if at == "create" {
				if !errors.Is(err, ErrAmbiguous) {
					t.Fatal(err)
				}
			} else if err != nil || !r.Completed {
				t.Fatal(r, err)
			}
		})
	}
}
func TestInvalidAndConflictingProvenance(t *testing.T) {
	for _, bad := range []string{"unknown-field", "duplicate-key", "version", "work-item", "branch", "base-ref", "base-sha", "repository", "event", "completion-only", "different-completion"} {
		t.Run(bad, func(t *testing.T) {
			f := setup(t)
			f.reserve()
			if bad == "completion-only" || bad == "different-completion" {
				_, e, _ := f.evidence(authorizationNamespace)
				if err := f.controller.record(context.Background(), creationNamespace, "first", e); err != nil {
					t.Fatal(err)
				}
			}
			f.controller.store = faultyStore{Store: f.store, read: func(ctx context.Context, ns, k string) (storage.Provenance, error) {
				p, err := f.store.Provenance(ctx, ns, k)
				if err != nil {
					return p, err
				}
				if bad == "completion-only" && ns == authorizationNamespace {
					return storage.Provenance{}, storage.ErrNotFound
				}
				if ns != authorizationNamespace && bad != "different-completion" {
					return p, nil
				}
				if bad == "different-completion" && ns != creationNamespace {
					return p, nil
				}
				var e Evidence
				json.Unmarshal(p.Metadata, &e)
				switch bad {
				case "unknown-field":
					p.Metadata = append(append([]byte(nil), p.Metadata[:len(p.Metadata)-1]...), []byte(`,"prose":"arbitrary"}`)...)
					return p, nil
				case "duplicate-key":
					p.Metadata = append(append([]byte(nil), p.Metadata[:len(p.Metadata)-1]...), []byte(`,"version":1}`)...)
					return p, nil
				case "version":
					e.Version = 2
				case "work-item":
					e.WorkItem.Number++
				case "branch":
					e.Branch = "development/issue-100"
				case "base-ref":
					e.BaseRef = "refs/heads/trunk"
				case "base-sha":
					e.BaseSHA = "main"
				case "repository":
					e.Repository.ID = "other"
				case "event":
					p.DeliveryID = "missing"
				case "different-completion":
					e.BaseSHA = strings.Repeat("a", 40)
				}
				p.Metadata, _ = json.Marshal(e)
				return p, nil
			}}
			r, err := f.ensure("first", input(sp.Task, ""))
			if err == nil || r.Completed || f.creates != 0 {
				t.Fatal(r, err, f.creates)
			}
		})
	}
}
func TestIdentityAndMetadataDeterminism(t *testing.T) {
	n, err := Name(resource(37))
	if err != nil || n != "development/issue-37" {
		t.Fatal(n, err)
	}
	upper := resource(37)
	upper.Owner = strings.ToUpper(upper.Owner)
	upper.Repository = strings.ToUpper(upper.Repository)
	upper.NodeID = "new-current-node"
	same, _ := Name(upper)
	different, _ := Name(resource(38))
	if same != n || different == n {
		t.Fatal(same, different)
	}
	for _, bad := range []storage.Resource{{}, {Owner: repo.Owner, Repository: repo.Name, Kind: "issue", Number: -1}, {Owner: repo.Owner, Repository: "a/b", Kind: "issue", Number: 37}, {Owner: repo.Owner, Repository: repo.Name, Kind: "pull_request", Number: 37}} {
		if _, err := Name(bad); !errors.Is(err, ErrInvalid) {
			t.Fatal(bad, err)
		}
	}
	var baseline []byte
	for i := 0; i < 10; i++ {
		f := setup(t)
		in := input(sp.Task, "")
		in.Context.Nodes = append(in.Context.Nodes, node(38, sp.Feature, ""), node(45, sp.Phase, ""))
		in.PhaseOrder = []storage.Resource{resource(45)}
		rand.New(rand.NewSource(int64(i))).Shuffle(len(in.Context.Nodes), func(a, b int) { in.Context.Nodes[a], in.Context.Nodes[b] = in.Context.Nodes[b], in.Context.Nodes[a] })
		before, _ := json.Marshal(in)
		r, err := f.ensure("first", in)
		after, _ := json.Marshal(in)
		if err != nil || !r.Completed || !bytes.Equal(before, after) {
			t.Fatal(r, err)
		}
		p, _, _ := f.evidence(authorizationNamespace)
		if i == 0 {
			baseline = append([]byte(nil), p.Metadata...)
		} else if !bytes.Equal(baseline, p.Metadata) {
			t.Fatal("unstable metadata")
		}
	}
}
func TestStorageFailurePreventsMutationAndConfigurationIsCopied(t *testing.T) {
	f := setup(t)
	f.controller.store = faultyStore{Store: f.store, record: func(context.Context, storage.Provenance) (bool, error) { return false, storage.ErrConflict }}
	_, err := f.ensure("first", input(sp.Task, ""))
	if !errors.Is(err, ErrProvenance) || f.creates != 0 {
		t.Fatal(err, f.creates)
	}
	repos := []github.Repository{repo}
	c, err := New(Config{Repositories: repos, Store: f.store, Branches: f.fake})
	if err != nil {
		t.Fatal(err)
	}
	repos[0].ID = "caller mutation"
	if c.repositories[0] != repo {
		t.Fatal(c.repositories)
	}
	for _, cfg := range []Config{{}, {Repositories: []github.Repository{repo, repo}, Store: f.store, Branches: f.fake}, {Repositories: []github.Repository{repo}, Store: (*storage.Store)(nil), Branches: f.fake}} {
		if _, err := New(cfg); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}
func TestConcurrentDuplicateOnOneController(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.ensure("first", input(sp.Task, ""))
			if err == nil && !r.Completed {
				err = errors.New("missing completion")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != 1 {
		t.Fatal(f.creates)
	}
}

func TestReadAndCreateIdentityFailures(t *testing.T) {
	for _, at := range []string{"default-error", "default-identity", "lookup-error", "lookup-identity", "create-identity", "create-sha", "create-malformed"} {
		t.Run(at, func(t *testing.T) {
			f := setup(t)
			fake := f.fake
			switch at {
			case "default-error":
				fake.DefaultBranchFunc = func(context.Context, github.Repository) (github.Branch, error) {
					return github.Branch{}, &github.Error{Category: github.RateLimited}
				}
			case "default-identity":
				fake.DefaultBranchFunc = func(context.Context, github.Repository) (github.Branch, error) {
					return github.Branch{Repository: github.Repository{ID: "wrong", Owner: repo.Owner, Name: repo.Name}, Name: "trunk", SHA: sha}, nil
				}
			case "lookup-error":
				fake.BranchFunc = func(context.Context, github.Repository, string) (github.Branch, error) {
					return github.Branch{}, &github.Error{Category: github.Transient}
				}
			case "lookup-identity":
				fake.BranchFunc = func(context.Context, github.Repository, string) (github.Branch, error) {
					return github.Branch{Repository: repo, Name: "wrong", SHA: sha}, nil
				}
			default:
				fake.CreateBranchFunc = func(ctx context.Context, r github.Repository, name, base string) (github.Branch, error) {
					b, err := f.fake.CreateBranchFunc(ctx, r, name, base)
					if err != nil {
						return b, err
					}
					switch at {
					case "create-identity":
						b.Repository.ID = "wrong"
					case "create-sha":
						b.SHA = strings.Repeat("a", 40)
					case "create-malformed":
						return github.Branch{}, &github.Error{Category: github.Malformed}
					}
					return b, nil
				}
			}
			f.controller.branches = fake
			r, err := f.ensure("first", input(sp.Task, ""))
			if err == nil || r.Completed {
				t.Fatal(r, err)
			}
			_, _, auth := f.evidence(authorizationNamespace)
			_, _, done := f.evidence(creationNamespace)
			remoteEffect := strings.HasPrefix(at, "create-")
			if auth != remoteEffect || done || f.creates != boolInt(remoteEffect) {
				t.Fatal(auth, done, f.creates)
			}
			if remoteEffect {
				f.controller.branches = f.fake
				_, err = f.ensure("first", input(sp.Task, ""))
				if !errors.Is(err, ErrAmbiguous) {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestCompletedBranchIsNeverRecreated(t *testing.T) {
	f := setup(t)
	r, err := f.ensure("first", input(sp.Task, ""))
	if err != nil {
		t.Fatal(err)
	}
	delete(f.remote, r.Evidence.Branch)
	f.calls = nil
	in := input(sp.Task, "")
	in.Context.Nodes[0].State = ec.Closed
	again, err := f.ensure("first", in)
	if err != nil || again != r || len(f.calls) != 0 || f.creates != 1 {
		t.Fatal(again, err, f.calls)
	}
}
func TestInvalidPolicyAndDeliveryCannotAuthorize(t *testing.T) {
	for _, bad := range []string{"policy", "delivery", "binding", "deployment"} {
		t.Run(bad, func(t *testing.T) {
			f := setup(t)
			in := input(sp.Task, "")
			delivery := "first"
			switch bad {
			case "policy":
				in.Context.Nodes[0].Type = "invented"
			case "delivery":
				delivery = "missing"
			case "binding":
				f.delivery("other", resource(99))
				delivery = "other"
			case "deployment":
				in.Context.Root.Repository = "outside"
				in.Context.Nodes[0].Resource = in.Context.Root
			}
			r, err := f.ensure(delivery, in)
			if err == nil || r.Completed || len(f.calls) != 0 {
				t.Fatal(r, err, f.calls)
			}
		})
	}
}
func TestAuthorizationOnlyRestartRetainsExactReservation(t *testing.T) {
	f := setup(t)
	f.reserve()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	// Default lookup is deliberately absent: restart must not choose a moving base.
	f.controller, err = New(Config{Repositories: []github.Repository{repo}, Store: f.store, Branches: github.BranchFake{BranchFunc: f.fake.BranchFunc, CreateBranchFunc: f.fake.CreateBranchFunc}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.ensure("first", input(sp.Task, ""))
	if err != nil || !r.Completed || r.Evidence.BaseSHA != sha || f.creates != 1 {
		t.Fatal(r, err)
	}
}

func TestGenericNotFoundDoesNotProveBranchAbsence(t *testing.T) {
	for _, bad := range []string{"generic", "wrong-repository", "wrong-name"} {
		t.Run(bad, func(t *testing.T) {
			f := setup(t)
			fake := f.fake
			fake.BranchFunc = func(_ context.Context, r github.Repository, name string) (github.Branch, error) {
				if bad == "generic" {
					return github.Branch{}, &github.Error{Category: github.NotFound, Status: 404}
				}
				if bad == "wrong-repository" {
					r.ID = "different"
				} else {
					name = "different"
				}
				return github.Branch{}, &github.MissingBranch{Repository: r, Name: name}
			}
			f.controller.branches = fake
			result, err := f.ensure("first", input(sp.Task, ""))
			if err == nil || result.Completed || f.creates != 0 {
				t.Fatal(result, err)
			}
			_, _, auth := f.evidence(authorizationNamespace)
			if auth {
				t.Fatal("unverified absence produced authorization")
			}
		})
	}
}

func TestReadOnlyCompletedEvidence(t *testing.T) {
	for _, state := range []string{"absent", "authorization", "completed", "creation-only", "corrupt", "conflict", "delivery"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			if state != "absent" {
				f.reserve()
			}
			if state != "absent" && state != "authorization" {
				_, err := f.ensure("first", input(sp.Task, ""))
				if err != nil {
					t.Fatal(err)
				}
			}
			if state == "creation-only" || state == "corrupt" || state == "conflict" || state == "delivery" {
				f.controller.store = faultyStore{Store: f.store, read: func(ctx context.Context, ns, k string) (storage.Provenance, error) {
					if state == "creation-only" && ns == authorizationNamespace {
						return storage.Provenance{}, storage.ErrNotFound
					}
					p, err := f.store.Provenance(ctx, ns, k)
					if err != nil {
						return p, err
					}
					if ns == creationNamespace {
						switch state {
						case "corrupt":
							p.Metadata = []byte(`{"version":1,"unknown":true}`)
						case "conflict":
							var e Evidence
							json.Unmarshal(p.Metadata, &e)
							e.BaseSHA = strings.Repeat("c", 40)
							p.Metadata, _ = json.Marshal(e)
						case "delivery":
							p.DeliveryID = "missing"
						}
					}
					return p, nil
				}}
			}
			// Any accidental read or write capability use fails through empty fake hooks.
			f.controller.branches = github.BranchFake{}
			result, err := f.controller.Completed(context.Background(), resource(37))
			valid := state == "absent" || state == "authorization" || state == "completed"
			if (err == nil) != valid || result.Completed != (state == "completed") {
				t.Fatal(result, err)
			}
			if result.Completed {
				result.Evidence.BaseSHA = "caller change"
				again, err := f.controller.Completed(context.Background(), resource(37))
				if err != nil || again.Evidence.BaseSHA != sha {
					t.Fatal(again, err)
				}
			}
		})
	}
}

func TestCompletedExposesValidatedCreationTime(t *testing.T) {
	f := setup(t)
	f.reserve() // Authorization's provenance time is now.
	creationTime := now.Add(time.Hour)
	f.controller.clock = func() time.Time { return creationTime }
	if r, err := f.ensure("first", input(sp.Task, "")); err != nil || !r.Completed {
		t.Fatal(r, err)
	}
	completion, err := f.controller.Completed(context.Background(), resource(37))
	if err != nil || !completion.Completed || !completion.CreatedAt.Equal(creationTime) || completion.CreatedAt.Location() != time.UTC {
		t.Fatal(completion, err)
	}
	// Timestamp is loaded from durable creation completion, never from a new clock.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.controller, err = New(Config{Repositories: []github.Repository{repo}, Store: f.store, Branches: github.BranchFake{}, Clock: func() time.Time { return creationTime.Add(24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.controller.Completed(context.Background(), resource(37))
	if err != nil || again != completion {
		t.Fatal(again, err)
	}
	again.CreatedAt = time.Time{}
	f.controller.store = faultyStore{Store: f.store, read: func(ctx context.Context, ns, k string) (storage.Provenance, error) {
		p, err := f.store.Provenance(ctx, ns, k)
		if ns == creationNamespace {
			p.CreatedAt = time.Time{}
		}
		return p, err
	}}
	if r, err := f.controller.Completed(context.Background(), resource(37)); !errors.Is(err, ErrProvenance) || r.Completed || !r.CreatedAt.IsZero() {
		t.Fatal(r, err)
	}
}

func TestPlatformIntNumberBoundaries(t *testing.T) {
	for _, tc := range []struct {
		number int64
		want   bool
	}{
		{-1, false}, {0, false}, {1, true}, {math.MaxInt32, true}, {int64(math.MaxInt32) + 1, strconv.IntSize == 64}, {math.MaxInt64, strconv.IntSize == 64},
	} {
		r := resource(tc.number)
		name, err := Name(r)
		if (err == nil) != tc.want {
			t.Fatalf("Name(%d)=%q, %v; want accepted=%v", tc.number, name, err, tc.want)
		}
		if tc.want && name != "development/issue-"+strconv.FormatInt(tc.number, 10) {
			t.Fatal(name)
		}
	}
}
