# Durable persistence

`internal/storage` owns SQLite through a concrete `Store`. SQL, table layouts,
connection configuration, and migrations stay private. Persistence stores
historical facts and generic execution metadata; it makes no workflow decisions
and does not interpret webhook payloads as current GitHub state.

## Opening and schema

`storage.Open(ctx, path)` accepts a caller-supplied filesystem database filename.
Relative names resolve against the caller's current directory. The parent
directory must already exist; Open creates the database file and initial schema.
Names are URI-escaped internally, so they cannot inject SQLite connection options.
Empty names and `:memory:` are rejected. This package chooses no production location or directory policy; the app
provides those as documented in [runtime.md](runtime.md).
Callers own `Close()` and must keep transaction callbacks short.

`PRAGMA user_version` is the schema version. Version 0 is accepted only when no
application schema objects exist. Ordered, private migrations create version 1;
DDL and version changes share one transaction. Failure rolls both back. Existing
current schemas are verified against the known schema definitions; missing,
changed, or additional application objects fail with `ErrSchema`. Future versions
fail explicitly. Opening a current database is idempotent. Released migrations
must remain unchanged; later migrations are appended, with current-schema
verification updated to describe their resulting schema when necessary.

## Delivery evidence and identity

`InsertDelivery` persists a delivery ID, event name, exact nonempty payload bytes,
caller-supplied acceptance time, and optional acceptance resource evidence.
The accepted record is immutable through the API. A separate monotonically
increasing database sequence establishes durable insertion order, independent of
caller clocks. This is acceptance order, not a guarantee of GitHub occurrence
order. All timestamps use UTC text with nine fractional digits:
`YYYY-MM-DDTHH:MM:SS.nnnnnnnnnZ`. Returned timestamps are UTC. There are no hidden
wall-clock calls.

A SQLite UNIQUE constraint is the final delivery-ID authority. The API returns
`inserted=true` only after committing new work, or `false, nil` for an equivalent
redelivery. Event name, payload bytes, and canonical acceptance resource evidence
must agree. Conflicting evidence returns `ErrConflict`; reception time on a
redelivery is ignored and never replaces the first acceptance time. Duplicates
never reset attempts, retry time, execution state, resource bindings, or provenance.
JSON payloads are opaque evidence: semantically equivalent JSON with different
bytes is considered conflicting delivery evidence.

## Resource identity and queries

`Resource` contains owner, repository, kind, positive resource number, and optional
GitHub node ID. Owner/repository names are normalized to lowercase. Kind is a
short lowercase identifier, such as `issue` or `pull_request`, with no workflow
taxonomy. The query key is `(owner, repository, kind, number)`; node ID is evidence
and is excluded from key comparison in work queries.

Acceptance resource evidence can be absent. `BindResource` later attaches a
resolved identity without changing accepted evidence. Repeated identical bindings
are no-ops; changing any bound field fails with `ErrConflict`. Storage performs
no resource extraction or resolution itself. `Event` retrieves by delivery ID.
`Work` enumerates unfinished events, including unresolved ones.
`WorkByResource` retrieves unfinished events for one key. Both queries use
acceptance order and include processing records for later recovery. They neither
claim work nor apply retry eligibility; callers must not treat enumeration as an
execution lock.

## Processing and transactions

New deliveries start as `pending`, with zero attempts, no retry time, and no
error category. Generic states mean:

| State | Execution meaning |
| --- | --- |
| `pending` | Accepted, awaiting processing |
| `processing` | Caller has recorded an in-progress attempt |
| `retryable` | Caller has decided another attempt is appropriate |
| `completed` | Caller has recorded successful completion |
| `failed` | Caller has recorded terminal failure |

These names are unrelated to Project statuses. Storage validates state names,
nonnegative attempt counts, timestamps, and safe error-category identifier syntax.
It does not validate a transition graph or choose terminal/retry decisions.
`UpdateState` replaces status, attempt count, nullable next-attempt time, and
error category in one statement. A nil time clears the previous retry time;
an empty category clears the previous identifier. Callers choose categories
that exclude secrets; identifier validation is not a credential scrubber. Raw
provider error strings should never be supplied.

`Transaction(ctx, func(*storage.Tx) error)` exposes only focused methods:
`Event`, `UpdateState`, `BindResource`, and `RecordProvenance`. It commits all
changes together on callback success and rolls them back on callback error,
context cancellation, or panic. Read-then-update decisions can use `Tx.Event`
within the same transaction. Transaction handles expire after the callback;
they must not be retained or shared between goroutines. Calling Store methods
from the callback would wait on its own connection and must be avoided. Return
operation errors from the callback to request rollback. Transactions cannot make
external GitHub side effects atomic.

