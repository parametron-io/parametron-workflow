# AGENTS.md

## Repository purpose

`parametron-workflow` implements the deterministic GitHub workflow controller
for Parametron.

The controller observes GitHub state, computes canonical desired workflow state,
and performs authorized GitHub mutations.

The central authority rules are:

```text
LLM interprets semantic content.
Go owns policy and mutations.
GitHub records observable state.
```

and:

```text
observation != authorization
```

Observed GitHub state, events, branches, commits, relationships, or user actions
must not be treated as authorization unless controller policy explicitly permits
the resulting side effect.

## Canonical design documents

Before changing workflow behavior, read the relevant design contract:

- `system-overview.md`
  - shared controller architecture
  - Parametron Engineering workflow
  - directives
  - classification boundaries
  - branch and Pull Request lifecycle
  - reconciliation rules
  - canonical label taxonomy

- `bug-tracker.md`
  - Bug Tracker lifecycle
  - triage
  - Priority Score
  - blocker urgency inheritance
  - Ready admission and rebalancing

Implementation must remain consistent with these documents.

If implementation work requires behavior that is not defined by the current
contract, do not silently invent policy. Keep the implementation within the
active issue scope and report the missing design decision.

## Scope discipline

Work only on the active Issue or Pull Request scope.

Do not implement deferred behavior merely because the surrounding architecture
suggests it may be useful later.

Prefer the smallest change that completely satisfies the active issue.

Do not combine unrelated cleanup, refactoring, or future-phase work with the
current task.

Respect existing repository and package boundaries unless the active issue
explicitly requires changing them.

## Controller ownership

Deterministic workflow policy belongs in Go.

This includes, where applicable:

- lifecycle transitions
- dependency eligibility
- Project routing
- Project field reconciliation
- branch authorization
- branch provenance
- Pull Request binding
- relationship projection
- close/reopen synchronization
- one-shot action provenance
- managed feedback decisions

LLM or agent output must never directly mutate GitHub.

Model output is advisory input that must be validated by deterministic Go code
before it can affect controller-owned state.

## LLM boundaries

Cheap semantic models and future heavy agents are judgement providers, not
workflow authorities.

Do not couple workflow policy to a specific model vendor, executable, or CLI.

Provider-specific execution belongs behind replaceable runtime boundaries.

All bounded model output must be validated against explicit schemas,
allowlists, or equivalent deterministic contracts before use.

Prompt templates and model-output contracts should remain version-controlled
when introduced.

## GitHub event handling

Webhook payloads are notifications that state may have changed.

Do not treat webhook payload state as canonical workflow state.

Before making a policy decision, fetch the current GitHub state required by the
decision.

Processing must eventually support duplicate delivery, retry, restart, and
per-resource ordering without repeating one-shot side effects.

Do not weaken these properties for convenience.

## Configuration

Workflow semantics belong in code.

Deployment-specific GitHub bindings belong in configuration.

Do not hardcode organization-specific GitHub node IDs, Project field IDs, or
Project option IDs as durable workflow semantics.

Human-readable configuration may identify GitHub resources by stable names or
other documented identifiers; runtime code may resolve those values to current
GitHub IDs.

Missing or ambiguous required configuration should fail explicitly rather than
be guessed.

## Development environment

Use the repository Nix development environment for build and test commands.

Do not assume globally installed development dependencies are part of the
repository contract.

The initial development shell intentionally contains only tools required by the
repository itself.

Run Go tests with:

```sh
nix develop --command go test ./...
```

Format changed Go files with `gofmt` from the Nix environment.

Do not add development tools to `flake.nix` merely because they are convenient.
Add a tool only when the repository build, test, generation, or runtime
development contract actually requires it.

## Testing

New behavior requires focused tests at the narrowest useful boundary.

For deterministic workflow behavior, cover both the successful transition and
important rejected or no-op cases.

When changing persistence, event processing, authorization, or reconciliation
behavior, include tests for the relevant idempotency or failure boundary.

Do not claim a test or validation step passed unless it was actually run.

## Documentation

When implementation changes a documented contract, update the relevant
canonical documentation in the same change.

Do not rewrite historical or unrelated design text merely to make documentation
look cleaner.

Keep implementation status and design claims accurate. Planned behavior must
not be described as implemented before it exists.

## Git workflow

Development targets `main` through Pull Requests.

Use the active issue as the implementation boundary.

Keep commits focused and use conventional, descriptive commit messages.

Do not bypass repository rules or rewrite shared history.

## Current bootstrap boundary

During the initial repository bootstrap, keep the implementation intentionally
small.

The bootstrap may establish:

- the Go module and package layout
- the controller process entry point
- context and signal handling
- clean startup and shutdown
- the reproducible Nix development environment
- baseline tests
- development documentation

Do not pull later Phase work into the bootstrap.

In particular, GitHub authentication, Project schema resolution, SQLite event
storage, webhook processing, semantic classification, and workflow lifecycle
implementation belong to their dedicated issues.
