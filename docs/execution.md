# Durable execution

`internal/worker.New(Config)` constructs an independently usable worker. It
requires a Store, ResourceResolver, Processor, classifier for non-GitHub errors,
retry scheduling function, and positive concurrency. Clock defaults to time.Now.
There is no payload interpretation, GitHub fetching, policy, mutation, HTTP
handling, or production startup wiring in this package.

`Step(ctx)` executes one durable acceptance-ordered snapshot. `Recover(ctx)`
explicitly releases interrupted attempts. `Run(ctx, interval)` recovers once,
then executes snapshots with a caller-supplied polling interval of at least one
second. Poll timing is separate from retry eligibility and specifies no product
backoff schedule. New arrivals are handled in subsequent snapshots. Step calls
on one Worker serialize. One runtime/dispatcher must exclusively own a database;
these APIs do not provide distributed worker coordination. Recovery must never
run alongside live attempts, including attempts in another process.

## Claims and attempts

Work enumeration is observation, not execution authority. `Store.Claim(id, now)`
uses an immediate SQLite transaction to check eligibility and atomically change
status to processing and increment attempts. Pending and due retryable records
are eligible; processing, completed, failed, and future retries are not.
ErrConflict means the claim cannot proceed. The attempt count is a monotonic
claim generation and never resets on retry, recovery, or equivalent redelivery.
An attempt includes resource resolution where needed, not just processor work.

`Store.Settle(id, attempt, status, next, category)` checks processing status and
expected generation in the same transaction as the outcome write. Older claims
cannot settle newer attempts. Success becomes completed; terminal failure becomes
failed; retryable failure stores its safe category and next eligibility time.
Interrupted attempts release to pending with their generation preserved.
A completed or failed event is never normally claimed again.

## Resource dispatch and ordering

The dispatcher traverses durable sequence order and resolves unbound events
sequentially under durable claims. Successful resolution uses BindResource, then
reads back storage's normalized binding. Bound events bypass resolution.
ResourceResolver owns the meaning of the payload; `internal/observe.Resolver`
supplies the production implementation. Worker code never parses webhook schemas
or fetches current state. `observe.Processor` refetches and normalizes current
GitHub state before policy handoff; see [observation.md](observation.md).

Explicit FIFO queues use owner, repository, kind, and number; NodeID is evidence
and is excluded from the key. A bounded pool executes one resource queue at a
time, in sequence order, while unrelated queues can execute concurrently.
The goroutine count is bounded by configured concurrency; snapshot/queue memory
scales with unfinished work. No goroutine is allocated per queued event.

A retry or interrupted predecessor prevents later events in that lane from
executing. Future retries block only their known resource. An unresolved event
whose retry is not due, whose processing claim is unfinished, or whose resolution
fails is a conservative dispatch barrier: later identities cannot safely be
assumed unrelated. Previously resolved earlier queues may still run. This avoids
reordering when the unknown event eventually resolves to an existing lane.
Resolved queued claims that do not execute are released without resetting their
attempt count. Resolution can therefore account for an attempt even if processor
execution is deferred. Failed resolution/processing may defer subsequent work
until the next snapshot.

## Failure and retry boundaries

Wrapped `github.Error` values in categories rate_limited and transient retry.
The other documented GitHub categories are terminal. Errors outside that taxonomy
use the explicitly injected Classifier; there is no error-message matching or
implicit classification of arbitrary errors. Classifiers must supply safe,
stable identifiers, never raw error text. Storage validates identifier syntax;
it cannot scrub a caller-supplied credential disguised as an identifier.

RetrySchedule receives classification, attempt count, and current time. Its result
must be strictly in the future and a valid storage timestamp. The worker persists
it and checks durable eligibility on later claims. No jitter, default backoff,
hidden transport retries, or immediate retry loops are introduced. Invalid
classification/schedule or database failures return an error; unresolved active
state remains recoverable through explicit recovery. No provider errors or
payloads are logged or persisted by the worker.

## Recovery and cancellation

