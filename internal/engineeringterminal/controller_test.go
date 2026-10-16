package engineeringterminal

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type terminalFixture struct {
	t                       *testing.T
	repo                    github.Repository
	project                 config.ResolvedProject
	issues                  map[int64]github.Issue
	statuses                map[int64]config.StatusRole
	resolver                *ec.Resolver
	c                       *Controller
	closes, reopens, writes int
	canonical               config.StatusRole
	canonicalHook           func()
	writeFailure            error
	missing, mismatch       bool
	store                   *storage.Store
	mergeGrants             map[string]bool
}

func terminalSetup(t *testing.T, typ, state, directive string) *terminalFixture {
	t.Helper()
	f := &terminalFixture{t: t, repo: github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}, issues: map[int64]github.Issue{}, statuses: map[int64]config.StatusRole{}, canonical: config.Blocked}
	f.project = config.ResolvedProject{ID: "ENG", Fields: map[config.FieldRole]string{config.Status: "STATUS"}, StatusOptions: map[config.StatusRole]string{}}
	for _, s := range []config.StatusRole{config.Backlog, config.Blocked, config.Ready, config.InProgress, config.InReview, config.Done} {
		f.project.StatusOptions[s] = string(s)
	}
	f.add(50, typ, state, directive)
	f.mergeGrants = map[string]bool{}
	var openErr error
	f.store, openErr = storage.Open(context.Background(), filepath.Join(t.TempDir(), "terminal.sqlite"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	t.Cleanup(func() { f.store.Close() })
	f.statuses[50] = config.Done
	d := config.ResolvedConfig{Organization: config.Organization{ID: "ORG", Login: f.repo.Owner}, Repositories: []config.Repository{f.repo}, IssueTypes: map[string]string{"Phase": "type-Phase", "Task": "type-Task", "Feature": "type-Feature", "Bug": "type-Bug"}}
	var err error
	f.resolver, err = ec.New(ec.Config{Deployment: d, Primary: f, Lister: f, Accepted: f})
	if err != nil {
		t.Fatal(err)
	}
	f.rebuild()
	return f
}
func (f *terminalFixture) rebuild() {
	var err error
	f.c, err = New(Config{Project: f.project, BugTracker: "BUG", Snapshots: f.resolver, Reader: f, Canonical: f, Issues: f, Items: f, Fields: f, Timeline: f, Store: f.store, Merges: terminalMergeFunc(func(ctx context.Context, r storage.Resource, facts github.IssueClosure) (bool, error) {
		return f.mergeGrants[r.NodeID] && facts.Issue.ID == r.NodeID && facts.State == "CLOSED", ctx.Err()
	})})
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *terminalFixture) resource(n int64) storage.Resource {
	return storage.Resource{Owner: f.repo.Owner, Repository: f.repo.Name, Kind: "issue", Number: n, NodeID: fmt.Sprintf("I%d", n)}
}
func (f *terminalFixture) IssueClosure(ctx context.Context, i github.Identity) (github.IssueClosure, error) {
	current, ok := f.issues[int64(i.Number)]
	if !ok || current.Identity != i {
		return github.IssueClosure{}, github.ErrClosureIncomplete
	}
	at := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	facts := github.IssueClosure{Issue: i, State: current.State, Order: github.ClosureCursorOrder, CreatedAt: at.Add(-time.Hour), UpdatedAt: at, Complete: true}
	if current.State == "CLOSED" {
		facts.Items = []github.ClosureItem{{Issue: i, ID: "CLOSE-" + i.ID, Kind: "ClosedEvent", Cursor: "CURSOR-" + i.ID, CreatedAt: at, Actor: &github.Actor{ID: "OPERATOR", Login: "operator", Type: "User"}}}
	}
	return facts, ctx.Err()
}
func (f *terminalFixture) add(n int64, typ, state, body string) {
	f.issues[n] = github.Issue{Identity: github.Identity{ID: f.resource(n).NodeID, Repository: f.repo, Number: int(n)}, State: state, Body: "Classification: false\n" + body, Type: &github.IssueType{ID: "type-" + typ, Name: typ}}
	f.statuses[n] = config.Ready
}
func (f *terminalFixture) ReadPrimary(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	i, ok := f.issues[r.Number]
	if !ok {
		return observe.ObservedState{Resource: r, Presence: observe.Missing}, ctx.Err()
	}
	return observe.ObservedState{Resource: f.resource(r.Number), Presence: observe.Present, Issue: &i}, ctx.Err()
}
func (f *terminalFixture) ReadCurrent(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	o, err := f.ReadPrimary(ctx, r)
	s := f.statuses[r.Number]
	items := []observe.ProjectItem{{ID: fmt.Sprintf("ITEM%d", r.Number), Fields: map[config.FieldRole]observe.FieldValue{config.Status: {FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: f.project.StatusOptions[s]}, StatusRole: s}}}}
	if f.missing {
		items = nil
	}
	o.Projects = []observe.ProjectState{{Profile: config.Engineering, ID: "ENG", Items: items}, {Profile: config.BugTracker, ID: "BUG"}}
	return o, err
}
func (f *terminalFixture) ListIssues(ctx context.Context, r github.Repository) ([]github.ListedIssue, error) {
	var out []github.ListedIssue
	for _, i := range f.issues {
		out = append(out, github.ListedIssue{Identity: i.Identity, State: i.State, Body: i.Body, Type: i.Type})
	}
	return out, ctx.Err()
}
func (f *terminalFixture) AcceptedIssue(context.Context, storage.Resource) (sp.AcceptedIssue, bool, error) {
	return sp.AcceptedIssue{}, false, nil
}
func (f *terminalFixture) CanonicalStatus(ctx context.Context, g ec.Context, r storage.Resource) (config.StatusRole, error) {
	for _, n := range g.Nodes {
		if n.Resource == r && n.State != ec.Open {
			f.t.Fatal("canonical received terminal node")
		}
	}
	if f.canonicalHook != nil {
		f.canonicalHook()
	}
	return f.canonical, ctx.Err()
}
func (f *terminalFixture) CurrentProjectItem(ctx context.Context, id string) (github.CurrentProjectItem, error) {
	var n int64
	if _, err := fmt.Sscanf(id, "ITEM%d", &n); err != nil {
		f.t.Fatal(err)
	}
	i := f.issues[n].Identity
	if f.mismatch {
		i.ID = "OTHER"
	}
	return github.CurrentProjectItem{ID: id, ProjectID: "ENG", ContentKind: "Issue", Issue: &i}, ctx.Err()
}
func (f *terminalFixture) CloseIssue(context.Context, github.Identity) (github.IssueStateReceipt, error) {
	f.closes++
	f.t.Error("Project Done authorized native close")
	return github.IssueStateReceipt{}, nil
}
func (f *terminalFixture) ReopenIssue(ctx context.Context, id github.Identity) (github.IssueStateReceipt, error) {
	if id != f.issues[50].Identity {
		f.t.Fatal("unrelated reopen", id)
	}
	f.reopens++
	i := f.issues[50]
	i.State = "OPEN"
	f.issues[50] = i
	return github.IssueStateReceipt{Issue: id, State: "OPEN"}, ctx.Err()
}
func (f *terminalFixture) SetProjectOption(ctx context.Context, p, id, field, option string) error {
	if p != "ENG" || field != "STATUS" {
		f.t.Fatal(p, id, field)
	}
	if f.writeFailure != nil {
		return f.writeFailure
	}
	var n int64
	fmt.Sscanf(id, "ITEM%d", &n)
	f.statuses[n] = config.StatusRole(option)
	f.writes++
	return ctx.Err()
}

