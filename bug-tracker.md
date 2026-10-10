# Bug Tracker Workflow Policy

This document defines the planned workflow policy for the public GitHub Project
**Bug Tracker**.

Bug Tracker reuses the shared controller architecture, authority model,
declarative directives, relationship projection, branch authorization, Pull
Request lifecycle, reconciliation, managed feedback, and close/reopen
synchronization defined in
[system-overview.md](system-overview.md).

This document defines only the policy that differs for Bug Tracker.

The controller described here is planned behavior. This document does not claim
that the implementation already exists.

---

## 1. Purpose

Bug Tracker manages reported defects independently from the planned Phase
roadmap used by Parametron Engineering.

The main policy difference is ordering:

```text
Parametron Engineering
→ planned dependency structure determines roadmap order

Bug Tracker
→ dynamically recomputed urgency determines work order
```

Bug Tracker therefore uses a deterministic numeric `Priority Score` as a
first-class Project runtime field.

The score is not an LLM decision. The cheap semantic model supplies bounded
metadata; the Go controller calculates the score deterministically.

---

## 2. Status Model

Bug Tracker uses the following Project statuses:

```text
Backlog
To Triage
Ready
In Progress
In Review
Done
```

Bug Tracker intentionally does not use a `Blocked` status.

Dependency blocking is represented through native `Blocked-By` / `Blocks`
relationships. A blocker affects readiness eligibility without introducing a
separate lifecycle state.

The intended meanings are:

```text
Backlog
→ open bug that is not currently admitted to the active Ready set

To Triage
→ investigation is required or incomplete

Ready
→ actionable, unblocked, and admitted to the current top-priority work set

In Progress
→ authorized implementation has started

In Review
→ implementation is under review

Done
→ terminal closed/completed work
```

---

## 3. Project Fields

Bug Tracker uses:

- Status
- Sub-issues progress
- Priority
- Estimate
- Start date
- Effort
- Priority Score

`Priority Score` is a numeric custom Project field owned by the controller.

The primary board view should sort by:

```text
Priority Score DESC
```

so that the highest-priority work appears first.

Bug Tracker v1 does not use `Iteration` or `Target date`.

`Iteration` may be reconsidered later if the Project adopts real time-boxed
planning. It must not duplicate the work-selection meaning already provided by
Status and Priority Score.

---

## 4. Classification and Project Ingress

A newly classified Bug is routed deterministically to Bug Tracker.

Initial semantic classification may provide:

```text
Type = Bug
Labels
Priority
Effort
```

The cheap model does not directly write `Priority Score`.

Issue #28 implements accepted Bug metadata normalization and deterministic Bug
Tracker routing, initializing unset Status to Backlog when Set-Status permits.
Triage judgement is later policy, not an initial classifier output field; no
To Triage transition or Priority Score write is implemented by Phase #3.

Conceptually:

```text
Bug enters Bug Tracker
        ↓
Backlog
        ↓
triage required?
├─ yes → To Triage
└─ no  → evaluate readiness eligibility and Ready admission
```

Classification itself does not authorize implementation.

The explicit directive:

```text
Triage: true
```

forces the triage gate even when the initial classifier would otherwise consider
the Bug sufficiently understood.

`Triage: false` disables automated/heavy-agent triage capability; it does not
force an insufficiently understood Bug to bypass `To Triage`.

---

## 5. Triage Model

Triage answers whether a reported defect is sufficiently understood and
actionable for implementation.

Triage does not perform implementation.

Today, a maintainer may perform the investigation manually and publish the
result as a standardized Issue comment.

In the future, a heavy agent may perform the same investigation and publish the
same artifact without changing downstream workflow semantics.

The boundary is:

```text
investigator
→ Triage Report comment
→ cheap semantic evaluation
→ Go policy decision
```

The investigator may therefore change from human to agent without changing the
workflow contract.

---

## 6. Triage Report Contract

A triage report is recognized deterministically by the version marker:

```text
<!-- parametron-triage-report:v1 -->
```

The canonical v1 structure is:

```markdown
<!-- parametron-triage-report:v1 -->

## Triage Report

**Verdict:** confirmed
**Reproduction:** confirmed
**Root cause:** suspected

### Findings

Describe what was observed and how the report was evaluated.

### Scope

Describe the affected subsystem, behavior, or contract boundary.

### Priority evidence

Record evidence that may justify changing Priority or semantic risk labels.

### Recommended action

Describe the implementation direction or next engineering step.
```

Allowed `Verdict` values are:

```text
confirmed
needs-information
not-reproducible
duplicate
invalid
```

Allowed `Reproduction` values are:

