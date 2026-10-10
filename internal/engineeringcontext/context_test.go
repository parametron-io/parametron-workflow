package engineeringcontext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semantic"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type sink struct{}

func (sink) Evaluate(context.Context, observe.PolicyInput) error { return nil }

type semanticSink struct{}

func (semanticSink) Evaluate(context.Context, semanticflow.PolicyInput) error { return nil }
func deployment() config.ResolvedConfig {
	project := func(id string, bug bool) config.ResolvedProject {
		p := config.ResolvedProject{ID: id, Fields: map[config.FieldRole]string{}, StatusOptions: map[config.StatusRole]string{}}
		for _, v := range []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate} {
			p.Fields[v] = id + string(v)
		}
		for _, v := range []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done} {
			p.StatusOptions[v] = id + string(v)
		}
		if bug {
			p.Fields[config.PriorityScore] = id + "score"
			p.StatusOptions[config.ToTriage] = id + "triage"
		} else {
			p.StatusOptions[config.Blocked] = id + "blocked"
		}
		return p
	}
	return config.ResolvedConfig{Organization: config.Organization{ID: "org", Login: "parametron-io"}, Repositories: []config.Repository{{ID: "engine", Owner: "parametron-io", Name: "parametron-engine"}, {ID: "freecad", Owner: "parametron-io", Name: "parametron-freecad"}}, Engineering: project("E", false), BugTracker: project("B", true)}
}
func resource(repo string, n int64) storage.Resource {
	return storage.Resource{Owner: "parametron-io", Repository: repo, Kind: "issue", Number: n}
}
func issue(repo string, n int, body string) github.Issue {
	var repository config.Repository
	for _, r := range deployment().Repositories {
		if r.Name == repo {
			repository = r
		}
	}
	return github.Issue{Identity: github.Identity{ID: fmt.Sprintf("%s-%d", repo, n), Repository: repository, Number: n}, Title: "current", Body: body, State: "OPEN", Type: &github.IssueType{ID: "drift", Name: "Bug"}}
}

type fixture struct {
	t              *testing.T
	issues         map[storage.Resource]github.Issue
	missing        map[storage.Resource]bool
	store          *storage.Store
	primary        *observe.Processor
	loader         *semanticflow.AcceptedLoader
	resolver       *Resolver
	calls          []storage.Resource
	runs           int
	random         *rand.Rand
	listedOverride func(storage.Resource, github.Issue) github.Issue
}

