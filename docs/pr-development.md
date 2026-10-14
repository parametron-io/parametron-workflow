# Accepted PR Development relationships

Issue #51 implements standalone deterministic `internal/prdevelopment` policy and
controller plus separate GitHub transport capabilities. Implementation commit
`4fba77a8e7b4c29d4aebe18b060d2f93c1b242e2` and independent test/fix commit
`22779b380b8a0e3a7c7af2f104ed1f7d5c18e0db` establish this boundary. Production-wide
invocation, automatic reconciliation on every supported webhook and complete
Phase #5 global orchestration remain deferred. No dedicated production worker
classification is composed for this controller.

## Authority and pure policy

`observation != authorization`. [#46 binding](pr-binding.md) supplies the current
accepted Target/branch/author identity; `internal/pronboarding.Completed` supplies
validated #47 completion evidence. Historical completion is not permanent
authorization. The controller separately requires current managed Engineering
membership for both PR and Target. Neither native links, `Refs:`, labels, Project
positions nor branch names alone select or replace the Target. Exactly one valid
`Target:` directive must agree with accepted binding. Cross-repository identities
retain repository owner/name/node ID, resource number and resource node ID.

`Evaluate(Input) ([]Action, error)` is pure. `Input` contains `Binding`
(`prbinding.Decision`), `Onboarding` (`pronboarding.Completion`), current `PR`,
current `Target`, `References` (`github.DevelopmentReferences`) and `Project`.
It validates accepted evidence, supported Task/Feature/Phase kind, completion
resource/Project/delivery/time/binding, exact current identities, author, head
repository/ref/SHA, lifecycle and parsed Target. Completion binding comparison
permits subsequent head SHA movement; the current PR must still match the fresh
binding SHA. Membership observation is the controller's separate responsibility.

For an eligible OPEN PR with OPEN Target:

- Correct canonical manual link: no-op.
- Missing canonical link: `Action{Add: true, Target: canonical}`.
- Contradictory manual links: `Action{Add: false, Target: observed}` removals.
- Mixed drift: remove contradictions before adding the missing canonical link.
- Correct keyword-derived canonical link: no redundant manual add.
- Any conflicting keyword-derived reference: fail closed, never remove it.

Removals sort by lowercased repository owner/name, zero-padded number, repository
node ID and Issue node ID. Canonical presence in either class suppresses the add.
Malformed/incomplete evidence, duplicate endpoints within a class, inconsistent
repository or endpoint identities and ambiguous Target evidence fail closed with
no plan. Identical manual/keyword overlap is allowed. Numbers alone are not
identities. Multiple accepted PRs may independently share one Target; acceptance
of one PR cannot authorize another. This is only the owned manual implementation
Development link class, not arbitrary native relationship editing.

## Narrow GitHub transport

`github.DevelopmentReader` exposes `DevelopmentReferences(ctx, pr)`.
`github.DevelopmentWriter` exposes `AddDevelopmentReference(ctx, pr, issue)` and
`RemoveDevelopmentReference(ctx, pr, issue)`. These remain separate from
`github.Client` and `github.Mutator`; policy receives no arbitrary GraphQL.

`DevelopmentReferences` carries exact `PR`, separate `Manual` and `Keyword`
Issue identity lists, and `Complete`. Transport fully paginates
`closingIssuesReferences` with `userLinkedOnly:true` for manual links and
`excludeUserLinked:true` for non-manual/keyword links. Strict Issue typename and
identity decoding rejects null/malformed nodes, incomplete pagination, duplicate
endpoints within a class, inconsistent repository names/node IDs and conflicting
endpoint numbers/node IDs. An Issue endpoint cannot reuse the PR node identity.
Validation includes consistency with the PR repository and authoritative Target
repository, including cross-repository Targets.

Reads preflight exact PR identity/state and repeat it after both connections;
lifecycle changes across those observations return typed Conflict. Writes require
an OPEN PR, exact current Issue identity and fresh complete reference observation.
Already-present manual adds and absent manual removals are transport no-ops;
keyword-only references are never removed. Target OPEN eligibility belongs to
policy, not this transport identity check.

Mutations are only `addCloseIssueReferences` and `removeCloseIssueReferences`,
with `issueId` and a single-element `pullRequestIds` input. The returned Issue
identity must exactly match the requested endpoint; null/mismatched receipts fail
closed. This validates a response, not an atomic relationship snapshot. Typed
GitHub errors and context cancellation/deadlines propagate with no hidden retries.
The controller observes convergence afterward. Local schema/wire fixtures do not
prove authenticated live API compatibility or deployed GitHub App permissions;
no live permission rehearsal or speculative permission configuration is claimed.

## Standalone controller

`prdevelopment.New(Config)` requires nonnil (including non-typed-nil) `Binding`,
`Onboarding`, `Reader`, `Relationships` and `Writer`. Their narrow methods are
binding `Evaluate`, onboarding `Completed`, current `ReadCurrent`, and the
Development read/write methods above. `Repositories` must be nonempty valid
`parametron-io` repositories without duplicate names or node IDs. `Project` and
`BugProject` must be distinct nonempty IDs without whitespace. Repository
configuration is copied. PR and Target must match exact configured repository
identities, not just names.

`Reconcile(ctx, storage.Resource)` accepts a configured, lowercase repository
resource of kind `pull_request`, positive number and nonempty node ID. One
context-aware lane serializes all calls on that controller instance, including
calls for different PRs. It provides no cross-instance/process or distributed
locking, GitHub transaction or atomic snapshot.

Each fresh barrier evaluates binding, obtains validated completion, reads current
PR and Target, checks exact resource/content identity, validates membership and
reads complete typed references. For each resource there must be exactly one
`engineering` profile with the configured Project ID and one nonarchived,
nonempty-ID item, plus exactly one `bug_tracker` profile with the configured
BugProject ID and no items. Missing/duplicate/unknown profiles, archived/invalid
items or Bug membership fail closed. The controller does not repair membership.

Before each effect, two independently fresh barriers must produce equal relevant
inputs (relationship ordering is normalized). Drift in authority, PR/Target,
completion, membership or relationships prevents the write. It executes only the
first action, then observes and plans again. Transport failures return immediately;
a later call retries from new observations and skips already-converged effects,
including remote success accompanied by a lost/error response. No cached plan or
durable relationship replay record supplies authority. Successful effects still
appearing in the next plan cause `ErrChanged` rather than repeated writes.
Persistent drift is bounded to 256 effects per call, then `ErrChanged`; this is
not a promise of convergence under concurrent external edits. Cancellation
propagates through reads, writes and lane waits. Invalid composition/identity,
rejected authority, changed state and ambiguous policy evidence use `ErrInvalid`,
`ErrRejected`, `ErrChanged` and `ErrAmbiguous`; typed transport errors propagate.

## Lifecycle and exclusions

OPEN accepted PRs reconcile only while all current prerequisites remain valid.
CLOSED or MERGED PRs receive no relationship writes, even if a historical link is
missing or contradictory. Evidence must still validate; malformed history does
not become permission to repair it. Existing history is preserved, not synthesized.
Closing without merge preserves traceability and does not prove Target completion.
Reopened PRs require fresh current binding and lifecycle prerequisites. An OPEN PR
with CLOSED Target is rejected; an old native link cannot grant fresh authority.

No Parent, dependency, Blocks/Blocked-By, SubIssues or Refs reconciliation is added.
PR companion numeric orchestration remains separate and deferred. No new review,
rework, merge, abandonment, Status, Draft or native Issue completion effect or
policy is introduced. Future production composition must preserve these ownership
boundaries; see [architecture](../system-overview.md#18-pull-request-binding-and-onboarding)
and [execution](execution.md#standalone-development-relationship-reconciliation).

## Committed verification and audit evidence

Inspected independent tests at `22779b3` include:

- [policy_test.go](../internal/prdevelopment/policy_test.go):
  `TestEvaluateCanonicalDevelopment` checks no-op/add/remove/order, keyword
  ownership and duplicates; `TestEvaluateDevelopmentAuthority` rejects missing or
  changed authority; `TestEvaluateHistoricalAndCrossRepository`,
  `TestEvaluateTargetRepositoryConflict` and `TestEvaluateRepositoryQualifiedNumbers`
  cover history and exact cross-repository identities.
- [controller_test.go](../internal/prdevelopment/controller_test.go):
  `TestDevelopmentControllerConvergenceAndRecovery` checks partial failures,
  uncertain success, cancellation and no replay after convergence;
  `TestDevelopmentControllerFreshness` and
  `TestDevelopmentMembershipRequiredAtEveryBarrier` reject stale evidence;
  `TestDevelopmentControllerHistoricalNoWrites` covers CLOSED/MERGED history.
  `TestDevelopmentControllerSerialization`,
  `TestDevelopmentCancellationWaitingForActiveReconciliation`,
  `TestDevelopmentReorderedBarriers`, `TestDevelopmentSharedTargetIndependentPRs`
  and `TestDevelopmentPersistentDriftIsBounded` cover concurrency, ordering,
  independent authority and the 256-effect limit.
- [integration_test.go](../internal/prdevelopment/integration_test.go):
  `TestDevelopmentConsumesValidatedOnboardingCompletion` composes real #46
  validation and #47 onboarding with temporary SQLite completion evidence. It
  asserts one Development write across repeated calls, no lifecycle writes, and
  rejection after current developer permission is revoked despite historical
  completion. GitHub observations and branch completion are fixture providers.
- [development_test.go](../internal/github/development_test.go):
  `TestDevelopmentReferencesValidate`, `TestDevelopmentReadAndMutationWire`,
  `TestDevelopmentReadPaginationAndMalformed`,
  `TestDevelopmentMutationPreflightIdentity`,
  `TestDevelopmentMutationRejectsFreshClosedPR`,
  `TestDevelopmentReadCrossRepositoryExactIdentity` and
  `TestDevelopmentWireFailuresNoRetry` verify identity, filters, complete pagination,
  exact inputs/receipts, lifecycle rejection and typed errors without retries.
  In-flight read/mutation and canceled-input tests verify cancellation/deadlines.

On 2026-10-14, the documentation audit reran these commands at HEAD `22779b3`:

```sh
nix develop --command go test -count=1 ./...
nix develop --command go test -race -count=1 ./internal/prdevelopment ./internal/github ./internal/prbinding ./internal/pronboarding ./internal/prreview ./internal/prcompletion ./internal/engineeringflow ./internal/app
nix develop --command go build -o /tmp/parametron-workflow-issue51-docs-check ./cmd/parametron-workflow
```

All passed, including existing binding/onboarding/review/completion regressions.
These are local unit, transport, integration and race checks, not authenticated
deployment tests. Production routing, distributed coordination and live GitHub App
permission verification remain outside this evidence. No verification matrix is
introduced.
