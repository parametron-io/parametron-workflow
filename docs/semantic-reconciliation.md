# Semantic metadata reconciliation

Issue #28 completes the implemented Phase #3 normalization path:

```text
durable observation → directives → bounded execution → strict Go validation
 → durable safe acceptance → fresh refetch → metadata / Project convergence
```

`internal/semanticreconcile.New(deployment, reader, mutator)` constructs a
`semanticflow.Consumer`. Its dependencies are semanticflow, semanticpolicy,
intent, observe types/read interfaces, config, storage's resource type, and the
narrow github.Mutator. No model/provider is selected here. Accepted classification
is semantic input; deterministic Go policy authorizes each difference below.
Observation alone is never authorization.

`DesiredIssueState` produces `DesiredIssue` with Type, ManagedLabels, Priority,
Effort, and ProjectProfile. Phase/Task/Feature route to Engineering; Bug routes
to Bug Tracker. `DesiredPRState` produces only `DesiredPR.ManagedLabels`.
Both pure functions retain canonical taxonomy ordering. Current GitHub Type
cannot determine desired routing.

Only Accepted permits convergence, and the matching Issue()/PR() accessor must
succeed. Pending, zero/unknown states, and kind mismatches fail closed with a
local `*semanticreconcile.Error` matching ErrReconcile. NotRequired returns
success without semantic reads/writes or loading historical completion.

## Fresh state and explicit intent

Issues refetch through `observe.Processor.ReadCurrent`, which verifies primary
identity and reads both configured Projects without calling a Consumer. PRs use
ReadPrimary; reconciliation never reads or writes their Project membership.
The old handoff's body and metadata are not mutation authority. Missing primary
resources return success with no writes. Project read errors propagate.

After refetch the current body is parsed with the existing intent parser and
normalized owner/repository. Invalid current directives fail closed; effective
Classification false (including Automation false) suppresses all semantic writes.
Directives and bodies are never overwritten. Ordinary prose edits do not rerun
initial classification; #27's durable completion remains one-shot.

## Live bindings and ownership

Organization Issue Types are discovered as generic config.IssueType ID/Name
records and resolved into ResolvedConfig.IssueTypes[name]. Construction requires
unique, nonblank live IDs for Phase, Task, Feature, and Bug. Native Type compares
IDs and is written only when absent or different, even if display names match.

ResolvedProject.FieldOptions[FieldRole][optionName] retains live single-select
IDs in defensively copied maps. Construction requires Critical/High/Medium/Low
Priority and XS/S/M/L/XL/Unknown Effort options for both Projects. SourceConfig
still contains only deployment names/numbers, with no semantic option values or
hand-maintained IDs. Priority/Effort compare current OptionID and write only a
different/unset field on the routed item.

Managed labels are exactly semanticpolicy.ManagedLabels(). Differences are
computed in canonical order; additions then removals use incremental label APIs.
Existing exact repository label definitions are resolved before any write;
missing definitions fail safely and are never created. Unmanaged helper and
arbitrary user labels survive, including labels added concurrently. For example:

```text
current: docs, dependencies, custom-human-label
accepted managed: workflow
writes: add workflow; remove docs
result: workflow, dependencies, custom-human-label
```

Only the configured Engineering and Bug Tracker memberships are reconciled.
The desired Project must contain the Issue; the other managed Project must not.
Unrelated Projects are untouched. Multiple current items for a managed Project
fail closed before writes; no arbitrary item or cleanup policy is selected.

## Write sequence and ingress

Issue mutation order is deterministic:

1. Native Issue Type.
2. Managed label additions, then managed label removals (each taxonomy ordered).
3. Ensure desired Project membership.
4. Priority, then Effort.
5. Initialize Status to configured Backlog only when Status is unset and current
   effective Set-Status permits it.
6. Remove conflicting managed Project membership.

Both Engineering and Bug Tracker enter through Backlog. Existing nonempty Status
is preserved, including Ready/In Progress/In Review/Done. Set-Status false
suppresses Status initialization. It does not suppress Type/labels/routing or
Priority/Effort. PR order is fresh primary read then managed label delta only.

The separate github.Mutator offers SetIssueType, ResolveLabels (read-only exact
lookup), AddLabels, RemoveLabels, AddProjectItem, RemoveProjectItem, and
SetProjectOption. Transport implements it using the existing GraphQL/token/HTTP
stack. There is no arbitrary GraphQL policy API, complete-label replacement,
label creation, retries, or second SDK. Client and observer remain read-only.
Inputs are validated before network calls; responses validate returned identities.
Malformed response structures return the existing github.Malformed category.

## Convergence, retry, and runtime

Equivalent current/desired state produces no writes. There is no reconciliation
completion flag or storage migration. Every attempt refetches and applies only
remaining differences. If Type succeeds and a later call fails, worker retry sees
the correct Type and skips it. Desired membership and semantic fields precede
conflicting membership removal, preserving useful routing under partial failure.
There is no rollback. A later event corrects manual drift using the same durable
accepted classification without invoking the model again.

Context is checked before writes and between mutation stages. Cancellation is
returned without converting shutdown into a terminal failure. GitHub errors
propagate unchanged so worker rate-limit/transient retry and terminal categories
remain intact. App classifies local reconciliation errors as semantic_reconcile
without persisting raw strings.

Runner + explicit Mutator + no custom SemanticConsumer composes the reconciler
behind semanticflow.Coordinator. Runner + custom SemanticConsumer preserves the
explicit consumer path. Mutator without Runner, or Mutator together with a custom
SemanticConsumer, is rejected to avoid ambiguous ownership. Runner alone retains
SemanticFoundationSink. Default CLI remains FoundationSink/read-only: no provider,
CLI flags, new dependencies, Nix changes, or GitHub writes are activated by default.

Tests use real Store, Worker, Processor, Coordinator, and Reconciler with
synchronized deterministic GitHub fixtures. They prove durable acceptance,
current refetch, both routes, Type/labels/fields, worker completion, duplicate and
later-event no-ops, model completion reuse, manual drift correction, partial
failure/retry, directive drift, cancellation, and unmanaged-label preservation.
Transport tests use local HTTP for exact operations/variables, response identity,
errors, credential isolation, and no retry; discovery tests cover pagination and
malformed/ambiguous records.

Later lifecycle policy remains unimplemented: no relationships, branches,
Phase activation, readiness/dependencies, PR onboarding/review, close/reopen,
Bug triage/Priority Score, Estimate, Start Date, or Project position. Priority,
Effort, and narrow Backlog ingress are the only Project fields written. Other
fields are neither overwritten nor cleared. Phase closure in GitHub remains the
merge/issue workflow's responsibility.
