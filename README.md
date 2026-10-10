# Parametron Workflow

Parametron Workflow is the planned deterministic workflow controller for
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

The planned controller uses bounded semantic classification and relative estimation
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
default CLI retains the no-semantic foundation because no production provider exists.
Workflow lifecycle capabilities are not yet implemented.

The current design covers the shared controller model, the **Parametron
Engineering** Project workflow, and a separate **Bug Tracker** policy layer.

## Design highlights

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
inbox feeds current-state observation and an explicit foundation sink. The sink
performs no lifecycle policy or mutations. SIGINT/SIGTERM stop ingress, cancel
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
Phase 2 and injectable durable initial classification are integrated. GitHub
semantic reconciliation and workflow lifecycle policy remain later work.

## License

See [LICENSE](LICENSE).
