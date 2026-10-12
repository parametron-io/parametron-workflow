package prbinding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	eo "github.com/parametron-io/parametron-workflow/internal/engineeringorder"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type acceptedReader struct {
	values map[storage.Resource]sp.AcceptedIssue
}

func (a *acceptedReader) AcceptedIssue(_ context.Context, r storage.Resource) (sp.AcceptedIssue, bool, error) {
	r.NodeID = ""
	v, ok := a.values[r]
	return v, ok, nil
}

type bindingFixture struct {
	t                *testing.T
	deployment       config.ResolvedConfig
	issues           []github.Issue
	pr               github.PullRequest
	target           storage.Resource
	completion       eb.Completion
	branch           github.Branch
	user             github.Actor
	membership       github.Membership
	permission       github.Permission
	calls            []string
	counts           map[string]int
	hook             func(string, int)
	failure          map[string]error
	missing          map[string]bool
	reverse          bool
	accepted         acceptedReader
	resolver         *ec.Resolver
	completionReader CompletionReader
}

func issueResource(i github.Issue) storage.Resource {
	return storage.Resource{Owner: i.Repository.Owner, Repository: i.Repository.Name, Kind: "issue", Number: int64(i.Number), NodeID: i.ID}
}
func newBindingFixture(t *testing.T) *bindingFixture {
	t.Helper()
	f := &bindingFixture{t: t, counts: map[string]int{}, failure: map[string]error{}, missing: map[string]bool{}, accepted: acceptedReader{values: map[storage.Resource]sp.AcceptedIssue{}}}
	f.deployment = config.ResolvedConfig{Organization: auditOrg, Repositories: []config.Repository{auditRepo}, IssueTypes: map[string]string{"Task": "T", "Feature": "F", "Phase": "P", "Bug": "B"}}
	f.addIssue(auditRepo, 46, sp.Task, "")
	f.target = issueResource(f.issues[0])
	f.pr = github.PullRequest{Identity: github.Identity{ID: "PR", Repository: auditRepo, Number: 9}, Body: "Target: #46", Title: "Implement controller", State: "OPEN", Draft: true, HeadRepository: auditRepo, HeadRef: "development/issue-46", HeadSHA: auditSHA, BaseRef: "main", BaseSHA: strings.Repeat("b", 40), Author: &github.Actor{ID: auditUser.ID, Login: auditUser.Login, Type: auditUser.Type}, Labels: []string{"workflow"}, ClosingIssues: []github.Identity{f.issues[0].Identity}}
	work := f.target
	work.NodeID = ""
	f.completion = eb.Completion{Completed: true, CreatedAt: time.Unix(1700000000, 0).UTC(), Evidence: eb.Evidence{Version: 1, WorkItem: work, Repository: auditRepo, Branch: f.pr.HeadRef, BaseRef: "main", BaseSHA: auditSHA}}
	f.branch = github.Branch{Repository: auditRepo, Name: f.pr.HeadRef, SHA: auditSHA}
	f.user = auditUser
	f.membership = github.Membership{Organization: auditOrg, User: auditUser, State: "active"}
	f.permission = github.Permission{Repository: auditRepo, User: auditUser, Base: "write"}
	f.resetResolver()
	return f
}
func (f *bindingFixture) addIssue(repo github.Repository, n int, typ sp.IssueType, body string) {
	f.issues = append(f.issues, github.Issue{Identity: github.Identity{ID: fmt.Sprintf("I-%s-%d", repo.Name, n), Repository: repo, Number: n}, Title: "Current work", Body: "Classification: false\n" + body, State: "OPEN", Type: &github.IssueType{ID: f.deployment.IssueTypes[string(typ)], Name: string(typ)}})
}
func (f *bindingFixture) read(key string) error {
	f.calls = append(f.calls, key)
	f.counts[key]++
	if f.hook != nil {
		f.hook(key, f.counts[key])
	}
	return f.failure[key]
}
func (f *bindingFixture) ReadPrimary(ctx context.Context, r storage.Resource) (observe.ObservedState, error) {
	key := "target"
	if r.Kind == "pull_request" {
		key = "pr"
	}
	if err := f.read(key); err != nil {
		return observe.ObservedState{}, err
	}
	if err := ctx.Err(); err != nil {
		return observe.ObservedState{}, err
	}
	if f.missing[key] {
		return observe.ObservedState{Resource: r, Presence: observe.Missing}, nil
	}
	if r.Kind == "pull_request" {
		p := f.pr
		p.Author = nil
		if f.pr.Author != nil {
			a := *f.pr.Author
			p.Author = &a
		}
		r.NodeID = p.ID
		return observe.ObservedState{Resource: r, Presence: observe.Present, PullRequest: &p}, nil
	}
	for _, i := range f.issues {
		if int64(i.Number) == r.Number && i.Repository.Name == r.Repository {
			r.NodeID = i.ID
			i.Assignees = slices.Clone(i.Assignees)
			return observe.ObservedState{Resource: r, Presence: observe.Present, Issue: &i}, nil
		}
	}
	return observe.ObservedState{Resource: r, Presence: observe.Missing}, nil
}
func (f *bindingFixture) resetResolver() {
	lister := &github.ListerFake{ListIssuesFunc: func(ctx context.Context, repo github.Repository) ([]github.ListedIssue, error) {
		if err := f.read("list"); err != nil {
			return nil, err
		}
		out := []github.ListedIssue{}
		for _, i := range f.issues {
			if i.Repository == repo {
				out = append(out, github.ListedIssue{Identity: i.Identity, Title: i.Title, Body: i.Body, State: i.State, Type: i.Type})
			}
		}
		if f.reverse {
			slices.Reverse(out)
		}
		return out, ctx.Err()
	}}
	var err error
	f.resolver, err = ec.New(ec.Config{Deployment: f.deployment, Primary: f, Lister: lister, Accepted: &f.accepted})
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *bindingFixture) ResolveSnapshot(ctx context.Context) (ec.Snapshot, error) {
	if err := f.read("snapshot"); err != nil {
		return ec.Snapshot{}, err
	}
	return f.resolver.ResolveSnapshot(ctx)
}
func (f *bindingFixture) Completed(ctx context.Context, r storage.Resource) (eb.Completion, error) {
	if err := f.read("completion"); err != nil {
		return eb.Completion{}, err
	}
	if f.completionReader != nil {
		return f.completionReader.Completed(ctx, r)
	}
	return f.completion, nil
}
func (f *bindingFixture) Branch(_ context.Context, r github.Repository, name string) (github.Branch, error) {
	if r != f.completion.Evidence.Repository || name != f.completion.Evidence.Branch {
		f.t.Fatalf("wrong branch lookup %v %s", r, name)
	}
	if err := f.read("branch"); err != nil {
		return github.Branch{}, err
	}
	return f.branch, nil
}
func (f *bindingFixture) User(_ context.Context, login string) (github.Actor, error) {
	if !strings.EqualFold(login, auditUser.Login) {
		f.t.Fatal("wrong developer lookup", login)
	}
	if err := f.read("user"); err != nil {
		return github.Actor{}, err
	}
	return f.user, nil
}
func (f *bindingFixture) OrganizationMembership(_ context.Context, org config.Organization, login string) (github.Membership, error) {
	if org != auditOrg || login != auditUser.Login {
		f.t.Fatal("wrong membership lookup")
	}
	if err := f.read("membership"); err != nil {
		return github.Membership{}, err
	}
	return f.membership, nil
}
func (f *bindingFixture) RepositoryPermission(_ context.Context, repo github.Repository, login string) (github.Permission, error) {
	if repo != f.permission.Repository || login != auditUser.Login {
		f.t.Fatal("wrong permission lookup", repo, login)
	}
	if err := f.read("permission"); err != nil {
		return github.Permission{}, err
	}
	return f.permission, nil
}
func (f *bindingFixture) config() Config {
	return Config{Deployment: f.deployment, Primary: f, Context: f, Completion: f, Branches: f, Developers: f}
}
func (f *bindingFixture) evaluate() (Decision, error) {
	v, err := New(f.config())
	if err != nil {
		f.t.Fatal(err)
	}
	return v.Evaluate(context.Background(), storage.Resource{Owner: auditRepo.Owner, Repository: auditRepo.Name, Kind: "pull_request", Number: 9})
}
func requireAccepted(t *testing.T, f *bindingFixture, d Decision, err error) {
	t.Helper()
	if err != nil || d.Outcome != Accepted || d.Reason != "" || d.Evidence.PR != f.pr.Identity || d.Evidence.Target != f.issues[0].Identity || d.Evidence.Branch != f.branch || d.Evidence.Developer != f.user || d.Evidence.Policy != "active_org_member_repository_write_assignee_v1" {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
}

func TestBindingTargetAndEngineeringAuthority(t *testing.T) {
	cases := []struct {
		name string
		edit func(*bindingFixture)
		want Reason
	}{
		{"parentless Task", func(*bindingFixture) {}, ""},
		{"Feature", func(f *bindingFixture) { f.issues[0].Type = &github.IssueType{ID: "F", Name: "Feature"} }, ""},
		{"explicit direct Phase", func(f *bindingFixture) {
			f.issues[0].Type = &github.IssueType{ID: "P", Name: "Phase"}
			f.issues[0].Body += "Create-Branch: true"
		}, ""},
		{"Phase default", func(f *bindingFixture) { f.issues[0].Type = &github.IssueType{ID: "P", Name: "Phase"} }, IneligibleTarget},
		{"Bug unsupported", func(f *bindingFixture) { f.issues[0].Type = &github.IssueType{ID: "B", Name: "Bug"} }, UnsupportedTarget},
		{"closed", func(f *bindingFixture) { f.issues[0].State = "CLOSED" }, IneligibleTarget},
		{"disabled creation", func(f *bindingFixture) { f.issues[0].Body += "Create-Branch: false" }, IneligibleTarget},
		{"missing Target despite metadata", func(f *bindingFixture) { f.pr.Body = ""; f.pr.Title = "Target: #46" }, MissingTarget},
		{"conflicting Target", func(f *bindingFixture) { f.pr.Body = "Target: #46\nTarget: #47" }, InvalidTarget},
		{"malformed Target", func(f *bindingFixture) { f.pr.Body = "Target: https://github.com/parametron-io/engine/issues/46" }, InvalidTarget},
		{"noncanonical Target", func(f *bindingFixture) { f.pr.Body = "Target: #046" }, InvalidTarget},
		{"qualified local noncanonical", func(f *bindingFixture) { f.pr.Body = "Target: parametron-io/engine#46" }, InvalidTarget},
		{"unconfigured Target", func(f *bindingFixture) { f.pr.Body = "Target: parametron-io/outside#46" }, OutsideDeployment},
		{"missing Target issue", func(f *bindingFixture) { f.pr.Body = "Target: #999" }, UnresolvedTarget},
		{"active blocker despite observed relationships", func(f *bindingFixture) { f.issues[0].Body += "Blocked-By: #47"; f.addIssue(auditRepo, 47, sp.Task, "") }, IneligibleTarget},
		{"closed blocker", func(f *bindingFixture) {
			f.issues[0].Body += "Blocked-By: #47"
			f.addIssue(auditRepo, 47, sp.Task, "")
			f.issues[1].State = "CLOSED"
		}, ""},
		{"active parent", func(f *bindingFixture) { f.issues[0].Body += "Parent: #45"; f.addIssue(auditRepo, 45, sp.Phase, "") }, ""},
		{"closed parent", func(f *bindingFixture) {
			f.issues[0].Body += "Parent: #45"
			f.addIssue(auditRepo, 45, sp.Phase, "")
			f.issues[1].State = "CLOSED"
		}, IneligibleTarget},
		{"competing Phase", func(f *bindingFixture) {
			f.issues[0].Body += "Parent: #45"
			f.addIssue(auditRepo, 45, sp.Phase, "")
			f.addIssue(auditRepo, 44, sp.Phase, "")
		}, IneligibleTarget},
		{"accepted Type overrides native Bug", func(f *bindingFixture) {
			f.issues[0].Type = &github.IssueType{ID: "B", Name: "Bug"}
			r := f.target
			r.NodeID = ""
			f.accepted.values[r] = sp.AcceptedIssue{Classification: sp.IssueClassification{Type: sp.Task, Labels: []sp.Label{}, Priority: sp.Medium, Effort: sp.M}}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBindingFixture(t)
			tc.edit(f)
			d, err := f.evaluate()
			if tc.want == "" {
				requireAccepted(t, f, d, err)
			} else {
				requireRejected(t, d, err, tc.want)
			}
		})
	}
}

func TestBindingProvenanceHeadAndDeveloperBoundaries(t *testing.T) {
	cases := []struct {
		name string
		edit func(*bindingFixture)
		want Reason
	}{
		{"manual lookalike", func(f *bindingFixture) { f.completion = eb.Completion{} }, MissingProvenance},
		{"creation not completed", func(f *bindingFixture) { f.completion.Completed = false }, MissingProvenance},
		{"wrong work item", func(f *bindingFixture) { f.completion.Evidence.WorkItem.Number++ }, InvalidProvenance},
		{"wrong provenance repo", func(f *bindingFixture) { f.completion.Evidence.Repository.ID = "other" }, InvalidProvenance},
		{"wrong provenance branch", func(f *bindingFixture) { f.completion.Evidence.Branch = "development/issue-47" }, InvalidProvenance},
		{"missing creation time", func(f *bindingFixture) { f.completion.CreatedAt = time.Time{} }, InvalidProvenance},
		{"fork same ref", func(f *bindingFixture) { f.pr.HeadRepository.Owner = "fork"; f.pr.HeadRepository.ID = "fork" }, HeadMismatch},
		{"other repository same ref", func(f *bindingFixture) { f.pr.HeadRepository.Name = "other" }, HeadMismatch},
		{"head node mismatch", func(f *bindingFixture) { f.pr.HeadRepository.ID = "other" }, HeadMismatch},
		{"wrong ref", func(f *bindingFixture) { f.pr.HeadRef = "development/issue-47" }, HeadMismatch},
		{"wrong SHA", func(f *bindingFixture) { f.pr.HeadSHA = strings.Repeat("a", 40) }, HeadMismatch},
		{"missing repository", func(f *bindingFixture) { f.pr.HeadRepository = github.Repository{} }, MissingHead},
		{"missing ref", func(f *bindingFixture) { f.pr.HeadRef = "" }, MissingHead},
		{"missing SHA", func(f *bindingFixture) { f.pr.HeadSHA = "" }, MissingHead},
		{"malformed current branch", func(f *bindingFixture) { f.branch.SHA = "invalid" }, HeadMismatch},
		{"unrelated current branch", func(f *bindingFixture) { f.branch.Name = "other" }, HeadMismatch},
		{"current branch repository mismatch", func(f *bindingFixture) { f.branch.Repository.ID = "other" }, HeadMismatch},
		{"deleted branch", func(f *bindingFixture) {
			f.failure["branch"] = &github.MissingBranch{Repository: auditRepo, Name: f.branch.Name}
		}, MissingHead},
		{"missing author", func(f *bindingFixture) { f.pr.Author = nil }, InvalidDeveloper},
		{"bot author", func(f *bindingFixture) { f.pr.Author.Type = "Bot" }, InvalidDeveloper},
		{"unverifiable author", func(f *bindingFixture) { f.pr.Author.ID = "" }, InvalidDeveloper},
		{"reused login", func(f *bindingFixture) { f.user.ID = "other" }, InvalidDeveloper},
		{"pending member", func(f *bindingFixture) { f.membership.State = "pending" }, InactiveMembership},
		{"read permission", func(f *bindingFixture) { f.permission.Base = "read" }, InsufficientPermission},
		{"manual other assignee", func(f *bindingFixture) {
			f.issues[0].Assignees = []github.Actor{{ID: "other", Login: "other", Type: "User"}}
		}, AssigneeMismatch},
		{"manual matching assignee", func(f *bindingFixture) { f.issues[0].Assignees = []github.Actor{auditUser} }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBindingFixture(t)
			tc.edit(f)
			d, err := f.evaluate()
			if tc.want == "" {
				requireAccepted(t, f, d, err)
			} else {
				requireRejected(t, d, err, tc.want)
			}
		})
	}
}