Useful stable errors are `ErrNotFound`, `ErrConflict`, `ErrSchema`, `ErrInvalid`,
and `ErrTransactionDone`, detectable using `errors.Is`. Constraint conflicts wrap
underlying SQLite errors for diagnostics without requiring string parsing.
Other database errors remain wrapped/detectable as supplied by database/sql,
including context cancellation where supported. No error-code taxonomy or retry
policy is imposed for busy/unavailable databases.

## Durable execution operations

`Claim(ctx, id, now)` atomically checks pending/due retryable eligibility, marks
processing, and increments the durable attempt count. Ineligible claims return
ErrConflict. `Settle(ctx, id, attempt, status, next, category)` requires the current
processing generation and preserves its count while completing, retrying,
terminally failing, or releasing to pending. A stale settlement returns
ErrConflict. These operations use the existing version-1 fields; no migration
is required. Generic UpdateState remains a low-level API and must not be used
to bypass the worker's ownership protocol.

`RecoverInterrupted(ctx)` releases only processing rows to pending and clears
their transient scheduling/category metadata. It preserves attempts, evidence,
bindings, provenance, scheduled retries, and terminal states. Callers must hold
exclusive runtime ownership and ensure no live attempts exist before recovery.
Open itself does not perform recovery. See [execution.md](execution.md) for the
worker contract and limitations around external effects.

## Provenance

#37's `internal/engineeringbranch` reuses this generic API without a migration.
Two immutable namespaces, `engineering_branch_authorization_v1` and
`engineering_branch_creation_v1`, share a canonical stable key
`<lowercase-owner>/<lowercase-repository>/issue/<positive-number>`.
Both contain identical canonical metadata: same-target authorization retains
v1 JSON Evidence for the work item, repository identity, exact branch,
default/base ref, and exact base SHA. Cross-target authorization uses a version-2
origin envelope in these EXISTING namespaces, binding the actual trigger
DeliveryID/resource to target v1 branch Evidence. Legacy same-target v1 remains
readable; paired records must agree exactly. No new namespace/schema migration
was introduced. The first records
authorization before mutation; the second proves a validated controller create
response. Completion alone or conflicting metadata/event binding fails closed.
Both facts retain the original authorization delivery ID. Duplicate/restart
reads reuse facts without rewriting them under a later event. No arbitrary
Issue prose, title, or provider error text is recorded. Strict decoding checks
canonical bytes, supported version, identities, branch/ref/SHA, and the
originating delivery's durable resource binding. Reservation is not creation.
The exact partial-failure protocol is in [execution.md](execution.md).

`RecordProvenance` stores namespace, stable key, associated delivery ID, opaque
metadata bytes, and caller-supplied creation time. A foreign key requires the
delivery to exist. Resource association is available through that event's binding.
The `(namespace, stable_key)` primary key is database-wide: callers own meaningful
stable keys and namespaces. Equivalent duplicates return `false, nil` and retain
the original creation time; conflicting delivery association or metadata returns
`ErrConflict`. `Provenance` looks up that key. Records can commit atomically with
processing metadata and resource binding changes.

This surface records generic one-shot facts only. Branch creation and first
activity and accepted PR onboarding use the protocols described here; managed
feedback remains later work.
Issue #27 uses this unchanged surface for namespace `semantic.classification`
and format-1 bounded, canonical initial-classification completion records;
decoding, gating, and authorization stay in `internal/semanticflow`. See
[semantic-integration.md](semantic-integration.md). No migration is required.
A committed record alone does not establish exactly-once execution of an external
side effect.

## SQLite settings and restart assumptions

The driver is pure-Go `modernc.org/sqlite`; no CGO, SQLite executable, ORM,
external migration framework, or Nix shell additions are required.

- `journal_mode=WAL`: explicitly selected and checked at opening.
- `synchronous=FULL`: commits synchronize the WAL for SQLite's full durability.
- `foreign_keys=ON`: prevents orphaned state, resource, and provenance rows.
- `busy_timeout=5000`: bounded SQLite lock waiting, without application retries.
- Immediate write transactions acquire the writer lock before callback reads.
- One pooled connection per Store serializes database operations. Per-connection
  pragmas are in the driver DSN so replacement connections receive them too.

WAL permits another connection to read while writing; SQLite still has one writer.
This supports short transactions in a single controller process with future
concurrent workers, not multi-host coordination. Durability depends on the local
filesystem/storage honoring SQLite's synchronization and locking requirements.
The database and its WAL/SHM sidecars must be treated as one live database;
copying only the main file while open is not a backup protocol.

