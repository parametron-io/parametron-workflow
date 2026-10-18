# Standalone PR binding authorization

`internal/prbinding` evaluates current implementation PR binding without side
effects. Production Engineering consumes it through `internal/pronboarding`;
the validator itself performs no Project mutation, branch creation, storage write,
assignment, managed feedback,
or PR lifecycle transition. The architecture's [binding conjunction](../system-overview.md#18-pull-request-binding-and-onboarding)
remains owned here; onboarding owns permitted initial mutations and provenance,
while `internal/prreview` consumes fresh accepted decisions for
[review/rework](execution.md#accepted-pr-review-and-rework).

[#48 pure companion ordering](pr-companion-ordering.md) consumes current accepted
decisions without taking over #46 authorization. Production `prcompanionreconcile`
consumes that pure plan; discovery, review, Development and supported rejection
feedback also consume current binding through their own narrow contracts.
Completion separately validates accepted onboarding and effect-specific terminal
evidence rather than treating an OPEN-only binding as permanent authority.

## Construction and inputs

`prbinding.New(Config)` returns a `*Validator` or `ErrInvalid`. Required fields:

| Field | Contract |
| --- | --- |
| `Deployment` | Resolved organization and configured repositories; other deployment fields are not retained |
| `Primary` | `engineeringcontext.PrimaryReader.ReadPrimary` for current Issue/PR observations |
| `Context` | `ContextReader.ResolveSnapshot` for complete Engineering state |
| `Completion` | `CompletionReader.Completed(ctx, storage.Resource)`; use `engineeringbranch.Controller.Completed` for validated durable evidence |
| `Branches` | `BranchReader.Branch(ctx, github.Repository, string)` for exact current heads state |
| `Developers` | `github.DeveloperReads` for current user, membership, and permission |

Absent and typed-nil dependencies fail construction. The configured organization
must have a valid login and a nonblank ID containing no space/tab/CR/LF.
Repositories must have valid branch repository identities, belong to that
organization, and have unique case-insensitive names and unique node IDs. The
repository slice is copied. This adds no configuration file or persisted schema.

`Validator.Evaluate(ctx, resource) (Decision, error)` requires a configured
repository, `Kind: "pull_request"`, and positive number. Invalid input returns
`ErrInvalid`. Current primary reads verify repository node ID, owner/name, resource
kind/number, current node ID, and any supplied NodeID. Missing current PR is a
policy rejection, rather than an accepted absent resource.

## Target and branch authority

The current PR body is parsed by strict `intent.Parse`; any parser failure yields
`InvalidTarget`. The parser canonicalizes its organization context to lowercase,
so qualified references require that exact lowercase prefix even when the resolved
login contains uppercase letters. Reference text is not case-folded. Snapshot
parsing and Target-body revalidation use the same rule; the original resolved
organization identity remains unchanged for membership and authorization checks.
Syntax acceptance does not authorize a repository. Target references resolve
within configured repositories,
including cross-repository Targets. A complete Engineering Snapshot supplies
current Issue identity, authoritative Type and declared graph facts. Type retains
the existing accepted semantic authority or validated native fallback contract.
`BuildRoadmapOrder` and lifecycle `Evaluate` must yield `ExecutionEligible`.
Supported Targets are OPEN Task, Feature, or Phase with explicit
`Create-Branch: true`; Bug Tracker work is unsupported. Observed Status, labels,
native Development links, webhook state, and assignees alone grant no authority.

Historical branch authorization is insufficient: validated successful creation
completion must exist. `Controller.Completed` validates the paired authorization
and creation facts and original durable trigger. Both same-target v1 and
cross-target origin v2 remain supported in their existing namespaces; no format,
namespace, or historical-authority semantics change. See
[branch evidence](execution.md#branch-identity-and-evidence).

Completion must identify the exact Target, configured repository, canonical
`development/issue-<number>` branch, and nonzero creation time. Separately, the
current branch must exist with that repository/ref and a valid commit SHA. The
current PR `HeadRepository`, `HeadRef`, and `HeadSHA` must match that current branch
exactly, including repository node identity. The current SHA may differ from the
historical creation base SHA; neither matching names nor SHA alone proves creation.

## Developer policy

`DeveloperEligibility(author, user, org, repo, membership, permission, assignees)`
is pure policy returning a rejection `Reason`, or the empty reason when eligible.
The PR author and current user must be verifiable human Actors (`Type: "User"`)
with valid login and stable node ID. IDs match exactly and logins match case
insensitively across author, user, membership subject, and permission subject.
Membership must refer to the exact configured organization and have `State:
"active"`; permission must refer to the exact Target repository with `Base:
"write"` or `"admin"`.

GitHub's effective base permission is decisive. Maintain/custom roles qualify
only through an effective `write`/`admin` response; `role_name` supplies no grant.
Membership and permission are both mandatory, including for outside collaborators.
If any current manual assignees exist, every assignee must have valid human
identity and the author must match at least one. Without assignees, membership
and write permission suffice. No assignee is initialized or changed, and existing
first-activity ownership is unchanged. The author need not have triggered branch
creation; branch provenance contains no developer-author requirement.

## Results and failures

`Decision` contains `Outcome`, `Reason`, and `Evidence`. `Accepted` (`accepted`)
has an empty Reason and value-only bounded Evidence: `PR` and `Target` current
`github.Identity`, `Kind` (`semanticpolicy.IssueType`), current `Branch`
(`github.Branch`), verified `Developer` (`github.Actor`), and `Policy` equal to
`active_org_member_repository_write_assignee_v1`. It contains no bodies, complete
Snapshot, credentials, or durable grant. `Rejected` (`rejected`) has a typed
Reason and zero Evidence, never partial accepted evidence.

| Reason constant | Value | Meaning |
| --- | --- | --- |
| `MissingPR` | `missing_pr` | Current PR absent |
| `InvalidTarget` | `invalid_target_directive` | Strict PR body parsing failed |
| `MissingTarget` | `missing_target_directive` | No Target directive |
| `OutsideDeployment` | `target_outside_deployment` | Target/configured context outside deployment |
| `UnresolvedTarget` | `unresolved_target` | Target unavailable or applicable invalid Snapshot/directive authority |
| `UnsupportedTarget` | `unsupported_target` | Type outside Task/Feature/Phase |
| `IneligibleTarget` | `ineligible_target` | Closed, Phase not explicitly branch-enabled, or not execution eligible |
| `MissingProvenance` | `missing_branch_completion` | No successful completed creation |
| `InvalidProvenance` | `invalid_branch_provenance` | Corrupt or mismatched completed provenance |
| `MissingHead` | `missing_head` | Missing PR head evidence or initially proven missing exact branch |
| `HeadMismatch` | `head_mismatch` | Current repository/ref/SHA mismatch or invalid branch SHA |
| `InvalidDeveloper` | `invalid_developer_identity` | Invalid/mismatched human or authorization subject identity, including assignees |
| `InactiveMembership` | `inactive_membership` | Verified membership is not active |
| `InsufficientPermission` | `insufficient_repository_permission` | Verified base permission is neither write nor admin |
| `AssigneeMismatch` | `assignee_mismatch` | Author matches no current assignee |
| `Changed` | `current_state_changed` | Material observable authority drift |

Reasons are deliberately bounded and sometimes coarse. Snapshot `ErrMissing` and
applicable `ErrInvalid` or directive errors map to `UnresolvedTarget`;
`ErrDeployment` maps to `OutsideDeployment`, and `ErrChanged` to `Changed`.
Branch `ErrProvenance` maps to `InvalidProvenance`. Other errors propagate with a
zero Decision: invalid primary identity can return `prbinding.ErrInvalid`, while
incomplete/cyclic context, roadmap/policy failures, storage failures, and typed
GitHub failures are not fabricated policy rejections. A matching typed
`github.MissingBranch` initially yields `MissingHead`; branch disappearance during
the final barrier yields `Changed`.

Explicit inactive membership or insufficient permission differs from inability
to verify either through the API. Authorization endpoint `404`/`NotFound` remains
an operational error, not proof of nonmembership. Cancellation/deadline remains
detectable with `errors.Is`; a canceled evaluation returns no accepted result.
No managed feedback comment is created.

## Freshness and consumption

Every evaluation fetches new PR, Target, complete context, completed provenance,
branch, user, membership, permission, and assignee evidence. A second read barrier
compares the relevant graph, Target body/state/identity/Type and assignees,
completion, branch, developer facts, and PR body/state/author/head. Material drift
rejects; invalid identities or provider failures can instead return errors.
Repeated evaluation reuses no accepted authorization grant.

The onboarding controller calls `Evaluate` again and checks current PR observation
and exact PR/Target/Project identity before effects. Rejected binding produces no
accepted onboarding effects or provenance. The review controller also
revalidates immediately before consequential actions. Acceptance is a current
result, not permission for future
actions or transactional snapshot isolation. GitHub reads are not atomic. The
current branch/provenance contracts cannot distinguish deletion and recreation
with identical ref and SHA; no ref-incarnation tracking or revocation is added.

`internal/prdevelopment` consumes fresh accepted binding and validated #47
completion, with independent current membership checks. It does not redefine
#46 eligibility or derive authority from native links. See
[Development relationships](pr-development.md) for its standalone contract.

## Verification and deployment

Committed tests cover constructor validation, pure developer eligibility, strict
Target syntax/identity/Type/lifecycle, exact PR head binding, cross-repository
Targets, real v1/v2 completion through temporary SQLite, corrupt/missing authority,
fail-closed rejection, current-state drift, repeated evaluation, cancellation,
typed GitHub failures, and read-only side-effect isolation. GitHub tests cover
current identity/permission endpoints, malformed stable identities, head repository
and assignee normalization, response limits, redirects, and credential isolation.

Run focused regression checks from the repository root:

```sh
nix develop --command go test -count=1 ./internal/prbinding ./internal/github
```

Tests use local fixtures and HTTP servers, not live deployment credentials.
Organization `Members: read`, repository `Metadata: read`, and the composed
Issue/PR/repository/ref readers' existing permissions are deployment prerequisites;
see [GitHub integration](github-integration.md). Credential grants and a live
permission rehearsal remain unverified. App-owned installation minting is
implemented and independently tested offline; see the [credential contract](github-integration.md#authentication-and-github-app-direction).
