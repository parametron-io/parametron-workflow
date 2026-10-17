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
onboarding requires no semantic Runner. Deterministic review/rework is also
implemented: qualifying native post-onboarding readiness starts review, and
validated changes requests trigger one-shot Draft conversion and owned Status
rollback across durable cycles. See
[review execution](docs/execution.md#accepted-pr-review-and-rework) and
[provenance](docs/persistence.md#accepted-pr-onboarding-provenance).
Issue #53 composes complete current PR discovery and canonical notification routing
with binding/onboarding, review/rework, merge/abandonment, companion Roadmap Order,
native Development projection and supported typed managed feedback. Discovery
selects consumers; each owner independently validates current mutation authority.
Pure [companion policy](docs/pr-companion-ordering.md) remains separate from its
production numeric reconciler. Continuous reconciliation preserves durable
one-shot protection for initial Draft and rework effects.

Task/Feature native closure requires applicable verified implementation-PR merge
authority or exact single-use `Force-Close` comment/closure correlation. An explicit
normal Phase close requires every current direct child CLOSED, including the valid
empty-child case. Project Done never supplies closure authority. See
[terminal policy](system-overview.md#25-close-done-and-reopen-synchronization),
[Development](docs/pr-development.md) and [feedback](docs/managed-feedback.md).

Stage 5A code/test closure is **PASS** at
`6f750a5797fa94bb399f234af7f89524c01c5720`: Engineering implementation and independent
offline tests are complete, including production closure-timeline transport tests.
Fresh-Nix build, full tests, vet, repeated regressions and race checks passed with
no known material offline implementation defect. Stage 5B documentation
synchronization completed in `4fe582a` and `bbd661b`. The final independent Stage 5
integration audit at `81dcf97` returned **PASS WITH NOTES**: 18 of 19 criteria
passed; it identified final current-status documentation synchronization as the
remaining criterion addressed by these post-audit corrections.
Final documentation review, PR integration/review and Issue #53 closure remain
pending. Issue #53 has not been merged or closed; parent #5 remains open and
requires its own closure decision.

[GitHub App credentials](docs/github-integration.md#authentication-and-github-app-direction)
are implemented and independently verified offline. Separate maintainer-supplied
production Go checks passed real App JWT authentication, installation-token
acquisition, configured repository scope and Bot identity; tested IssueClosure
acquisition; and isolated comment creation, receipt, exact readback, complete
enumeration and corrected REST/GraphQL Bot ownership. The ownership recheck was
read-only. These selected provider proofs do not establish every operation's live
permissions, deployed managed-feedback reconciliation or a full Engineering
controller end-to-end rehearsal. See the [evidence and remaining limits](docs/github-integration.md#issue-53-offline-verification-and-live-proof-gate).

Bug Tracker PR lifecycle/provider work is deferred beyond #6; Bug ordering remains
separate Priority Score policy. Broad nonterminal manual-intent feedback production,
other companion classes, nested lifecycle, unrelated relationship projection,
branch deletion/revocation, heavy agents, automatic merge, native Project positioning
and distributed coordination remain deferred. Assignees remain unchanged while
reliable assignability evidence is unavailable.

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

Accepted PR Development authority, transport, lifecycle and verification boundaries
are documented in [docs/pr-development.md](docs/pr-development.md).

## Development

Enter the reproducible Go development environment:

```sh
nix develop
```

Illustrative user-credential invocation: replace all placeholders with verified
deployment bindings. Installation mode is the default and requires different
settings; see [runtime](docs/runtime.md#command-and-deployment). This example is
not a live verification procedure.

```sh
nix develop --command go run ./cmd/parametron-workflow \
  --config '<bindings-json-path>' \
  --data-dir '<data-directory>' \
  --github-credential-type user \
  --github-token-file '<user-token-file>' \
  --feedback-author-id '<verified-user-node-id>' \
  --feedback-author-login '<verified-user-login>' \
  --feedback-author-type User \
  --webhook-secret-file '<webhook-secret-file>'
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