```text
confirmed
partial
not-reproduced
not-applicable
```

Allowed `Root cause` values are:

```text
confirmed
suspected
unknown
```

The marker identifies the comment without asking an LLM whether an arbitrary
comment "looks like" a triage report.

The cheap model may interpret the report contents and return bounded semantic
results, but the Go controller owns all lifecycle mutations.

Only a `confirmed` report can satisfy the triage gate automatically in v1.
Other verdicts remain non-ready and require follow-up or explicit maintainer
resolution. They do not automatically close the Bug.

Native dependency relationships remain canonical. A triage report may describe
a blocker, but the actual workflow dependency must be represented through
`Blocked-By:` / `Blocks:` and the corresponding GitHub relationship.

---

## 7. Triage Completion

When a valid confirmed Triage Report is accepted, the controller may
re-evaluate semantic metadata using the new evidence.

For example, triage may reveal that an initially ordinary defect is actually a
regression affecting deterministic output.

The sequence is:

```text
To Triage
    ↓
confirmed Triage Report
    ↓
re-evaluate relevant Priority / labels / Effort
    ↓
recompute Priority Score
    ↓
evaluate blockers
    ↓
rebalance Ready set
```

The result is either:

```text
To Triage → Ready
```

when the Bug is actionable, unblocked, and admitted to the Ready top five, or:

```text
To Triage → Backlog
```

when triage is complete but the Bug is not currently admitted to Ready.

Priority rebalancing must never produce:

```text
Ready → To Triage
```

`To Triage` exists only because investigation is required or incomplete.

---

## 8. Estimate Policy

Bug Tracker uses the same relative Estimate scale defined by the shared workflow
contract:

```text
1 | 2 | 3 | 5 | 8 | 13
```

Estimate is a relative planning point, not a duration.

For a Bug that does not require triage, Estimate may be produced when the Bug
first becomes actionable and is evaluated for Ready admission.

For a Bug that requires triage, Estimate should be produced after the accepted
Triage Report, when the implementation scope is better understood.

A completed Estimate does not guarantee Ready admission.

`Estimate = 13` is valid but indicates that decomposition should normally be
considered.

Pull Requests do not receive their own Estimate. The Target Bug owns the
planning Estimate.

---

## 9. Priority Score

`Priority Score` is a deterministic derived runtime value written by the Go
controller.

It is calculated from normalized Project metadata rather than emitted directly
by the LLM.

The v1 base score is:

```text
Priority:
Critical = 8
High     = 6
Medium   = 4
Low      = 2
```

Configured semantic risk modifiers are:

```text
security        +2
regression      +1
determinism     +1
breaking-change +1
```

Effort is only a small modifier:

```text
XS      +0.25
S       +0.25
M        0
L       -0.25
XL      -0.25
Unknown  0
```

Area/subsystem labels such as `runtime`, `docs`, or `adapter` do not change
Priority Score.

The base formula is:

```text
base score
=
Priority weight
+ configured risk modifiers
+ small Effort modifier
```

Age-based score amplification is intentionally outside v1.

A future policy may introduce an aging multiplier to prevent long-lived
actionable Bugs from starving indefinitely. That behavior must be designed from
observed backlog behavior rather than added speculatively.

---

## 10. Blocker Urgency Inheritance

A blocked high-priority Bug must not hide behind a low-scoring prerequisite in
Backlog.

Bug Tracker therefore propagates urgency through dependency edges.

If Bug B blocks Bug A:

```text
A base/effective score = 9
B base score           = 3
```

then B inherits the urgency of A:

```text
B effective Priority Score
=
max(B base score, A effective Priority Score)
```

Dependency inheritance is transitive.

For example:

```text
C blocks B
B blocks A
A effective score = 9

→ B effective score = 9
→ C effective score = 9
```

If A and B are blocked but C is executable, C becomes the visible high-priority
prerequisite.

The controller must reject dependency cycles as invalid workflow state. A cycle
cannot produce a meaningful execution order and should result in managed
feedback rather than an arbitrary transition.

---

## 11. Blockers and Ready Eligibility

Bug Tracker has no `Blocked` status.

An item with an active blocker is not eligible for Ready.

Conceptually:

```text
triage complete or not required
+ active Blocked-By relationship
→ not Ready
→ Backlog
```

An item still requiring investigation remains `To Triage` even when another
Issue also blocks implementation. Triage work may continue while implementation
is blocked.

When triage is complete and the blocker remains active, the Bug is `Backlog`.

Urgency inheritance ensures that the executable prerequisite receives the
priority needed to unblock important downstream work.

---

## 12. Ready Capacity

Bug Tracker limits `Ready` to five Issues:

