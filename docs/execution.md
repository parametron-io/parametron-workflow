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

Freshness/completeness of Input belongs to the caller. Implemented production
composition supplies fresh GitHub observation → complete Snapshot context →
roadmap PhaseOrder → lifecycle → reconciliation. #37 has no dependency on engineeringorder and does
not assemble context from #34 Resolve(root). That resolver is relevance-scoped:
resolving one of two unrelated OPEN Phases can omit its competitor. Its result
alone is not proof of complete activation input. Integration tests use explicitly
complete known fixtures and caller-supplied PhaseOrder at the standalone boundary;
production integration tests now additionally exercise Snapshot/policy/flow assembly.
At #37 standalone completion no app/worker/CLI composition was introduced;
current production composition is described below.

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
canonical encoding/json bytes. Same-target authorization retains v1 `Evidence`,
in this fixed field order:

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
it is not inferred from a later branch read. Both facts retain the original
authorization delivery, whose durable binding must match the trigger resource.
Reads verify namespace/key, canonical JSON,
version/domain/identity, base ref/SHA, and event identity. Unknown/duplicate JSON
keys, extra prose, inconsistent completion/reservation, and changed repository
bindings fail closed. Evidence and returned results contain values rather than
caller-owned mutable maps/slices/pointers. No Issue prose/title or provider error
text is persisted.

Production global branch convergence uses `EnsureFrom(ctx, Origin, Input)`.
Origin separates actual triggering DeliveryID/resource from the authorized target
Context.Root. A delivery can cause another currently eligible work item to receive
its branch; event observation never supplies eligibility. Same-target Ensure and
EnsureFrom retain legacy v1 bytes. Cross-target authorization uses canonical
`{version: 2, origin: {delivery_id, resource}, branch: Evidence}` metadata INSIDE
the existing `engineering_branch_authorization_v1` and
`engineering_branch_creation_v1` namespaces. The envelope binds the actual durable
trigger to exact target branch evidence; creation copies identical authorization
metadata. Both v1 and v2 are validated/readable, with matching paired metadata and
original event binding required. No schema migration/new namespace was introduced.

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
supplies worker error classification through the current app composition.
Cancellation is returned as an error, never successful authorization or
creation. A committed reservation can survive cancellation and be retried; a
remote success without completion remains ambiguous. If cancellation races a
committed completion, that durable fact remains valid and can be reused on retry.

## First authorized development activity

Issue #38 implements standalone `internal/engineeringactivity.New(Config)` and
`Handle(ctx, deliveryID, engineeringpolicy.Input)`. Config supplies configured
repositories, the resolved Engineering Project, the read-only #37 Completed
boundary, exact branch/comparison readers, a fresh normalized Project reader,
`github.LifecycleFields`, and the existing provenance Store. At #38 standalone
completion it added no app/CLI/worker or complete context assembly. The caller
must provide complete authoritative current Context and PhaseOrder; #38 calls
#35 Evaluate itself and acts only on Context.Root. Implemented production composition
supplies routing, complete activation context, PhaseOrder, reconciliation and
error classification.
Resolve(root) remains relevance-scoped and is not a complete activation snapshot.

`ResolvePush(ctx, repositories, Delivery)` returns a candidate Issue with an
applicable boolean, a non-applicable result, or ErrInvalid. Only event name
`push` and exact `refs/heads/development/issue-<positive-decimal-number>` apply.
Owner/name must match deployment case-insensitively and are returned lowercase.
The number must fit positive int64 and platform int, with no sign or leading
zeros. Name is checked against engineeringbranch.Name. Main, arbitrary branches,
tags and other event names are non-applicable; malformed candidate numbers or
repository identities fail. Duplicate keys at parsed identity levels fail.
Before/after SHAs, titles, commits and actor fields are ignored. Candidate
identity is notification evidence only. No generic commit event or ignored-push
worker state is introduced. Existing Issue/PR resolution remains unchanged.

The qualification rule is conjunctive:

1. Require a durable push delivery bound to the exact candidate resource.
2. Read validated completed #37 provenance through engineeringbranch.Completed.
   It shares Ensure's private strict decoder and paired-fact validator, checks
   both original durable delivery bindings, matching metadata/delivery, configured
   repository, work-item identity, exact name/base ref/SHA. Authorization alone
   is insufficient. No branch is created or adopted by the read API.
