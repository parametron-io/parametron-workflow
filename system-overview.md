# Parametron Engineering Workflow System Overview

This document defines the planned workflow automation model for the public
GitHub Project **Parametron Engineering**.

It describes the controller authority model, issue and pull-request lifecycle,
Phase behavior, deterministic ordering and reconciliation, semantic
classification, declarative body directives, GitHub relationship projection,
and user-facing automation feedback.

This is a design contract for the planned automation system. It does not claim
that the controller described here is already implemented.

Issue #34 now implements the read-side declared Engineering graph and accepted
semantic context described in [docs/engineering-context.md](docs/engineering-context.md).
Issue #35 implements pure deterministic pre-development lifecycle policy in
[docs/engineering-lifecycle-policy.md](docs/engineering-lifecycle-policy.md).
GitHub Status/Roadmap Order/branch reconciliation and relationship projection remain
later Phase #4 work.

Issue #36 computes deterministic PhaseOrder and numeric Engineering Roadmap Order
in [docs/engineering-ordering.md](docs/engineering-ordering.md).
Actual numeric-field convergence remains #39.

The **Bug Tracker** reuses the controller architecture and shared lifecycle
primitives defined here, but applies a separate Project-specific policy for
triage, priority scoring, and Ready admission. That policy is defined in
[bug-tracker.md](bug-tracker.md).

---

## 1. Goals

The workflow system exists to make GitHub planning and execution predictable,
reproducible, and resistant to accidental UI state drift.

The system must:

- keep live engineering state in GitHub Issues and GitHub Projects
- preserve GitHub-native relationships where they are useful
- express automation intent in plain text where GitHub UI mutation would be
  ambiguous or non-authoritative
- use deterministic policy for lifecycle transitions and side effects
- use an LLM only where semantic judgement is genuinely useful
- reconcile accidental or unauthorized UI changes back to canonical state
- explain rejected user intent without producing comment spam
- preserve repository ownership and cross-repository dependency boundaries
- avoid making the LLM an authority over GitHub lifecycle state

The central rule is:

```text
LLM interprets semantic content.
Go owns policy and mutations.
GitHub records observable state.
```

A related authority rule is:

```text
observation != authorization
```

Seeing a GitHub state, relationship, branch, commit, or user action does not by
itself authorize a downstream side effect. Side effects are produced only by a
validated controller policy decision.

---

## 2. System Shape

The planned controller is an event-driven Go service with durable local state.

```text
GitHub event
    ↓
durable SQLite inbox
    ↓
fetch current GitHub state
    ↓
parse explicit body directives
    ↓
optional semantic classification
    ↓
compute canonical desired state
    ↓
compare actual vs desired state
    ↓
reconcile controller-owned GitHub state
    ↓
record decision / feedback metadata
```

The webhook payload is a notification that something changed. It is not the
canonical state used for policy decisions.

Before acting, the controller fetches the current GitHub state of the affected
resource and evaluates policy against that current state.

---

## 3. Authority Model

### 3.1 Go controller

The Go service is the workflow authority and side-effect executor.

It owns:

- GitHub Project routing
- normal Project Status lifecycle policy
- Engineering Roadmap Order derived numeric field
- Phase and child eligibility rules
- dependency gating
- authorized development-branch creation
- authorized branch provenance
- Start Date initialization
- GitHub relationship projection owned by the workflow
- PR onboarding normalization
- PR review-state lifecycle transitions
- close / reopen synchronization
- reconciliation of controller-owned state
- managed workflow feedback
- validation of all LLM output before mutation

The controller must remain useful without an LLM for already-classified work.
If semantic classification is temporarily unavailable, existing deterministic
workflow behavior must continue to operate.

### 3.2 Cheap semantic model

Issue #25 implements a standalone replaceable execution boundary in
`internal/semantic` for `classify_issue`, `classify_pr`, and `estimate_issue`.
Workflow callers request a capability; separate deployment configuration selects
one cheap provider/model through constructor-injected adapters. Canonical prompt
and schema assets with content-digest provenance and bounded execution are documented in
[docs/semantic-execution.md](docs/semantic-execution.md). Issue #26 adds strict
classification/Estimate validation and typed normalized planning context in
`internal/semanticpolicy`, documented in
[docs/semantic-policy.md](docs/semantic-policy.md). Issue #27 implements injectable
durable initial classification, gating, Pending safety, completion reuse, and
stale-result rejection in `internal/semanticflow`, documented in
[docs/semantic-integration.md](docs/semantic-integration.md). No production
provider is implemented. Issue #28 adds accepted semantic metadata and deterministic
Issue Project convergence through explicit Mutator injection, completing Phase #3
normalization; see [docs/semantic-reconciliation.md](docs/semantic-reconciliation.md).
Pre-development lifecycle policy is implemented separately by #35; lifecycle
mutation and runtime integration remain later Phase #4 work.
Semantic output is not workflow authority.

A cheap LLM is used only for bounded semantic interpretation: initial
classification and relative work estimation.

For Issues it may initially classify:

- Issue Type
- labels
- Priority
- coarse Effort

