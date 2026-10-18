package prbinding

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/intent"
	sp "github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

// Retain the fixture's exact developer checks, replacing only its fixed test
// organization with the configured identity. Other reads use the existing fake.
type configuredDevelopers struct{ *bindingFixture }

func (f configuredDevelopers) OrganizationMembership(_ context.Context, org config.Organization, login string) (github.Membership, error) {
	if org != f.deployment.Organization || login != f.user.Login {
		f.t.Fatal("membership did not use exact configured identity")
	}
	if err := f.read("membership"); err != nil {
		return github.Membership{}, err
	}
	return f.membership, nil
}

func TestSnapshotBindingCanonicalOrganization(t *testing.T) {
	for _, org := range []string{"parametron-io", "Parametron-IO", "prm-workflow", "PRM-Workflow"} {
		for _, scenario := range []string{"canonical", "canonical PR reference", "foreign PR", "foreign Target body", "noncanonical PR", "noncanonical Target body", "unconfigured repository"} {
			t.Run(org+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				f := newBindingFixture(t)
				f.deployment.Organization.Login = org
				repo := f.deployment.Repositories[0]
				repo.Owner = org
				other := github.Repository{ID: "R2", Owner: org, Name: "workflow_repo_2"}
				f.deployment.Repositories = []config.Repository{repo, other}
				f.issues[0].Repository = repo
				canonical := strings.ToLower(org)
				noncanonical := strings.ToUpper(canonical)
				if org != canonical {
					noncanonical = org
				}
				ref := canonical + "/workflow_repo_2#7"
				f.issues[0].Body = "Classification: false\nBlocked-By: " + ref
				f.addIssue(other, 7, sp.Task, "")
				// A closed blocker preserves the declared edge without preventing
				// Target execution eligibility, so full binding can be accepted.
				f.issues[1].State = "CLOSED"
				f.target = issueResource(f.issues[0])
				f.target.Owner = canonical
				f.pr.Repository, f.pr.HeadRepository = repo, repo
				f.pr.Body = "Target: #46"
				f.branch.Repository = repo
				f.completion.Evidence.Repository = repo
				f.completion.Evidence.WorkItem = f.target
				f.completion.Evidence.WorkItem.NodeID = ""
				f.membership.Organization = f.deployment.Organization
				f.permission.Repository = repo
				want := Reason("")
				var snapshotError error
				switch scenario {
				case "canonical PR reference":
					f.pr.Body += "\nRefs: " + ref
				case "foreign PR":
					f.pr.Body = "Target: other-org/workflow_repo_2#7"
					want = InvalidTarget
				case "foreign Target body":
					f.issues[0].Body = "Classification: false\nBlocked-By: other-org/workflow_repo_2#7"
					want, snapshotError = UnresolvedTarget, ec.ErrInvalid
				case "noncanonical PR":
					f.pr.Body = "Target: #46\nRefs: " + noncanonical + "/workflow_repo_2#7"
					want = InvalidTarget
				case "noncanonical Target body":
					f.issues[0].Body = "Classification: false\nBlocked-By: " + noncanonical + "/workflow_repo_2#7"
					want, snapshotError = UnresolvedTarget, ec.ErrInvalid
				case "unconfigured repository":
					f.issues[0].Body = "Classification: false\nBlocked-By: " + canonical + "/unconfigured#7"
					want, snapshotError = OutsideDeployment, ec.ErrDeployment
				}
				f.resetResolver()
				cfg := f.config()
				cfg.Developers = configuredDevelopers{f}
				v, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				var previous Decision
				for attempt := 0; attempt < 3; attempt++ {
					f.reverse = attempt%2 == 1
					d, err := v.Evaluate(ctx, storage.Resource{Owner: canonical, Repository: repo.Name, Kind: "pull_request", Number: int64(f.pr.Number)})
					if want == "" {
						requireAccepted(t, f, d, err)
						if f.counts["membership"] == 0 || f.counts["completion"] == 0 || f.counts["branch"] == 0 {
							t.Fatal("binding bypassed authorization evidence")
						}
					} else {
						requireRejected(t, d, err, want)
					}
					snapshot, err := f.resolver.ResolveSnapshot(ctx)
					if snapshotError != nil {
						if !errors.Is(err, snapshotError) {
							t.Fatalf("snapshot: %v, want %v", err, snapshotError)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						graph, err := snapshot.Context(f.target)
						if err != nil || len(graph.Dependencies) != 1 {
							t.Fatalf("graph=%+v error=%v", graph, err)
						}
						for _, node := range graph.Nodes {
							if node.Resource.NodeID == f.target.NodeID {
								// Snapshot and binding consume the same unchanged body
								// through normalized-resource and configured-login contexts.
								parsed, err := intent.Parse(f.issues[0].Body, intent.Context{Organization: org, Repository: repo.Name})
								if err != nil || !reflect.DeepEqual(parsed, node.Intent) || !reflect.DeepEqual(parsed.BlockedBy, []intent.IssueRef{{Repository: other.Name, Number: 7}}) {
									t.Fatalf("snapshot intent=%+v binding intent=%+v error=%v", node.Intent, parsed, err)
								}
							}
						}
					}
					if attempt > 0 && d != previous {
						t.Fatalf("unstable decision: %+v vs %+v", d, previous)
					}
					previous = d
				}
			})
		}
	}
}