func TestBindingFreshnessBarrier(t *testing.T) {
	cases := []struct {
		name, key string
		at        int
		edit      func(*bindingFixture)
		want      Reason
		wantErr   error
	}{
		{"PR body", "pr", 2, func(f *bindingFixture) { f.pr.Body = "Target: #47" }, Changed, nil},
		{"PR identity", "pr", 2, func(f *bindingFixture) { f.pr.ID = "other" }, "", ErrInvalid},
		{"PR author", "pr", 2, func(f *bindingFixture) { f.pr.Author.ID = "other" }, Changed, nil},
		{"PR repository", "pr", 2, func(f *bindingFixture) { f.pr.HeadRepository.ID = "other" }, Changed, nil},
		{"PR ref", "pr", 2, func(f *bindingFixture) { f.pr.HeadRef = "other" }, Changed, nil},
		{"PR SHA", "pr", 2, func(f *bindingFixture) { f.pr.HeadSHA = strings.Repeat("a", 40) }, Changed, nil},
		{"Target body", "snapshot", 2, func(f *bindingFixture) { f.issues[0].Body += "\nCreate-Branch: false" }, Changed, nil},
		{"Target identity", "snapshot", 2, func(f *bindingFixture) { f.issues[0].ID = "other" }, Changed, nil},
		{"Target Type", "snapshot", 2, func(f *bindingFixture) { f.issues[0].Type = &github.IssueType{ID: "F", Name: "Feature"} }, Changed, nil},
		{"Target state", "snapshot", 2, func(f *bindingFixture) { f.issues[0].State = "CLOSED" }, Changed, nil},
		{"Target body after final Snapshot", "target", 4, func(f *bindingFixture) { f.issues[0].Body += "\nCreate-Branch: false" }, Changed, nil},
		{"Target Type after final Snapshot", "target", 4, func(f *bindingFixture) { f.issues[0].Type = &github.IssueType{ID: "F", Name: "Feature"} }, Changed, nil},
		{"Target node after final Snapshot", "target", 4, func(f *bindingFixture) { f.issues[0].ID = "other" }, "", ErrInvalid},
		{"Target state after final Snapshot", "target", 4, func(f *bindingFixture) { f.issues[0].State = "CLOSED" }, Changed, nil},
		{"Target assignees", "target", 4, func(f *bindingFixture) {
			f.issues[0].Assignees = []github.Actor{{ID: "other", Login: "other", Type: "User"}}
		}, Changed, nil},
		{"Snapshot new competitor", "snapshot", 2, func(f *bindingFixture) { f.addIssue(auditRepo, 44, sp.Phase, "") }, Changed, nil},
		{"completion changed", "completion", 2, func(f *bindingFixture) { f.completion.CreatedAt = f.completion.CreatedAt.Add(time.Second) }, Changed, nil},
		{"completion corrupted", "completion", 2, func(f *bindingFixture) { f.failure["completion"] = eb.ErrProvenance }, InvalidProvenance, nil},
		{"branch SHA", "branch", 2, func(f *bindingFixture) { f.branch.SHA = strings.Repeat("a", 40) }, Changed, nil},
		{"branch deletion", "branch", 2, func(f *bindingFixture) {
			f.failure["branch"] = &github.MissingBranch{Repository: auditRepo, Name: f.branch.Name}
		}, Changed, nil},
		{"user identity", "user", 2, func(f *bindingFixture) { f.user.ID = "other" }, Changed, nil},
		{"membership", "membership", 2, func(f *bindingFixture) { f.membership.State = "pending" }, Changed, nil},
		{"permission", "permission", 2, func(f *bindingFixture) { f.permission.Base = "read" }, Changed, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBindingFixture(t)
			f.hook = func(key string, n int) {
				if key == tc.key && n == tc.at {
					tc.edit(f)
				}
			}
			d, err := f.evaluate()
			if tc.wantErr != nil {
				requireError(t, d, err, tc.wantErr)
			} else {
				requireRejected(t, d, err, tc.want)
			}
		})
	}
}