func TestNativeOpenDoneNeverClosesIssue(t *testing.T) {
	for _, typ := range []string{"Task", "Feature", "Phase"} {
		for _, directive := range []string{"", "Set-Status: true", "Set-Status: false"} {
			t.Run(typ+"/"+directive, func(t *testing.T) {
				f := terminalSetup(t, typ, "OPEN", directive)
				for attempt := 0; attempt < 3; attempt++ {
					f.rebuild()
					r, err := f.c.Synchronize(context.Background(), f.resource(50))
					wantRejections := 0
					if attempt == 0 {
						wantRejections = 1
					}
					if err != nil || len(r) != wantRejections || (len(r) > 0 && r[0].Reason != UnauthorizedDone) || f.issues[50].State != "OPEN" || f.closes != 0 || f.reopens != 0 || f.statuses[50] != config.Blocked {
						t.Fatal(r, err, f.statuses, f.closes)
					}
				}
				if f.writes != 1 {
					t.Fatal("duplicate drift write", f.writes)
				}
			})
		}
	}
}
func TestTerminalConsistencyAndPhaseRejection(t *testing.T) {
	for _, typ := range []string{"Task", "Feature", "Phase"} {
		for _, openChild := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/child-open-%v", typ, openChild), func(t *testing.T) {
				f := terminalSetup(t, typ, "CLOSED", "Set-Status: false")
				f.statuses[50] = config.Ready
				if typ == "Phase" {
					s := "CLOSED"
					if openChild {
						s = "OPEN"
					}
					f.add(51, "Task", s, "Parent: #50")
					// Closed child authority is independent of this Phase gate test.
					f.mergeGrants[f.resource(51).NodeID] = !openChild
				}
				r, err := f.c.Synchronize(context.Background(), f.resource(50))
				if err != nil {
					t.Fatal(err)
				}
				if typ == "Phase" && openChild {
					if f.reopens != 1 || f.statuses[50] != config.Blocked || len(r) != 1 || r[0].Reason != OpenChildren {
						t.Fatal(r, f.reopens, f.statuses)
					}
				} else if typ != "Phase" {
					if f.reopens != 1 || f.statuses[50] != config.Blocked || len(r) != 1 || r[0].Reason != UnauthorizedClosure {
						t.Fatal(r, f.statuses, f.reopens)
					}
				} else if f.reopens != 0 || f.statuses[50] != config.Done || f.issues[50].State != "CLOSED" {
					t.Fatal(r, f.statuses)
				}
				if f.closes != 0 {
					t.Fatal("automatic close")
				}
				_, err = f.c.Synchronize(context.Background(), f.resource(50))
				if err != nil || f.reopens > 1 {
					t.Fatal(err, f.reopens)
				}
			})
		}
	}
}
func TestPhaseMembershipChangeBeforeMutationFailsClosed(t *testing.T) {
	f := terminalSetup(t, "Phase", "CLOSED", "")
	f.add(51, "Task", "OPEN", "Parent: #50")
	f.canonicalHook = func() { i := f.issues[51]; i.Body = "Classification: false"; f.issues[51] = i }
	_, err := f.c.Synchronize(context.Background(), f.resource(50))
	if !errors.Is(err, ec.ErrChanged) || f.reopens != 0 || f.writes != 0 {
		t.Fatal(err, f.reopens, f.writes)
	}
}
func TestTerminalRecoveryAfterPartialProjectWrite(t *testing.T) {
	f := terminalSetup(t, "Phase", "CLOSED", "")
	f.add(51, "Task", "OPEN", "Parent: #50")
	f.writeFailure = errors.New("write failed")
	r, err := f.c.Synchronize(context.Background(), f.resource(50))
	if err == nil || len(r) != 1 || f.reopens != 1 || f.statuses[50] != config.Done {
		t.Fatal(r, err)
	}
	f.writeFailure = nil
	f.rebuild()
	_, err = f.c.Synchronize(context.Background(), f.resource(50))
	if err != nil || f.statuses[50] != config.Blocked || f.reopens != 1 || f.closes != 0 {
		t.Fatal(err, f.statuses, f.reopens)
	}
}
func TestTerminalMembershipAndOrdinaryOwnership(t *testing.T) {
	for _, mode := range []string{"missing", "mismatch", "ordinary", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := terminalSetup(t, "Task", "OPEN", "Set-Status: false")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "missing":
				f.missing = true
			case "mismatch":
				f.mismatch = true
			case "ordinary":
				f.statuses[50] = config.Ready
			case "cancelled":
				cancel()
			}
			_, err := f.c.Synchronize(ctx, f.resource(50))
			if mode == "mismatch" && !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if f.writes != 0 || f.closes != 0 || f.reopens != 0 {
				t.Fatal("unauthorized write", f.writes)
			}
		})
	}
}

func TestPhaseExplicitEmptyCloseAndReopenedChild(t *testing.T) {
	f := terminalSetup(t, "Phase", "CLOSED", "Set-Status: false")
	f.statuses[50] = config.Ready
	if rejected, err := f.c.Synchronize(context.Background(), f.resource(50)); err != nil || len(rejected) != 0 || f.statuses[50] != config.Done || f.reopens != 0 || f.closes != 0 {
		t.Fatal("explicit empty Phase close rejected", rejected, err)
	}
	f.add(51, "Task", "OPEN", "Parent: #50")
	if rejected, err := f.c.Synchronize(context.Background(), f.resource(51)); err != nil || len(rejected) != 1 || rejected[0].Reason != OpenChildren || f.issues[50].State != "OPEN" || f.statuses[50] != config.Blocked || f.reopens != 1 || f.closes != 0 {
		t.Fatal("new open direct child did not reopen Phase", rejected, err)
	}
	if f.issues[51].State != "OPEN" {
		t.Fatal("child was closed")
	}
}