Before an eligible Issue first leaves `Backlog`, the same cheap semantic model
may also produce a bounded relative `Estimate` using the normalized planning
context defined later in this document.

For Pull Requests it may classify:

- labels

The model does not directly mutate GitHub and receives no GitHub credential.
It returns strict structured output which is validated by the Go service
against configured allowlists and schemas.

The model does not decide:

- lifecycle Status
- Engineering Roadmap Order
- branch creation
- dependency eligibility
- Parent / blocked-by / blocking relationships
- PR Target relationship
- Development relationship
- close / reopen behavior
- Phase activation
- review transitions

### 3.3 Future heavy agents

Triage, Audit, Validation, Review, and other repository-aware capabilities may
later be implemented by heavier agents.

Those agents remain judgement providers rather than GitHub authorities. The Go
controller continues to validate capability policy and perform allowed
mutations.

Heavy-agent behavior is not required for the Parametron Engineering lifecycle
defined in this document.

---

## 4. Durable Event Processing

SQLite provides durable local coordination for webhook processing and
controller provenance.

At minimum, the controller should be able to persist:

- GitHub delivery identity for deduplication
- event payload or a stable event reference
- processing state
- retry state
- affected resource identity
- authorized branch identity
- branch creation/base SHA
- PR onboarding completion
- managed automation comment identity
- relevant reconciliation/violation state

Delivery identity must be unique so repeated webhook deliveries do not repeat
one-shot side effects.

Events affecting the same Issue or Pull Request must be serialized in resource
order. Unrelated resources may be processed concurrently.

---

## 5. Live Workflow State

The **Parametron Engineering** GitHub Project is the source of truth for live
workflow state.

The canonical statuses are:

```text
Backlog
Ready
In Progress
In Review
Blocked
Done
```

These statuses describe workflow state. They must not be duplicated with
status labels.

---

## 6. Issue Types

The workflow recognizes the existing GitHub Issue Types:

- `Phase`
- `Task`
- `Feature`
- `Bug`

A Phase is a bounded engineering objective that groups related work and has
explicit completion criteria.

Task, Feature, and Bug are work-item types. Task creation remains a
maintainer-owned planning operation according to the existing development
workflow.

A Bug classification is routed outside Parametron Engineering to the Bug
Tracker according to deterministic project-routing policy. Bug Tracker
lifecycle, triage, scoring, and Ready-admission policy is defined in
[bug-tracker.md](bug-tracker.md).

---

## 7. Declarative Body Directives

Automation directives are optional plain-text controls parsed deterministically
from an Issue or Pull Request body.

The parser must use an exact grammar and allowlist. Loose substring matching is
not sufficient.

### 7.1 Boolean directives

```text
Automation: true | false

Classification: true | false
Triage: true | false
Audit: true | false
Validation: true | false
Review: true | false

Set-Status: true | false
Set-Position: true | false
Create-Branch: true | false
```

### 7.2 Relationship directives

Within the same repository, a relationship may use:

```text
#<issue-number>
```

Across repositories, use the canonical qualified form:

```text
parametron-io/<repository>#<issue-number>
```

Supported relationship directives are:

```text
Parent: <issue-ref>
Blocked-By: <issue-ref>...
Blocks: <issue-ref>...
Refs: <issue-ref>...
Target: <issue-ref>
```

`Parent` and `Target` are singular. `Blocked-By`, `Blocks`, and `Refs` support
one or more whitespace-separated references. Repeated lines normalize into a
deterministic deduplicated collection in first-seen order. Conflicting singular
declarations fail; comma and semicolon separators are rejected. The implemented
pure parsing boundary and exact line grammar are documented in
[docs/directives.md](docs/directives.md).

An absent relationship directive implies no relationship of that directive
type from the body contract.

---

## 8. Directive Defaults

Missing boolean directives use policy defaults.

### 8.1 Global defaults

| Directive | Default | Meaning |
| --- | --- | --- |
| `Automation` | `true` | Enables model/agent-driven capabilities for the item. |
| `Classification` | `true` | Enables initial semantic classification. |
| `Triage` | `false` | Heavy technical triage is opt-in. |
| `Audit` | `false` | Repository audit is opt-in. |
| `Validation` | `false` | Heavy implementation/contract validation is opt-in. |
| `Review` | `false` | Heavy agent review is opt-in. |
| `Set-Status` | `true` | Allows normal Project lifecycle status ownership and reconciliation. |
| `Set-Position` | `true` | Retained grammar; does not gate Engineering Roadmap Order or Bug Tracker Priority Score. |

### 8.2 `Create-Branch` defaults

`Create-Branch` is type-sensitive.

| Item type | Default |
| --- | --- |
| `Phase` | `false` |
| `Task` | `true` |
| `Feature` | `true` |
| `Bug` | `true` |
| Pull Request | not applicable |

A Phase therefore receives no development branch merely because it is active.
A Bug uses the normal implementation branch path by default; Bug-specific
readiness gating is defined in [bug-tracker.md](bug-tracker.md).

