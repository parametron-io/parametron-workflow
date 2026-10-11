package engineeringbranch

import (
	"context"
	"fmt"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	ep "github.com/parametron-io/parametron-workflow/internal/engineeringpolicy"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/observe"
	"github.com/parametron-io/parametron-workflow/internal/semanticflow"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

type observationSink struct{}

func (observationSink) Evaluate(context.Context, observe.PolicyInput) error { return nil }
func integrationDeployment() config.ResolvedConfig {
	project := func(id string, bug bool) config.ResolvedProject {
		p := config.ResolvedProject{ID: id, Fields: map[config.FieldRole]string{}, StatusOptions: map[config.StatusRole]string{}}
		for _, r := range []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate} {
			p.Fields[r] = id + string(r)
		}
		for _, r := range []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done} {
			p.StatusOptions[r] = id + string(r)
		}
		if bug {
			p.Fields[config.PriorityScore] = id + "score"
			p.StatusOptions[config.ToTriage] = id + "triage"
		} else {
			p.Fields[config.RoadmapOrder] = id + "order"
			p.StatusOptions[config.Blocked] = id + "blocked"
		}
		return p
	}
	return config.ResolvedConfig{Organization: config.Organization{ID: "O", Login: repo.Owner}, Repositories: []config.Repository{repo}, Engineering: project("E", false), BugTracker: project("B", true), IssueTypes: map[string]string{"Phase": "phase", "Task": "task", "Feature": "feature", "Bug": "bug"}}
}

// This fixture contains one complete known Phase work set. It does not claim
// Resolve(root) constructs a complete production activation context; #39 owns
// repository-wide assembly. PhaseOrder here is an explicit caller-owned input.
func TestCurrentFactsPolicyCreationIntegration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     sp.IssueType
		body    string
		blocked bool
		want    bool
	}{
		{"Task", sp.Task, "Parent: #45", false, true},
		{"Feature", sp.Feature, "Parent: #45", false, true},
		{"explicit Phase", sp.Phase, "Create-Branch: true", false, true},
		{"normal Phase", sp.Phase, "", false, false},
		{"blocked child observed Ready", sp.Task, "Parent: #45\nBlocked-By: #38\nSet-Status: false", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			d := integrationDeployment()
			issues := map[int]github.Issue{}
			makeIssue := func(n int, typ sp.IssueType, body string) github.Issue {
				return github.Issue{Identity: github.Identity{ID: fmt.Sprintf("I%d", n), Repository: repo, Number: n}, Title: "fresh title", Body: "Classification: false\n" + body, State: "OPEN", Type: &github.IssueType{ID: d.IssueTypes[string(typ)], Name: string(typ)}}
			}
			issues[37] = makeIssue(37, tc.typ, tc.body)
			phase := resource(37)
			if tc.typ != sp.Phase {
				issues[45] = makeIssue(45, sp.Phase, "")
				phase = resource(45)
			}
			if tc.blocked {
				issues[38] = makeIssue(38, sp.Task, "Parent: #45")
			}
			reads := 0
			client := github.Fake{IssueFunc: func(ctx context.Context, r github.Ref) (github.Issue, error) { reads++; return issues[r.Number], nil }, ProjectItemsFunc: func(ctx context.Context, id, project string, fields map[string]config.FieldKind) ([]github.ProjectItem, error) {
				// All observed work is Ready. This is deliberately irrelevant to branch authority.
				binding := d.Engineering
				if project == d.BugTracker.ID {
					binding = d.BugTracker
				}
				return []github.ProjectItem{{ID: project + "item", ProjectID: project, Content: &github.Content{ID: id, Kind: "Issue"}, Values: map[string]github.FieldValue{binding.Fields[config.Status]: {Kind: config.SingleSelect, OptionID: binding.StatusOptions[config.Ready]}}}}, nil
			}}
			primary, err := observe.NewProcessor(d, &client, observationSink{})
			if err != nil {
				t.Fatal(err)
			}
			loader, err := semanticflow.NewAcceptedLoader(f.store)
			if err != nil {
				t.Fatal(err)
			}
			lister := github.ListerFake{ListIssuesFunc: func(context.Context, github.Repository) ([]github.ListedIssue, error) {
				out := []github.ListedIssue{}
				for _, i := range issues {
					out = append(out, github.ListedIssue{Identity: i.Identity, Title: i.Title, Body: i.Body, State: i.State})
				}
				return out, nil
			}}
			resolver, err := ec.New(ec.Config{Deployment: d, Primary: primary, Lister: &lister, Accepted: loader})
			if err != nil {
				t.Fatal(err)
			}
			observed, err := primary.ReadCurrent(context.Background(), resource(37))
			if err != nil {
				t.Fatal(err)
			}
			if observed.Projects[0].Items[0].Fields[config.Status].StatusRole != config.Ready {
				t.Fatal(observed)
			}
			current, err := resolver.Resolve(context.Background(), resource(37))
			if err != nil {
				t.Fatal(err)
			}
			if len(current.Nodes) != len(issues) || reads < len(issues) {
				t.Fatal("fixture must supply complete current input", current)
			}
			r, err := f.ensure("first", ep.Input{Context: current, PhaseOrder: []storage.Resource{phase}})
			if err != nil || r.Completed != tc.want || f.creates != boolInt(tc.want) {
				t.Fatal(r, err, f.calls)
			}
			// Branches has no Issue/Project/lifecycle mutators; all local Issue facts and
			// observed Status remain unchanged. No In Progress surface is available.
			if issues[37].State != "OPEN" || observed.Projects[0].Items[0].Fields[config.Status].StatusRole != config.Ready {
				t.Fatal("later lifecycle mutation")
			}
			if tc.want {
				issues[37] = makeIssue(37, tc.typ, tc.body)
				v := issues[37]
				v.Title = "renamed after authorization"
				issues[37] = v
				current, err = resolver.Resolve(context.Background(), resource(37))
				if err != nil {
					t.Fatal(err)
				}
				again, err := f.ensure("first", ep.Input{Context: current, PhaseOrder: []storage.Resource{phase}})
				if err != nil || again != r || f.creates != 1 {
					t.Fatal(again, err)
				}
			}
		})
	}
}
