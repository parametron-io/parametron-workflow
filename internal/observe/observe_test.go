package observe

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
)

type consumerFunc func(context.Context, PolicyInput) error

func (f consumerFunc) Evaluate(c context.Context, p PolicyInput) error { return f(c, p) }
func identity(n int) github.Identity {
	return github.Identity{ID: "current-node", Number: n, Repository: deployment().Repositories[0]}
}
func currentIssue() github.Issue {
	return github.Issue{Identity: identity(12), Title: "new", Body: "current text", State: "CLOSED", Labels: []string{"z", "current", "a"}, Assignees: []github.Actor{{Login: "z"}, {Login: "a"}}, Author: &github.Actor{Login: "author"}, Type: &github.IssueType{ID: "type", Name: "unclassified"}, Parent: ptr(identity(2)), SubIssues: []github.Identity{identity(9), identity(3)}, BlockedBy: []github.Identity{identity(8), identity(4)}, Blocking: []github.Identity{identity(7), identity(5)}, LinkedPullRequests: []github.Identity{identity(20), identity(10)}}
}
func ptr[T any](v T) *T { return &v }
func currentPR() github.PullRequest {
	return github.PullRequest{Identity: identity(12), Title: "new", Body: "current text", State: "MERGED", Draft: true, HeadRef: "current-head", HeadSHA: "current-head-sha", BaseRef: "current-base", BaseSHA: "current-base-sha", Labels: []string{"z", "current", "a"}, Author: &github.Actor{Login: "author"}, ClosingIssues: []github.Identity{identity(9), identity(3)}}
}
func stale(kind string) storage.Event {
	key := kind
	if kind == "pull_request" {
		key = "pull_request"
	}
	return event(`{` + prefix + `"` + key + `":{"number":12,"node_id":"stale-node","title":"old","body":"stale text","state":"open","labels":["old"],"draft":false,"head":{"ref":"stale","sha":"stale"}}}`)
}
func bound(kind string) storage.Event {
	e := stale(kind)
	e.Resource = &storage.Resource{Owner: "org", Repository: "repo", Kind: kind, Number: 12, NodeID: "stale-node"}
	return e
}
func processor(t *testing.T, f *github.Fake, c consumerFunc) *Processor {
	t.Helper()
	p, err := NewProcessor(deployment(), f, c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func emptyProjects(context.Context, string, string, map[string]config.FieldKind) ([]github.ProjectItem, error) {
	return nil, nil
}

func TestCurrentStateHandoff(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		t.Run(kind, func(t *testing.T) {
			d := deployment()
			calls := []string{}
			var got []PolicyInput
			issue, pr := currentIssue(), currentPR()
			fake := &github.Fake{
				IssueFunc: func(_ context.Context, r github.Ref) (github.Issue, error) {
					if kind != "issue" || r != (github.Ref{Owner: "Org", Repository: "Repo", Number: 12}) {
						t.Fatalf("unexpected Issue read %+v", r)
					}
					calls = append(calls, "primary")
					return issue, nil
				},
				PullRequestFunc: func(_ context.Context, r github.Ref) (github.PullRequest, error) {
					if kind != "pull_request" || r != (github.Ref{Owner: "Org", Repository: "Repo", Number: 12}) {
						t.Fatalf("unexpected PR read %+v", r)
					}
					calls = append(calls, "primary")
					return pr, nil
				},
				ProjectItemsFunc: func(_ context.Context, content, project string, fields map[string]config.FieldKind) ([]github.ProjectItem, error) {
					if content != "current-node" {
						t.Fatalf("content=%s", content)
					}
					b := d.Engineering
					if project == d.BugTracker.ID {
						b = d.BugTracker
					} else if project != b.ID {
						t.Fatal("unconfigured Project")
					}
					want := map[string]config.FieldKind{b.Fields[config.Status]: config.SingleSelect, b.Fields[config.Priority]: config.SingleSelect, b.Fields[config.Effort]: config.SingleSelect, b.Fields[config.Estimate]: config.Number, b.Fields[config.StartDate]: config.Date}
					if project == d.BugTracker.ID {
						want[b.Fields[config.PriorityScore]] = config.Number
					} else {
						want[b.Fields[config.RoadmapOrder]] = config.Number
					}
					if !reflect.DeepEqual(fields, want) {
						t.Fatalf("fields=%v want %v", fields, want)
					}
					calls = append(calls, project)
					typ := "Issue"
					if kind == "pull_request" {
						typ = "PullRequest"
					}
					vals := map[string]github.FieldValue{b.Fields[config.Status]: {Kind: config.SingleSelect, OptionID: b.StatusOptions[config.Done]}, b.Fields[config.Priority]: {Kind: config.SingleSelect, OptionID: "unmapped-priority"}, b.Fields[config.Estimate]: {Kind: config.Number, Number: 0}, b.Fields[config.StartDate]: {Kind: config.Date, Date: "2026-10-09"}}
					if project == d.Engineering.ID {
						vals[b.Fields[config.RoadmapOrder]] = github.FieldValue{Kind: config.Number, Number: 10020}
					}
					return []github.ProjectItem{{ID: "z-item", ProjectID: project, Archived: true, Content: &github.Content{ID: content, Kind: typ}, Values: vals}, {ID: "a-item", ProjectID: project, Content: &github.Content{ID: content, Kind: typ}, Values: map[string]github.FieldValue{b.Fields[config.Status]: {Kind: config.SingleSelect, OptionID: "unmapped-status"}}}}, nil
				},
			}
			r, err := NewResolver(d)
			if err != nil {
				t.Fatal(err)
			}
			e := stale(kind)
			resolved, err := r.Resolve(context.Background(), e)
			if err != nil {
				t.Fatal(err)
			}
			e.Resource = &resolved
			p := processor(t, fake, func(_ context.Context, in PolicyInput) error { got = append(got, in); return nil })
			if err := p.Process(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			// Reverse every unordered source collection before repeating.
			reverse(issue.Labels)
			reverse(issue.Assignees)
			reverse(issue.SubIssues)
			reverse(issue.BlockedBy)
			reverse(issue.Blocking)
			reverse(issue.LinkedPullRequests)
			reverse(pr.Labels)
			reverse(pr.ClosingIssues)
			if err := p.Process(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got[0], got[1]) {
				t.Fatalf("unstable normalization: %+v vs %+v", got[0], got[1])
			}
			in := got[0]
			o := in.Observed
			if in.DeliveryID != e.Delivery.ID || in.Sequence != 7 || o.Presence != Present || o.Resource.NodeID != "current-node" {
				t.Fatalf("envelope=%+v", in)
			}
			if kind == "issue" {
				if o.PullRequest != nil || o.Issue == nil {
					t.Fatal("wrong kind")
				}
				v := o.Issue
				if v.Title != "new" || v.Body != "current text" || v.State != "CLOSED" || v.Parent.Number != 2 || v.Type.Name != "unclassified" || v.Author.Login != "author" {
					t.Fatalf("Issue=%+v", v)
				}
				for _, links := range [][]github.Identity{v.SubIssues, v.BlockedBy, v.Blocking, v.LinkedPullRequests} {
					if len(links) != 2 || links[0].Number >= links[1].Number {
						t.Fatal("unordered relationships")
					}
				}
				if v.Assignees[0].Login != "a" {
					t.Fatal("unordered assignees")
				}
			} else {
				if o.Issue != nil || o.PullRequest == nil {
					t.Fatal("wrong kind")
				}
				v := o.PullRequest
				if v.Title != "new" || v.Body != "current text" || v.State != "MERGED" || !v.Draft || v.HeadRef != "current-head" || v.HeadSHA != "current-head-sha" || v.BaseRef != "current-base" || v.BaseSHA != "current-base-sha" || v.ClosingIssues[0].Number != 3 {
					t.Fatalf("PR=%+v", v)
				}
			}
			for _, project := range o.Projects {
				if len(project.Items) != 2 || project.Items[0].ID != "a-item" || project.Items[1].ID != "z-item" || !project.Items[1].Archived {
					t.Fatal("item normalization")
				}
				fields := project.Items[1].Fields
				if fields[config.Status].StatusRole != config.Done || fields[config.Priority].OptionID != "unmapped-priority" || fields[config.Priority].StatusRole != "" {
					t.Fatal("role mapping")
				}
				if v, ok := fields[config.Estimate]; !ok || v.Number != 0 {
					t.Fatal("zero lost")
				}
				if project.Profile == config.Engineering {
					if v, ok := fields[config.RoadmapOrder]; !ok || v.Kind != config.Number || v.Number != 10020 {
						t.Fatal("Roadmap Order lost")
					}
				}
				if _, ok := fields[config.Effort]; ok {
					t.Fatal("unset became present")
				}
				if project.Items[0].Fields[config.Status].StatusRole != "" || project.Items[0].Fields[config.Status].OptionID != "unmapped-status" {
					t.Fatal("unmapped status lost")
				}
			}
			if !reflect.DeepEqual(calls, []string{"primary", "engineering", "bugs", "primary", "engineering", "bugs"}) {
				t.Fatal(calls)
			}
			data, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{"stale", "old", "payload", "signature", "token"} {
				if strings.Contains(string(data), bad) {
					t.Fatalf("historical/transport data leaked: %s", data)
				}
			}
		})
	}
}
func reverse[T any](v []T) {
	for i, j := 0, len(v)-1; i < j; i, j = i+1, j-1 {
		v[i], v[j] = v[j], v[i]
	}
}

func TestMissingAndErrors(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		for _, cat := range []github.Category{github.NotFound, github.Transient, github.RateLimited, github.Unauthorized, github.Forbidden, github.Malformed, github.Permanent, github.Conflict} {
			t.Run(kind+"/"+string(cat), func(t *testing.T) {
				failure := &github.Error{Category: cat}
				var inputs []PolicyInput
				f := &github.Fake{IssueFunc: func(context.Context, github.Ref) (github.Issue, error) { return github.Issue{}, failure }, PullRequestFunc: func(context.Context, github.Ref) (github.PullRequest, error) { return github.PullRequest{}, failure }, ProjectItemsFunc: func(context.Context, string, string, map[string]config.FieldKind) ([]github.ProjectItem, error) {
					t.Fatal("Project read after missing/failed primary")
					return nil, nil
				}}
				p := processor(t, f, func(_ context.Context, in PolicyInput) error { inputs = append(inputs, in); return nil })
				for i := 0; i < 2; i++ {
					err := p.Process(context.Background(), bound(kind))
					if cat == github.NotFound {
						if err != nil {
							t.Fatal(err)
						}
					} else if err != failure {
						t.Fatalf("error changed: %v", err)
					}
				}
				if cat == github.NotFound {
					if len(inputs) != 2 || !reflect.DeepEqual(inputs[0], inputs[1]) {
						t.Fatal("missing unstable")
					}
					o := inputs[0].Observed
					if o.Presence != Missing || o.Issue != nil || o.PullRequest != nil || o.Projects != nil || o.Resource.NodeID != "" {
						t.Fatalf("stale missing observation %+v", o)
					}
				} else if len(inputs) != 0 {
					t.Fatal("handoff on failure")
				}
			})
		}
	}
}
func TestProjectErrorsDiscardObservation(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		for _, project := range []string{"engineering", "bugs"} {
			for _, cat := range []github.Category{github.NotFound, github.Transient, github.RateLimited, github.Forbidden, github.Unauthorized, github.Malformed, github.Permanent} {
				t.Run(kind+"/"+project+"/"+string(cat), func(t *testing.T) {
					failure := &github.Error{Category: cat}
					f := &github.Fake{IssueFunc: func(context.Context, github.Ref) (github.Issue, error) { return currentIssue(), nil }, PullRequestFunc: func(context.Context, github.Ref) (github.PullRequest, error) { return currentPR(), nil }, ProjectItemsFunc: func(_ context.Context, _, p string, _ map[string]config.FieldKind) ([]github.ProjectItem, error) {
						if p == project {
							return nil, failure
						}
						return nil, nil
					}}
					p := processor(t, f, func(context.Context, PolicyInput) error { t.Fatal("partial observation emitted"); return nil })
					if err := p.Process(context.Background(), bound(kind)); err != failure {
						t.Fatalf("error changed: %v", err)
					}
				})
			}
		}
	}
}
func TestCurrentIdentityVerification(t *testing.T) {
	cases := []struct {
		name  string
		alter func(*github.Identity)
	}{{"owner", func(i *github.Identity) { i.Repository.Owner = "other" }}, {"repo", func(i *github.Identity) { i.Repository.Name = "other" }}, {"number", func(i *github.Identity) { i.Number = 13 }}, {"repository ID", func(i *github.Identity) { i.Repository.ID = "wrong" }}, {"missing node", func(i *github.Identity) { i.ID = "" }}}
	for _, kind := range []string{"issue", "pull_request"} {
		for _, c := range cases {
			t.Run(kind+"/"+c.name, func(t *testing.T) {
				i, pr := currentIssue(), currentPR()
				c.alter(&i.Identity)
				c.alter(&pr.Identity)
				f := &github.Fake{IssueFunc: func(context.Context, github.Ref) (github.Issue, error) { return i, nil }, PullRequestFunc: func(context.Context, github.Ref) (github.PullRequest, error) { return pr, nil }}
				p := processor(t, f, func(context.Context, PolicyInput) error { t.Fatal("mismatch emitted"); return nil })
				if err := p.Process(context.Background(), bound(kind)); !errors.Is(err, ErrObservation) {
					t.Fatal(err)
				}
			})
		}
	}
}
func TestBindingAndConsumerErrors(t *testing.T) {
	f := &github.Fake{}
	p := processor(t, f, func(context.Context, PolicyInput) error { return nil })
	for _, r := range []*storage.Resource{nil, {Owner: "org", Repository: "repo", Kind: "issue", Number: 0}, {Owner: "org", Repository: "repo", Kind: "branch", Number: 12}} {
		e := event(`{}`)
		e.Resource = r
		if err := p.Process(context.Background(), e); !errors.Is(err, ErrBinding) {
			t.Fatal(err)
		}
	}
	e := bound("issue")
	e.Resource.Repository = "other"
	if err := p.Process(context.Background(), e); !errors.Is(err, ErrRepository) {
		t.Fatal(err)
	}
	sentinel := errors.New("consumer failure")
	f.IssueFunc = func(context.Context, github.Ref) (github.Issue, error) { return currentIssue(), nil }
	f.ProjectItemsFunc = emptyProjects
	p = processor(t, f, func(_ context.Context, in PolicyInput) error {
		if len(in.Observed.Projects) != 2 || len(in.Observed.Projects[0].Items) != 0 || len(in.Observed.Projects[1].Items) != 0 {
			t.Fatal("absence not preserved")
		}
		return sentinel
	})
	if err := p.Process(context.Background(), bound("issue")); err != sentinel {
		t.Fatal(err)
	}
}

