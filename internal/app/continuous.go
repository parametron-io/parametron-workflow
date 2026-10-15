package app

import (
	"context"
	"errors"
	"strings"

	ec "github.com/parametron-io/parametron-workflow/internal/engineeringcontext"
	feedback "github.com/parametron-io/parametron-workflow/internal/engineeringfeedback"
	et "github.com/parametron-io/parametron-workflow/internal/engineeringterminal"
	"github.com/parametron-io/parametron-workflow/internal/github"
	"github.com/parametron-io/parametron-workflow/internal/prbinding"
	"github.com/parametron-io/parametron-workflow/internal/prcompanionreconcile"
	"github.com/parametron-io/parametron-workflow/internal/prdevelopment"
	"github.com/parametron-io/parametron-workflow/internal/prdiscovery"
	"github.com/parametron-io/parametron-workflow/internal/prreview"
	"github.com/parametron-io/parametron-workflow/internal/storage"
	"github.com/parametron-io/parametron-workflow/internal/worker"
	wf "github.com/parametron-io/parametron-workflow/internal/workflowfeedback"
)

// engineeringContinuous runs under Flow's existing deployment lane. Discovery
// selects consumers; each controller independently revalidates mutation authority.
// Missing onboarding never filters numeric companion eligibility.
type engineeringContinuous struct {
	discovery   *prdiscovery.Coordinator
	development *prdevelopment.Controller
	review      *prreview.Controller
	feedback    *wf.Controller
	terminal    *et.Controller
	store       *storage.Store
	snapshots   *ec.Resolver
	companions  *prcompanionreconcile.Controller
}