3. Require the candidate push’s immutable Delivery.ReceivedAt to be strictly
   after the validated successful-creation provenance CreatedAt. Completed returns
   only typed Completion (Evidence plus this UTC creation-completion time), never
   arbitrary storage provenance. Processing-time existence of #37 completion is
   insufficient: a push accepted on a manual lookalike before controller creation,
   or at the same acceptance time as completion, grants no activity. Reject it
   without activity facts or Status/Date/parent writes, even when the currently
   authorized branch has since advanced. Reloaded activity facts also validate
   this ordering for their original delivery.
4. For every unfinished attempt require the exact root #35 Decision and current
   ExecutionEligible. An active blocker, inactive parent or Create-Branch false
   suppresses all remaining effects, even with an accepted first-activity fact.
5. Refetch the exact authorized repository/name. Missing branch or head equal to
   BaseSHA grants no activity. Compare the exact BaseSHA/current HeadSHA through
   github.CommitComparison. Only validated strictly ahead ancestry qualifies;
   behind/diverged, unrelated/unverifiable history or malformed evidence fail
   closed. SHA inequality alone is never sufficient. Stale payload SHAs cannot
   override current reads. #38 never repairs deleted/replaced branches.
6. Read fresh normalized Engineering state for the target and any owned parent
   write. Require exact current Issue/node/repository evidence and exactly one
   nonarchived Project item. Missing/ambiguous identities return ErrProject;
   no membership is guessed or created.

### Activity facts and partial-write convergence