func TestWorkerIntegration(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := stale("issue")
	e.Delivery.ReceivedAt = time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	if _, err := s.InsertDelivery(ctx, e.Delivery); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(deployment())
	if err != nil {
		t.Fatal(err)
	}
	var got []PolicyInput
	f := &github.Fake{IssueFunc: func(_ context.Context, ref github.Ref) (github.Issue, error) {
		if ref != (github.Ref{Owner: "Org", Repository: "Repo", Number: 12}) {
			t.Fatal(ref)
		}
		return currentIssue(), nil
	}, ProjectItemsFunc: emptyProjects}
	p := processor(t, f, func(_ context.Context, in PolicyInput) error { got = append(got, in); return nil })
	w, err := worker.New(worker.Config{Store: s, Resolver: r, Processor: p, Concurrency: 1, Classifier: func(error) worker.Classification { return worker.Classification{Category: "observation_invalid"} }, RetrySchedule: func(_ worker.Classification, _ int64, now time.Time) time.Time { return now.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Step(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Event(ctx, e.Delivery.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State.Status != storage.Completed || stored.State.Attempts != 1 || stored.Resource.Kind != "issue" || stored.Resource.NodeID != "" {
		t.Fatalf("event=%+v", stored)
	}
	if len(got) != 1 || got[0].Sequence != stored.Sequence || got[0].DeliveryID != e.Delivery.ID || got[0].Observed.Issue.Title != "new" || got[0].Observed.Issue.State != "CLOSED" || got[0].Observed.Issue.Body != "current text" {
		t.Fatalf("handoff=%+v", got)
	}
	if err := w.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatal("completed event repeated")
	}
}

func TestProcessorRejectsProjectIdentity(t *testing.T) {
	for _, kind := range []string{"issue", "pull_request"} {
		for _, mismatch := range []string{"project", "content", "kind"} {
			t.Run(kind+"/"+mismatch, func(t *testing.T) {
				f := &github.Fake{IssueFunc: func(context.Context, github.Ref) (github.Issue, error) { return currentIssue(), nil }, PullRequestFunc: func(context.Context, github.Ref) (github.PullRequest, error) { return currentPR(), nil }, ProjectItemsFunc: func(_ context.Context, c, p string, _ map[string]config.FieldKind) ([]github.ProjectItem, error) {
					typ := "Issue"
					if kind == "pull_request" {
						typ = "PullRequest"
					}
					item := github.ProjectItem{ID: "item", ProjectID: p, Content: &github.Content{ID: c, Kind: typ}}
					switch mismatch {
					case "project":
						item.ProjectID = "other"
					case "content":
						item.Content.ID = "other"
					case "kind":
						item.Content.Kind = "DraftIssue"
					}
					return []github.ProjectItem{item}, nil
				}}
				p := processor(t, f, func(context.Context, PolicyInput) error { t.Error("inconsistent Project emitted"); return nil })
				if err := p.Process(context.Background(), bound(kind)); !errors.Is(err, ErrObservation) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestConfigurationAndObservationOwnership(t *testing.T) {
	d := deployment()
	original := currentIssue()
	var got PolicyInput
	f := &github.Fake{IssueFunc: func(context.Context, github.Ref) (github.Issue, error) { return original, nil }, ProjectItemsFunc: func(_ context.Context, _, p string, fields map[string]config.FieldKind) ([]github.ProjectItem, error) {
		if p != "engineering" && p != "bugs" {
			t.Fatal("configuration mutated")
		}
		if _, ok := fields[p+"-status"]; !ok {
			t.Fatal("configuration fields mutated")
		}
		return nil, nil
	}}
	p, err := NewProcessor(d, f, consumerFunc(func(_ context.Context, in PolicyInput) error { got = in; return nil }))
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(d)
	if err != nil {
		t.Fatal(err)
	}
	d.Repositories[0].Name = "Changed"
	d.Engineering.Fields[config.Status] = "changed"
	d.Engineering.StatusOptions[config.Done] = "changed"
	e := stale("issue")
	resource, err := r.Resolve(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	e.Resource = &resource
	if err := p.Process(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got.Observed.Issue.Parent.Number = 100
	got.Observed.Issue.Author.Login = "changed"
	got.Observed.Issue.Type.Name = "changed"
	got.Observed.Issue.Labels[0] = "changed"
	got.Observed.Issue.Assignees[0].Login = "changed"
	got.Observed.Issue.SubIssues[0].Number = 100
	if original.Parent.Number != 2 || original.Author.Login != "author" || original.Type.Name != "unclassified" || original.Labels[0] != "z" || original.Assignees[1].Login != "a" || original.SubIssues[1].Number != 3 {
		t.Fatal("client-owned state aliased")
	}
	if _, err := NewProcessor(deployment(), (*github.Fake)(nil), consumerFunc(func(context.Context, PolicyInput) error { return nil })); !errors.Is(err, ErrConfiguration) {
		t.Fatal(err)
	}
	if _, err := NewProcessor(deployment(), f, consumerFunc(nil)); !errors.Is(err, ErrConfiguration) {
		t.Fatal(err)
	}
}
