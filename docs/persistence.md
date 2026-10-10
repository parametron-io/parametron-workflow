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

`RecordProvenance` stores namespace, stable key, associated delivery ID, opaque
metadata bytes, and caller-supplied creation time. A foreign key requires the
delivery to exist. Resource association is available through that event's binding.
The `(namespace, stable_key)` primary key is database-wide: callers own meaningful
stable keys and namespaces. Equivalent duplicates return `false, nil` and retain
the original creation time; conflicting delivery association or metadata returns
`ErrConflict`. `Provenance` looks up that key. Records can commit atomically with
processing metadata and resource binding changes.

This surface records generic one-shot facts only. Branch creation, PR onboarding,
managed feedback, and their authorization/recovery protocols remain later work.
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
