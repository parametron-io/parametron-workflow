# Pure Engineering lifecycle policy

Issue #35 implements `internal/engineeringpolicy.Evaluate(Input) (Plan, error)`.
It consumes #34's `engineeringcontext.Context` and an explicit
`PhaseOrder []storage.Resource`. It computes canonical pre-development policy
without GitHub reads/writes, model execution, persistence, clock, randomness,
filesystem access, or runtime integration. The default CLI is unchanged.

LLM interprets semantic content. Go owns policy and mutations. GitHub records
observable state. Observation is not authorization: observed Project Status or
branches never enter this API.

## Phase order consumer contract

Every Phase in Context, including CLOSED Phases, must appear exactly once in
PhaseOrder. Entries must exactly match the Context resource, including current
node-ID evidence. Unknown resources, duplicate Phases, and Task/Feature/Bug
entries fail closed. Every Phase → Phase dependency requires the blocker to
appear before the blocked Phase, even if the blocker is CLOSED. Non-Phase
dependency edges impose no ordering constraint here.

Evaluate does not repair or sort PhaseOrder. Supplied unrelated Phase order is
consumed unchanged; Issue numbers, IDs, titles, and map order never select an
active Phase. #36 now produces this contract using dependency topology and valid
manual Project position; see [engineering-ordering.md](engineering-ordering.md).
Keeping ordering production separate avoids giving #36
lifecycle authority or duplicating its roadmap policy in #35.

For each repository represented in Context, scan PhaseOrder and select the first
OPEN Phase with no active blocker. At most one Phase per repository is active;
two repositories can each have an active Phase. A blocked earlier Phase does not
prevent a later unblocked Phase in that repository from being selected.
Cross-repository blockers use the same rule.

## Blockers and canonical status

An active blocker is a declared dependency edge `blocker → target` whose blocker
node is OPEN. Phase, Task, Feature, and Bug blockers all gate execution. CLOSED
blockers are inactive, but the graph edge is retained. Parent does not imply a
dependency. CLOSED work items receive no Engineering decision; CLOSED does not
infer Done or authorize close/reopen synchronization.

The package-owned `Status` constants are only `Backlog`, `Blocked`, and `Ready`.
For OPEN Phases:

- Any active blocker produces Blocked before activation selection.
- The selected unblocked Phase is Ready and PhaseActive.
- Every other unblocked Phase is Backlog and inactive.

An OPEN Task/Feature with a Phase parent is Ready when that parent is active,
even when the child has an active blocker. With a Backlog, Blocked, or CLOSED
parent, it stays Backlog and execution-ineligible. Cross-repository parents
behave identically. Relevant OPEN Task/Feature parents of Type Task, Feature,
or Bug fail with ErrParent instead of being treated as parentless. OPEN Phases
cannot have non-Phase parents. Phase-to-Phase Parent facts do not select activation;
PhaseOrder and declared dependencies remain activation authority. Native SubIssues
are never inferred.

Ready means membership in the active Phase work set. ExecutionEligible is
computed separately as `Status == Ready && !HasActiveBlocker && CreateBranch`.
Parent activation is already encoded in canonical child Status. A blocked child
therefore remains Ready while execution-ineligible.

## Parentless work and relative rank

OPEN parentless Task and Feature are Blocked with an active blocker and Ready
otherwise. All unblocked items may be Ready: no capacity, threshold, preemption,
or displacement exists. They do not change Phase selection.

Each parentless Ready decision carries `ParentlessRank *Rank`; other decisions
have nil rank. `Rank.Available` means accepted semantic Priority/Effort exist.
`Plan.ParentlessReady` orders these resources by:

1. Accepted semantic metadata before unavailable metadata.
2. Priority: Critical, High, Medium, Low.
3. Effort, smaller first: XS, S, M, L, XL, Unknown.
4. Canonical resource identity.

Manual/native items remain lifecycle-eligible without classification. Their
rank has Available false and empty Priority/Effort, and they sort by identity
after ranked items. No semantic values are fabricated. Labels, including
runtime/docs or risk labels, have no modifier. No numeric weights, creation
timestamps, or GitHub reads are introduced. This relative list is neither
Project position nor Ready admission.

## Branch defaults and ownership

`Decision.CreateBranch` resolves current explicit Create-Branch intent without
modifying Intent. Phase defaults false; Task/Feature default true. Explicit true
or false overrides the default. Direct Phase implementation requires explicit
Create-Branch true, a selected Ready Phase, and no active blocker. This is policy
evidence for #37, which must revalidate current eligibility before mutation.
Evaluate never inspects, names, or creates a branch.

`StatusOwned` equals current `Intent.Effective().SetStatus`. Canonical Status
and ExecutionEligible are independent of that ownership flag. Set-Status false
can yield Ready, StatusOwned false, ExecutionEligible true. It suppresses future
ordinary Status writes, not canonical computation or branch policy.

Automation false does not disable deterministic lifecycle, dependencies, Parent,
Set-Status, or Create-Branch. Type authority remains #34's accepted-semantic or
validated manual/native path; #35 adds no LLM requirement. Bugs can block work
but receive no Engineering lifecycle decision or routing change.

## Validation and deterministic output

Malformed public Context values fail closed with a zero Plan and package-owned
`*Error`, supporting errors.Is/errors.As. Categories are:

- ErrInvalid: root/resources, duplicate identities/IDs, unsupported Type/state,
  invalid TypeSource, missing/mismatched/invalid accepted classification, or
  fabricated manual classification (manual source requires classification disabled).
- ErrParent: missing/mismatched endpoints or parsed Parent intent, self-parent,
  duplicate/multiple parents, missing Parent edge, or unsupported open parent Type.
- ErrLifecycle: missing dependency endpoints, self/duplicate dependencies, or
  Parent/dependency cycles.
- ErrPhaseOrder: incomplete, duplicate, unknown, non-Phase, identity-mismatched,
  or dependency-inconsistent supplied order.

This validates the facts-only handoff; it does not re-resolve GitHub or redeclare
#34's discovery contract. Node identity is lowercase supported owner/repository,
Issue kind, positive number, with current nonblank unique node-ID evidence.
Semantic identity uniqueness ignores node ID, while endpoints/root/order require
exact evidence agreement. Existing strict semantic decoding validates accepted
domains without running a model. Errors contain no Issue body text.

Plan exposes slices and typed decisions, no canonical maps. Decisions sort by
lowercase owner, lowercase repository, number, then node ID. Returned parent and
rank pointers are independent of input. Equivalent valid input yields identical
encoding/json bytes despite shuffled nodes/edges. Tests include twenty repeated
shuffled evaluations and a pure multi-repository integration fixture using
engine Phase #45 / Tasks #48 and #49 and freecad Phase #60 / Feature #61, plus
Blocked/Backlog Phases and parentless work. No GitHub fake is needed.

## Remaining Phase #4 boundaries

- #36 implements stable topological PhaseOrder, unrelated manual position
  preservation, child grouping/order, inherited anchors, and Set-Position ownership.
- #37: consume current eligibility for branch creation, naming, base resolution,
  durable provenance, retries, and rejection of manual lookalikes.
- #38: qualifying authorized development activity, In Progress, Start Date,
  and permitted Assignee initialization.
- #39: production integration and lifecycle/position/work authorization
  reconciliation, including fresh context and current order production.

No Status/Project mutation, relationship projection, close/reopen policy,
PR lifecycle, Bug Tracker lifecycle, Estimate execution, storage migration,
external dependency, or Nix change is added by #35.