func TestBindingErrorsAndCancellation(t *testing.T) {
	for _, key := range []string{"pr", "snapshot", "target", "completion", "branch", "user", "membership", "permission"} {
		for _, at := range []int{1, 2} {
			for _, cause := range []error{context.Canceled, context.DeadlineExceeded, &github.Error{Category: github.Transient}, &github.Error{Category: github.RateLimited}, &github.Error{Category: github.Unauthorized}, &github.Error{Category: github.Forbidden}, &github.Error{Category: github.NotFound}, &github.Error{Category: github.Malformed}} {
				t.Run(fmt.Sprintf("%s/%d/%v", key, at, cause), func(t *testing.T) {
					f := newBindingFixture(t)
					f.hook = func(k string, n int) {
						if k == key && n == at {
							f.failure[k] = cause
						}
					}
					d, err := f.evaluate()
					requireError(t, d, err, cause)
				})
			}
		}
	}
	for _, cause := range []error{eb.ErrStorage, ec.ErrIncomplete, ec.ErrParentCycle, ec.ErrDependencyCycle} {
		f := newBindingFixture(t)
		key := "snapshot"
		if cause == eb.ErrStorage {
			key = "completion"
		}
		f.failure[key] = cause
		d, err := f.evaluate()
		requireError(t, d, err, cause)
	}
	for _, tc := range []struct {
		cause  error
		reason Reason
	}{{ec.ErrMissing, UnresolvedTarget}, {ec.ErrChanged, Changed}, {ec.ErrDeployment, OutsideDeployment}, {ec.ErrInvalid, UnresolvedTarget}, {eb.ErrProvenance, InvalidProvenance}} {
		f := newBindingFixture(t)
		key := "snapshot"
		if tc.cause == eb.ErrProvenance {
			key = "completion"
		}
		f.failure[key] = tc.cause
		d, err := f.evaluate()
		requireRejected(t, d, err, tc.reason)
	}
	f := newBindingFixture(t)
	v, err := New(f.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err := v.Evaluate(ctx, storage.Resource{})
	requireError(t, d, err, context.Canceled)
	if len(f.calls) != 0 {
		t.Fatal("read after cancellation")
	}
}

func TestRepeatedBindingUsesCurrentAuthority(t *testing.T) {
	f := newBindingFixture(t)
	f.addIssue(auditRepo, 47, sp.Task, "")
	f.issues[0].Assignees = []github.Actor{auditUser, {ID: "other", Login: "other", Type: "User"}}
	v, err := New(f.config())
	if err != nil {
		t.Fatal(err)
	}
	r := storage.Resource{Owner: auditRepo.Owner, Repository: auditRepo.Name, Kind: "pull_request", Number: 9}
	before, _ := json.Marshal(f.issues)
	first, err := v.Evaluate(context.Background(), r)
	requireAccepted(t, f, first, err)
	after, _ := json.Marshal(f.issues)
	if string(before) != string(after) {
		t.Fatal("evaluation mutated Issue facts")
	}
	for i := 0; i < 3; i++ {
		f.reverse = !f.reverse
		slices.Reverse(f.issues[0].Assignees)
		d, err := v.Evaluate(context.Background(), r)
		if err != nil || d != first {
			t.Fatal(d, err)
		}
	}
	slices.SortFunc(f.issues[0].Assignees, func(a, b github.Actor) int { return strings.Compare(a.ID, b.ID) })
	original := []github.Actor{auditUser, {ID: "other", Login: "other", Type: "User"}}
	slices.SortFunc(original, func(a, b github.Actor) int { return strings.Compare(a.ID, b.ID) })
	if !reflect.DeepEqual(f.issues[0].Assignees, original) {
		t.Fatal("assignees mutated")
	}
	f.membership.State = "pending"
	d, err := v.Evaluate(context.Background(), r)
	requireRejected(t, d, err, InactiveMembership)
}

// The only write-capable fixture is used to seed legitimate historical #37 facts.
// During Evaluate its write method fails, and the completion controller's branch
// capability is an empty fake: Completed must need neither remote nor local writes.
type readOnlyBranchStore struct {
	store *storage.Store
	t     *testing.T
	edit  func(string, storage.Provenance) (storage.Provenance, error)
}

func (s *readOnlyBranchStore) Provenance(ctx context.Context, ns, key string) (storage.Provenance, error) {
	p, err := s.store.Provenance(ctx, ns, key)
	if err == nil && s.edit != nil {
		return s.edit(ns, p)
	}
	return p, err
}
func (s *readOnlyBranchStore) Event(ctx context.Context, id string) (storage.Event, error) {
	return s.store.Event(ctx, id)
}
func (s *readOnlyBranchStore) RecordProvenance(context.Context, storage.Provenance) (bool, error) {
	s.t.Fatal("binding attempted provenance write")
	return false, errors.New("unexpected write")
}
func (f *bindingFixture) seedProvenance(cross bool) *readOnlyBranchStore {
	f.t.Helper()
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(f.t.TempDir(), "binding.sqlite"))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { s.Close() })
	origin := f.target
	if cross {
		f.addIssue(auditRepo, 47, sp.Task, "")
		origin = issueResource(f.issues[len(f.issues)-1])
	}
	_, err = s.InsertDelivery(ctx, storage.Delivery{ID: "origin", EventName: "issues", Payload: []byte(`{}`), ReceivedAt: f.completion.CreatedAt.Add(-time.Minute), Resource: &origin})
	if err != nil {
		f.t.Fatal(err)
	}
	remote := false
	branches := github.BranchFake{DefaultBranchFunc: func(context.Context, github.Repository) (github.Branch, error) {
		return github.Branch{Repository: f.branch.Repository, Name: "main", SHA: auditSHA}, nil
	}, BranchFunc: func(_ context.Context, r github.Repository, name string) (github.Branch, error) {
		if remote {
			return f.branch, nil
		}
		return github.Branch{}, &github.MissingBranch{Repository: r, Name: name}
	}, CreateBranchFunc: func(context.Context, github.Repository, string, string) (github.Branch, error) {
		remote = true
		return f.branch, nil
	}}
	c, err := eb.New(eb.Config{Repositories: f.deployment.Repositories, Store: s, Branches: branches, Clock: func() time.Time { return f.completion.CreatedAt }})
	if err != nil {
		f.t.Fatal(err)
	}
	snapshot, err := f.resolver.ResolveSnapshot(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	graph, err := snapshot.Context(f.target)
	if err != nil {
		f.t.Fatal(err)
	}
	order, err := eo.BuildRoadmapOrder(graph)
	if err != nil {
		f.t.Fatal(err)
	}
	result, err := c.EnsureFrom(ctx, eb.Origin{DeliveryID: "origin", Resource: origin}, ep.Input{Context: graph, PhaseOrder: order.PhaseOrder})
	if err != nil || !result.Completed {
		f.t.Fatal(result, err)
	}
	guard := &readOnlyBranchStore{store: s, t: f.t}
	reader, err := eb.New(eb.Config{Repositories: f.deployment.Repositories, Store: guard, Branches: github.BranchFake{}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.completionReader = reader
	f.calls = nil
	f.counts = map[string]int{}
	return guard
}
func TestDurableCompletionReadOnlyIntegration(t *testing.T) {
	for _, cross := range []bool{false, true} {
		for _, kind := range []sp.IssueType{sp.Task, sp.Feature, sp.Phase} {
			t.Run(fmt.Sprintf("originCross=%v/%s", cross, kind), func(t *testing.T) {
				f := newBindingFixture(t)
				f.issues[0].Type = &github.IssueType{ID: f.deployment.IssueTypes[string(kind)], Name: string(kind)}
				if kind == sp.Phase {
					f.issues[0].Body += "Create-Branch: true"
				}
				guard := f.seedProvenance(cross)
				key := "parametron-io/engine/issue/46"
				before, err := guard.store.Provenance(context.Background(), "engineering_branch_creation_v1", key)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					d, err := f.evaluate()
					requireAccepted(t, f, d, err)
					if d.Evidence.Kind != kind {
						t.Fatal(d)
					}
				}
				after, err := guard.store.Provenance(context.Background(), before.Namespace, key)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("durable facts changed", err)
				}
			})
		}
	}
}
func TestCrossRepositoryBinding(t *testing.T) {
	f := newBindingFixture(t)
	other := github.Repository{ID: "R2", Owner: auditRepo.Owner, Name: "adapter"}
	f.deployment.Repositories = append(f.deployment.Repositories, other)
	f.issues[0].Repository = other
	f.issues[0].ID = "I-adapter-46"
	f.issues[0].Body += "Parent: parametron-io/engine#45\nBlocked-By: parametron-io/engine#48"
	f.addIssue(auditRepo, 45, sp.Phase, "")
	f.addIssue(auditRepo, 48, sp.Task, "Parent: #45")
	f.issues[2].State = "CLOSED"
	f.target = issueResource(f.issues[0])
	f.pr.Body = "Target: parametron-io/adapter#46"
	f.pr.HeadRepository = other
	f.branch.Repository = other
	f.completion.Evidence.Repository = other
	f.completion.Evidence.WorkItem = f.target
	f.completion.Evidence.WorkItem.NodeID = ""
	f.permission.Repository = other
	f.resetResolver()
	f.seedProvenance(true)
	d, err := f.evaluate()
	requireAccepted(t, f, d, err)
}
func TestCorruptDurableCompletionCannotBind(t *testing.T) {
	for _, cross := range []bool{false, true} {
		for _, bad := range []string{"no records", "authorization only", "completion only", "corrupt", "conflicting pair", "wrong repository", "wrong work item", "wrong branch", "wrong base SHA", "missing timestamp", "invalid origin"} {
			t.Run(fmt.Sprintf("cross=%v/%s", cross, bad), func(t *testing.T) {
				f := newBindingFixture(t)
				guard := f.seedProvenance(cross)
				guard.edit = func(ns string, p storage.Provenance) (storage.Provenance, error) {
					if bad == "no records" || bad == "authorization only" && ns == "engineering_branch_creation_v1" || bad == "completion only" && ns == "engineering_branch_authorization_v1" {
						return storage.Provenance{}, storage.ErrNotFound
					}
					switch bad {
					case "corrupt":
						p.Metadata = []byte(`{"unknown":true}`)
					case "missing timestamp":
						p.CreatedAt = time.Time{}
					case "invalid origin":
						p.DeliveryID = "unbound"
					default:
						// Alter public Evidence fields in the persisted envelope without reproducing
						// the provenance decoder. The production Completed reader validates it.
						var payload map[string]json.RawMessage
						if err := json.Unmarshal(p.Metadata, &payload); err != nil {
							t.Fatal(err)
						}
						raw := p.Metadata
						if cross {
							raw = payload["branch"]
						}
						var e eb.Evidence
						if err := json.Unmarshal(raw, &e); err != nil {
							t.Fatal(err)
						}
						switch bad {
						case "conflicting pair":
							if ns == "engineering_branch_creation_v1" {
								e.BaseSHA = strings.Repeat("a", 40)
							}
						case "wrong repository":
							e.Repository.ID = "other"
						case "wrong work item":
							e.WorkItem.Number++
						case "wrong branch":
							e.Branch = "development/issue-999"
						case "wrong base SHA":
							e.BaseSHA = "invalid"
						}
						encoded, _ := json.Marshal(e)
						if cross {
							p.Metadata = []byte(strings.Replace(string(p.Metadata), string(raw), string(encoded), 1))
						} else {
							p.Metadata = encoded
						}
					}
					return p, nil
				}
				d, err := f.evaluate()
				reason := InvalidProvenance
				if bad == "no records" || bad == "authorization only" {
					reason = MissingProvenance
				}
				requireRejected(t, d, err, reason)
			})
		}
	}
}

