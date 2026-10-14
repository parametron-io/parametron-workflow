# Engineering PR companion ordering

Issue #48 implements and independently tests pure deterministic allocation in
`internal/prcompanion`. It produces desired numeric assignments and clears;
it performs no storage, GitHub reads/writes, or lifecycle transitions.

## Authority boundaries

[#36 primary ordering](engineering-ordering.md) owns roadmap topology, Issue
numeric assignments, primary clearing, compaction, and generic companion
reservation. [#46 PR binding](pr-binding.md) owns current accepted PR/Target
authorization. #48 consumes those results and owns only pure companion assignment
and desired clearing. [#47 onboarding](execution.md#accepted-pr-onboarding)
retains its separate initial membership, Status, Draft, and provenance authority.
Production integration and GitHub mutation belong to later work.

## Inputs and outputs

```go
prcompanion.Evaluate(prcompanion.Input) (prcompanion.Plan, error)
```

`Input.Primary` is the current authoritative `engineeringorder.Plan`. Its
`Assignments`, `Clear`, and child `AnchorPhase` evidence are validated for visible
contradictions; topology is not rebuilt. `RoadmapRoots` and `PhaseOrder` are not
revalidated by this package.

`Input.PullRequests` contains `PullRequest` values: exact `storage.Resource`,
`github.Identity`, current `State` (`OPEN`, `CLOSED`, or `MERGED`), and current
`prbinding.Decision` in `Binding`. A zero Decision means unknown. Acceptance is
current evidence, never a durable authorization grant; the caller must supply
fresh authoritative facts. The policy checks accepted evidence structure and
exact identities without reimplementing #46's authorization reads.

`Input.Owned` contains explicit caller-validated `OwnedField{PR, Target}` evidence
of a previously controller-owned Engineering Roadmap Order field. Both members
are exact `github.Identity` values. The caller must authenticate provenance:
structural/conflict validation here cannot prove historical ownership.

`Plan.Assignments` contains `Assignment{PR, Target, RoadmapOrder}` with canonical
`storage.Resource` identities and an integer desired number. `Plan.Clear` contains
canonical PR resources whose owned Engineering Roadmap Order should be unset.
Neither output is an immediate mutation or a Project-item mutation command.

## Numeric allocation

An OPEN PR with an exact accepted #46 binding and current primary Target
assignment is eligible. For Target primary Roadmap Order `N`, its namespace is:

```text
N+1 ... N+9
```

Eligible PRs are grouped by exact Target identity and sorted by normalized
(lowercase) owner, repository, numeric PR number, then node identity. Dense offsets
`+1` through `+9` follow that canonical identity order. For example:

```text
Target Issue #48 = 10010

Accepted PR A = 10011
Accepted PR B = 10012
Accepted PR C = 10013
```

A/B/C denote canonical identity order, not webhook chronology or review order.
Each Target permits at most nine eligible companions; the tenth returns
`ErrCapacity` with no partial plan. Different Targets cannot share or cross
namespaces. A Target rebase recomputes all companion numbers from its current
primary assignment; removal from the eligible set compacts remaining PRs.
Historical numeric slots are neither identities nor allocation authority.

All nine offsets remain generic companion capacity, without a permanent
PR-exclusive slot. Other companion categories and shared occupancy arbitration
are deferred and are not represented in this input. Later composition must
explicitly arbitrate other occupants before consuming this plan.

## Supported topology

Current primary assignments can represent Phase roots, parentless Task roots,
parentless Feature roots, and direct Phase children, including cross-repository
PR/Target identities. A CLOSED Phase may retain its segment while OPEN direct
children remain: the Phase is in primary `Clear`, its children carry that Phase's
anchor and receive companion assignments for their own Targets. The CLOSED Phase
itself has no active primary assignment for an OPEN accepted PR to consume.

Primary validation rejects contradictory retained Phase segment ownership,
including two distinct retained anchors sharing a segment or a child anchor
conflicting with the segment's assigned root. This package consumes #36 authority;
it does not rebuild or replace full topology validation.

## Lifecycle and eligibility

- OPEN + accepted binding + assigned Target: eligible.
- CLOSED/MERGED: no active companion assignment, even with accepted evidence.
- Reopened OPEN: requires current accepted evidence and gets a freshly computed
  position; historical slots provide no authority.
- Rejected or unknown binding: no new assignment.
- OPEN accepted binding with missing Target assignment: `ErrTarget`.

Malformed binding evidence still fails validation for terminal PRs. Draft and
review Status are deliberately absent from the interface and do not change order.
`Set-Position`, manual Project card position, webhook arrival order, timestamps,
and observed numeric values do not determine ordering. The package itself never
reopens, closes, merges, or transitions Status.

## Safe clearing

A desired clear requires explicit prior controller-owned field evidence plus
current ineligibility established by CLOSED/MERGED lifecycle state or a definitive
current rejection. For OPEN PRs, unknown binding and `MissingPR`,
`UnresolvedTarget`, and `Changed` rejections cannot authorize speculative clearing.

The other supported current #46 rejection reasons are definitive denials for this
policy: `InvalidTarget`, `MissingTarget`, `OutsideDeployment`, `UnsupportedTarget`,
`IneligibleTarget`, `MissingProvenance`, `InvalidProvenance`, `HeadMismatch`,
`MissingHead`, `InvalidDeveloper`, `InactiveMembership`, `InsufficientPermission`,
and `AssigneeMismatch`. They still require separate ownership evidence.

Historical ownership Target is not current authorization and may legitimately
differ after retargeting. An eligible PR receives no simultaneous clear. Absent
ownership authorizes no clear; malformed, duplicate, conflicting ownership or
ownership without a matching current PR fails safely with `ErrOwnership` (earlier
identity contradictions can fail at their own validation boundary).

An arbitrary observed field value or historical binding is not ownership proof.
`Clear` means desired unset of only the owned Engineering Roadmap Order field,
not numeric zero, another field, or an immediate mutation. The policy cannot
independently authenticate caller-supplied provenance; it adds no ownership
discovery, storage, or reconciliation mechanism.

## Validation and determinism

Errors are bounded typed categories supporting `errors.Is` through `Error.Unwrap`:

| Category | Boundary |
| --- | --- |
| `ErrInvalid` | Invalid current PR identity or lifecycle input |
| `ErrBinding` | Duplicate/conflicting or malformed binding evidence |
| `ErrPrimary` | Contradictory primary assignments, clears, or anchors |
| `ErrTarget` | Unavailable exact primary Target assignment for an eligible PR |
| `ErrCapacity` | More than nine eligible PRs for one Target |
| `ErrOwnership` | Unsafe supplied field-ownership evidence |

Every error returns the zero `Plan`, never partial assignments or clears.
Validation checks exact resource/identity agreement, positive numbers, valid
repository and node identities, node reuse, conflicting node evidence, and
repository name/ID conflicts across supplied identities. Primary values must obey
the existing range, strides, unique assignments, and segment/anchor constraints.

Assignments sort by Roadmap Order then canonical PR identity; clears sort by
canonical PR identity. Evaluation does not mutate caller inputs. The committed
[independent tests](../internal/prcompanion/order_test.go) cover capacity,
eligibility/rejection/lifecycle ownership matrices, rebasing, compaction,
reopening, cross-repository/multiple Targets, malformed identities, primary
contradictions, zero-plan failures, and input immutability. Twenty-five equivalent
shuffled fixtures produce byte-identical JSON plans.

## Remaining integration

Pure plan output is available for later production reconciliation. Issue #48 does
not introduce production composition, live PR numeric-field writes/clears,
persisted ownership-provenance acquisition, fresh field convergence/drift repair,
retry or cross-resource sequencing, or webhook-driven companion orchestration.
The Engineering Project does not automatically display these newly computed PR
numbers today. Shared review/rework and #50 completion/terminal synchronization
are implemented separately from companion allocation. Relationship projection,
managed feedback, and Bug Tracker changes remain deferred. See [runtime](runtime.md),
[execution](execution.md), and
[architectural lifecycle contracts](../system-overview.md#19-pull-request-roadmap-order-companions).
