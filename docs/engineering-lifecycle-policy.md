# Pure Engineering lifecycle policy

Issue #35 implements `internal/engineeringpolicy.Evaluate(Input) (Plan, error)`.
It consumes #34's `engineeringcontext.Context` and an explicit
`PhaseOrder []storage.Resource`. It computes canonical pre-development policy
without GitHub reads/writes, model execution, persistence, clock, randomness,
filesystem access, or runtime assembly. At #35 standalone completion the CLI
was unchanged; production now consumes this pure API through Engineering composition.

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
active Phase. #36 now filters this Phase-only contract from one deterministic
top-level traversal of Phase and parentless Task/Feature roots. Root dependencies
may induce Phase order transitively; standalone resources never enter PhaseOrder.
See [engineering-ordering.md](engineering-ordering.md).
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
evidence for #37, which evaluates caller-supplied authoritative current Input
before authorization or an unfinished branch creation attempt. Freshness and
completeness are integration responsibilities; implemented production composition
supplies complete Snapshot context and PhaseOrder. #37 never constructs activation state from Resolve(root) or
observed Project Ready. See [execution.md](execution.md).
Evaluate never inspects, names, or creates a branch.

`StatusOwned` equals current `Intent.Effective().SetStatus`. Canonical Status
and ExecutionEligible are independent of that ownership flag. Set-Status false
can yield Ready, StatusOwned false, ExecutionEligible true. It suppresses ordinary
Status writes, not canonical computation or branch policy.

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

## Phase #4 package ownership and production integration

- #36 implements canonical roadmap-root topology with a Phase-only projection,
  first-class standalone Task/Feature roots, direct-child grouping and
  numeric Roadmap Order with inherited anchors and current-work compaction.
- #37 implements standalone exact branch creation from #35 eligibility, naming,
  base resolution, durable authorization/completion provenance, retries, and
  rejection of manual lookalikes. Branch creation does not imply In Progress.
- #38 implements standalone qualifying authorized development activity, owned
  In Progress, parent Phase progression and one-shot Start Date; Assignee
  initialization is suppressed for insufficient reliable assignability evidence.
- Production integration consumes complete Snapshot → #36 BuildRoadmapOrder →
  #35 Evaluate → #36 Evaluate, then fresh Project comparison and authorized
  lifecycle/Roadmap Order/work authorization convergence. A second complete
  Snapshot and FULL pipeline precede branch fan-out.

No Status/Project mutation, relationship projection, close/reopen policy,
PR lifecycle, Bug Tracker lifecycle, Estimate execution, storage migration,
external dependency, or Nix change is added by #35.

## First-activity consumer (#38)

Standalone engineeringactivity now evaluates this unchanged #35 Input itself
before every unfinished transition. Exact root ExecutionEligible is required;
observed Ready/In Progress and previously accepted activity never override an
active blocker, inactive parent or Create-Branch false. Target/parent Status
writes each require their own StatusOwned. Set-Status false affects writes only;
Start Date initialization remains separate. ParentPhase comes from the validated
Decision, never native relationship drift. Direct Phase activity writes only the
Phase target. Completed #37 evidence, current exact branch and strict forward
ancestry are also required. Start Date and durable retries are described in
[execution.md](execution.md). Assignee initialization is suppressed for
insufficient human/assignability evidence. Production assembly is implemented
in engineeringflow and app; the standalone package does not own assembly.

For #38 qualification, the push’s immutable Delivery.ReceivedAt must be strictly
after the validated #37 successful-creation provenance CreatedAt exposed by its
read-only Completion value. Processing-time completion alone is insufficient.
Earlier or equal-time notifications grant no activity and produce no new activity
provenance or lifecycle writes, including on restart or out-of-order processing.

## Completed-activity Status overlay

The pure Evaluate API still emits only Backlog, Blocked, and Ready. Production
reconciliation overlays desired In Progress for an OPEN exact work item with
validated COMPLETED first activity. Later blockers, inactive parent, or
Create-Branch false prevent new starts but do not erase that completed start.
Observed manual In Progress is never equivalent to activity evidence. Each Status
write still requires that resource's StatusOwned; CLOSED resources have no Status
reconciliation.

An OPEN Phase is desired In Progress from its own completed activity OR any
CURRENT direct Task/Feature child's completed activity, including a CLOSED child.
Bugs and non-direct/nested descendants do not progress ancestors. Current
reparenting moves the contribution; an old parent loses it unless another
qualifying contribution exists. Parent Set-Status independently gates its write.
No historical parent identity was added to activity provenance.

Accepted unfinished activity is not an overlay. Production resumes with its
ORIGINAL push DeliveryID only when a current #35 Decision exists, retaining
#38's current eligibility and branch checks. CLOSED/no-Decision acceptance stays
durable, skips Handle, creates no In Progress authority or completion, and does
not prevent normal reconciliation/roadmap clearing. See [execution.md](execution.md).
The pure pre-development policy remains unchanged by PR review/rework.
Production separately validates durable review authority for exact Target Status
overlays and guards first-activity Status writes; it does not retrofit review
semantics into this pure API or move a parent Phase into In Review for child review.
See [review execution](execution.md#accepted-pr-review-and-rework).
PR completion and terminal close/reopen synchronization remain deferred.