RecoverInterrupted changes only processing rows to pending, retaining attempts,
immutable delivery evidence, bindings, and provenance. Opening storage alone does
not recover. Completed/failed rows and scheduled retries remain unchanged.

Cancellation stops dispatch/claims and signals resolvers/processors via context.
They must respect cancellation and return; the worker waits for in-flight cleanup
without an arbitrary timeout. Queued owned claims release, and canceled active
attempts settle to pending rather than terminal failure. Cleanup uses a context
without cancellation so SQLite can commit the release. Storage cleanup failures
are reported; if storage is unavailable, processing records remain durable and
explicit recovery is required. Cancellation can race a successful processor
return: an interrupted attempt may be retried even if an external effect happened.

SQLite cannot atomically commit arbitrary GitHub effects and local completion.
These are durable claims and at-least-once recovery of unfinished work, not
exactly-once external effects. Completed events are not intentionally re-executed,
and delivery deduplication does not reset their state. Later side-effecting
processors must use appropriate idempotency/provenance protocols; the generic
storage provenance surface is available. #27's semantic coordinator now uses it
for initial classification completion; see [semantic-integration.md](semantic-integration.md).

## Runtime integration

`internal/app` constructs the worker with the production observer, an explicit
local-error classifier (including stable semantic retry/terminal categories),
and a fixed positive retry delay (default 30s).
It runs `Worker.Run` with a poll interval of at least 1s (default 1s) beside HTTP
ingress. The worker retains sole recovery, eligibility, ordering, and settlement
authority. One process must exclusively own the database; no distributed
coordination is added. See [runtime.md](runtime.md).

## Authorized development branches

Issue #37 implements the standalone `internal/engineeringbranch` boundary:

```go
controller, err := engineeringbranch.New(engineeringbranch.Config{
    Repositories: deployment.Repositories,
    Branches: transport,
    Store: store,
})
result, err := controller.Ensure(ctx, deliveryID, currentLifecycleInput)
```

`currentLifecycleInput` is `engineeringpolicy.Input`, including complete current
Engineering facts and authoritative PhaseOrder. Ensure calls #35 Evaluate itself
and acts only on Context.Root. Observed Project Status, webhook title/body/state,
and existing refs cannot be passed as eligibility decisions. New authorization
and unfinished creation attempts require `ExecutionEligible = true`. Set-Status
ownership does not replace or suppress canonical branch eligibility. Task and
Feature retain default Create-Branch true; Phase requires explicit true and
current eligibility. There are no Status, In Progress, Start Date, Assignee,
relationship, PR, Roadmap Order, deletion, or default-branch writes.

Freshness/completeness of Input belongs to the caller. #39 owns production
fresh GitHub observation → complete context → roadmap PhaseOrder → lifecycle →
reconciliation composition. #37 has no dependency on engineeringorder and does
not assemble context from #34 Resolve(root). That resolver is relevance-scoped:
resolving one of two unrelated OPEN Phases can omit its competitor. Its result
alone is not proof of complete activation input. Integration tests use explicitly
complete known fixtures and caller-supplied PhaseOrder, not a production assembler.
No app/worker/CLI composition is introduced by #37.

### Branch identity and evidence

The exact v1 short name is `development/issue-<number>` in the Issue's owning
configured repository. Positive base-10 Issue numbers use no leading zeros;
positive int64 gives a maximum of 37 ASCII bytes. Task, Feature, and explicitly
branch-enabled Phase share the format. Title, Type, labels, ordering, and current
node ID do not affect it. Equal numbers in different repositories identify
different repository-local branches. Malformed/unrepresentable resources and
branch/ref/SHA inputs fail before mutation. Repository bindings are validated
and copied at construction; runtime node IDs are deployment evidence, not
hardcoded workflow semantics.

Both immutable facts use stable key
`<lowercase-owner>/<lowercase-repository>/issue/<number>` with no node ID.
Namespaces are `engineering_branch_authorization_v1` (reservation) and
`engineering_branch_creation_v1` (successful creation). Both have identical
canonical encoding/json bytes for `Evidence`, in this fixed field order:

```text
version: 1
work_item: storage.Resource (canonical owner/repository, issue, number, empty NodeID)
repository: configured Repository (ID, Owner, Name)
branch: exact authorized short name
base_ref: exact default short heads name resolved at authorization
base_sha: exact lowercase 40-hex commit SHA
```

Each fact carries the originating authorization DeliveryID and its own storage
CreatedAt. Time is not part of the canonical metadata or branch identity.
Completion's namespace asserts a validated successful create response at base_sha;
it is not inferred from a later branch read. Both originating events must have
the matching durable resource binding. Reads verify namespace/key, canonical JSON,
version/domain/identity, base ref/SHA, and event identity. Unknown/duplicate JSON
keys, extra prose, inconsistent completion/reservation, and changed repository
bindings fail closed. Evidence and returned results contain values rather than
caller-owned mutable maps/slices/pointers. No Issue prose/title or provider error
text is persisted.

### Two-fact creation protocol

1. Evaluate the supplied current #35 Input and validate the root, configured
   repository, and durable triggering delivery binding. Load and strictly
   validate both facts. If completion exists, require the matching reservation
   and original delivery, then reuse completion without any branch read/write.
   Completed provenance remains historical fact if current eligibility changes
   or the branch is later changed/deleted; this Issue does not recreate it.
2. Without completion, require current ExecutionEligible. For a new reservation,
   read the configured repository's current default heads ref and exact commit
   SHA. Validate repository/ref/Commit identity. Never use a moving base name for
   creation. For an existing reservation, reuse its exact name/ref/SHA, including
   after title changes or default-branch changes; do not resolve a new base.
3. Look up the exact proposed/reserved development name. If present, return
   ErrAmbiguous without adopting it, even when its SHA equals the base. A manual
   lookalike before authorization receives neither reservation nor completion.
   Only typed github.MissingBranch for the matching validated repository/name
   proves absence. Generic NotFound (including HTTP 404 or missing repository)
   and other lookup failures propagate without mutation.
4. After proving absence and checking cancellation, durably record the new
   reservation. Check cancellation again before external mutation. Existing
   reservation retries skip this write and retain the original event.
5. Create the exact branch at the exact reserved SHA. Validate the returned
   repository, heads name, Commit kind, and SHA. Check cancellation and durably
   record completion under the original authorization delivery. Only after this
   succeeds return `Completed = true` and the value evidence.

Authorization alone + existing branch + absent completion always fails closed.
This may be manual creation, a race, remote success with a lost response, or a
crash/storage failure/cancellation between remote success and completion. Exact
name + exact SHA is never sufficient attribution. There is no deletion,
destructive recovery, or invented successful provenance. If a retryable failure
leaves the reserved branch absent, later current-eligible attempts may safely
create that same exact branch from that same SHA. Completed facts suppress
creation on duplicate delivery and restart, even if the remote branch disappears.

Calls on one Controller serialize with cancellation checked after lock admission.
The existing worker still owns per-resource lanes, exclusive database/runtime
ownership, retries, and recovery. Independent live controllers/processes must not
concurrently execute the same resource. Provenance uniqueness prevents competing
reservation metadata but does not provide distributed external-effect locking.
No SQLite transaction is held across GitHub calls. No migration or hidden retry
is added, and worker recovery behavior remains unchanged.

Local stable errors are ErrInvalid (boundary/identity), ErrProvenance
(conflicting/corrupt evidence), ErrAmbiguous (unattributable existing ref), and
ErrStorage (bounded persistence failure). #35 and classified GitHub errors
propagate; cancellation/deadline remains detectable with errors.Is. The caller
must provide worker error classification when composing production execution in
#39. Cancellation is returned as an error, never successful authorization or
creation. A committed reservation can survive cancellation and be retried; a
remote success without completion remains ambiguous. If cancellation races a
committed completion, that durable fact remains valid and can be reused on retry.