A Phase that directly carries implementation work must opt in explicitly:

```text
Create-Branch: true
```

`Automation: false` disables model/agent capabilities such as Classification,
Triage, Audit, Validation, and Review.

It does not disable deterministic relationship directives, dependency
processing, Project membership synchronization, close/reopen synchronization,
or the deterministic lifecycle controller itself.

Because Estimate generation is model-driven, the master `Automation: false`
switch also disables automatic Estimate generation.

Explicit child capability flags do not override the master automation switch:

```text
Automation: false
Audit: true
```

still does not run an audit.

---

## 9. Label Taxonomy

Labels are semantic metadata. They must not duplicate native Issue Type,
Project Status, Priority, or automation directives.

The cheap classifier may select labels only from the configured organization
allowlist. It may return an empty label set. It should prefer the smallest
sufficient label set rather than labelling aggressively.

### 9.1 Area / subsystem labels

```text
engine
freecad
pdm
studio
configurator
workflow

dsl
planning
runtime
records
verification
adapter
docs
infra
ci
```

These answer where the work belongs or what subsystem it concerns.

Product-specific labels such as `engine`, `freecad`, `pdm`, `studio`,
`configurator`, and `workflow` may be combined with concern labels such as
`runtime`, `verification`, or `adapter`.

`adapter` represents generic CAD or external-system adapter architecture.
`freecad` represents the Parametron FreeCAD adapter specifically.

### 9.2 Engineering characteristic / risk labels

```text
determinism
regression
breaking-change
compatibility
security
performance
migration
```

These describe a meaningful engineering property or risk.

### 9.3 Coordination label

```text
external
```

`external` is reserved for cross-repository dependency/coordination signalling.

### 9.4 Unmanaged helper labels

The canonical repository label set also contains a small set of helper labels
that are intentionally outside the classifier-managed allowlist:

```text
dependencies
good first issue
help wanted
```

The controller must preserve these labels when present, but semantic
classification must not add or remove them.

### 9.5 Labels that must not be introduced

Do not duplicate Issue Type:

```text
bug
feature
task
phase
```

Do not duplicate Project Status:

```text
backlog
ready
in-progress
in-review
blocked
done
```

Do not duplicate Priority with labels such as:

```text
high-priority
critical
```

Do not encode automation capabilities as labels such as:

```text
needs-audit
needs-review
needs-validation
```

Those belong in body directives.

Do not encode native relationship state as labels.

---

## 10. Initial Classification and Project Ingress

A newly created Issue or Pull Request first enters a normalization path.

Classification may enrich the item, but classification itself never advances
an item out of `Backlog`.

### 10.1 Issue classification

For a naked Issue, the cheap semantic model may propose:

```text
Type
Labels
Priority
Effort
```

Effort is coarse planning metadata and should support uncertainty, for example:

```text
XS | S | M | L | XL | Unknown
```

The controller validates the returned values against configured fields and
allowlists.

Project routing is deterministic and derived from the normalized type. The LLM
does not independently choose the Project.

`Estimate` is not part of initial classification. It is Project planning
metadata produced later, when the item is about to leave `Backlog` for the
first time.

### 10.2 Estimate on first Backlog exit

Before an eligible Task or Feature first moves from `Backlog` to `Ready` or
`Blocked`, the controller may request a bounded relative Estimate from the
cheap semantic model.

The allowed Estimate values are:

```text
1 | 2 | 3 | 5 | 8 | 13
```

Estimate is a relative planning point, not a duration in hours or days.

The intended scale is:

| Estimate | Intended meaning |
| --- | --- |
| `1` | Very small, localized change with one obvious implementation surface and focused verification. |
| `2` | Small change across a few related files or one narrow component with straightforward tests. |
| `3` | Medium change with meaningful implementation in one subsystem and multiple tests and/or documentation synchronization. |
| `5` | Large change spanning several related implementation surfaces, contract/behavior changes, or substantial verification. |
| `8` | Very large change involving multiple components/packages or significant integration and coordination work. |
| `13` | Exceptional scope. Valid, but normally a decomposition candidate. |

The estimator receives normalized planning context rather than raw repository
credentials or mutation authority. Useful inputs include:

- Type
- title and body
- canonical labels
- Priority
- Effort
- Parent
- Blocked-By / Blocks
- Refs
- repository identity
- concise parent Phase context when relevant

The controller validates the model result against the exact allowed value set
before writing the Project field.

Effort and Estimate are intentionally different:

```text
Effort
→ coarse semantic size impression from initial classification

Estimate
→ relative planning point produced at Backlog exit
```

Effort must not be mapped mechanically to Estimate.

Estimate also does not determine readiness, dependency eligibility, Phase
ordering, or branch authorization.

Estimate ownership is type-sensitive:

```text
Task / Feature
→ may receive Estimate before first Backlog exit

normal Phase
→ no Estimate

Phase + Create-Branch: true
→ may receive Estimate for the Phase's direct implementation work only

Pull Request
→ no Estimate; Target Issue or Phase owns planning Estimate
```

A Phase Estimate must never represent the sum of its child work.