Two immutable namespaces share key
`<lowercase-owner>/<lowercase-repository>/issue/<number>`:
`engineering_first_activity_v1` records acceptance before any external write;
`engineering_activity_completion_v1` records successful completion afterwards.
Both carry identical canonical encoding/json bytes for Evidence in fixed order:
`version` (1), `branch` (the existing typed #37 Evidence), `head_sha` (first
qualifying current head), `received_at` (original Delivery.ReceivedAt in UTC),
`start_date` (that timestamp's UTC YYYY-MM-DD). Both retain the original accepted
push DeliveryID, and CreatedAt uses its ReceivedAt. Reads strictly validate
canonical bytes, version/domain/identity, original push candidate/binding and
immutable timestamp/date source. Unknown/duplicate keys and conflicting facts
fail ErrProvenance. No prose, commit messages, provider diagnostics, payload copy,
actor or mutable Project state is persisted. No migration is required.

The first committed acceptance wins permanently. Later/duplicate/out-of-order
notifications never change its head, date or delivery. Acceptance alone is not
completion and never bypasses unfinished current eligibility or branch checks.
Completion requires matching acceptance. Once complete, later notifications
return the historical result without further lifecycle writes; general drift
reconciliation belongs to #39. A completed transition is not a new start even
when a later head changes or the branch disappears.

Target In Progress is written only when its StatusOwned is true. A Task/Feature's
exact authoritative ParentPhase from #35 may also become In Progress only when
that parent's Decision owns Status. Native relationships, Project proximity and
Roadmap Order never select a parent. Direct Phase implementation progresses only
itself, with no second parent write. Set-Status false suppresses only that
resource's Status write; it neither changes eligibility nor blocks Start Date.
An already converged In Progress option is not rewritten. Observed manual
In Progress or Ready never creates activity provenance without qualification.

Start Date is initialization, separate from Status authority. If absent, use the
UTC calendar date of the immutable first accepted qualifying notification's
Delivery.ReceivedAt. Author timestamps and processing time have no authority.
Preserve any existing valid DATE value; never overwrite or clear it. Later
commits, delayed pushes, retries on later days, review/rework or later Status
changes cannot reset it after completion. Assignees always remain unchanged:
current Actor only carries a login and notification actors do not prove reliable
human identity plus repository assignability. No assignment capability or actor
provenance is introduced; existing assignees are preserved.

Status, Date and parent Status are independent external effects. No SQLite
transaction spans GitHub calls. After any failure/cancellation/storage failure,
restart reloads the original acceptance and revalidates current policy and branch,
then refetches Project state and writes only remaining differences. A Status
success followed by Date failure does not repeat converged Status; Date success
followed by parent failure preserves Date. Storage failure never manufactures
completion. Cancellation remains detectable and checks run before subsequent
effects and provenance writes. Callers own retries/recovery/classification, with
exclusive database/resource-lane ownership; one Controller serializes its calls.
There are no hidden retries or distributed effect locks.

Local stable errors are ErrInvalid, ErrProvenance, ErrStorage and ErrProject.
#35 and typed GitHub errors propagate. Focused integration tests use real
file-backed SQLite, #37 creation/completion, explicit complete #35 fixtures and
fresh Project-reader fakes. They prove restart/partial convergence and negative
manual-lookalike, base-only and blocked paths. They are standalone boundary tests,
preserving the package boundary; Stage 2 production composition tests additionally
exercise the integrated Engineering flow. PR lifecycle remains Phase #5.

## Production Engineering composition

Phase #4 is implemented: app composes engineeringflow with the shared Store,
resolved deployment, observer and narrow GitHub capabilities. CLI enables it by
default; embedded callers opt in with Config.Engineering, whose zero value
retains the foundation path. No semantic Runner
or provider is automatically activated and narrow Engineering capabilities do not
grant semantic mutation authority.

Engineering uses complete `ResolveSnapshot`, including every configured
repository's OPEN and CLOSED Issues (no PR nodes), accepted/native Type authority,
Bugs as graph facts, and all current declarative edges. It preserves the separate
root-scoped Resolve API. See [engineering-context.md](engineering-context.md) for
the E1/materialize/E2 consistency barrier, complete Context projection and
CheckCurrent; these reads are not transactional GitHub snapshot isolation.

```text
complete Snapshot → Snapshot.Context(trigger)
  → #36 BuildRoadmapOrder → #35 Evaluate → #36 Evaluate
  → #38 push handling / eligible unfinished-activity resume
  → new complete Snapshot if activity was handled
  → current field reconciliation (full pure computation + fresh Project comparison)
  → ANOTHER complete Snapshot and FULL #36/#35/#36 computation
  → #37 EnsureFrom fan-out to current eligible targets
```

After field convergence, the fresh full computation is a pre-branch
authorization barrier. Fresh blockers, Phase activation, Create-Branch directives,
roadmap hierarchy, segment capacity, and direct-child capacity can stop all branch
authorization before EnsureFrom. Every target is projected from the SAME fresh
Snapshot and receives the validated fresh PhaseOrder. #37 still independently
reevaluates exact target eligibility; observed Status/Roadmap values cannot
substitute for this pipeline.

### Read-only activity state and Status convergence

`engineeringactivity.ActivityState(ctx, resource)` exposes validated read-only
State. Completed #37 branch with no accepted first activity yields EMPTY state,
not work-started authority. Only acceptance populates WorkItem, ORIGINAL push
DeliveryID, and activity Evidence. Valid paired completion sets Completed=true;
completion without acceptance, corrupt/mismatched facts, or orphan activity without
branch completion fails closed. This reader performs no branch/commit/GitHub
mutation work.

Accepted unfinished activity is not an In Progress overlay. Production can resume
it only when the target has a current #35 Decision, using the ORIGINAL accepted
push DeliveryID and unchanged #38 eligibility/branch checks. CLOSED/no-Decision
acceptance remains durable, skips Handle, supplies no completion/Status authority,
and allows normal roadmap clearing/compaction. Completion is never fabricated.

Validated COMPLETED first activity overlays desired In Progress for an OPEN exact
work item, even after a new blocker, inactive parent Phase or Create-Branch false.
Those facts deny NEW authorization/start, not an already completed start. Manual
In Progress supplies no activity authority. #35 stays pure Backlog/Blocked/Ready;
each actual Status write requires its own Set-Status, and CLOSED work has no
Status reconciliation.

An OPEN Phase is desired In Progress from its own completed activity OR any CURRENT
direct Task/Feature child's completed activity, even if the child is CLOSED.
Bugs and non-direct/nested descendants do not contribute. Reparenting moves the
contribution to the current authoritative parent; the old parent loses it unless
another qualifying contribution exists. Parent Set-Status independently gates the
write. No historical parent identity was added to #38 provenance. Assignees stay
unchanged because reliable human/assignability evidence is unavailable.

### Field drift and partial failure

engineeringreconcile consumes EXACT #36 Assignments/Clear and #35 Status decisions
plus the completed-activity overlay. It refetches the entire target union and
validates Snapshot.CheckCurrent plus exact nonarchived Engineering item/field
identity before writes. Missing/stale numbers converge; matching numbers no-op;
finite zero/fractional/out-of-domain numbers are drift. Clear truly unsets a field,
never writes zero. Root/direct-child compaction, standalone roots and CLOSED
Phase/open-child retained segments follow #36. Roadmap Order is independent of
Status and Set-Position false. No native Project position/afterId/movement exists.
See [engineering-ordering.md](engineering-ordering.md) and
[github-integration.md](github-integration.md) for the narrow number capability.

Failure → retry → fresh complete Snapshot → fresh Project observations → fresh
desired state → only remaining/current differences. Already successful effects
become no-ops when current observations prove convergence. Changed topology wins
over old plans. This is desired-state reconciliation, with no batch provenance,
reconciliation completion record, replay log, or SQLite schema migration.
#37/#38 keep their one-shot provenance because observation alone cannot reconstruct
its authority. No branch adoption, deletion, revocation, or completed-branch
recreation was added.

### Routing, lane, and semantic coexistence

Production routes canonical configured development push and bounded configured
Engineering Project-item notifications; see [webhooks.md](webhooks.md). Payload
fields remain candidate notification evidence. `worker.ErrNotApplicable` settles
claimed work Completed without binding a fake Resource, calling Processor, or
persisting a failure category, then continues later eligible work in the SAME Step.
Cancellation still releases interrupted claims to Pending and recovery is preserved.
This is routing behavior, not lifecycle policy; no ignored-state schema was added.

One context-aware deployment-wide process-local Engineering lane is acquired
BEFORE complete state computation. It covers downstream semantic mutation when
explicitly composed, #38 activity, reconciliation and branch work, releases on all
exits, and holds no SQLite transaction across GitHub I/O. Worker FIFO is only
same-resource; it cannot serialize multi-resource plans alone. The lane is not
a distributed lock or cross-process guarantee.

Runner/model enrichment executes OUTSIDE the lane. Accepted downstream semantic
mutation executes INSIDE it before Engineering when an explicit Mutator or custom
SemanticConsumer authorizes that path. Runner-only composition still uses
SemanticFoundationSink; no implicit semantic GitHub mutation or Snapshot-wide
semantic backfill exists. Enrichment failure stops that delivery before Engineering.
PR semantic enrichment may run outside the lane; its semantic consumer and
accepted onboarding execute inside the same Engineering lane. Managed PR
Project-item notifications skip semantic enrichment and enter onboarding directly.

Engineering-local engineering_changed and engineering_incomplete retry. Generic
engineering_activity_storage and engineering_branch_storage are terminal; structural
graph/policy/order/provenance/Project/configuration corruption is terminal with
bounded categories. Existing typed GitHub transient/rate-limit handling remains
separate; no provider/storage diagnostics leak into stable categories.
[#48 pure PR companion ordering](pr-companion-ordering.md) is implemented and
independently tested. Production companion composition, numeric convergence,
ownership-provenance acquisition and orchestration remain deferred.
Later Phase #5 work retains relationships and continuous/global orchestration.
Nested lifecycle, Bug Tracker scoring/admission,
heavy agents, automatic merge, and distributed coordination remain deferred.

## Accepted PR onboarding

Production `internal/engineeringflow` routes PR notifications and relevant managed
Project-item candidates to `internal/pronboarding`. #46 `internal/prbinding` owns
authorization; onboarding repeatedly revalidates accepted binding and current PR
observation around consequential effects. Stored evidence, payload state, matching
branch names and observed membership are not grants. Exact PR/Target identity,
current author/head, OPEN state and configured Project/item/content identity must
remain valid. Rejected binding cannot create accepted onboarding side effects.

The worker retains exclusive database ownership and per-resource FIFO. One
process-local Engineering lane serializes Issue and PR effects across resources;
the onboarding controller has its own context-aware lane for standalone calls.
Model enrichment stays outside the Engineering lane, while the semantic consumer
and onboarding run inside it. An enrichment/consumer failure stops the delivery.
No Runner is required for deterministic onboarding, and model output owns no
workflow authorization. No distributed lock or cross-process atomicity is added.

Membership can converge from fresh accepted binding after interruption or durable
completion. Initial owned In Progress and Draft normalization instead use immutable
preparation, attempt and success facts. Completed onboarding never overwrites
later Status or reverts subsequent Ready-for-review. `Set-Status: false` suppresses
normal initial Status ownership only. Already-Draft and already matching Status
need no redundant mutation. See [provenance](persistence.md#accepted-pr-onboarding-provenance)
for exact ordering, original delivery retention and strict identity validation.

Cancellation releases worker claims to pending; restart recovery preserves facts
and revalidates current authority. Safe unfinished stages can resume, but a durable
one-shot attempt without success is `ErrAmbiguous`, even when current state matches.
Remote success followed by failed success recording is indeterminate; retry must
not fabricate completion or repeat that mutation. GitHub/SQLite atomicity and
exactly-once mutation execution are not provided.

`internal/app` classifies onboarding errors as follows:

| Error | Category | Worker disposition |
| --- | --- | --- |
| `ErrInvalid` | `pr_onboarding_invalid` | Terminal: invalid composition/identity |
| `ErrRejected` | `pr_onboarding_rejected` | Terminal: rejected current binding |
| `ErrChanged` | `pr_onboarding_changed` | Retry from fresh authority/current state |
| `ErrProject` | `pr_onboarding_project` | Terminal: conflicting Project evidence |
| `ErrProvenance` | `pr_onboarding_provenance` | Terminal: corrupt/conflicting durable facts |
| `ErrAmbiguous` | `pr_onboarding_ambiguous` | Terminal: indeterminate one-shot outcome |
| `ErrStorage` | `pr_onboarding_storage` | Terminal: unclassified storage failure |

`prbinding.ErrInvalid` is terminal `pr_binding_invalid`. Propagated errors retain
existing classification; typed GitHub transient/rate-limit errors retry, and
cancellation follows worker release semantics. A transport retry can encounter a
persisted attempt and then fail closed with `ErrAmbiguous`; it is not automatic
convergence. No recovery operation synthesizes successful provenance.

Relationship projection, production companion integration and numeric
reconciliation remain deferred. Separate #50 completion and terminal
synchronization composition is described below.

## Accepted PR review and rework

Production `internal/engineeringflow` consumes native readiness and supported
review-submission notifications through `internal/prreview`, inside the existing
Engineering lane and durable worker boundary. Ordinary PR notifications retain
onboarding, then call review `Converge`; that method repairs Status only and never
starts a cycle or attempts Draft conversion. Review routing bypasses semantic
enrichment and requires no Runner. #46 retains binding authority; #47 exports
validated historical `Completed` evidence, not a new permanent grant.

Before authorization and each effect, review reads current binding, onboarding
completion, PR, OPEN Target, exact nonarchived Engineering items and native review
facts. It repeats binding, PR/Target and review reads and rejects changes across
them. PR and Target must each have one Engineering item and no Bug Tracker item;
item content identities are checked through their narrow readers. Current body
directives independently determine normal PR/Target `Set-Status` ownership.
Stored binding identity remains fixed across cycles while current head changes
are freshly validated; stored evidence never replaces current authorization.
These reads provide no atomic GitHub snapshot.

The [review policy](../system-overview.md#20-in-progress-to-in-review) requires
an unambiguous developer readiness transition after completed onboarding and
strict native transition ordering. COMMENTED and APPROVED initiate no effects.
Only a matching active-cycle CHANGES_REQUESTED submission still present in native
latest-opinion evidence authorizes rework. Dismissed or superseded submissions
cannot newly authorize it. Completed rework is historical evidence even after
later dismissal; dismissal itself introduces no reversal policy.

Cycle start is persisted before In Review convergence. Rework persists Draft
intent before the narrow conversion call, then validates current Draft state,
the immediately following native Draft transition and the qualifying current
opinion before recording completion. Only then does it converge owned Status to
In Progress. Each PR and Target Status effect gets fresh authority separately;
failure between them can leave partial convergence. Duplicates can repair current
drift for the applicable cycle, without repeating completed Draft effects.
See [provenance](persistence.md#pr-review-cycle-provenance) for the stored contract.

Ordinary Engineering reconciliation applies review's exact Target overlay after
pure lifecycle computation and rechecks current Snapshot/item facts before writing.
The first-activity field wrapper independently suppresses In Progress writes for
active In Review, and checks Snapshot/item/directive drift before retained writes.
Both suppress older effects when newer native readiness supersedes a cycle, but
do not adopt that readiness without its explicit accepted delivery. PR `Converge`
treats supersession as a safe no-op; an older duplicate delivery attempting
convergence may return terminal `ErrSuperseded`, allowing worker FIFO to advance
to the new readiness delivery. A parent Phase keeps its own lifecycle policy.
Review does not change Start Date, assignees, accepted Target association, branch
identity or native review history. Competing PR evidence for the same Target is
conservatively rejected; this is not general multi-PR arbitration.

Cancellation follows worker claim-release semantics. Restart reloads validated
cycle history and resumes applicable Status drift with fresh authorization.
If Draft intent exists without a valid completion receipt, neither retries nor
the current Draft flag can establish success or safely replay the call. This
returns `ErrAmbiguous` when authority is otherwise valid and requires manual
investigation/recovery; no automatic recovery operation synthesizes receipts.
Even a retryable transport failure can leave such an attempt and become terminal
ambiguity on retry. Failure to persist intent prevents the conversion call.
There is no GitHub/SQLite transaction or exactly-once external mutation guarantee.

`internal/app` uses these review categories:

| Error | Category | Worker disposition |
| --- | --- | --- |
| `prreview.ErrChanged`, `github.ErrReviewFactsChanged` | `pr_review_changed` | Retry from fresh state |
| `prreview.ErrInvalid` | `pr_review_invalid` | Terminal: composition/identity |
| `prreview.ErrRejected` | `pr_review_rejected` | Terminal: rejected authority |
| `prreview.ErrSuperseded` | `pr_review_superseded` | Terminal: older cycle superseded |
| `prreview.ErrCorrelation` | `pr_review_correlation` | Terminal: ambiguous native correlation |
| `prreview.ErrProvenance` | `pr_review_provenance` | Terminal: corrupt/conflicting evidence |
| `prreview.ErrAmbiguous` | `pr_review_ambiguous` | Terminal: indeterminate Draft outcome |
| `prreview.ErrProject` | `pr_review_project` | Terminal: conflicting Project evidence |
| `prreview.ErrStorage` | `pr_review_storage` | Terminal: unclassified storage failure |

Propagated binding/onboarding errors retain their existing classification.
Typed GitHub transient/rate-limit errors retry; other GitHub categories remain
terminal. Cancellation retains its separate pending/recovery handling.
Local tests cover repeated cycles, duplicates/reordering, current authorization
drift, partial writes, cancellation/restart, corrupt history and ambiguous effects.
Transport/GraphQL fixtures validate bounded local behavior, not authenticated live
API execution or deployed GitHub App grants. Merge/completion and terminal
synchronization remain outside the #49 review controller and are implemented by
the separate #50 composition below. Relationships, managed feedback and broader
continuous/global orchestration remain deferred.

## Accepted PR completion and terminal synchronization

`internal/prcompletion` and `internal/engineeringterminal` implement #50 in the
existing process-local Engineering lane. PR completion routing runs before stale
onboarding/review effects, including for delayed review notifications. Current
native state selects the path, never webhook payload state. A valid OPEN PR
without Done drift continues through existing accepted onboarding/review contracts.
An OPEN PR with Done drift requires completed onboarding, fresh accepted #46
binding and validated canonical PR review Status before correction; correction
alone does not replay onboarding or Draft effects.

For an accepted authoritatively verified MERGED implementation PR, PR Status
converges to Done and the normal Task/Feature Target may be natively closed.
Terminal Issue synchronization then projects valid CLOSED state to Done. Approval
alone grants no completion. A CLOSED PR without merge is abandonment: PR Done,
Target remains OPEN and returns to In Progress where ordinary Set-Status ownership
permits. A validated current replacement review in In Review wins over abandonment
rollback. Durable branch/start provenance is retained; a replacement PR needs its
own accepted authority. A direct Phase implementation PR requires explicit
`Create-Branch: true`; merge completes only that attempt and never closes the
Phase, even if all children are CLOSED.

Repository-native Issue Close is authoritative for manual completion. Project
Done is terminal-state projection, never closure intent. Valid native CLOSED
Issues converge to Done; OPEN Issues with Done stay OPEN and receive canonical
nonterminal Status, including after native reopen, delayed Project delivery or
restart. These consistency corrections apply with `Set-Status: false`; ordinary
Backlog/Blocked/Ready/In Progress/In Review transitions retain normal ownership.
Reopen reuses #35/#36 policy, validated completed activity and accepted
implementation/#49 review/rework evidence, never unconditional Ready. Native
close/reopen intervals invalidate stale review effects; fresh supported readiness
with completed rework may resume review, while ambiguous continuation fails closed.

The authoritative complete declarative Parent graph, including cross-repository
direct children, gates Phase closure. All direct children CLOSED permits explicit
maintainer native Close; it triggers no automatic close. Invalid native Phase
closure with an OPEN direct child is corrected by native reopen followed by a
freshly computed nonterminal Status. An OPEN Phase with Done is corrected even
when all children are CLOSED. The typed `open_direct_children` rejection is
feedback-ready; no managed feedback comment is emitted. The mechanical gate does
not replace maintainer Verification Standard responsibility. Nested lifecycle is
deferred.

Completion revalidates durable accepted onboarding against exact current PR,
Target, branch, author/developer eligibility, Type, nonarchived Engineering items
and native terminal timeline evidence. The terminal transition must follow
onboarding completion. #46's binding validator remains OPEN-only. Fresh primary,
Project and complete graph barriers precede consequential writes; changed facts
stop the attempt for recomputation. After native writes, new snapshots drive
terminal consistency and existing #35/#36 dependency/numeric roadmap reconciliation.
No separate downstream ordering authority is introduced.

Issue terminal consistency runs before ordinary Engineering convergence. It does
not wait for optional semantic execution. For supported Issue notifications,
configured semantic enrichment/consumption remains available after correction;
missing Type authority may take that path, but an incomplete graph authorizes no
terminal effect. Push and Project-item routing retain their semantic exclusions.
Terminal PR handling takes precedence over onboarding/review routing. This is
bounded production integration, not general polling or full Phase #5 global
orchestration.

Partial Project writes retry against fresh current drift. Native merge-close is
more conservative: an immutable attempt precedes the mutation and a validated
response receipt precedes completed provenance. A completed receipt prevents the
same historical merge from reclosing a later explicitly reopened Target. An
attempt without a valid completed receipt is indeterminate, even if current native
state is CLOSED; retries/restart never infer success or issue another close.
Lost responses, cancellation after attempt/close, and receipt persistence failure
may require manual investigation. Native reopen evidence at or after the merge
also blocks historical close authority. See [close provenance](persistence.md#pr-completion-close-provenance).
GitHub and SQLite are not atomic and exactly-once external execution is not guaranteed.

The worker classifier adds these stable local categories:

| Error | Category | Handling |
| --- | --- | --- |
| `prcompletion.ErrChanged` | `pr_completion_changed` | Retry fresh state |
| `prcompletion.ErrInvalid` | `pr_completion_invalid` | Terminal invalid composition/identity |
| `prcompletion.ErrRejected` | `pr_completion_rejected` | Terminal rejected authority |
| `prcompletion.ErrProject` | `pr_completion_project` | Terminal conflicting Project identity |
| `prcompletion.ErrProvenance` | `pr_completion_provenance` | Terminal corrupt/conflicting facts |
| `prcompletion.ErrAmbiguous` | `pr_completion_ambiguous` | Terminal indeterminate close |
| `prcompletion.ErrStorage` | `pr_completion_storage` | Terminal unclassified storage failure |
| `prcompletion.ErrReopened` | `pr_completion_reopened` | Terminal Target reopen after merge |
| `engineeringterminal.ErrInvalid` | `engineering_terminal_invalid` | Terminal invalid graph/state |

Propagated context, onboarding, binding, review and GitHub errors retain their
existing handling. Typed GitHub transient/rate-limit failures retry; other typed
GitHub categories are terminal. Cancellation retains pending/recovery behavior,
but never clears an indeterminate close attempt or fabricates its receipt.

Committed local tests cover merge versus abandonment, ownership exceptions,
direct Phase merge without objective closure, cross-repository direct-child gates,
invalid Phase-close recovery, native-open Done drift, changed membership,
partial Project writes, exact transport identities/pagination/errors/cancellation,
adversarial provenance, lost responses/receipts, cancellation and SQLite restart,
later native reopen, stale reviews and fresh supported review continuation.
Production worker fixtures exercise merge/restart/reopen, manual Done drift,
invalid Phase close and direct Phase implementation. These establish local
contracts, not authenticated live GitHub permissions or end-to-end deployment proof.
Companion production allocation/numeric convergence, Development projection,
managed feedback, nested lifecycle, Bug Tracker policy, automatic merge, branch
deletion/revocation, continuous/global orchestration and distributed coordination
remain deferred.