Committed deliveries, execution metadata, bindings, uniqueness, and provenance
survive close/reopen and SQLite recovery after process failure. Opening does not
reset processing records or invent a recovery transition. Real on-disk restart
tests verify committed state and deduplication after reopening; they do not
simulate power loss or prove hardware durability.

## Runtime consumers

The webhook handler acknowledges only after InsertDelivery succeeds. The worker
owns claims, resource serialization, retries, recovery, and terminal recording.
The observer owns resource identity and current-state refetch. `internal/app`
opens one shared Store at `<data-dir>/workflow.sqlite` and closes it only after
worker cleanup and admitted HTTP handlers finish. See [runtime.md](runtime.md).
Storage itself does not choose a production location or interpret resources.

## First-activity provenance (#38)

The unchanged generic API now also stores `engineering_first_activity_v1` and
`engineering_activity_completion_v1`, keyed canonically per work item by
owner/repository/issue/number. Identical canonical v1 Evidence bytes contain
validated #37 branch evidence, the first qualifying current head, immutable
accepted push ReceivedAt in UTC and its calendar Start Date. Both facts retain
the original accepted push DeliveryID. No schema migration is needed.
Acceptance commits before external effects; completion commits only after all
currently owned effects succeed or are already observed converged. An accepted
fact alone never suppresses unfinished work or bypasses current eligibility.
Original event binding, push identity, timestamp/date, paired facts and canonical
JSON are validated on reads. Partial writes converge from fresh Project state;
external writes are not atomic with SQLite. See [execution.md](execution.md).

For #38 qualification, the push’s immutable Delivery.ReceivedAt must be strictly
after the validated #37 successful-creation provenance CreatedAt exposed by its
read-only Completion value. Processing-time completion alone is insufficient.
Earlier or equal-time notifications grant no activity and produce no new activity
provenance or lifecycle writes, including on restart or out-of-order processing.

## Engineering desired-state reconciliation storage boundary

Engineering composition reuses the unchanged branch and activity namespaces.
Read-only ActivityState validates accepted/paired completion facts; branch
completion without accepted activity returns empty activity state. It creates
no historical parent provenance. Accepted unfinished activity retains the original
push DeliveryID, including when CLOSED/no-Decision work cannot currently resume.

Ordinary Status/Roadmap Order convergence adds no batch provenance, reconciliation
completion record, replay log or schema migration. Retry recomputes current desired
state from complete Snapshot and fresh Project observations. Branch/activity keep
one-shot provenance because their authority cannot be safely derived from observed
GitHub state. No SQLite transaction is held across GitHub I/O; the Engineering
serialization lane is process-local rather than a distributed storage lock.

## Accepted PR onboarding provenance

`internal/pronboarding` uses the existing generic Store without a schema migration.
All namespaces share key
`<lowercase-owner>/<lowercase-repository>/pull_request/<positive-number>`:

| Namespace | Durable meaning |
| --- | --- |
| `pr_onboarding_membership_intent_v1` | Initial intent before adding missing membership |
| `pr_onboarding_prepared_v1` | Immutable preparation with exact current item and initial Status |
| `pr_onboarding_status_attempt_v1` | Committed before a required one-shot Status mutation |
| `pr_onboarding_status_done_v1` | Status stage satisfied after validated effect or permitted no-op |
| `pr_onboarding_draft_attempt_v1` | Committed before required Draft conversion |
| `pr_onboarding_draft_done_v1` | Draft stage satisfied after validated effect or permitted no-op |
| `pr_onboarding_completed_v1` | Both initialization stages satisfied and current membership validated |

Metadata is exact canonical `encoding/json` output for version-1 evidence, in
field order: `version`, `resource`, `binding`, `organization`, `project`, `field`,
`option`, `item`, `initially_draft`, `status_owned`, `status_present`, `status`.
Resource includes exact PR node ID as well as canonical owner/repository/kind/number;
the stable key excludes node ID. Binding retains #46 PR and Target identities,
Target kind, branch repository/ref/SHA, verified developer and policy identifier.
Organization and Project IDs, Status field and In Progress option must match
resolved deployment bindings. Preparation records exact item ID and initial
normalized Status evidence. No body, model output or credentials are persisted.

Membership intent is optional when membership already exists; it has empty item
and Status evidence. Preparation preserves its original Draft/ownership facts and
delivery association when present. Subsequent stages must match preparation bytes
and delivery exactly. Every record validates namespace/key, nonzero CreatedAt,
canonical bytes, version, identities and the original stored delivery's exact
resource binding (including node ID when supplied). Unknown/duplicate JSON keys,
noncanonical encoding, conflicting records or identity mismatch fail closed as
`ErrProvenance`; generic storage failures become `ErrStorage`. Original DeliveryID
is retained across retries/restart rather than reassociated to the latest event.