### 10.3 Project ingress

When an item enters Parametron Engineering, the controller initializes the
canonical workflow state rather than relying on GitHub's built-in
"item added" workflow.

Initial classification/normalization results in:

```text
Project membership
+ normalized metadata
+ Backlog
```

Further lifecycle policy then determines whether the item should remain
Backlog, become Blocked, or become Ready.

---

## 11. Relationship Model

GitHub-native relationships are used where they carry useful semantics, but
controller-owned relationship state is projected from the declarative body
contract.

### 11.1 Parent

```text
Parent: <issue-ref>
```

represents Phase/work hierarchy.

A child may live in a different repository from its parent Phase. The
repository owning the behavior continues to own the child Issue; the parent
Phase determines workflow grouping.

### 11.2 Blocked-By / Blocks

```text
Blocked-By: <issue-ref>
Blocks: <issue-ref>
```

represent dependency edges.

These relationships affect lifecycle/execution eligibility. Dependencies between
roadmap roots order the complete root graph: Phase, parentless Task, and parentless
Feature. Phase-owned child dependencies do not order direct children or become
root edges; invalid cross-level roadmap dependencies fail closed without promotion.
Cross-repository dependency edges use qualified Issue references and the
`external` label according to existing repository conventions.

### 11.3 Refs / Relates to

```text
Refs: <issue-ref>
```

projects to GitHub's native `Relates to` relationship.

This is traceability only.

`Refs` does not:

- affect readiness
- affect branch creation
- affect Phase ordering
- block closure
- trigger lifecycle transitions

### 11.4 Target / Development

```text
Target: <issue-ref>
```

is Pull-Request-specific implementation intent.

It identifies the Issue or Phase whose implementation is represented by the
Pull Request and projects to GitHub's native Development relationship.

`Refs` must never substitute for `Target` in lifecycle policy.

### 11.5 Security Alert

GitHub Security Alert relationships are treated as external/native metadata.
They are not currently controller-owned and carry no Parametron Engineering
workflow semantics.

Bug Tracker currently treats Security Alert relationships the same way: they
remain native/external metadata with no v1 workflow semantics. Future security
policy may extend that boundary.

---

## 12. Phase Dependency and Activation Model

Phase ordering is not determined by a global readiness score.

Phase dependencies define roadmap order as a partial order.

For a Phase:

```text
any active OPEN blocker
→ Blocked

unblocked and selected current roadmap Phase for its repository
→ Ready

otherwise (OPEN and unblocked)
→ Backlog
```

When a Phase becomes `Ready`, all of its child work items become `Ready`, even
when an individual child still has a sibling or other work-item blocker.

This is intentional:

```text
Ready
= included in the active Phase work set

execution eligible
= Ready + no active blocker + branch policy permits execution
```

A blocked child therefore remains visible as `Ready` under an active Phase but
cannot begin execution until its blocker clears.

The normal organizational expectation remains **one active Phase per
repository**, unless there is an explicit reason otherwise. An active Phase may
still have multiple child work items in progress concurrently.

If the parent Phase is not active, its children must not independently advance
to `Ready` or `In Progress`.

A child whose parent Phase is blocked remains in `Backlog`.

---

## 13. Phase and Child Ordering

Dependencies between top-level roadmap roots define roadmap order.

A stable iterative topological ordering uses dependencies between roadmap roots:
Phases and parentless Task/Feature, including CLOSED topology evidence. Canonical
owner, repository, Issue number, then node evidence breaks ties among eligible roots.
PhaseOrder for #35 is the Phase-only projection of that single traversal.
Phase-internal Task/Feature membership comes from authoritative #34 Parent edges.
Native card position,
manual drag/drop, Project view order, and metadata do not supply roadmap authority.

Engineering requires a controller-owned numeric **Roadmap Order** Project field.
A human administrator configures the view to sort **Roadmap Order ASC**; the
field need not appear on cards. This is recomputable derived state, not identity
or provenance. Set-Position and Automation do not suppress its computation.

Full RoadmapRoots contains every top-level item; PhaseOrder contains every Phase.
Numeric segments retain OPEN parentless Task/Feature roots and Phases that are
OPEN or have OPEN authoritative direct Task/Feature children. Retained
segment bases are `10000 + index * 1000`: 10000, 11000, through 99000. OPEN
children sort by canonical resource identity and receive base + 10, +20, through
+990. Every primary roadmap Issue slot, including roots, reserves final-digit
+1..+9 offsets as nine generic companion slots. Future companions require an
explicit contract; #36 neither assigns nor interprets them. Capacity is 90 retained top-level segments
across mixed Phases/standalone Tasks/standalone Features, and 99 OPEN
direct children per segment; excess fails closed. Assigned values stay five-digit.

CLOSED roots and direct children have canonical Roadmap Order unset. Remaining
OPEN siblings and retained segments compact immediately: completing A=10000 and
all A's children rebases B=11000 to 10000. A CLOSED parent with OPEN children
retains its segment for those children without assigning the parent itself.

