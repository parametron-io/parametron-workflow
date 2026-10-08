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

This repository currently defines the planned workflow system contract.

The controller described here is not yet implemented.

The current design covers the **Parametron Engineering** Project. Bug Tracker
workflow policy will be added as a separate layer.

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

The canonical system design is documented in
[system-overview.md](system-overview.md).

It defines the authority model, directives, label taxonomy, lifecycle state
machine, Phase ordering, branch and Pull Request behavior, Estimate policy,
reconciliation rules, and the boundary between the controller and GitHub
built-in workflows.

## License

See [LICENSE](LICENSE).
