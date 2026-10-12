# Parametron Workflow

Parametron Workflow is the deterministic workflow controller for
Parametron's GitHub Issues and Projects.

It is designed to keep engineering workflow state predictable and
version-controlled while preserving GitHub as the visible collaboration
surface.

The core authority model is intentionally simple:

```text
LLM interprets semantic content.
Go owns policy and mutations.
GitHub records observable state.
```

The controller contract uses bounded semantic classification and relative estimation
where free-form engineering text benefits from model judgement. Lifecycle
transitions, dependency gating, Phase behavior, branch authorization, Pull
Request binding, review transitions, ordering, reconciliation, and GitHub
mutations remain deterministic controller responsibilities.

## Status

This repository contains the planned workflow system contract and a runnable Go
service foundation, validated deployment configuration, deterministic Project
schema binding, a replaceable read-only GitHub client with live discovery,
durable SQLite event and generic provenance storage, signed webhook ingress,
and a durable worker execution foundation with resource FIFO scheduling,
current GitHub observation, a normalized policy-input handoff, and independently
testable deterministic body-directive parsing, and a standalone provider-agnostic
cheap semantic execution boundary with strict classification and Estimate policy.
Durable initial classification is available through an explicitly injected Runner,
with gating, Pending safety, stale-result rejection, and completion reuse. The
explicit Runner + Mutator path now reconciles accepted semantic metadata and
deterministic Issue Project routing, completing Phase #3 normalization.
Phase #4 Engineering Issue lifecycle/reconciliation is implemented and verified.
The default CLI enables complete deployment-wide Engineering Snapshot resolution,
pure roadmap/lifecycle computation, owned Status and numeric Roadmap Order
convergence, authorized branches, and first development activity/Start Date.
It can mutate GitHub through narrow authorized capabilities. It does not activate
a semantic provider: Runner remains optional and explicitly supplied; Runner-only
composition uses SemanticFoundationSink unless an explicit Mutator or custom
SemanticConsumer is supplied. Engineering enablement grants no semantic mutation
capability. Every enumerated Issue needs accepted semantic Type authority, or exact
validated native Type with Classification disabled. Enabled Classification
without completion retries ErrIncomplete; invalid native fallback fails closed. No arbitrary semantic backfill is performed.

See [Engineering context](docs/engineering-context.md),
[pure lifecycle policy](docs/engineering-lifecycle-policy.md),
[Roadmap Order](docs/engineering-ordering.md), and [execution](docs/execution.md).
Engineering views sort by Roadmap Order ASC; native card positions are not reconciled.
One process-local Engineering lane serializes global convergence. Partial failures
retry from fresh state and apply only current drift.