Root-to-root dependencies determine roadmap segments, including Phase → standalone
Task → Phase chains. A root cannot use another Phase's owned Task/Feature child as
a dependency endpoint; incorrect cross-level intent fails closed. The controller
never promotes child edges into Phase edges. Child-to-child and same-Phase
parent/child dependencies remain lifecycle facts and do not reorder root segments.
Closing a standalone root clears its value and compacts later mixed-root slots.

When a Phase is `Ready`, its OPEN direct Task/Feature children are displayed
contiguously with it under Roadmap Order sorting:

```text
Ready

Phase #45
  #48
  #49
  #50

Phase #60
  #61
  #62
```

### 13.1 Numeric anchor after Phase activation

When the first child starts work, the parent Phase moves to `In Progress`.
Unstarted children may remain `Ready`.

Those Ready children must not become visually detached from the Phase's Roadmap
Order segment.

A child inherits its parent Phase's Roadmap Order segment regardless of the
parent's current workflow status.

Conceptually:

```text
effective child order
=
parent retained segment base + canonical direct-child offset
```

Therefore, if Phase #45 moves to `In Progress`, its remaining Ready children
continue to occupy the Phase #45 roadmap segment and stay ahead of a later
Phase #60:

```text
Ready

#49
#50
#51

Phase #60
#61
#62
```

The absence of Phase #45 from the Ready column must not allow a later Phase to
interleave above #45's remaining Ready work.

For example, #45=10000, #48=10010, #49=10020, and #60=11000. Filtering out #45
and started #48 leaves #49's unchanged value ahead of #60. Child dependencies
affect execution eligibility, not numeric sibling order. Cross-repository Parent
edges use the same rules. #39 will reconcile numeric assignments and clears from
fresh normal observation; native item-position mutation is not that mechanism.

---

## 14. Parentless Work Items

A normal Task or Feature without a parent Phase is allowed but expected to be
uncommon.

For parentless work:

```text
blocked
→ Blocked

unblocked
→ Ready
```

All unblocked parentless Task/Feature work may be Ready; no capacity limit or
numeric admission score is defined. The v1 relative readiness order uses accepted
Priority (Critical, High, Medium, Low), then smaller Effort (XS, S, M, L, XL,
Unknown), then canonical resource identity. Manual/native items without accepted
classification remain eligible, with ranking metadata unavailable; ranked work
precedes unranked work, and unranked work sorts by resource identity.

Labels (including area and risk labels) and creation timestamps do not modify
v1 rank. Numeric weights remain undefined. This order does not reorder the Phase
roadmap or assign Roadmap Order. Parentless Task/Feature are first-class roadmap
roots under #36's dependency/canonical topology, with OPEN root assignments and
CLOSED clears. Their dependencies can induce relative Phase order, but #35 still
receives only Phases as PhaseOrder. See
[the pure policy contract](docs/engineering-lifecycle-policy.md).

---

## 15. Execution Eligibility and Branch Creation

A development branch is a controller-authorized side effect, not a consequence
of merely observing `Status = Ready`.

A normal work item becomes branch-eligible only when all applicable conditions
hold:

```text
Status = Ready
+ no active Blocked-By relationship
+ parent Phase is active, when a parent exists
+ Create-Branch policy resolves to true
```

When eligibility is satisfied, the controller creates the deterministic
development branch and records its provenance, including the branch creation or
base SHA.

The exact branch-name format is intentionally deferred, but Issue identity must
remain part of the deterministic branch identity. Renaming an Issue title must
not rename an already-authorized branch.

A manually created lookalike branch is not equivalent to an automation-created
branch and does not authorize lifecycle progression.

Branch creation alone does not mean work has started.

---

## 16. Ready to In Progress

After branch creation, the controller waits for qualifying development activity
on the authorized branch.

Conceptually:

```text
authorized branch created at SHA A
        ↓
branch head advances to SHA B
        ↓
current eligibility is revalidated
        ↓
Issue → In Progress
```

The commit or push event is observation. The controller rechecks current
workflow and dependency state before authorizing the transition.

On first authorized development activity:

- the work item becomes `In Progress`
- its parent Phase becomes `In Progress`, when applicable
- Start Date is initialized once
- an appropriate Assignee may be initialized from reliable GitHub actor context
  when policy permits

Start Date is not reset by later commits, review rework, or subsequent
transitions.

If the work item is currently blocked, a commit does not override the blocker.

---

## 17. In-Progress Board Grouping

An active Phase appears directly above its active work.

For a child Issue without an implementation PR:

```text
In Progress

Phase #45
Issue #48
```

If multiple children of the same Phase are concurrently active, they remain
contiguous under the Phase in canonical resource identity order. Child dependency
edges affect lifecycle/execution eligibility, not sibling Roadmap Order.

Cross-repository children remain grouped under their parent Phase.

Ready children not yet started remain in the Ready column while preserving the
parent Phase roadmap anchor described earlier.

---

## 18. Pull Request Binding and Onboarding

A Pull Request does not become valid workflow implementation work merely by
being opened or manually moved on the Project board.

A normal implementation Pull Request must declare:

```text
Target: <issue-ref>
```

The controller validates the binding using all required policy conditions,
including:

- Target resolves to the intended Issue or Phase
- PR head branch is the controller-authorized development branch for that Target
- PR author matches the expected developer/assignee policy for the Target

The Target validation is conjunctive. Matching only the Issue or only the
branch is insufficient.

### 18.1 PR Project and relationship projection

After a valid binding is accepted, the controller:

- adds the PR to the appropriate Project
- sets the accepted implementation PR to `In Progress`
- projects `Target` into the native GitHub Development relationship
- classifies PR labels through the cheap semantic model when enabled
- assigns the PR a Roadmap Order companion slot after its Target Issue under
  Phase #5's allocation/reconciliation policy

Manual Development relationships that contradict `Target` are drift and are
reconciled back to the canonical relationship.

### 18.2 Initial Draft normalization

PR creation is never treated as an explicit review-start signal.

On first accepted PR onboarding only:

```text
valid Target
+ valid authorized branch
+ valid author policy
+ onboarding not previously completed
        ↓
if PR is not Draft
→ convert to Draft once
        ↓
record onboarding complete
```

Draft conversion is a one-shot ingress normalization action, not a permanent
invariant.

The controller must not continuously enforce "PR must be Draft" after
onboarding, because doing so would prevent the later Ready-for-review
transition.

---

## 19. Pull Request Roadmap Order Companions

An accepted Engineering PR belongs after its Target Issue within that Target's
reserved +1..+9 companion namespace. For example, conceptual Roadmap Order values
for a Draft PR representing an active child are:

```text
In Progress

Phase #45                    10000
Target Issue #48             10010
PR #80 (Draft, Target #48)   10011
```

This visual grouping represents one implementation unit rather than three
independent Project cards.

Phase #5 owns deterministic Engineering PR companion allocation and numeric-field
reconciliation. The example does not designate +1 as a PR-specific slot or define
an exact multiple-PR allocation rule. Every primary roadmap Issue reserves nine
generic companion slots; PRs are one possible companion type.

#36 assigns primary Roadmap Order values for Phase roots, standalone Task/Feature
roots, and direct Phase children. It only reserves the companion namespace and
does not assign or interpret companions. Native Project item position is not the
Engineering ordering mechanism.

---

## 20. In Progress to In Review

The explicit developer signal for review readiness is GitHub's native Draft to
Ready-for-review transition.

```text
PR Draft
→ developer marks Ready for review
→ controller revalidates binding and eligibility
→ PR → In Review
→ Target Issue → In Review
```

The parent Phase remains `In Progress` while child review is occurring.

A PR being opened as non-Draft does not bypass onboarding because the controller
normalizes the initial accepted PR to Draft once.

Only a later explicit Ready-for-review action starts review workflow.

---

## 21. Review Outcomes and Rework

GitHub native review states drive review outcomes deterministically.

### 21.1 Comment-only review

```text
COMMENTED
→ no workflow transition
```

### 21.2 Approval

```text
APPROVED
→ remain In Review
→ wait for merge
```

Approval alone does not mark the work Done.

### 21.3 Changes requested

```text
CHANGES_REQUESTED
→ controller converts PR to Draft
→ PR → In Progress
→ Target Issue → In Progress
```

This Draft conversion is triggered specifically by the changes-requested review
event. It is not a continuous "In Progress means Draft" reconciliation rule.

Rework preserves:

- original Start Date
- Assignee
- review history
- Target relationship
- authorized branch identity

When the developer finishes rework, the developer explicitly marks the PR Ready
for review again:

```text
Draft
→ Ready for review
→ PR + Target Issue → In Review
```

A reviewer can request rework, but a reviewer does not independently return the
work to review-ready state.

---

## 22. Merge, Completion, and Next Work

For a normal Task or Feature target, the successful merge path is:

```text
PR merged
→ PR closed
→ PR Status = Done
→ Target Issue closed
→ Target Issue Status = Done
```

The controller then recomputes downstream dependency eligibility.

If closing the target removes the final blocker from the next child in the same
active Phase:

```text
next child already Ready
+ Blocked-By becomes empty
+ branch policy allows creation
        ↓
create authorized development branch
        ↓
remain Ready until first qualifying commit
```

This intentionally prepares the next work item without pretending that work has
started.

The parent Phase remains `In Progress` while open child work remains.

### 22.1 Pull Request closed without merge

Closing an accepted implementation Pull Request without merging it is an
abandoned implementation attempt, not successful completion of its Target.

The canonical recovery path is:

```text
PR closed without merge
→ PR Status = Done
→ Target Issue remains open
→ Target Issue → In Progress
```

The Target Issue must not become `Done` or close merely because its
implementation PR was closed.

The existing Development relationship may remain as historical traceability.
A later replacement PR may bind to the same Target through a valid `Target:`
directive and the authorized development branch policy.

Under normal status ownership, the target returns directly to `In Progress`;
it is not reset through `Backlog` or `Ready`.

---

## 23. Phase-Owned Implementation

A Phase may itself carry direct implementation work when explicitly enabled:

```text
Create-Branch: true
```

In that case, an eligible Phase may receive its own authorized development
branch and may be the valid target of a Pull Request:

```text
Target: #45
```

`Phase` is therefore not an invalid PR Target solely because of Issue Type.

Direct Phase implementation does not replace or implicitly complete the Phase's
child work.

```text
Phase implementation completed
!=
Phase objective completed
```

---

## 24. Phase Closure

Completing all child Issues makes a Phase eligible for closure. It does not
close the Phase automatically.

The normal Phase closure remains an explicit maintainer action, including the
existing close-with-comment practice.

Workflow closure eligibility does not replace the existing Verification
Standard. Implementation, required tests, deterministic expectations,
documentation synchronization, and applicable exit criteria remain required
for legitimate closure. The controller rule below is an additional lifecycle
gate, not a substitute for engineering verification.

A Phase may be closed only when all of its child Issues are closed.

Conceptually:

```text
Phase close requested
        ↓
all child Issues closed?
   ├─ no
   │   → reject / reconcile closure
   │   → preserve canonical open workflow state
   │   → explain the rejection through managed feedback
   │
   └─ yes
       → close Phase
       → Status = Done
```

The same closure gate applies when a merged PR directly targets the Phase. A
Phase-targeted implementation merge does not close the Phase while child Issues
remain open.

A Phase must never persist as `Done` while it still has open child work.

---

## 25. Close, Done, and Reopen Synchronization

Normal workflow status ownership and repository-native open/closed state are
separate concerns.

`Set-Status` controls ordinary workflow progression and rollback among:

```text
Backlog
Blocked
Ready
In Progress
In Review
```

Repository-native terminal/open state is stronger and must remain synchronized.

The following canonical synchronization events bypass `Set-Status`:

```text
Issue closed
Pull Request closed
Pull Request merged
Issue reopened
Pull Request reopened
```

Therefore:

```text
Set-Status: false
+ Issue closed
→ Status = Done
```

and:

```text
Set-Status: false
+ Item reopened
→ recompute canonical open workflow state
```

Reopen is not equivalent to unconditional `Ready`.

The controller recomputes the correct state from current policy, for example:

```text
blocked Phase
→ Blocked

unblocked Phase
→ Ready

child of active Phase
→ Ready

child of inactive Phase
→ Backlog
```

For Issues, a valid `Status = Done` state may also cause the controller to close
the open Issue when closure conditions permit.

Phase closure conditions still apply and cannot be bypassed by setting Done.

Repository-native state and Project state must not remain contradictory, for
example:

```text
Issue = closed
Project Status = In Progress
```

or:

```text
Issue = reopened
Project Status = Done
```

---

## 26. Reconciliation Model

The controller is a desired-state reconciler.

```text
event
→ fetch actual state
→ parse directives
→ obtain optional semantic classification
→ compute desired state
→ compare actual / desired
→ reconcile controller-owned differences
```

It does not use a "reset to Backlog and replay" strategy.

If an item is manually placed in an invalid state, it moves directly to its
canonical state.

Examples:

```text
blocked child manually moved to In Progress
→ direct correction to Ready or Backlog according to parent policy
```

```text
blocked Phase manually moved to In Progress
→ direct correction to Blocked
```

```text
child manually moved to Ready while parent Phase is inactive
→ direct correction to Backlog
```

The controller only reconciles fields it owns and only where the relevant
ownership directive permits normal reconciliation.

---

## 27. One-Shot Actions vs Continuous Invariants

The implementation must distinguish initialization/transition side effects from
continuous desired-state invariants.

Examples of one-shot actions:

- initial PR Draft normalization
- authorized branch creation
- Start Date initialization
- changes-requested Draft conversion

Examples of continuous invariants:

- valid Project membership
- normal lifecycle eligibility
- canonical Engineering Roadmap Order assignments and clears
- Target / Development consistency
- Parent/dependency relationship consistency where controller-owned
- Phase closure constraints

A one-shot action must not be repeatedly replayed merely because its resulting
state later changes legitimately.

---

## 28. Controller-Owned UI Projections

The GitHub UI is not authoritative for controller-owned relationships or
workflow fields.

For example:

```text
Target: #48
→ canonical intent

GitHub Development relationship
→ projected state
```

If a user manually adds an unrelated Development Issue through the GitHub UI,
the controller removes that drift and restores the declared Target projection.

The same principle applies to other controller-owned relationships.

This does not make all GitHub UI state controller-owned. Human discussion and
review content remain human-owned.

Typical controller-owned surfaces include:

- Project Status, when `Set-Status` permits normal ownership
- Engineering Roadmap Order, independent of `Set-Position`
- workflow-managed Project membership
- Development relationship derived from `Target`
- Parent/dependency relationships derived from explicit directives
- automation-created branch provenance

Typical human-owned surfaces include:

- Issue/PR prose
- comments
- review text
- normal discussion

---

## 29. Rejected Intent and Managed Feedback

Routine successful reconciliation should be quiet.

When a human action is rejected because it violates workflow policy, the
controller should explain:

