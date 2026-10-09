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
storage provenance surface is available but no workflow-specific facts are added.

## Remaining integration

Webhook ingress still ends at durable acceptance and HTTP acknowledgement.
`internal/observe` implements resource semantics, current Issue/PR/Project/relationship
reads, stale/deleted handling, ObservedState and policy handoff. Issue #14 owns
production boundaries and retry schedule, worker construction, database paths,
credentials/secrets, HTTP server, and end-to-end startup/shutdown. The command
and bootstrap app are unchanged.