func TestMalformedDeveloperEvidenceCannotAuthorize(t *testing.T) {
	f := newBindingFixture(t)
	f.pr.Author.ID = "U invalid"
	f.user.ID = "U invalid"
	f.membership.User.ID = "U invalid"
	f.permission.User.ID = "U invalid"
	d, err := f.evaluate()
	requireRejected(t, d, err, InvalidDeveloper)
}

func TestIncompleteAndInvalidSnapshotAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*bindingFixture)
		reason Reason
		cause  error
	}{
		{"classification enabled without acceptance", func(f *bindingFixture) { f.issues[0].Body = "" }, "", ec.ErrIncomplete},
		{"native Type missing", func(f *bindingFixture) { f.issues[0].Type = nil }, UnresolvedTarget, nil},
		{"native Type node mismatch", func(f *bindingFixture) { f.issues[0].Type = &github.IssueType{ID: "other", Name: "Task"} }, UnresolvedTarget, nil},
		{"unsupported native Type", func(f *bindingFixture) { f.issues[0].Type = &github.IssueType{ID: "other", Name: "Unknown"} }, UnresolvedTarget, nil},
		{"Target repository node mismatch", func(f *bindingFixture) { f.issues[0].Repository.ID = "other" }, UnresolvedTarget, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBindingFixture(t)
			tc.edit(f)
			d, err := f.evaluate()
			if tc.cause != nil {
				requireError(t, d, err, tc.cause)
			} else {
				requireRejected(t, d, err, tc.reason)
			}
		})
	}
}