1. what was reverted or corrected
2. why it was invalid
3. what the user must do to make the action valid

Examples include:

- missing PR Target
- wrong PR branch
- invalid PR author binding
- blocked item manually moved to In Progress
- child advanced while parent Phase is inactive
- invalid Phase closure with open children
- manually altered Development relationship

The controller should avoid comment spam by maintaining one managed automation
feedback comment per resource.

Repeated equivalent violations should reuse or update the existing managed
comment rather than creating a new comment for every attempt.

A new violation may update the same comment with the current constraint set.

Routine housekeeping corrections such as normal item reordering need not
produce user-facing comments.

The general rule is:

```text
normal reconciliation
→ silent

rejected user intent
→ explain and deduplicate
```

---

## 30. GitHub Built-In Workflow Boundary

Most lifecycle automation belongs to the Go controller so there is a single
version-controlled workflow authority.

The controller should own:

- item added to Project → canonical ingress / Backlog initialization
- sub-issue added → correct Project membership
- item closed → Done synchronization
- valid Done Issue → close Issue
- item reopened → canonical open-state recomputation

GitHub's built-in completed-item archival may remain enabled:

```text
closed/completed item
→ after 14 days
→ Archive item
```

Archival is housekeeping rather than engineering lifecycle policy and does not
need to become controller-owned unless future requirements justify it.

---

## 31. Security and Credentials

The LLM must not receive a GitHub credential.

The Go service fetches selected GitHub context, sends only the required content
to the semantic model, validates the structured response, and performs all
GitHub mutations itself.

A prompt instruction is not a security boundary.

For production, GitHub authentication should use a GitHub App with narrowly
scoped repository and organization Project permissions.

A prototype may use the authenticated `gh` CLI, but production transport should
remain replaceable behind an internal GitHub client boundary.

---

## 32. Failure and Recovery Principles

The controller must be safe under duplicate deliveries, restarts, partial
failure, and temporary external API failure.

Required properties include:

- webhook deduplication
- idempotent reconciliation
- durable retry state
- per-resource event serialization
- current-state re-fetch before consequential mutation
- one-shot side-effect provenance
- no lifecycle authority delegated to LLM output

If classification fails, the controller should fail closed for semantic
mutation rather than inventing metadata. Existing deterministic workflow state
must remain usable.

---

## 33. Deferred Decisions

The following are intentionally not defined by this overview:

- exact numeric readiness-score weights
- exact deterministic branch-name format
- future heavy-agent implementation/provider selection
- advanced security-alert workflow behavior

These decisions can be specified independently without changing the authority
and lifecycle model defined here.

---

## 34. End-to-End Example

A normal Phase child flow is:

```text
Phase #45 unblocked
→ Phase #45 Ready
→ children #48..#54 Ready

#48 unblocked
→ controller creates authorized #48 branch
→ #48 remains Ready

first qualifying commit on #48 branch
→ #48 In Progress
→ Phase #45 In Progress
→ Start Date initialized

PR #80 opened
Target: #48
→ Target + branch + author validated
→ PR added to Project
→ Development relationship projected
→ PR normalized to Draft once
→ Phase #5 allocates PR Roadmap Order after Target within Target's +1..+9 companions

PR #80 Draft → Ready for review
→ PR #80 In Review
→ #48 In Review
→ Phase #45 remains In Progress

review CHANGES_REQUESTED
→ PR #80 Draft
→ PR #80 In Progress
→ #48 In Progress

PR #80 Draft → Ready for review
→ PR #80 In Review
→ #48 In Review

PR #80 merged
→ PR #80 Done
→ #48 closed + Done

#49 blocker now resolved
→ controller creates authorized #49 branch
→ #49 remains Ready
→ waits for first qualifying commit

...

all Phase #45 children closed
→ Phase #45 becomes eligible for explicit maintainer closure
→ maintainer closes Phase with comment
→ Phase #45 Done
```

This sequence captures the intended separation between readiness, execution,
review, completion, and Phase-level objective closure.

---

## 35. Summary

```text
GitHub Issues / Project
        ↓ observable state and human intent

Go workflow controller
        ↓ authority, policy, reconciliation, mutations

Cheap semantic LLM
        ↓ bounded classification and relative estimation

SQLite
        ↓ durable event processing and one-shot provenance
```

The workflow is intentionally deterministic wherever deterministic policy is
possible.

The LLM is used where free-form engineering text must be interpreted, not where
workflow state can be derived from explicit relationships, GitHub-native events,
or stable controller rules.

Phases define bounded engineering objectives and roadmap structure. Children
inherit Phase workflow membership and Roadmap Order anchors. Root dependencies
determine roadmap-root order; dependencies also determine execution eligibility.
Authorized branch activity starts work. Draft/Ready
transitions start review. Changes requested returns work to implementation.
Merge completes normal child work. Phase closure remains an explicit maintainer
action after all child work is closed.

The result is a declarative, reconciling workflow in which GitHub remains the
visible collaboration surface while lifecycle authority remains centralized,
version-controlled, and testable.