Production Engineering consumes read-only [PR binding validation](docs/pr-binding.md)
for accepted implementation PR onboarding: exact managed Project membership, owned
initial In Progress Status, and one-shot initial Draft normalization. Durable
completion suppresses repeated Status/Draft initialization and preserves later
review readiness; membership repair requires fresh accepted binding. Deterministic
onboarding requires no semantic Runner. See [execution](docs/execution.md) and
[provenance](docs/persistence.md#accepted-pr-onboarding-provenance).
Relationship projection, review transitions, PR companion ordering, merge/completion,
managed feedback, and general PR lifecycle reconciliation remain deferred.
Terminal close/reopen synchronization, nested
lifecycle, Bug Tracker
scoring/admission, branch deletion/revocation, heavy agents, automatic merge,
native Project positioning, and distributed coordination are deferred. Assignees
remain unchanged because reliable assignability evidence is unavailable.

The current design covers the shared controller model, the **Parametron
Engineering** Project workflow, and a separate **Bug Tracker** policy layer.

## Design highlights (current and future contract)

- deterministic desired-state reconciliation
- GitHub Project lifecycle management
- Phase and child Issue workflow semantics
- cross-repository Parent and dependency relationships
- declarative body directives such as `Parent:`, `Blocked-By:`, and `Target:`
- controller-authorized development branches
- Pull Request onboarding and Development relationship projection
- Draft / Ready-for-review based review lifecycle
- deterministic rework on `CHANGES_REQUESTED`
- bounded LLM classification and relative Estimate generation
- durable webhook processing and one-shot action provenance
- managed feedback for rejected user intent
- GitHub-native state synchronization for close, merge, and reopen events

## Documentation

The shared controller design and Parametron Engineering workflow are documented
in [system-overview.md](system-overview.md).

Bug Tracker-specific triage, deterministic Priority Score, blocker urgency
inheritance, Ready top-five admission, and score-based ordering are documented
in [bug-tracker.md](bug-tracker.md).

Together these documents define the current planned workflow system contract.

Deployment configuration and the live-discovery boundary are documented in
[docs/configuration.md](docs/configuration.md).

GitHub reads, authentication, errors, and test fakes are documented in
[docs/github-integration.md](docs/github-integration.md).

Durable delivery evidence, processing metadata, transactions, and SQLite settings
are documented in [docs/persistence.md](docs/persistence.md).

Signed HTTP ingress, durable acknowledgement, and transport validation are documented
in [docs/webhooks.md](docs/webhooks.md).

Durable claims, resource serialization, retries, recovery, and execution boundaries
are documented in [docs/execution.md](docs/execution.md).

Resource identity, current-state observation, and the policy-facing authority
handoff are documented in [docs/observation.md](docs/observation.md).

Deterministic explicit intent, defaults, references, and parser boundaries are
documented in [docs/directives.md](docs/directives.md). Explicit semantic composition
uses the parser at the current-state boundary.

Cheap semantic capabilities, provider injection, canonical prompt/schema assets
with content-digest provenance,
timeouts, errors, and test fakes are documented in
[docs/semantic-execution.md](docs/semantic-execution.md). Strict classification, Estimate ownership, and normalized
planning context are documented in [docs/semantic-policy.md](docs/semantic-policy.md).
Durable gating, freshness, completion, and recovery are documented in
[docs/semantic-integration.md](docs/semantic-integration.md).
Accepted metadata/routing convergence is documented in
[docs/semantic-reconciliation.md](docs/semantic-reconciliation.md).

## Development

Enter the reproducible Go development environment:

```sh
nix develop
```

Run the controller foundation:

```sh
nix develop --command go run ./cmd/parametron-workflow \
  --config /etc/parametron-workflow/bindings.json \
  --data-dir /var/lib/parametron-workflow \
  --github-token-file /run/secrets/github-token \
  --webhook-secret-file /run/secrets/github-webhook
```

Startup validates deployment bindings against live GitHub schema before serving
signed deliveries at `/webhooks/github` on `127.0.0.1:8080`. One file-backed SQLite
inbox feeds current-state observation and Engineering reconciliation. The CLI
enables Engineering by default while supplying no semantic Runner. SIGINT/SIGTERM
stop ingress, cancel
processing, wait for cleanup, and close SQLite. See [docs/runtime.md](docs/runtime.md)
for settings, secrets, restart behavior, and deployment requirements.

Run the baseline tests and build the binary:

```sh
nix develop --command go test ./...
nix develop --command go build ./cmd/parametron-workflow
```

The build writes `./parametron-workflow`, which can be run directly. Format Go
changes with `nix develop --command gofmt -w <files>`.

`cmd/parametron-workflow` owns process signals and error reporting.
`internal/app` owns schema preparation, one Store, observer/worker/webhook
construction, HTTP serving, cancellation coordination, and shutdown.
`app.Prepare` composes live discovery with deterministic configuration resolution.
Phase 2 and injectable Phase #3 semantic normalization are integrated.
Pure pre-development lifecycle and ordering packages retain their separate
ownership; production composes them with Engineering reconciliation, branch
provenance, and first activity. Embedded callers opt in with Config.Engineering;
its zero value retains the foundation path.

## License

See [LICENSE](LICENSE).