type noPolicyConsumer struct{ t *testing.T }

func (c noPolicyConsumer) Evaluate(context.Context, observe.PolicyInput) error {
	c.t.Fatal("binding called observation policy consumer")
	return nil
}
func TestBindingThroughObservationNormalization(t *testing.T) {
	f := newBindingFixture(t)
	project := func(id string, bug bool) config.ResolvedProject {
		p := config.ResolvedProject{ID: id, Fields: map[config.FieldRole]string{}, StatusOptions: map[config.StatusRole]string{}}
		fields := []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate}
		statuses := []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done}
		if bug {
			fields = append(fields, config.PriorityScore)
			statuses = append(statuses, config.ToTriage)
		} else {
			fields = append(fields, config.RoadmapOrder)
			statuses = append(statuses, config.Blocked)
		}
		for _, r := range fields {
			p.Fields[r] = id + string(r)
		}
		for _, r := range statuses {
			p.StatusOptions[r] = id + string(r)
		}
		return p
	}
	f.deployment.Engineering = project("E", false)
	f.deployment.BugTracker = project("B", true)
	client := &github.Fake{IssueFunc: func(_ context.Context, r github.Ref) (github.Issue, error) {
		if r.Number != 46 || r.Owner != auditRepo.Owner || r.Repository != auditRepo.Name {
			t.Fatal(r)
		}
		return f.issues[0], nil
	}, PullRequestFunc: func(_ context.Context, r github.Ref) (github.PullRequest, error) {
		if r.Number != 9 {
			t.Fatal(r)
		}
		return f.pr, nil
	}, ProjectItemsFunc: func(context.Context, string, string, map[string]config.FieldKind) ([]github.ProjectItem, error) {
		t.Fatal("binding queried Project state")
		return nil, nil
	}}
	primary, err := observe.NewProcessor(f.deployment, client, noPolicyConsumer{t})
	if err != nil {
		t.Fatal(err)
	}
	lister := &github.ListerFake{ListIssuesFunc: func(context.Context, github.Repository) ([]github.ListedIssue, error) {
		i := f.issues[0]
		return []github.ListedIssue{{Identity: i.Identity, Title: i.Title, Body: i.Body, State: i.State, Type: i.Type}}, nil
	}}
	f.resolver, err = ec.New(ec.Config{Deployment: f.deployment, Primary: primary, Lister: lister, Accepted: &f.accepted})
	if err != nil {
		t.Fatal(err)
	}
	f.seedProvenance(false)
	cfg := f.config()
	cfg.Primary = primary
	v, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := storage.Resource{Owner: auditRepo.Owner, Repository: auditRepo.Name, Kind: "pull_request", Number: 9}
	for i := 0; i < 2; i++ {
		d, err := v.Evaluate(context.Background(), r)
		requireAccepted(t, f, d, err)
	}
}

