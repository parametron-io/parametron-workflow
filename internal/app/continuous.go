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
	trigger := *event.Resource
	if trigger.Kind == "pull_request" {
		canonical, found, err := continuousPRTrigger(ctx, current, trigger)
		if err != nil {
			return err
		}
		if !found {
			// A complete current list need not contain an old notification's PR.
			// Absence selects no consumer and grants no historical authority.
			return ctx.Err()
		}
		trigger = canonical
	}
	if trigger.Kind == "issue" {
		snapshot, err := c.snapshots.ResolveSnapshot(ctx)
		if err != nil {
			return err
		}
		graph, err := continuousIssueContext(snapshot, trigger)
		if err != nil {
			return err
		}
		trigger = graph.Root
	}
	var failures []error
	// Terminal repair must remain reachable even when an earlier lifecycle route
	// returned before its ordinary Target synchronization. This invokes the same
	// owner, never replays onboarding/rework, and refreshes discovery afterward.
	root := storage.Resource{}
	if event.Resource.Kind == "issue" {
		root = trigger
	}
	if event.Resource.Kind == "pull_request" {
		for _, entry := range current.Entries {
			if entry.Resource == trigger && entry.Target != nil && entry.Target.Issue != nil && entry.Target.Issue.Type != nil && entry.Target.Issue.Type.Name != "Bug" {
				root = entry.Target.Resource
			}
		}
	}
	if root.Kind != "" {
		rejections, err := c.terminal.SynchronizeFrom(ctx, root, event.Delivery.ID)
		if err != nil {
			return err
		}
		if len(rejections) > 0 {
			if err := c.companions.Reconcile(ctx, event.Delivery.ID); err != nil {
				failures = append(failures, err)
			}
		}
		current, err = c.discovery.Discover(ctx)
		if err != nil {
			return err
		}
		if trigger.Kind == "pull_request" {
			canonical, found, err := continuousPRTrigger(ctx, current, trigger)
			if err != nil {
				return err
			}
			if !found || canonical != trigger {
				return prdiscovery.ErrChanged
			}
		}
	}
	targets := map[storage.Resource]bool{}
	if event.Resource.Kind == "issue" {
		targets[trigger] = true
	}
	for _, entry := range current.Entries {
		if entry.Resource == trigger && entry.Target != nil {
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
			if feedback.Resource(owned.PR, "pull_request") == trigger {
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
		graph, err := continuousIssueContext(snapshot, trigger)
		if err != nil {
			return err
		}
		for changed := true; changed; {
			changed = false
			for _, edge := range graph.Parents {
				if targets[edge.Child] && !targets[edge.Parent] {
					targets[edge.Parent] = true
					changed = true
				}
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
	for _, entry := range current.Entries {
		applicable := entry.Resource == trigger || entry.Target != nil && targets[entry.Target.Resource]
		if !applicable {
			continue
		}
		// Bug Targets are outside this Engineering gate. Engineering binding
		// rejection must not be presented as a supported Bug lifecycle policy.
		if entry.Target != nil && entry.Target.Issue != nil && entry.Target.Issue.Type != nil && entry.Target.Issue.Type.Name == "Bug" {
			failures = append(failures, feedback.ErrUnsupportedBugTarget)
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
	if event.Resource.Kind == "issue" || event.Resource.Kind == "pull_request" {
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
		if event.Resource.Kind == "issue" {
			graph, err = continuousIssueContext(snapshot, trigger)
		}
		if err != nil {
			return continuousFailures(append(failures, err))
		}
		for _, node := range graph.Nodes {
			if !wanted[node.Resource.NodeID] {
				continue
			}
			// Terminal corrections carry actor-neutral owner evidence. No broad
			// manual-action attribution is needed to refresh these exact Issues.
			correction, err := c.terminal.CurrentCorrection(ctx, node.Resource)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if correction == nil && node.Resource != trigger && !targets[node.Resource] {
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

// Complete discovery supplies resource-selection identity, including for
// rejected bindings. Each selected owner must still verify effect authority.
// Initial absence is not applicable; callers rechecking an established trigger
// must treat absence as changed evidence instead.
func continuousPRTrigger(ctx context.Context, current prdiscovery.Result, locator storage.Resource) (storage.Resource, bool, error) {
	if err := ctx.Err(); err != nil {
		return storage.Resource{}, false, err
	}
	if current.Completeness != prdiscovery.Complete || current.Freshness != prdiscovery.StableReads {
		return storage.Resource{}, false, prdiscovery.ErrIncomplete
	}
	if locator.Kind != "pull_request" || locator.Number <= 0 {
		return storage.Resource{}, false, prdiscovery.ErrInvalid
	}
	locator.Owner, locator.Repository = strings.ToLower(locator.Owner), strings.ToLower(locator.Repository)
	configured := false
	for _, repo := range current.Repositories {
		if strings.EqualFold(repo.Owner, locator.Owner) && strings.EqualFold(repo.Name, locator.Repository) {
			configured = true
		}
	}
	if !configured {
		return storage.Resource{}, false, prdiscovery.ErrInvalid
	}
	wanted := locator
	wanted.NodeID = ""
	var canonical storage.Resource
	found := false
	for _, entry := range current.Entries {
		key := entry.Resource
		key.NodeID = ""
		if key != wanted {
			if locator.NodeID != "" && entry.Resource.NodeID == locator.NodeID {
				return storage.Resource{}, false, prdiscovery.ErrChanged
			}
			continue
		}
		if found || entry.Resource.NodeID == "" {
			return storage.Resource{}, false, prdiscovery.ErrInvalid
		}
		if locator.NodeID != "" && locator.NodeID != entry.Resource.NodeID {
			return storage.Resource{}, false, prdiscovery.ErrChanged
		}
		canonical, found = entry.Resource, true
	}
	return canonical, found, ctx.Err()
}

// Resolve only an Issue locator against a complete current snapshot. Snapshot
// projection retains the whole graph for fan-out; it does not grant any effect.
// A supplied node identity must agree, including on later freshness barriers.
func continuousIssueContext(snapshot ec.Snapshot, locator storage.Resource) (ec.Context, error) {
	graph, err := snapshot.Context(locator)
	if err != nil {
		return graph, err
	}
	expected := locator
	expected.Owner, expected.Repository = strings.ToLower(locator.Owner), strings.ToLower(locator.Repository)
	if expected.NodeID != "" && expected.NodeID != graph.Root.NodeID {
		return ec.Context{}, ec.ErrChanged
	}
	expected.NodeID = graph.Root.NodeID
	if expected != graph.Root || graph.Root.NodeID == "" {
		return ec.Context{}, ec.ErrChanged
	}
	return graph, nil
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
