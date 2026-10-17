# Managed rejection feedback

Issue #52 implements and independently tests a standalone consumer in
`internal/workflowfeedback` and narrow GitHub comment transport. That standalone
milestone remains distinct from Issue #53's current production composition:
`internal/engineeringfeedback` assembles supported typed current evidence and the
app/worker invokes the existing consumer. The consumer owns comment reconciliation,
not lifecycle authorization.

## Status and ownership

Feedback owns rejection-to-explanation mapping and safe managed-comment
reconciliation only. `internal/prbinding` retains Target, branch and developer
authorization; `internal/prcompanion` retains companion eligibility/capacity;
`internal/engineeringterminal` retains Phase closure decisions; and
`internal/prdevelopment` retains Development projection. Existing lifecycle
policy and Engineering controllers retain Status, execution and lifecycle
authority. Feedback neither grants authorization nor reverses legitimate
reconciliation when posting fails. See [architecture](../system-overview.md#29-rejected-intent-and-managed-feedback).

## Typed producer adapters

Adapters accept bounded typed decisions, not webhook observations or operational
errors. The caller must establish current authoritative producer evidence for
the exact resource; adapter validation is not a substitute for that provenance.

| Adapter | Accepted authority and explanation |
| --- | --- |
| `BindingRejection` | A rejected `prbinding.Decision` with empty accepted Evidence, for a PR. Target reasons cover invalid/missing directives, outside-deployment, unsupported and ineligible Targets. Branch reasons cover missing/invalid provenance, missing head and head mismatch. Developer reasons cover invalid identity, inactive membership, insufficient permission and assignee mismatch. Templates explain the rejected binding and required Target/branch/author correction. Accepted binding is invalid input. Changed state, unresolved Target and missing PR return `ErrStale`; unknown reasons return `ErrInvalid`. |
| `CapacityRejection` | Complete current `prcompanion.Input` must evaluate to `ErrCapacity`; that error alone is insufficient. The exact PR must be OPEN and accepted, and its validated Target must have 10..10000 eligible OPEN companions. A different Target's capacity failure cannot authorize feedback. The message asks for nine or fewer eligible companions. |
| `TerminalCorrection` | Validated `engineeringterminal.CorrectionEvidence` for exact current unauthorized native closure, open-child Phase closure or native-open Project Done correction. Current producer relevance and durable correction state are required; no human-action attribution is guessed. |
| `ClosureRejection` | An explicit `engineeringterminal.Rejection` for the exact Issue with `OpenChildren`. The explanation asks for required direct children to be closed before another Phase closure attempt. Ordinary observed Done or CLOSED state supplies no such authority. |
| `DevelopmentCorrection` | The exact PR and a removal action actually returned by `prdevelopment.Evaluate` on validated current input. Accepted binding, onboarding and complete relationship evidence remain producer requirements. Contradictory manual removals explain the accepted Target; ordinary canonical missing-link additions remain silent. The message describes selection for correction, not proof that a removal succeeded. |
| `LifecycleRejection` | Issue-only `ManualRejection` with `ActiveBlocker`, `InactiveParent` or `MissingExecutionAuthorization`. Templates describe the rejected manual advancement/execution and the prerequisite to satisfy. This is a bounded integration contract; the explicit lifecycle producer is not implemented. |

The future `ManualRejection` caller must verify explicit rejected human intent,
current policy and correction ownership. Project Status drift alone cannot
establish that authority. Existing lifecycle packages do not emit a complete
explicit manual-rejection signal. Unknown, stale, incomplete or unavailable
evidence must not become a user-facing rejection reason. Raw provider and
transport messages are never rendered as workflow reasons.

## Complete current feedback contract

`Current` contains `Resource`, `Complete` and `Violations`. Resource is a
`github.CommentResource`: exact repository owner/name/node ID, resource number
and node ID, plus `issue` or `pull_request` conversation kind. Each violation
must match that exact resource. `Complete` must be true even for an empty set.

`Refresh.CurrentFeedback(ctx, resource)` must reevaluate the complete current
effective rejection set, including withdrawal or resolution of previously
reported constraints. It must not replay lifecycle effects. Historical decisions,
webhook payloads, observed Status and comment contents independently grant no
authority. Incomplete evidence is an error, not evidence of resolution.

Private violation fields constrain construction; rendering revalidates
category-specific evidence. Inputs are limited to 256 violations before
deduplication. Canonical sorting uses category, detail, lowercased related
repository owner/name, padded resource number, repository node ID and resource
node ID. Exact duplicate violations are removed without mutating caller input.

## Message rendering

`Render` uses fixed templates with no LLM or model-provider dependency. The
canonical marker is:

```text
<!-- parametron-workflow:managed-feedback:v1 -->
```

One combined comment contains simultaneous current violations in stable order:
binding Target, branch, developer, capacity, lifecycle, Phase closure, Development.
Each explanation states the rejection/correction, policy constraint and valid next
action. Output is bounded to 60000 bytes. Equivalent canonical evidence produces
identical content; unchanged content causes no write.

An empty complete set renders non-active text: previously reported constraints
are resolved and no current rejection is reported. It does not assert workflow
acceptance or completion. With no existing managed comment, this produces no
comment. With an existing verified comment, it replaces active content rather
than deleting history or repeating obsolete rejections. Ordinary successful
reconciliation and Roadmap Order housekeeping remain quiet.

## Comment identity and ownership

Only Issue/PR conversation `IssueComment` objects are supported, never review
comments. Ownership requires the exact resource, nonempty comment node ID,
positive numeric database ID, trusted controller author stable node ID, login
(case-insensitive comparison) and account type (`User` or `Bot`), plus the
canonical versioned marker. `AuthorIdentity.FeedbackAuthor` supplies trusted
configured/current author evidence; credential author identity is not inferred.

Complete live enumeration must establish at most one candidate. The controller
requires the marker at the start followed by a newline, exactly once, and then
relooks up the selected comment and compares the entire record. A marker alone
never authorizes mutation. Unrelated human/bot comments, including copied
markers, are preserved. A controller-authored comment containing the managed
marker namespace with malformed, truncated or unknown-version marker fails
closed. Duplicate candidates or duplicate node/numeric identities, incomplete
lists, malformed authors and wrong resources also fail closed. An author sharing
the expected ID or login but disagreeing on full identity causes `ErrOwnership`.

Transport validates HTTPS origin with host exactly `github.com`, no userinfo or
query, exact repository/resource path (`/issues/N` or `/pull/N`), and
`#issuecomment-<databaseId>`. Foreign hosts, alternate ports and review paths are
rejected. GitHub Enterprise comment-origin configuration is not implemented.

## GitHub transport

`github.CommentReader` exposes `IssueComments` and exact `IssueComment` reads;
`github.CommentWriter` exposes only `CreateIssueComment` and
`UpdateIssueComment`. These interfaces are separate from broader `Client` and
`Mutator` responsibilities. They expose no arbitrary GraphQL or lifecycle,
Project, branch, review or relationship mutations.

Enumeration preflights native resource identity and repeats it after all pages.
Pages contain at most 100 comments, require exact parent identity/type and stable
total counts, and are bounded to 100000 total comments. Missing metadata, null
nodes, invalid continuations/cycles, duplicate identities and final count
mismatch fail rather than returning partial evidence of absence. Stable totals
do not make the enumeration an atomic snapshot.

Exact lookup validates the returned node ID and normalized comment identity.
Create uses GraphQL `addComment` with `subjectId` and body; update uses
`updateIssueComment` with comment ID and body. Update first relooks up the full
expected comment and rejects changes with Conflict. Writes preflight native
resource identity; receipts must contain the exact submitted body, valid comment
and author evidence, and the requested comment ID for update. The controller
separately checks the expected author and post-write live record. Transport
validates URL/identity evidence; marker ownership belongs to the controller.

Typed `github.Error` categories include NotFound, Unauthorized, Forbidden,
RateLimited, Conflict, Malformed, Transient and Permanent. Context cancellation
and deadlines propagate. No hidden blind retries occur. Local HTTP fixtures
verify wire behavior, not authenticated live GitHub API compatibility or deployed
GitHub App permissions. See [GitHub integration](github-integration.md#managed-feedback-comment-capabilities).

## Reconciliation and recovery

`New(Config)` requires nonnil (including non-typed-nil) Refresh, Author, Reader
and Writer dependencies. `Reconcile` validates the resource and acquires one of
64 context-aware striped lanes on that controller instance.

| Current state | Effect |
| --- | --- |
| No violations and no managed comment | No-op |
| Current violations and no managed comment | Create |
| Existing verified comment with identical effective body | No-op |
| Changed effective violations | Update the verified comment |
| Fully resolved violations with an existing verified comment | Update to non-active resolved content |

The controller renders fresh authority, obtains the trusted author and performs
complete comment lookup with exact candidate relookup. It refreshes authority
again before deciding no-op. Before a write it repeats complete lookup, compares
comment observations, reverifies author identity and refreshes authority a third
time. Changed effective content or comment evidence blocks the write. Successful
receipts are checked against fresh enumeration/exact lookup, followed by another
authority refresh. Withdrawal after a successful write returns `ErrChanged`;
a later call can resolve the same comment.

An inaccessible or deleted exact candidate returns the transport error without
speculative overwrite/create in that call. A later complete enumeration can
establish absence and permit creation if current violations remain. A lost
response, canceled write or malformed receipt leaves the remote outcome unknown.
Retry starts from live authority and comments rather than replaying a stored plan;
tests demonstrate convergence after remote success with lost responses and after
controller reconstruction. No persistent feedback decision log or SQLite schema
is introduced.

Serialization applies within one controller instance only. Independent
controllers/processes require external coordination; distributed exactly-once
creation, global uniqueness and atomic GitHub policy/comment snapshots are not
guaranteed. Freshness barriers detect observed changes, not all concurrent edits.

## Errors and safety boundaries

Package errors are `ErrInvalid` (invalid authority/identity), `ErrStale` (stale
binding authority), `ErrChanged` (changed effective facts), `ErrAmbiguous`
(ambiguous comment evidence) and `ErrOwnership` (unverified ownership). Producer,
refresh, author and typed transport errors propagate; cancellation is honored
through dependencies and lane waits. Errors stop feedback rather than granting
workflow authorization. A failure after a remote write does not prove rollback.
There is no production worker feedback error classification composed here.

## Committed verification evidence

Implementation commit `e730fc8bdba50c186e759c969f38800a28ea59bc` and independent
test/hardening commit `d67518c0a45b788b5dff6dec942e4111ae30a7c2` establish this
standalone boundary. The latter also tightens origin, author-conflict, marker and
malformed-authority validation.

- [policy_test.go](../internal/workflowfeedback/policy_test.go) covers typed
  adapters, malformed authority, bounds, canonical ordering/deduplication and
  actionable rendering; [development_test.go](../internal/workflowfeedback/development_test.go)
  requires validated contradictory removals.
- [controller_test.go](../internal/workflowfeedback/controller_test.go) covers
  exact ownership, no-op/resolution, freshness/withdrawal, uncertain writes,
  restart recovery, cancellation and same-instance concurrent deliveries.
- [comments_test.go](../internal/github/comments_test.go) covers exact resource,
  author/comment identity, origin, pagination completeness, typed failures,
  request/receipt validation and cancellation.
- [integration_test.go](../internal/workflowfeedback/integration_test.go) uses
  local HTTP with the real comment transport for both conversation kinds,
  lost create/update responses, deleted update targets, cancellation and malformed
  receipts. Unexpected non-comment mutations fail the fixture.

The independent test task reported successful runs of the following commands;
this historical report is distinct from subsequent documentation validation:

```sh
nix develop --command go test ./internal/workflowfeedback ./internal/github
nix develop --command go test -shuffle=on -count=20 ./internal/workflowfeedback ./internal/github
nix develop --command go test -race ./internal/workflowfeedback ./internal/github
nix develop --command go test ./...
nix develop --command go build ./cmd/parametron-workflow
```

These are local checks, not authenticated live tests.

## Production integration and remaining limits

`app.composeEngineering` constructs `engineeringfeedback.Refresh`, trusted author
verification and `workflowfeedback.Controller` using narrow capabilities on the
shared client. Supported current production producers are PR binding rejection,
companion capacity rejection, Development correction and Engineering terminal
correction (including Phase open-child rejection). The assembler independently
refreshes complete applicable evidence; incomplete/changed facts are errors, not
proof that warnings resolved. It skips unsupported Bug Tracker Targets.

Terminal and Development owners persist selected correction evidence and validate
current relevance across partial effects, restart and lost comment responses.
After successful managed-comment reconciliation they acknowledge reporting.
Later applicable notifications withdraw resolved constraints in the same exact
owned comment. Historical companion ownership can select a former Target for
feedback withdrawal; it cannot authorize new lifecycle or relationship mutations.
Equivalent content is a no-op. Missing-link additions, normal success and numeric
housekeeping remain quiet. Feedback failure neither grants closure nor reverses a
validated correction.

Trusted author evidence is checked against the actual shared credential, with
App-owned installation tokens and explicit user credentials supported by
[the credential boundary](github-integration.md#authentication-and-github-app-direction).
Offline identity/receipt tests remain distinct from separate maintainer-supplied
production Go checks. Those live checks passed App JWT authentication,
installation-token acquisition, configured repository scope and Bot identity;
tested IssueClosure acquisition/correlation; and isolated comment creation,
mutation receipt, exact readback and complete enumeration. The corrected
REST/GraphQL Bot ownership recheck passed read-only on the same existing comment.
That provider test comment had no managed-feedback marker and was not a canonical
managed-feedback comment. These checks do not establish deployed managed-feedback
policy reconciliation, every operation's live permissions or a full controller
rehearsal. See the [provider evidence and remaining limits](github-integration.md#issue-53-offline-verification-and-live-proof-gate).

The broad explicit nonterminal `ManualRejection` producer remains unimplemented;
its existing adapter/templates are not production intent evidence. Bug PR lifecycle
is deferred beyond #6. Distributed coordination and Enterprise comment origins
remain outside the guarantees.

Independent composed evidence includes [terminal feedback recovery](../internal/app/terminal_feedback_test.go),
[capacity feedback](../internal/app/capacity_feedback_test.go),
[canonical routing](../internal/app/continuous_routing_test.go) and
[Issue feedback relevance](../internal/app/current_snapshot_test.go). These extend,
without rewriting, the historical standalone verification above.
