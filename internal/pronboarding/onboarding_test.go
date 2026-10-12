package pronboarding

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	eb "github.com/parametron-io/parametron-workflow/internal/engineeringbranch"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

var auditFailure = errors.New("injected interruption")

type auditFixture struct {
	t                                        *testing.T
	path                                     string
	store                                    *storage.Store
	c                                        *Controller
	cfg                                      Config
	event                                    storage.Event
	d                                        prbinding.Decision
	o                                        observe.ObservedState
	identity                                 github.PullRequestProjectItem
	bindCalls, reads, adds, statuses, drafts int
	bindHook                                 func(int)
	readHook                                 func(int)
	itemErr, providerErr                     error
	addHook, statusHook, draftHook           func() error
}

func auditSetup(t *testing.T) *auditFixture {
	t.Helper()
	f := &auditFixture{t: t, path: filepath.Join(t.TempDir(), "onboarding.sqlite")}
	var err error
	f.store, err = storage.Open(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	repo := github.Repository{ID: "R", Owner: "parametron-io", Name: "repo"}
	actor := github.Actor{ID: "U", Login: "developer", Type: "User"}
	r := storage.Resource{Owner: repo.Owner, Repository: repo.Name, Kind: "pull_request", Number: 47, NodeID: "PR47"}
	name, err := eb.Name(storage.Resource{Owner: repo.Owner, Repository: repo.Name, Kind: "issue", Number: 46})
	if err != nil {
		t.Fatal(err)
	}
	e := prbinding.Evidence{PR: github.Identity{ID: r.NodeID, Repository: repo, Number: 47}, Target: github.Identity{ID: "I46", Repository: repo, Number: 46}, Kind: sp.Task, Branch: github.Branch{Repository: repo, Name: name, SHA: strings.Repeat("a", 40)}, Developer: actor, Policy: "active_org_member_repository_write_assignee_v1"}
	f.d = prbinding.Decision{Outcome: prbinding.Accepted, Evidence: e}
	f.o = observe.ObservedState{Resource: r, Presence: observe.Present, PullRequest: &github.PullRequest{Identity: e.PR, State: "OPEN", Body: "Target: #46", Author: &actor, HeadRepository: repo, HeadRef: name, HeadSHA: e.Branch.SHA}, Projects: []observe.ProjectState{{Profile: config.Engineering, ID: "ENG", Items: []observe.ProjectItem{{ID: "ITEM", Fields: map[config.FieldRole]observe.FieldValue{}}}}, {Profile: config.BugTracker, ID: "BUG"}}}
	f.identity = github.PullRequestProjectItem{ID: "ITEM", ProjectID: "ENG", ContentKind: "PullRequest", PullRequest: &e.PR}
	f.cfg = Config{Deployment: config.ResolvedConfig{Organization: config.Organization{ID: "ORG", Login: repo.Owner}, Repositories: []config.Repository{repo}, Engineering: config.ResolvedProject{ID: "ENG", Fields: map[config.FieldRole]string{config.Status: "STATUS"}, StatusOptions: map[config.StatusRole]string{config.InProgress: "PROGRESS", config.InReview: "REVIEW", config.Done: "DONE"}}, BugTracker: config.ResolvedProject{ID: "BUG"}}, Binding: f, Reader: f, Projects: f, Items: f, Draft: f, Store: f.store, Clock: func() time.Time { return time.Unix(1700000000, 0).UTC() }}
	f.rebuild()
	f.event = f.delivery("original")
	return f
}
func (f *auditFixture) rebuild() {
	f.t.Helper()
	var err error
	f.c, err = New(f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *auditFixture) delivery(id string) storage.Event {
	f.t.Helper()
	_, err := f.store.InsertDelivery(context.Background(), storage.Delivery{ID: id, EventName: "pull_request", Payload: []byte(`{"status":"In Progress","draft":true}`), ReceivedAt: time.Unix(1700000000, 0).UTC(), Resource: &f.o.Resource})
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := f.store.Event(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}
func (f *auditFixture) Evaluate(ctx context.Context, _ storage.Resource) (prbinding.Decision, error) {
	f.bindCalls++
	if f.bindHook != nil {
		f.bindHook(f.bindCalls)
	}
	if f.providerErr != nil {
		return prbinding.Decision{}, f.providerErr
	}
	return f.d, ctx.Err()
}
func (f *auditFixture) ReadCurrent(ctx context.Context, _ storage.Resource) (observe.ObservedState, error) {
	f.reads++
	if f.readHook != nil {
		f.readHook(f.reads)
	}
	// Each read is a snapshot, never an alias of mutable fake state.
	b, _ := json.Marshal(f.o)
	var o observe.ObservedState
	if err := json.Unmarshal(b, &o); err != nil {
		f.t.Fatal(err)
	}
	return o, ctx.Err()
}
func (f *auditFixture) CurrentPullRequestProjectItem(ctx context.Context, _ string) (github.PullRequestProjectItem, error) {
	if f.itemErr != nil {
		return github.PullRequestProjectItem{}, f.itemErr
	}
	return f.identity, ctx.Err()
}
func (f *auditFixture) AddProjectItem(ctx context.Context, p, id string) (string, error) {
	f.adds++
	if p != "ENG" || id != "PR47" {
		f.t.Fatalf("wrong membership identity: %s %s", p, id)
	}
	f.o.Projects[0].Items = []observe.ProjectItem{{ID: "ITEM", Fields: map[config.FieldRole]observe.FieldValue{}}}
	if f.addHook != nil {
		return "ITEM", f.addHook()
	}
	return "ITEM", ctx.Err()
}
func (f *auditFixture) SetProjectOption(ctx context.Context, p, item, field, option string) error {
	f.statuses++
	if p != "ENG" || item != "ITEM" || field != "STATUS" || option != "PROGRESS" {
		f.t.Fatalf("wrong status identity: %s %s %s %s", p, item, field, option)
	}
	f.setStatus(config.InProgress, "PROGRESS")
	if f.statusHook != nil {
		return f.statusHook()
	}
	return ctx.Err()
}
func (f *auditFixture) ConvertPullRequestToDraft(ctx context.Context, id string) error {
	f.drafts++
	if id != "PR47" {
		f.t.Fatal(id)
	}
	f.o.PullRequest.Draft = true
	if f.draftHook != nil {
		return f.draftHook()
	}
	return ctx.Err()
}
func (f *auditFixture) setStatus(role config.StatusRole, option string) {
	f.o.Projects[0].Items[0].Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.SingleSelect, OptionID: option}, StatusRole: role}
}
func (f *auditFixture) handle() error { return f.c.Handle(context.Background(), f.event) }
func (f *auditFixture) noWrites() {
	f.t.Helper()
	if f.adds+f.statuses+f.drafts != 0 {
		f.t.Fatalf("unauthorized writes: add=%d status=%d draft=%d", f.adds, f.statuses, f.drafts)
	}
	for _, stage := range stages {
		_, err := f.store.Provenance(context.Background(), prefix+stage+"_v1", key(f.o.Resource))
		if !errors.Is(err, storage.ErrNotFound) {
			f.t.Fatalf("unexpected %s record: %v", stage, err)
		}
	}
}
func requireError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestAcceptedOnboardingAndReviewReadiness(t *testing.T) {
	for _, initialDraft := range []bool{false, true} {
		t.Run(map[bool]string{false: "non_draft", true: "draft"}[initialDraft], func(t *testing.T) {
			f := auditSetup(t)
			f.o.PullRequest.Draft = initialDraft
			f.o.Projects[0].Items = nil
			if err := f.handle(); err != nil {
				t.Fatal(err)
			}
			wantDraft := 1
			if initialDraft {
				wantDraft = 0
			}
			if f.adds != 1 || f.statuses != 1 || f.drafts != wantDraft {
				t.Fatal(f.adds, f.statuses, f.drafts)
			}
			p, err := f.store.Provenance(context.Background(), prefix+"completed_v1", key(f.o.Resource))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.handle(); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.store, err = storage.Open(context.Background(), f.path)
			if err != nil {
				t.Fatal(err)
			}
			f.cfg.Store = f.store
			f.rebuild()
			f.o.PullRequest.Draft = false
			f.setStatus(config.InReview, "REVIEW")
			f.event = f.delivery("ready")
			if err := f.handle(); err != nil {
				t.Fatal(err)
			}
			f.o.Projects[0].Items = nil
			f.event = f.delivery("drift")
			if err := f.handle(); err != nil {
				t.Fatal(err)
			}
			if f.adds != 2 || f.statuses != 1 || f.drafts != wantDraft || f.o.PullRequest.Draft {
				t.Fatal("replayed ingress", f.adds, f.statuses, f.drafts)
			}
			later, err := f.store.Provenance(context.Background(), p.Namespace, p.Key)
			if err != nil || !reflect.DeepEqual(p, later) {
				t.Fatal("immutable completion changed", err)
			}
			f.o.Projects[0].Items = nil
			f.d = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.HeadMismatch}
			requireError(t, f.handle(), ErrRejected)
			if f.adds != 2 {
				t.Fatal("rejected drift repaired")
			}
		})
	}
}

func TestOnboardingAuthorizationFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		change func(*auditFixture)
		want   error
	}{
		{"rejected", func(f *auditFixture) {
			f.d = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.MissingProvenance}
		}, ErrRejected},
		{"rejected_partial", func(f *auditFixture) { f.d.Outcome = prbinding.Rejected; f.d.Reason = prbinding.HeadMismatch }, ErrInvalid},
		{"unknown_outcome", func(f *auditFixture) { f.d.Outcome = "maybe" }, ErrInvalid},
		{"accepted_reason", func(f *auditFixture) { f.d.Reason = prbinding.Changed }, ErrInvalid},
		{"missing_pr_node", func(f *auditFixture) { f.d.Evidence.PR.ID = "" }, ErrInvalid},
		{"missing_target_node", func(f *auditFixture) { f.d.Evidence.Target.ID = "" }, ErrInvalid},
		{"wrong_target_repository", func(f *auditFixture) { f.d.Evidence.Target.Repository.ID = "OTHER" }, ErrInvalid},
		{"wrong_pr_repository", func(f *auditFixture) { f.d.Evidence.PR.Repository.ID = "OTHER" }, ErrInvalid},
		{"unsupported_target", func(f *auditFixture) { f.d.Evidence.Kind = sp.Bug }, ErrInvalid},
		{"wrong_policy", func(f *auditFixture) { f.d.Evidence.Policy = "branch_name_only" }, ErrInvalid},
		{"wrong_branch", func(f *auditFixture) { f.d.Evidence.Branch.Name = "manual" }, ErrInvalid},
		{"partial_author", func(f *auditFixture) { f.d.Evidence.Developer.ID = "" }, ErrInvalid},
		{"stale_node", func(f *auditFixture) { f.o.PullRequest.ID = "other" }, ErrChanged},
		{"stale_author", func(f *auditFixture) { f.o.PullRequest.Author.ID = "other" }, ErrChanged},
		{"stale_head", func(f *auditFixture) { f.o.PullRequest.HeadSHA = strings.Repeat("b", 40) }, ErrChanged},
		{"closed", func(f *auditFixture) { f.o.PullRequest.State = "CLOSED" }, ErrRejected},
		{"merged", func(f *auditFixture) { f.o.PullRequest.State = "MERGED" }, ErrRejected},
		{"unsupported_state", func(f *auditFixture) { f.o.PullRequest.State = "UNKNOWN" }, ErrRejected},
		{"missing", func(f *auditFixture) { f.o.Presence = observe.Missing }, ErrChanged},
		{"provider_failure", func(f *auditFixture) { f.providerErr = &github.Error{Category: github.Transient} }, &github.Error{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := auditSetup(t)
			tc.change(f)
			err := f.handle()
			if tc.name == "provider_failure" {
				var ge *github.Error
				if !errors.As(err, &ge) || ge.Category != github.Transient {
					t.Fatal(err)
				}
			} else {
				requireError(t, err, tc.want)
			}
			f.noWrites()
		})
	}
	t.Run("cancelled", func(t *testing.T) {
		f := auditSetup(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		requireError(t, f.c.Handle(ctx, f.event), context.Canceled)
		f.noWrites()
	})
	t.Run("revalidation", func(t *testing.T) {
		f := auditSetup(t)
		f.bindHook = func(n int) {
			if n == 2 {
				f.d = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.Changed}
			}
		}
		requireError(t, f.handle(), ErrChanged)
		f.noWrites()
	})
	t.Run("non_atomic_observation", func(t *testing.T) {
		f := auditSetup(t)
		f.readHook = func(n int) {
			if n == 2 {
				f.o.PullRequest.Draft = true
			}
		}
		requireError(t, f.handle(), ErrChanged)
		f.noWrites()
	})
	t.Run("cross_repository_target", func(t *testing.T) {
		f := auditSetup(t)
		repo := github.Repository{ID: "OTHER", Owner: "parametron-io", Name: "target"}
		f.cfg.Deployment.Repositories = append(f.cfg.Deployment.Repositories, repo)
		f.d.Evidence.Target.Repository = repo
		f.d.Evidence.Branch.Repository = repo
		f.o.PullRequest.HeadRepository = repo
		f.rebuild()
		if err := f.handle(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestProjectEvidenceAndStatusOwnership(t *testing.T) {
	for _, name := range []string{"duplicate", "archived", "wrong_project", "missing_project", "duplicate_project", "wrong_content", "wrong_content_node", "missing_content", "both_content", "invalid_item", "malformed_status", "bug_membership"} {
		t.Run(name, func(t *testing.T) {
			f := auditSetup(t)
			switch name {
			case "duplicate":
				f.o.Projects[0].Items = append(f.o.Projects[0].Items, f.o.Projects[0].Items[0])
			case "archived":
				f.o.Projects[0].Items[0].Archived = true
			case "wrong_project":
				f.identity.ProjectID = "OTHER"
			case "missing_project":
				f.o.Projects = f.o.Projects[1:]
			case "duplicate_project":
				f.o.Projects = append(f.o.Projects, f.o.Projects[0])
			case "wrong_content":
				f.identity.ContentKind = "Issue"
			case "wrong_content_node":
				i := *f.identity.PullRequest
				i.ID = "OTHER"
				f.identity.PullRequest = &i
			case "missing_content":
				f.identity.PullRequest = nil
			case "both_content":
				f.identity.Issue = f.identity.PullRequest
			case "invalid_item":
				f.o.Projects[0].Items[0].ID = "bad id"
			case "malformed_status":
				f.o.Projects[0].Items[0].Fields[config.Status] = observe.FieldValue{FieldValue: github.FieldValue{Kind: config.Number}}
			case "bug_membership":
				f.o.Projects[1].Items = []observe.ProjectItem{{ID: "unrelated"}}
			}
			requireError(t, f.handle(), ErrProject)
			f.noWrites()
		})
	}
	for _, name := range []string{"missing", "correct", "disabled", "review", "terminal"} {
		t.Run("status_"+name, func(t *testing.T) {
			f := auditSetup(t)
			switch name {
			case "correct":
				f.setStatus(config.InProgress, "PROGRESS")
			case "disabled":
				f.o.PullRequest.Body += "\nSet-Status: false"
			case "review":
				f.setStatus(config.InReview, "REVIEW")
			case "terminal":
				f.setStatus(config.Done, "DONE")
			}
			err := f.handle()
			if name == "review" || name == "terminal" {
				requireError(t, err, ErrAmbiguous)
				f.noWrites()
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if name == "missing" {
				want = 1
			}
			if f.statuses != want || f.adds != 0 {
				t.Fatal(f.statuses, f.adds)
			}
		})
	}
}

type auditStore struct {
	eb.Store
	fail  string
	after bool
	hook  func(string)
}

func (s auditStore) RecordProvenance(ctx context.Context, p storage.Provenance) (bool, error) {
	stage := strings.TrimSuffix(strings.TrimPrefix(p.Namespace, prefix), "_v1")
	if stage == s.fail && !s.after {
		return false, auditFailure
	}
	ok, err := s.Store.RecordProvenance(ctx, p)
	if err != nil {
		return ok, err
	}
	if s.hook != nil {
		s.hook(stage)
	}
	if stage == s.fail {
		return ok, auditFailure
	}
	return ok, nil
}

func TestInterruptionAtEveryDurableBoundary(t *testing.T) {
	for _, stage := range stages {
		for _, after := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "_before", true: "_after"}[after], func(t *testing.T) {
				f := auditSetup(t)
				f.o.Projects[0].Items = nil
				f.cfg.Store = auditStore{Store: f.store, fail: stage, after: after}
				f.rebuild()
				requireError(t, f.handle(), ErrStorage)
				adds, statuses, drafts := f.adds, f.statuses, f.drafts
				if err := f.store.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				f.store, err = storage.Open(context.Background(), f.path)
				if err != nil {
					t.Fatal(err)
				}
				f.cfg.Store = f.store
				f.rebuild()
				f.event = f.delivery("retry")
				err = f.handle()
				ambiguous := stage == "status_done" && !after || stage == "draft_done" && !after || stage == "status_attempt" && after || stage == "draft_attempt" && after
				if ambiguous {
					requireError(t, err, ErrAmbiguous)
					if f.adds != adds || f.statuses != statuses || f.drafts != drafts {
						t.Fatal("unresolved attempt replayed")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if f.adds != 1 || f.statuses != 1 || f.drafts != 1 {
					t.Fatal("effects not converged exactly once in tested history", f.adds, f.statuses, f.drafts)
				}
			})
		}
	}
}

func TestRemotePartialFailureAndCancellation(t *testing.T) {
	for _, action := range []string{"membership", "status", "draft"} {
		for _, cancelled := range []bool{false, true} {
			t.Run(action+map[bool]string{false: "_error", true: "_cancel"}[cancelled], func(t *testing.T) {
				f := auditSetup(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				hook := func() error {
					if cancelled {
						cancel()
						return ctx.Err()
					}
					return &github.Error{Category: github.Transient}
				}
				switch action {
				case "membership":
					f.o.Projects[0].Items = nil
					f.addHook = hook
				case "status":
					f.statusHook = hook
				case "draft":
					f.draftHook = hook
				}
				err := f.c.Handle(ctx, f.event)
				if cancelled {
					requireError(t, err, context.Canceled)
				} else {
					var ge *github.Error
					if !errors.As(err, &ge) {
						t.Fatal(err)
					}
				}
				f.addHook = nil
				f.statusHook = nil
				f.draftHook = nil
				beforeStatus, beforeDraft := f.statuses, f.drafts
				err = f.handle()
				if action == "membership" {
					if err != nil || f.adds != 1 {
						t.Fatal(err, f.adds)
					}
				} else {
					requireError(t, err, ErrAmbiguous)
					if f.statuses != beforeStatus || f.drafts != beforeDraft {
						t.Fatal("blind replay")
					}
				}
			})
		}
	}
}

func TestPersistenceRequiresCanonicalBoundEvidence(t *testing.T) {
	for _, name := range []string{"version", "duplicate_key", "unknown_key", "whitespace", "invalid_json", "wrong_pr", "wrong_target", "wrong_project", "wrong_field", "wrong_option", "wrong_resource", "wrong_delivery", "completion_only", "draft_without_status"} {
		t.Run(name, func(t *testing.T) {
			f := auditSetup(t)
			f.o.PullRequest.Draft = true
			f.o.PullRequest.Body += "\nSet-Status: false"
			f.cfg.Store = auditStore{Store: f.store, fail: "prepared"}
			f.rebuild()
			requireError(t, f.handle(), ErrStorage)
			e := evidence{Version: 1, Resource: f.o.Resource, Binding: f.d.Evidence, Organization: f.cfg.Deployment.Organization, Project: "ENG", Field: "STATUS", Option: "PROGRESS", Item: "ITEM", InitiallyDraft: true}
			switch name {
			case "version":
				e.Version = 2
			case "wrong_pr":
				e.Binding.PR.ID = "other"
			case "wrong_target":
				e.Binding.Target.Repository.ID = "other"
			case "wrong_project":
				e.Project = "other"
			case "wrong_field":
				e.Field = "other"
			case "wrong_option":
				e.Option = "other"
			case "wrong_resource":
				e.Resource.Number = 48
			}
			data, _ := json.Marshal(e)
			switch name {
			case "duplicate_key":
				data = append([]byte(`{"version":1,`), data[1:]...)
			case "unknown_key":
				data = append([]byte(`{"unknown":true,`), data[1:]...)
			case "whitespace":
				data = append([]byte(" "), data...)
			case "invalid_json":
				data = []byte("{")
			}
			stage := "prepared"
			if name == "completion_only" {
				stage = "completed"
			}
			if name == "draft_without_status" {
				stage = "draft_done"
			}
			id := "original"
			if name == "wrong_delivery" {
				r := f.o.Resource
				r.Number = 48
				_, err := f.store.InsertDelivery(context.Background(), storage.Delivery{ID: "wrong", EventName: "pull_request", Payload: []byte(`{}`), ReceivedAt: time.Unix(1700000000, 0), Resource: &r})
				if err != nil {
					t.Fatal(err)
				}
				id = "wrong"
			}
			_, err := f.store.RecordProvenance(context.Background(), storage.Provenance{Namespace: prefix + stage + "_v1", Key: key(f.o.Resource), DeliveryID: id, Metadata: data, CreatedAt: time.Unix(1700000000, 0)})
			if err != nil {
				t.Fatal(err)
			}
			f.cfg.Store = f.store
			f.rebuild()
			requireError(t, f.handle(), ErrProvenance)
			if f.adds+f.statuses+f.drafts != 0 {
				t.Fatal("corruption authorized mutation")
			}
		})
	}
}

func TestPreparationAndAttemptAreNotMutationAuthority(t *testing.T) {
	for _, stage := range []string{"membership_intent", "prepared", "status_attempt", "draft_attempt"} {
		t.Run(stage, func(t *testing.T) {
			f := auditSetup(t)
			f.o.Projects[0].Items = nil
			f.cfg.Store = auditStore{Store: f.store, hook: func(s string) {
				if s == stage {
					f.d = prbinding.Decision{Outcome: prbinding.Rejected, Reason: prbinding.Changed}
				}
			}}
			f.rebuild()
			requireError(t, f.handle(), ErrChanged)
			switch stage {
			case "membership_intent":
				if f.adds != 0 {
					t.Fatal("stale add")
				}
			case "prepared", "status_attempt":
				if f.statuses != 0 {
					t.Fatal("stale status")
				}
			case "draft_attempt":
				if f.drafts != 0 {
					t.Fatal("stale draft")
				}
			}
		})
	}
}

func TestCurrentStatusDriftAfterPreparationFailsClosed(t *testing.T) {
	for _, role := range []config.StatusRole{config.InReview, config.Done} {
		t.Run(string(role), func(t *testing.T) {
			f := auditSetup(t)
			f.cfg.Store = auditStore{Store: f.store, hook: func(stage string) {
				if stage == "prepared" {
					option := "REVIEW"
					if role == config.Done {
						option = "DONE"
					}
					f.setStatus(role, option)
				}
			}}
			f.rebuild()
			requireError(t, f.handle(), ErrAmbiguous)
			if f.statuses+f.drafts != 0 {
				t.Fatal("overwrote later status")
			}
		})
	}
}

func TestFailureObservingSuccessfulRemoteEffects(t *testing.T) {
	for _, action := range []string{"membership", "status", "draft"} {
		t.Run(action, func(t *testing.T) {
			f := auditSetup(t)
			switch action {
			case "membership":
				f.o.Projects[0].Items = nil
				f.addHook = func() error { f.itemErr = auditFailure; return nil }
			case "status":
				f.statusHook = func() error { f.itemErr = auditFailure; return nil }
			case "draft":
				f.draftHook = func() error { f.providerErr = auditFailure; return nil }
			}
			requireError(t, f.handle(), auditFailure)
			f.itemErr = nil
			f.providerErr = nil
			f.addHook = nil
			f.statusHook = nil
			f.draftHook = nil
			if action == "membership" {
				if err := f.handle(); err != nil {
					t.Fatal(err)
				}
				if f.adds != 1 {
					t.Fatal("repeated membership")
				}
			} else {
				requireError(t, f.handle(), ErrAmbiguous)
				if action == "status" && f.statuses != 1 || action == "draft" && f.drafts != 1 {
					t.Fatal("repeated indeterminate action")
				}
			}
		})
	}
}

func TestConcurrentDuplicateCallsSerializeOneShotActions(t *testing.T) {
	f := auditSetup(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.handle() }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.statuses != 1 || f.drafts != 1 || f.adds != 0 {
		t.Fatal(f.statuses, f.drafts, f.adds)
	}
}

func TestOriginalDeliveryAndCanonicalEvidence(t *testing.T) {
	f := auditSetup(t)
	f.cfg.Store = auditStore{Store: f.store, fail: "completed"}
	f.rebuild()
	requireError(t, f.handle(), ErrStorage)
	f.o.PullRequest.Draft = false
	f.setStatus(config.Done, "DONE")
	f.event = f.delivery("later_notification")
	f.cfg.Store = f.store
	f.rebuild()
	if err := f.handle(); err != nil {
		t.Fatal(err)
	}
	facts, err := f.c.load(context.Background(), f.o.Resource)
	if err != nil {
		t.Fatal(err)
	}
	if facts.delivery != "original" || !facts.present["completed"] {
		t.Fatal(facts)
	}
	for _, stage := range stages {
		p, err := f.store.Provenance(context.Background(), prefix+stage+"_v1", key(f.o.Resource))
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var e evidence
		if err = json.Unmarshal(p.Metadata, &e); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(e)
		if string(b) != string(p.Metadata) || len(b) > 4096 || strings.Contains(string(b), "Target:") {
			t.Fatal("noncanonical or unbounded prose", len(b))
		}
		if p.DeliveryID != "original" || p.Key != "parametron-io/repo/pull_request/47" || e.Binding.Target.ID != "I46" || e.Project != "ENG" {
			t.Fatal(p, e)
		}
	}
	p, err := f.store.Provenance(context.Background(), prefix+"completed_v1", key(f.o.Resource))
	if err != nil {
		t.Fatal(err)
	}
	p.CreatedAt = p.CreatedAt.Add(time.Hour)
	ok, err := f.store.RecordProvenance(context.Background(), p)
	if err != nil || ok {
		t.Fatal("equivalent completion not immutable", ok, err)
	}
	p.Metadata = append(p.Metadata, ' ')
	_, err = f.store.RecordProvenance(context.Background(), p)
	requireError(t, err, storage.ErrConflict)
	if f.statuses != 1 || f.drafts != 1 || f.o.PullRequest.Draft {
		t.Fatal("failed completion replayed actions")
	}
}

type corruptReadStore struct {
	eb.Store
	edit func(*storage.Provenance)
}

func (s corruptReadStore) Provenance(ctx context.Context, ns, key string) (storage.Provenance, error) {
	p, err := s.Store.Provenance(ctx, ns, key)
	if err == nil {
		s.edit(&p)
	}
	return p, err
}

func TestCorruptProvenanceEnvelopeRejectsBeforeRepair(t *testing.T) {
	for _, field := range []string{"namespace", "key", "delivery", "timestamp"} {
		t.Run(field, func(t *testing.T) {
			f := auditSetup(t)
			if err := f.handle(); err != nil {
				t.Fatal(err)
			}
			f.o.Projects[0].Items = nil
			f.cfg.Store = corruptReadStore{Store: f.store, edit: func(p *storage.Provenance) {
				switch field {
				case "namespace":
					p.Namespace = "other"
				case "key":
					p.Key = "other/resource/pull_request/47"
				case "delivery":
					p.DeliveryID = "missing"
				case "timestamp":
					p.CreatedAt = time.Time{}
				}
			}}
			f.rebuild()
			requireError(t, f.handle(), ErrProvenance)
			if f.adds != 0 || f.statuses != 1 || f.drafts != 1 {
				t.Fatal("corrupt completion repaired membership")
			}
		})
	}
}

func TestConstructorRequiresNarrowCapabilities(t *testing.T) {
	for _, name := range []string{"binding", "reader", "projects", "items", "draft", "store", "field", "option", "project"} {
		t.Run(name, func(t *testing.T) {
			f := auditSetup(t)
			c := f.cfg
			switch name {
			case "binding":
				c.Binding = nil
			case "reader":
				c.Reader = nil
			case "projects":
				c.Projects = nil
			case "items":
				c.Items = nil
			case "draft":
				c.Draft = nil
			case "store":
				c.Store = nil
			case "field":
				c.Deployment.Engineering.Fields[config.Status] = ""
			case "option":
				c.Deployment.Engineering.StatusOptions[config.InProgress] = ""
			case "project":
				c.Deployment.Engineering.ID = c.Deployment.BugTracker.ID
			}
			_, err := New(c)
			requireError(t, err, ErrInvalid)
		})
	}
}

type missingFactStore struct {
	eb.Store
	stage string
}

func (s missingFactStore) Provenance(ctx context.Context, ns, key string) (storage.Provenance, error) {
	if ns == prefix+s.stage+"_v1" {
		return storage.Provenance{}, storage.ErrNotFound
	}
	return s.Store.Provenance(ctx, ns, key)
}

func TestCompletionRequiresPreparationAndSuccessfulStages(t *testing.T) {
	for _, missing := range []string{"prepared", "status_done", "draft_done"} {
		t.Run(missing, func(t *testing.T) {
			f := auditSetup(t)
			if err := f.handle(); err != nil {
				t.Fatal(err)
			}
			f.cfg.Store = missingFactStore{Store: f.store, stage: missing}
			f.rebuild()
			f.o.Projects[0].Items = nil
			requireError(t, f.handle(), ErrProvenance)
			if f.adds != 0 || f.statuses != 1 || f.drafts != 1 {
				t.Fatal("incomplete durable evidence permitted effects")
			}
		})
	}
}

func TestUnresolvedDraftAttemptCannotInferSuccessFromCurrentDraftFlag(t *testing.T) {
	for _, currentDraft := range []bool{true, false} {
		t.Run(map[bool]string{true: "currently_draft", false: "currently_review_ready"}[currentDraft], func(t *testing.T) {
			f := auditSetup(t)
			f.cfg.Store = auditStore{Store: f.store, fail: "draft_done"}
			f.rebuild()
			requireError(t, f.handle(), ErrStorage)
			f.o.PullRequest.Draft = currentDraft
			f.cfg.Store = f.store
			f.rebuild()
			requireError(t, f.handle(), ErrAmbiguous)
			if f.drafts != 1 {
				t.Fatal("inferred attribution from current flag")
			}
		})
	}
}

func TestCorrectInitialStateNeedsNoRemoteWrites(t *testing.T) {
	f := auditSetup(t)
	f.o.PullRequest.Draft = true
	f.setStatus(config.InProgress, "PROGRESS")
	if err := f.handle(); err != nil {
		t.Fatal(err)
	}
	if f.adds+f.statuses+f.drafts != 0 {
		t.Fatal("redundant remote write")
	}
	facts, err := f.c.load(context.Background(), f.o.Resource)
	if err != nil || !facts.present["completed"] || facts.present["status_attempt"] || facts.present["draft_attempt"] {
		t.Fatal(facts, err)
	}
}

func TestPreparationCancellationPreventsOneShotEffects(t *testing.T) {
	for _, stage := range []string{"membership_intent", "prepared", "status_attempt", "draft_attempt"} {
		t.Run(stage, func(t *testing.T) {
			f := auditSetup(t)
			f.o.Projects[0].Items = nil
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.cfg.Store = auditStore{Store: f.store, hook: func(s string) {
				if s == stage {
					cancel()
				}
			}}
			f.rebuild()
			requireError(t, f.c.Handle(ctx, f.event), context.Canceled)
			f.cfg.Store = f.store
			f.rebuild()
			err := f.handle()
			if stage == "status_attempt" || stage == "draft_attempt" {
				requireError(t, err, ErrAmbiguous)
				if stage == "status_attempt" && f.statuses != 0 || stage == "draft_attempt" && f.drafts != 0 {
					t.Fatal("cancelled intent replayed")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExactTargetIdentityMustRemainCurrent(t *testing.T) {
	f := auditSetup(t)
	f.bindHook = func(n int) {
		if n == 2 {
			f.d.Evidence.Target.ID = "RECREATED_TARGET"
		}
	}
	requireError(t, f.handle(), ErrChanged)
	f.noWrites()
}

func TestMembershipAppearingBeforeAddIsReobservedOnRetry(t *testing.T) {
	f := auditSetup(t)
	f.o.Projects[0].Items = nil
	f.cfg.Store = auditStore{Store: f.store, hook: func(stage string) {
		if stage == "membership_intent" {
			f.o.Projects[0].Items = []observe.ProjectItem{{ID: "ITEM", Fields: map[config.FieldRole]observe.FieldValue{}}}
		}
	}}
	f.rebuild()
	requireError(t, f.handle(), ErrChanged)
	if f.adds+f.statuses+f.drafts != 0 {
		t.Fatal("stale membership plan mutated")
	}
	f.cfg.Store = f.store
	f.rebuild()
	if err := f.handle(); err != nil {
		t.Fatal(err)
	}
	if f.adds != 0 || f.statuses != 1 || f.drafts != 1 {
		t.Fatal("retry duplicated correct membership", f.adds, f.statuses, f.drafts)
	}
}

func TestMalformedOwnershipAndUnconfirmedOneShotEffects(t *testing.T) {
	t.Run("ownership", func(t *testing.T) {
		f := auditSetup(t)
		f.o.PullRequest.Body += "\nSet-Status: invalid"
		requireError(t, f.handle(), ErrChanged)
		f.noWrites()
	})
	for _, action := range []string{"status", "draft"} {
		t.Run(action, func(t *testing.T) {
			f := auditSetup(t)
			if action == "status" {
				f.statusHook = func() error { f.setStatus(config.InReview, "REVIEW"); return nil }
			} else {
				f.draftHook = func() error { f.o.PullRequest.Draft = false; return nil }
			}
			requireError(t, f.handle(), ErrAmbiguous)
			f.statusHook = nil
			f.draftHook = nil
			requireError(t, f.handle(), ErrAmbiguous)
			if action == "status" && f.statuses != 1 || action == "draft" && f.drafts != 1 {
				t.Fatal("unconfirmed response replayed")
			}
		})
	}
}