func setup(t *testing.T, values ...github.Issue) *fixture {
	t.Helper()
	f := &fixture{t: t, issues: map[storage.Resource]github.Issue{}, missing: map[storage.Resource]bool{}, random: rand.New(rand.NewSource(34))}
	for _, v := range values {
		f.issues[resource(v.Repository.Name, int64(v.Number))] = v
	}
	var err error
	f.store, err = storage.Open(context.Background(), filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	fake := &github.Fake{IssueFunc: func(ctx context.Context, ref github.Ref) (github.Issue, error) {
		key := resource(strings.ToLower(ref.Repository), int64(ref.Number))
		f.calls = append(f.calls, key)
		v, ok := f.issues[key]
		if !ok || f.missing[key] {
			return github.Issue{}, &github.Error{Category: github.NotFound}
		}
		return v, nil
	}}
	f.primary, err = observe.NewProcessor(deployment(), fake, sink{})
	if err != nil {
		t.Fatal(err)
	}
	f.loader, err = semanticflow.NewAcceptedLoader(f.store)
	if err != nil {
		t.Fatal(err)
	}
	lister := &github.ListerFake{ListIssuesFunc: func(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
		out := []github.ListedIssue{}
		for key, v := range f.issues {
			if v.Repository.Name == r.Name {
				if f.listedOverride != nil {
					v = f.listedOverride(key, v)
				}
				out = append(out, github.ListedIssue{Identity: v.Identity, Title: v.Title, Body: v.Body, State: v.State})
			}
		}
		f.random.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out, nil
	}}
	d := deployment()
	f.random.Shuffle(len(d.Repositories), func(i, j int) { d.Repositories[i], d.Repositories[j] = d.Repositories[j], d.Repositories[i] })
	f.resolver, err = New(Config{Deployment: d, Primary: f.primary, Lister: lister, Accepted: f.loader})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Seed through the real coordinator, never by decoding/writing its private envelope.
func (f *fixture) accept(key storage.Resource, typ semanticpolicy.IssueType) {
	f.t.Helper()
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()
	id := fmt.Sprintf("%s-%d", key.Repository, key.Number)
	_, err := f.store.InsertDelivery(ctx, storage.Delivery{ID: id, EventName: "issues", Payload: []byte(`{"body":"Parent: #999","state":"CLOSED"}`), ReceivedAt: now, Resource: &key})
	if err != nil {
		f.t.Fatal(err)
	}
	catalog, err := semantic.LoadCatalog(os.DirFS("../.."))
	if err != nil {
		f.t.Fatal(err)
	}
	asset, err := catalog.Lookup(semantic.ClassifyIssue)
	if err != nil {
		f.t.Fatal(err)
	}
	runner := &semantic.Fake{ClassifyIssueFunc: func(context.Context, semantic.Input) (semantic.Result, error) {
		f.runs++
		return semantic.Result{Output: json.RawMessage(fmt.Sprintf(`{"type":%q,"labels":["workflow"],"priority":"High","effort":"S"}`, typ)), Provenance: semantic.Provenance{Capability: semantic.ClassifyIssue, Provider: "fake", Model: "cheap", PromptIdentity: asset.Prompt.Identity, PromptDigest: asset.Prompt.Digest, SchemaIdentity: asset.Schema.Identity, SchemaDigest: asset.Schema.Digest}}, nil
	}}
	flow, err := semanticflow.New(semanticflow.Config{Store: f.store, Primary: f.primary, Runner: runner, Consumer: semanticSink{}, Clock: func() time.Time { return now }})
	if err != nil {
		f.t.Fatal(err)
	}
	o, err := f.primary.ReadPrimary(ctx, key)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := flow.Enrich(ctx, observe.PolicyInput{DeliveryID: id, Observed: o}); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) acceptAll() {
	for k := range f.issues {
		typ := semanticpolicy.Task
		if k.Number == 45 {
			typ = semanticpolicy.Phase
		}
		f.accept(k, typ)
	}
}
func (f *fixture) resolve(root storage.Resource) (Context, error) {
	f.calls = nil
	runs := f.runs
	out, err := f.resolver.Resolve(context.Background(), root)
	if f.runs != runs {
		f.t.Fatal("Runner invoked")
	}
	return out, err
}

const engine = "parametron-engine"
const freecad = "parametron-freecad"

func TestIntegratedAuthoritativeContextDeterminism(t *testing.T) {
	phase := issue(engine, 45, "")
	local := issue(engine, 1, "Parent: #45\nBlocked-By: parametron-io/parametron-freecad#2")
	cross := issue(freecad, 1, "Parent: parametron-io/parametron-engine#45\nBlocked-By: #2")
	blocker := issue(freecad, 2, "Blocks: #1 parametron-io/parametron-engine#1\nBlocks: #1")
	blocker.State = "CLOSED"
	unrelated := issue(engine, 99, "")
	phase.SubIssues = []github.Identity{unrelated.Identity}
	cross.Parent = &unrelated.Identity
	f := setup(t, phase, local, cross, blocker, unrelated)
	f.accept(resource(engine, 45), semanticpolicy.Phase)
	f.accept(resource(engine, 1), semanticpolicy.Task)
	f.accept(resource(freecad, 1), semanticpolicy.Feature)
	f.accept(resource(freecad, 2), semanticpolicy.Bug)
	var baseline []byte
	for n := 0; n < 10; n++ {
		c := f.resolver.cfg
		c.Deployment = deployment()
		f.random.Shuffle(len(c.Deployment.Repositories), func(i, j int) {
			c.Deployment.Repositories[i], c.Deployment.Repositories[j] = c.Deployment.Repositories[j], c.Deployment.Repositories[i]
		})
		var err error
		f.resolver, err = New(c)
		if err != nil {
			t.Fatal(err)
		}
		out, err := f.resolve(resource(engine, 45))
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Nodes) != 4 || len(out.Parents) != 2 || len(out.Dependencies) != 2 {
			t.Fatal(out)
		}
		if out.Nodes[0].Resource.Repository != engine || out.Nodes[0].Resource.Number != 1 || out.Nodes[1].Classification.Type != semanticpolicy.Phase || out.Nodes[3].State != "CLOSED" {
			t.Fatal(out)
		}
		if out.Parents[0].Parent.Number != 45 || out.Parents[1].Child.Repository != freecad {
			t.Fatal(out.Parents)
		}
		for _, edge := range out.Dependencies {
			if edge.Blocker.Repository != freecad || edge.Blocker.Number != 2 || edge.Blocked.Number != 1 {
				t.Fatal(edge)
			}
		}
		data, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			baseline = data
		} else if !bytes.Equal(baseline, data) {
			t.Fatal("unstable JSON")
		}
		out.Nodes[0].Classification.Labels[0] = "docs"
		out.Nodes[0].Intent.BlockedBy[0].Number = 999
	}
}
func TestDependencyForms(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{{"blocked by", "Blocked-By: #2", ""}, {"blocks", "", "Blocks: #1"}, {"mirrored", "Blocked-By: #2\nBlocked-By: #2", "Blocks: #1\nBlocks: #1"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, issue(engine, 1, tc.a), issue(engine, 2, tc.b))
			f.acceptAll()
			out, err := f.resolve(resource(engine, 1))
			if err != nil || len(out.Dependencies) != 1 || out.Dependencies[0].Blocker.Number != 2 || out.Dependencies[0].Blocked.Number != 1 {
				t.Fatal(out, err)
			}
		})
	}
}
func TestInvalidGraphs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bodies []string
		want   error
	}{
		{"self parent", []string{"Parent: #1"}, ErrInvalid},
		{"self blocked by", []string{"Blocked-By: #1"}, ErrInvalid},
		{"self blocks", []string{"Blocks: #1"}, ErrInvalid},
		{"missing parent", []string{"Parent: #9"}, ErrMissing},
		{"missing blocker", []string{"Blocked-By: #9"}, ErrMissing},
		{"outside", []string{"Parent: parametron-io/unknown#9"}, ErrDeployment},
		{"outside dependency", []string{"Blocks: parametron-io/unknown#9"}, ErrDeployment},
		{"parent cycle", []string{"Parent: #2", "Parent: #1"}, ErrParentCycle},
		{"long parent cycle", []string{"Parent: #2", "Parent: #3", "Parent: #1"}, ErrParentCycle},
		{"dependency cycle", []string{"Blocked-By: #2", "Blocked-By: #1"}, ErrDependencyCycle},
		{"long dependency cycle", []string{"Blocks: #2", "Blocks: #3", "Blocks: #1"}, ErrDependencyCycle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := []github.Issue{}
			for n, b := range tc.bodies {
				values = append(values, issue(engine, n+1, b))
			}
			f := setup(t, values...)
			f.acceptAll()
			_, err := f.resolve(resource(engine, 1))
			if !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
			for _, read := range f.calls {
				if read.Repository == "unknown" {
					t.Fatal("unauthorized read")
				}
			}
		})
	}
}
func TestInvalidDiscoveryAndCurrentIntent(t *testing.T) {
	for _, body := range []string{"Parent: #2\nParent: #3", "Blocked-By: #1,#2", "Blocks: nope", "Target: nope", "Refs: nope", "Automation: True"} {
		t.Run(body, func(t *testing.T) {
			f := setup(t, issue(engine, 1, ""), issue(engine, 99, body))
			f.accept(resource(engine, 1), semanticpolicy.Task)
			_, err := f.resolve(resource(engine, 1))
			if !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
		})
	}
	f := setup(t, issue(engine, 1, "Parent: #2"), issue(engine, 2, ""))
	f.acceptAll()
	v := f.issues[resource(engine, 2)]
	v.Body = "Parent: nope"
	f.issues[resource(engine, 2)] = v
	_, err := f.resolve(resource(engine, 1))
	if !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestMissingSemanticState(t *testing.T) {
	f := setup(t, issue(engine, 45, ""), issue(freecad, 1, "Parent: parametron-io/parametron-engine#45"), issue(engine, 99, ""))
	f.accept(resource(engine, 45), semanticpolicy.Phase)
	_, err := f.resolve(resource(engine, 45))
	if !errors.Is(err, ErrIncomplete) {
		t.Fatal(err)
	}
	f.accept(resource(freecad, 1), semanticpolicy.Task)
	if _, err := f.resolve(resource(engine, 45)); err != nil {
		t.Fatal("unrelated unclassified Issue must not fail", err)
	}
}
func TestDisappearanceAndFreshAttempt(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprint(child), func(t *testing.T) {
			body := ""
			rootBody := "Blocked-By: #1"
			if child {
				body = "Parent: #45"
				rootBody = ""
			}
			f := setup(t, issue(engine, 45, rootBody), issue(engine, 1, body))
			f.acceptAll()
			f.missing[resource(engine, 1)] = true
			_, err := f.resolve(resource(engine, 45))
			if !errors.Is(err, ErrMissing) {
				t.Fatal(err)
			}
			f.missing[resource(engine, 1)] = false
			if _, err := f.resolve(resource(engine, 45)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestChangedDiscoveryFails(t *testing.T) {
	f := setup(t, issue(engine, 45, ""), issue(freecad, 1, "Parent: parametron-io/parametron-engine#45"))
	f.acceptAll()
	f.listedOverride = func(key storage.Resource, v github.Issue) github.Issue {
		if key.Repository == freecad {
			v.Body += "\nBlocks: #2"
		}
		return v
	}
	_, err := f.resolve(resource(engine, 45))
	if !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
	f.listedOverride = nil
	if _, err := f.resolve(resource(engine, 45)); err != nil {
		t.Fatal(err)
	}
}
func TestParentResolutionAndNoNativeInference(t *testing.T) {
	root := issue(freecad, 1, "Parent: parametron-io/parametron-engine#45")
	phase := issue(engine, 45, "")
	native := issue(engine, 2, "")
	root.Parent = &native.Identity
	root.BlockedBy = []github.Identity{native.Identity}
	phase.SubIssues = []github.Identity{native.Identity}
	f := setup(t, root, phase, native)
	f.accept(resource(freecad, 1), semanticpolicy.Task)
	f.accept(resource(engine, 45), semanticpolicy.Phase)
	out, err := f.resolve(resource(freecad, 1))
	if err != nil || len(out.Nodes) != 2 || len(out.Parents) != 1 || len(out.Dependencies) != 0 || out.Parents[0].Parent.Repository != engine {
		t.Fatal(out, err)
	}
}

type corruptStore struct{ *storage.Store }

func (s corruptStore) Provenance(ctx context.Context, ns, key string) (storage.Provenance, error) {
	p, err := s.Store.Provenance(ctx, ns, key)
	if err == nil {
		p.Metadata = []byte(`{}`)
	}
	return p, err
}
func TestCorruptAcceptanceFailsContext(t *testing.T) {
	f := setup(t, issue(engine, 45, ""))
	f.acceptAll()
	loader, err := semanticflow.NewAcceptedLoader(corruptStore{f.store})
	if err != nil {
		t.Fatal(err)
	}
	f.resolver.cfg.Accepted = loader
	_, err = f.resolve(resource(engine, 45))
	if !errors.Is(err, semanticflow.ErrCompletion) {
		t.Fatal(err)
	}
}
func TestDeploymentBoundaryAndConfiguration(t *testing.T) {
	f := setup(t, issue(engine, 45, ""))
	f.acceptAll()
	root := resource(engine, 45)
	root.Owner = "PARAMETRON-IO"
	root.Repository = "PARAMETRON-ENGINE"
	if _, err := f.resolve(root); err != nil {
		t.Fatal(err)
	}
	for _, root := range []storage.Resource{resource("unknown", 45), {Owner: "parametron-io", Repository: engine, Kind: "pull_request", Number: 45}} {
		_, err := f.resolve(root)
		if err == nil || len(f.calls) != 0 {
			t.Fatal(err, f.calls)
		}
	}
	for _, name := range []string{"nil primary", "nil accepted", "nil lister", "duplicate name", "duplicate id", "blank id", "wrong owner", "wrong org"} {
		t.Run(name, func(t *testing.T) {
			c := f.resolver.cfg
			c.Deployment = deployment()
			switch name {
			case "nil primary":
				c.Primary = (*observe.Processor)(nil)
			case "nil accepted":
				c.Accepted = (*semanticflow.AcceptedLoader)(nil)
			case "nil lister":
				c.Lister = (*github.ListerFake)(nil)
			case "duplicate name":
				c.Deployment.Repositories[1].Name = strings.ToUpper(c.Deployment.Repositories[0].Name)
			case "duplicate id":
				c.Deployment.Repositories[1].ID = c.Deployment.Repositories[0].ID
			case "blank id":
				c.Deployment.Repositories[0].ID = " "
			case "wrong owner":
				c.Deployment.Repositories[0].Owner = "other"
			case "wrong org":
				c.Deployment.Organization.Login = "other"
			}
			if _, err := New(c); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
		})
	}
}
func TestCurrentMalformedStateAndPRRejected(t *testing.T) {
	f := setup(t, issue(engine, 1, "Parent: #2"), issue(engine, 2, ""))
	f.acceptAll()
	// A PR with number 2 is absent from Issue reads; no PR fallback is attempted.
	delete(f.issues, resource(engine, 2))
	_, err := f.resolve(resource(engine, 1))
	if !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	bad := issue(engine, 2, "")
	bad.State = "MERGED"
	f.issues[resource(engine, 2)] = bad
	_, err = f.resolve(resource(engine, 1))
	if !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestDiscoveryDuplicatesAcrossRepositories(t *testing.T) {
	f := setup(t, issue(engine, 1, ""), issue(freecad, 1, ""))
	f.acceptAll()
	f.listedOverride = func(key storage.Resource, v github.Issue) github.Issue { v.ID = "same"; return v }
	_, err := f.resolve(resource(engine, 1))
	if !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestParentAndDependenciesRemainIndependent(t *testing.T) {
	f := setup(t, issue(engine, 1, "Parent: #45\nTarget: #99\nRefs: #98"), issue(engine, 45, ""))
	f.acceptAll()
	out, err := f.resolve(resource(engine, 1))
	if err != nil || len(out.Nodes) != 2 || len(out.Dependencies) != 0 || len(out.Parents) != 1 {
		t.Fatal(out, err)
	}
}
func TestDeepCycleTraversal(t *testing.T) {
	nodes := map[storage.Resource]IssueNode{}
	edges := map[[2]storage.Resource]bool{}
	const count = 10000
	for n := int64(1); n <= count; n++ {
		k := resource(engine, n)
		nodes[k] = IssueNode{}
		if n < count {
			edges[[2]storage.Resource{k, resource(engine, n+1)}] = true
		}
	}
	if cyclic(nodes, edges) {
		t.Fatal("acyclic chain rejected")
	}
	edges[[2]storage.Resource{resource(engine, count), resource(engine, 1)}] = true
	if !cyclic(nodes, edges) {
		t.Fatal("long cycle missed")
	}
}