Status precedes Draft: Draft facts require `status_done`; completion requires
preparation, `status_done` and `draft_done`. Attempts exist only for required
mutations; done facts also represent allowed no-op stages. Successful completion
suppresses repeated initialization, while separately authorized membership repair
can add a replacement item without rewriting initial facts.

Attempts commit before one-shot mutations. Success facts commit only after
validated effects. GitHub and SQLite do not share a transaction: remote success
followed by lost response, cancellation or failed durable success recording can
leave an indeterminate outcome. An attempt without matching done fact returns
`ErrAmbiguous` before even membership repair; observed current Draft/Status cannot
attribute success or distinguish later review readiness. Such attempts are not
automatically recoverable and are never replayed as success. Exactly-once GitHub
mutation execution is not promised. See [execution](execution.md#accepted-pr-onboarding).

## PR review cycle provenance

`internal/prreview` reuses the existing SQLite provenance table and Store without
a schema migration. `Store.ProvenanceRecords(ctx, namespace)` enumerates a
validated namespace in stable-key order; the review owner interprets and validates
all records before use. Enumeration is evidence, not authorization.

All three namespaces use the per-cycle key
`<lowercase-owner>/<lowercase-repository>/pull_request/<number>/<ReadyForReviewEvent-ID>`.
The PR node ID remains in metadata, not the resource-key prefix. Distinct native
readiness IDs distinguish repeated cycles; a cycle has at most one rework effect.
Metadata is canonical Go JSON: decoding and re-encoding must reproduce the exact
stored bytes; start, effect and receipt evidence each require version 1.

| Namespace | Metadata fields | Meaning |
| --- | --- | --- |
| `pr_review_cycle_v1` | `version`, `resource`, `binding`, `project`, `field`, `onboarding`, `ready`, `previous` | Accepted review start before Status effects |
| `pr_review_draft_attempt_v1` | `version`, `start`, `review` | Exact cycle start plus qualifying CHANGES_REQUESTED submission; intent before Draft call |
| `pr_review_draft_done_v1` | `version`, `effect`, `draft` | Exact attempt plus validated native ConvertToDraftEvent receipt |

Start retains exact current PR/Target binding evidence (including developer,
branch/head and binding policy), configured Engineering Project/Status field,
validated onboarding completion (resource, binding, Project, original delivery
and completion time), qualifying native readiness and previous readiness ID.
The first cycle has empty `previous`. Later cycles must link the immediately
preceding cycle, retain compatible binding and identical onboarding, follow its
completed rework/native Draft transition strictly, and be recorded no earlier
than its completion receipt. Head SHA is freshly checked for effects; historical
cross-cycle binding compatibility does not require an unchanged head SHA.

Each provenance record retains its original accepted delivery association:
readiness delivery for start, qualifying review delivery for attempt and done.
Attempt and receipt use the same delivery. Loading validates the stored event's
resource, PR node/number, notification kind, actor and native timestamp selectors.
Record times must be nonzero UTC, not precede delivery receipt or their supporting
native evidence, and preserve start → attempt → done ordering. Readiness and review
submission may not postdate their originating delivery receipt. The supplied
handling event is also checked against immutable stored payload, event name, receipt time, sequence
and resource binding.

Attempt embeds the exact start and a CHANGES_REQUESTED review with exact PR,
valid author and submission time strictly after readiness. Done embeds the exact
attempt and a valid Draft event for that PR, after readiness, no earlier than
the review, and no later than receipt creation. Its actor may be null; a present
actor must have valid identity. Orphaned attempts/receipts, mismatched keys,
noncanonical metadata, conflicting identity, unsupported versions, missing
origins, broken history and inconsistent chronology are rejected. Fresh native
facts and current binding remain necessary; provenance is never a permanent grant.

Draft intent commits before mutation. Successful receipt requires current Draft
state and the exact immediately following native Draft transition plus matching
current latest-opinion evidence. An attempt without receipt is indeterminate,
including cancellation before the call, lost response or failure recording
success after the call. Observing Draft cannot prove who performed the historical
effect. Restart never replays that attempt or synthesizes done; unresolved
ambiguity needs manual investigation/recovery.

Cycle start or completed rework can support safe remaining PR/Target Status
convergence after partial writes, only while fresh authority and applicable native
cycle facts still agree. New native readiness stops older effects without granting
a new cycle until its notification is accepted. No Status completion record or
new provenance type is introduced. Onboarding normalization remains in its own
namespaces. SQLite and GitHub are not atomic and external mutation execution is
not exactly-once. See [review execution](execution.md#accepted-pr-review-and-rework)
for retry/terminal categories and worker restart behavior.