```text
READY_LIMIT = 5
```

The candidate pool for Ready admission consists of open, actionable, unblocked
Bugs that are not already `In Progress`, `In Review`, or `Done`.

This includes:

- existing Ready Bugs
- actionable Bugs currently in Backlog
- Bugs whose triage has just completed successfully

Incomplete `To Triage` items are not Ready candidates.

The controller selects the five highest effective Priority Scores.

At the admission boundary, equal scores use deterministic tie-breaking:

1. older actionable item first
2. lower Issue number as the final tie-break

The tie-break affects admission selection and does not need to be encoded into
the visible Priority Score field.

---

## 13. Ready Rebalancing

Ready is continuously rebalanced while work has not started.

Example:

```text
Ready:
8.0  A
7.0  B
6.0  C
5.0  D
4.0  E
```

A newly classified or newly triaged Bug F becomes actionable with:

```text
F = 6.5
```

The new Ready set becomes:

```text
Ready:
8.0  A
7.0  B
6.5  F
6.0  C
5.0  D

Backlog:
4.0  E
```

The displaced item moves:

```text
Ready → Backlog
```

It must not move to `To Triage`, because priority rebalancing does not
invalidate completed triage.

`Priority Score DESC` sorting in the Project view determines the visible
Ready ordering. The controller does not need Engineering-style manual Project
position reconciliation for Bug Tracker.

`Set-Position` therefore has no effect on score-based Bug Tracker ordering.

---

## 14. Started Work Is Not Preempted

Priority Score controls the waiting/selection pool, not work that has already
started.

A score change or arrival of a more urgent Bug must never cause:

```text
In Progress → Ready
In Progress → Backlog
In Review   → Ready
In Review   → Backlog
```

The scoring/rebalancing policy acts on:

```text
Backlog
To Triage
Ready
```

Once a Bug enters `In Progress` or `In Review`, it remains on the normal
shared implementation/review lifecycle.

Bug Tracker v1 does not implement automatic work preemption.

### 14.1 Ready demotion and branch authorization

A Ready Bug may lose its top-five slot before qualifying development activity
has started.

Because branch creation occurs when a Ready Bug becomes execution-eligible,
the controller must reconcile branch authorization before demoting that Bug to
Backlog.

The controller records the branch creation/base SHA when the authorized branch
is created.

Before applying:

```text
Ready → Backlog
```

the controller fetches the current branch state and compares the branch head
with the recorded creation/base SHA.

If:

```text
current branch HEAD == recorded creation/base SHA
```

then no qualifying development activity has occurred.

The controller:

```text
revokes branch authorization
→ safely deletes the untouched remote branch
→ moves the Bug from Ready to Backlog
```

If instead:

```text
current branch HEAD != recorded creation/base SHA
```

the branch contains development activity.

The controller must not delete the branch or demote the Bug. It revalidates
current eligibility and, when the activity is valid, advances the Bug to
`In Progress`.

This current-state check prevents webhook delivery order from deciding whether
work is considered started.

### 14.2 Activity on a revoked branch

A branch that was safely deleted after Ready demotion may still exist in a
developer's local repository.

If that developer later pushes the old branch and recreates it remotely, the
push is observable GitHub activity but does not restore workflow authorization.

The canonical behavior is:

```text
Bug = Backlog
+ branch authorization revoked
+ commit/push observed
→ Bug remains Backlog
→ no In Progress transition
→ managed feedback
```

Observation does not recreate authorization.

The controller must not automatically delete a recreated branch when it
contains user commits. User work is preserved even when the branch is not
authorized for lifecycle progression.

If the Bug later re-enters the Ready top five, the controller must establish a
new authorization decision. An existing branch containing post-revocation user
commits must not be silently adopted as the newly authorized branch. Existing
work adoption, if supported later, requires an explicit policy rather than an
implicit bypass of Ready admission.

---

## 15. Priority Score Recalculation

The controller recalculates Priority Score when scoring inputs or dependency
urgency may have changed.

Important triggers include:

- initial Project normalization after classification
- Priority change
- scoring-label change
- Effort change
- Blocked-By / Blocks relationship change
- accepted Triage Report
- triage-driven semantic metadata revision
- close or reopen of a dependency
- Bug reopen

After a relevant recalculation, the controller re-evaluates the Ready top five
for the waiting pool.

A Triage Report may therefore change a Bug from, for example:

```text
To Triage
Priority Score = 4
```

to:

```text
confirmed regression
Priority updated
risk labels updated
Priority Score recomputed
→ Ready admission evaluated again
```

---

## 16. Ready to In Progress

After Ready admission, Bug Tracker reuses the shared branch and execution model.