func (c *engineeringContinuous) Converge(ctx context.Context, event storage.Event) error {
	ctx = github.WithRequestLimit(ctx, prdiscovery.MaxRequests)
	if event.Resource == nil {
		return prdiscovery.ErrInvalid
	}
	current, err := c.discovery.Discover(ctx)
	if err != nil {
		return err
	}
	if current.Completeness != prdiscovery.Complete || current.Freshness != prdiscovery.StableReads {
		return prdiscovery.ErrIncomplete
	}
	targets := map[storage.Resource]bool{}
	if event.Resource.Kind == "issue" {
		targets[*event.Resource] = true
	}
	for _, entry := range current.Entries {
		if entry.Resource == *event.Resource && entry.Target != nil {
			targets[entry.Target.Resource] = true
		}
	}
	if event.Resource.Kind == "pull_request" {
		input, err := c.companions.CurrentInput(ctx)
		if err != nil {
			return err
		}
		// Historical numeric ownership supplies only a former consumer identity
		// for feedback withdrawal, never fresh binding or lifecycle authority.
		for _, owned := range input.Owned {
			if owned.PR.ID == event.Resource.NodeID {
				targets[feedback.Resource(owned.Target, "issue")] = true
			}
		}
	}
	// A Phase/parent edit can change descendant eligibility without a PR event.
	// Follow only complete declarative Issue topology, never native Development.
	if event.Resource.Kind == "issue" {
		snapshot, err := c.snapshots.ResolveSnapshot(ctx)
		if err != nil {
			return err
		}
		graph, err := snapshot.CanonicalContext()
		if err != nil {
			return err
		}
		for changed := true; changed; {
			changed = false
			for _, edge := range graph.Parents {
				if targets[edge.Parent] && !targets[edge.Child] {
					targets[edge.Child] = true
					changed = true
				}
			}
			for _, edge := range graph.Dependencies {
				if targets[edge.Blocker] && !targets[edge.Blocked] {
					targets[edge.Blocked] = true
					changed = true
				}
			}
		}
	}
	var failures []error
	for _, entry := range current.Entries {
		applicable := entry.Resource == *event.Resource || entry.Target != nil && targets[entry.Target.Resource]
		if !applicable {
			continue
		}
		// Bug Targets require a Bug-owned branch/lifecycle provider. Engineering
		// binding rejection must not be presented as Bug lifecycle policy.
		if entry.Target != nil && entry.Target.Issue != nil && entry.Target.Issue.Type != nil && entry.Target.Issue.Type.Name == "Bug" {
			failures = append(failures, feedback.ErrBugAuthorityUnavailable)
			continue
		}
		switch entry.Binding.Reason {
		case prbinding.Changed, prbinding.MissingPR:
			failures = append(failures, prdiscovery.ErrChanged)
			continue
		case prbinding.UnresolvedTarget:
			failures = append(failures, prdiscovery.ErrIncomplete)
			continue
		}
		eligible := entry.Binding.Outcome == prbinding.Accepted && entry.Onboarding.Completed && entry.PR.PullRequest != nil && entry.PR.PullRequest.State == "OPEN" && entry.Target != nil && entry.Target.Issue != nil && entry.Target.Issue.State == "OPEN"
		if eligible {
			if err := c.review.Converge(ctx, entry.Resource); err != nil {
				failures = append(failures, err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := c.development.Reconcile(ctx, entry.Resource); err != nil {
				failures = append(failures, err)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Definitive rejected binding also reaches feedback, with no lifecycle or
		// Development authority. The assembler obtains new complete producer facts.
		if err := c.refreshPR(ctx, entry); err != nil {
			failures = append(failures, err)
		}
	}
	if event.Resource.Kind == "issue" {
		ids, err := c.store.CorrectionResources(ctx, "phase_closure_v1")
		if err != nil {
			return continuousFailures(append(failures, err))
		}
		wanted := map[string]bool{}
		for _, id := range ids {
			wanted[id] = true
		}
		snapshot, err := c.snapshots.ResolveSnapshot(ctx)
		if err != nil {
			return continuousFailures(append(failures, err))
		}
		graph, err := snapshot.CanonicalContext()
		if err != nil {
			return continuousFailures(append(failures, err))
		}
		for _, node := range graph.Nodes {
			if !wanted[node.Resource.NodeID] {
				continue
			}
			// Refresh only correction-bearing Issues; ordinary Status housekeeping is
			// silent. Missing manual-action evidence fails closed in Issue assembly.
			correction, err := c.terminal.CurrentCorrection(ctx, node.Resource)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if correction == nil && node.Resource != *event.Resource {
				continue
			}
			var repository github.Repository
			for _, repo := range current.Repositories {
				if strings.EqualFold(repo.Owner, node.Resource.Owner) && strings.EqualFold(repo.Name, node.Resource.Repository) {
					repository = repo
				}
			}
			r := wf.Resource{Identity: github.Identity{Repository: repository, ID: node.Resource.NodeID, Number: int(node.Resource.Number)}, Kind: "issue"}
			if err := c.feedback.Reconcile(ctx, r); err != nil {
				failures = append(failures, err)
				continue
			}
			if correction != nil {
				if err := c.terminal.AcknowledgeCorrection(ctx, node.Resource, *correction); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	return continuousFailures(failures)
}

func continuousFailures(failures []error) error {
	var primary error
	rank := 0
	for _, err := range failures {
		if err == nil {
			continue
		}
		current := 3 // malformed identity/provenance/ownership is terminal
		if errors.Is(err, prdevelopment.ErrRejected) || errors.Is(err, prreview.ErrRejected) {
			current = 1
		} else {
			var provider *github.Error
			if errors.As(err, &provider) {
				if provider.Category == github.Transient || provider.Category == github.RateLimited {
					current = 2
				}
			} else if classify(err).Retryable {
				current = 2
			}
		}
		if current > rank {
			primary, rank = err, current
		}
	}
	if primary == nil {
		return nil
	}
	return &worker.EffectError{Primary: primary, Additional: errors.Join(failures...)}
}
func (c *engineeringContinuous) refreshPR(ctx context.Context, entry prdiscovery.Entry) error {
	if entry.PR.PullRequest == nil {
		return prdiscovery.ErrIncomplete
	}
	correction, err := c.development.CurrentCorrection(ctx, entry.Resource)
	if err != nil {
		return err
	}
	r := wf.Resource{Identity: entry.PR.PullRequest.Identity, Kind: "pull_request"}
	if err := c.feedback.Reconcile(ctx, r); err != nil {
		return err
	}
	if correction != nil {
		return c.development.AcknowledgeCorrection(ctx, entry.Resource, *correction)
	}
	return nil
}
