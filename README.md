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

The controller uses bounded semantic classification and relative estimation
where free-form engineering text benefits from model judgement. Lifecycle
transitions, dependency gating, Phase behavior, branch authorization, Pull
Request binding, review transitions, ordering, reconciliation, and GitHub
mutations remain deterministic controller responsibilities.

## Status

This repository contains the planned workflow system contract and a runnable Go
service bootstrap, plus validated deployment configuration and deterministic
Project schema binding. Workflow lifecycle capabilities are not yet implemented.

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

## Development

Enter the reproducible Go development environment:

```sh
nix develop
```

Run the bootstrap service:

```sh
nix develop --command go run ./cmd/parametron-workflow
```

The service requires no credentials, network access, database, or webhook
configuration. It waits until cancellation; press Ctrl-C to shut down cleanly.
The binary also handles SIGTERM.

Run the baseline tests and build the binary:

```sh
nix develop --command go test ./...
nix develop --command go build ./cmd/parametron-workflow
```

The build writes `./parametron-workflow`, which can be run directly. Format Go
changes with `nix develop --command gofmt -w <files>`.

`cmd/parametron-workflow` owns process signals and error reporting.
`internal/app` exposes `Run(ctx, Config)` and treats context cancellation as a
successful shutdown. Its typed configuration accepts resolved deployment
bindings. The command continues to run the bootstrap without deployment
configuration until issue #9 provides live schema discovery and startup wiring.

## License

See [LICENSE](LICENSE).