For Bugs, `Create-Branch` defaults to `true`.

A Bug becomes branch-eligible only when:

```text
Status = Ready
+ no active blocker
+ Create-Branch resolves to true
```

The controller creates the authorized development branch and records its
provenance.

Branch creation alone does not start work.

The first qualifying commit or push on the authorized branch, after current
eligibility is revalidated, moves the Bug to:

```text
In Progress
```

and initializes Start date once according to the shared workflow contract.

Once this transition occurs, Ready top-five rebalancing no longer controls the
Bug.

---

## 17. Pull Request and Review Lifecycle

Bug Tracker reuses the shared Pull Request lifecycle unchanged.

A normal implementation PR declares:

```text
Target: <bug-issue-ref>
```

The controller validates the Target, authorized branch, and author policy,
projects the Development relationship, adds the PR to the Project, and
normalizes the accepted PR to Draft once during onboarding.

The shared lifecycle remains:

```text
Bug In Progress
→ accepted PR Draft
→ developer marks Ready for review
→ PR + Bug In Review

CHANGES_REQUESTED
→ PR Draft
→ PR + Bug In Progress

PR merged
→ PR Done
→ Bug closed + Done

PR closed without merge
→ PR Done
→ Bug remains open
→ Bug In Progress
```

Priority Score does not override these started-work transitions.

---

## 18. Reopen Policy

Reopening a Bug does not automatically invalidate previous triage evidence.

By default:

```text
Bug reopened
→ retain accepted triage history
→ recompute Priority Score
→ recompute blockers
→ evaluate Ready top-five admission
```

The result is typically:

```text
Ready
```

when the Bug is actionable, unblocked, and inside the top five, or:

```text
Backlog
```

when it is actionable but not currently admitted.

If renewed investigation is explicitly required, `Triage: true` routes the Bug
through `To Triage` again.

Reopen synchronization remains canonical and is not suppressed by
`Set-Status: false`, consistent with the shared workflow contract.

---

## 19. Sub-Issues

Bug Tracker may use native sub-issues where decomposition is useful.

`Sub-issues progress` remains visible as a Project field.

Sub-issues do not replace dependency relationships.

A parent Bug and its sub-issues participate in normal Bug Tracker scoring and
readiness policy unless a future specification introduces a more specialized
Bug decomposition policy.

---

## 20. Controller-Owned and Human-Owned State

Bug Tracker follows the same authority model as Parametron Engineering.

Controller-owned state includes:

- Bug Tracker Project membership
- Status where normal ownership applies
- Priority Score
- Ready top-five admission
- branch provenance
- Target / Development projection
- explicit Parent/dependency relationship projection
- canonical close/reopen synchronization

Human/model-authored semantic inputs include:

- Issue report prose
- Triage Report prose
- Priority and Effort proposals where classification policy permits
- comments and review discussion

The cheap model never writes GitHub state directly.

---

## 21. End-to-End Example

A triage-required Bug may flow as:

```text
Bug opened
→ classified as Bug
→ routed to Bug Tracker
→ Backlog
→ triage required
→ initial Priority Score calculated
→ To Triage

maintainer publishes parametron-triage-report:v1
→ Verdict = confirmed
→ semantic metadata re-evaluated
→ Priority Score recalculated
→ no active blocker
→ score enters top five
→ Ready

controller creates authorized branch
→ Bug remains Ready

first qualifying commit
→ Bug In Progress
→ Start date initialized

PR opened with Target
→ binding validated
→ PR normalized to Draft once

developer marks Ready for review
→ PR + Bug In Review

review requests changes
→ PR Draft
→ PR + Bug In Progress

developer marks Ready for review again
→ PR + Bug In Review

PR merged
→ PR Done
→ Bug closed + Done

Ready pool rebalanced
→ next highest-scoring actionable Bug admitted
```

If the Bug finishes triage but does not enter the top five:

```text
To Triage
→ confirmed
→ actionable
→ score below Ready cutoff
→ Backlog
```

If a more urgent Bug later enters the pool:

```text
new Bug enters Ready top five
→ lowest-scoring unstarted Ready Bug moves to Backlog
```

Started work is never demoted by this rebalance.

---

## 22. Deferred Bug Tracker Policy

The following are intentionally outside Bug Tracker v1:

- aging/long-wait Priority Score amplification
- automatic work preemption
- time-boxed Iteration planning
- Target date policy
- automated heavy-agent triage implementation/provider selection
- automatic closure semantics for non-confirmed triage verdicts

These can be added later without changing the core distinction between semantic
triage, deterministic priority scoring, bounded Ready admission, and the shared
implementation/review lifecycle.
