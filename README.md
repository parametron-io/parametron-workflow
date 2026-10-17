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

Parametron Workflow is an actively developed Go controller for deterministic
GitHub Issue, Project, and Pull Request automation. The Engineering workflow
is implemented and covered by automated tests, including Issue lifecycle and
roadmap ordering, authorized development branches, PR onboarding and review,
merge/reopen synchronization, Development relationships, and managed feedback.
Signed webhooks and SQLite-backed processing support durable retries and
reconciliation from current GitHub state.

Semantic classification and estimation are optional; the default controller
does not run an LLM. Bug Tracker PR automation, automatic merging, branch
deletion/recovery, and distributed coordination remain outside the current
implementation.

Automated tests and selected live GitHub API checks have passed, but a full
deployed end-to-end controller rehearsal has not been completed. See
[runtime](docs/runtime.md), [execution](docs/execution.md), and
[GitHub integration](docs/github-integration.md) for technical contracts and
verification details.

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