func TestCompletionStorageFailureIsOperational(t *testing.T) {
	for _, cause := range []error{errors.New("database unavailable"), context.Canceled} {
		f := newBindingFixture(t)
		guard := f.seedProvenance(true)
		guard.edit = func(string, storage.Provenance) (storage.Provenance, error) { return storage.Provenance{}, cause }
		d, err := f.evaluate()
		want := eb.ErrStorage
		if errors.Is(cause, context.Canceled) {
			want = context.Canceled
		}
		requireError(t, d, err, want)
	}
}

// Exact-ref reads expose repository/name/SHA, not a ref incarnation. Recreating
// the same ref at the same SHA supplies identical observable evidence. Historical
// #37 completion carries no stronger ref lineage, so this is an API evidence
// limitation; this test deliberately does not invent a new lineage policy.
func TestSameRefSameSHARecreationHasIdenticalObservableEvidence(t *testing.T) {
	f := newBindingFixture(t)
	f.seedProvenance(false)
	first, err := f.evaluate()
	requireAccepted(t, f, first, err)
	original := f.branch
	f.branch = github.Branch{}
	f.branch = original
	again, err := f.evaluate()
	if err != nil || again != first {
		t.Fatal(again, err)
	}
}

func TestMissingCurrentResourcesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		key  string
		at   int
		want Reason
	}{{"pr", 1, MissingPR}, {"pr", 2, Changed}, {"target", 1, Changed}, {"target", 2, UnresolvedTarget}, {"target", 4, Changed}} {
		t.Run(fmt.Sprintf("%s/%d", tc.key, tc.at), func(t *testing.T) {
			f := newBindingFixture(t)
			f.hook = func(key string, n int) {
				if key == tc.key && n == tc.at {
					f.missing[key] = true
				}
			}
			d, err := f.evaluate()
			requireRejected(t, d, err, tc.want)
		})
	}
}
