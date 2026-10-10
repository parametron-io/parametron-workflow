# Engineering current context

Issue #34 implements `internal/engineeringcontext`, a read-side input boundary
for #35. `New(Config)` accepts resolved deployment identities, an observation
primary reader, a separate `github.IssueLister`, and a
`semanticflow.AcceptedReader`. `Resolve(ctx, storage.Resource)` returns typed
`Context`, `IssueNode`, `ParentEdge`, and `DependencyEdge` structures. It has no
Runner, Mutator, worker, app, branch, or webhook input. Observation is not
authorization.

## Discovery and current facts

The production `github.Transport.ListIssues` uses the supported GraphQL
[Repository.issues IssueConnection](https://docs.github.com/en/graphql/reference/repos#repository).
It first verifies the configured repository node ID through repository lookup,
then enumerates all cursor pages of 100 through that node. No state filter is
applied: OPEN and CLOSED Issues are included. This is an Issue connection, not
search or issueOrPullRequest; returned `__typename` must also be Issue. Pull
Requests cannot become graph nodes. Repository identity, node IDs, positive
numbers, title/body/state presence, exact OPEN/CLOSED state, duplicate numbers
and node IDs, null nodes, page metadata, and continuation cursors are validated.
There are no retries or partial results. Existing typed github.Error categories,
HTTP/token infrastructure, response limits, and credential isolation apply.
`Client` and `Mutator` are unchanged; `ListerFake` fails unconfigured calls.

Every resolution enumerates all configured repositories. Discovery bodies are
parsed through `intent.Parse` with lowercase owning organization/repository.
The conservative discovery rule rejects **any invalid recognized directive in
any enumerated body**, including Target/Refs or booleans, because Parse returns
no partial intent. Valid unrelated Issues need no accepted classification and
are not refetched. Target and Refs never add graph edges, and their syntactically
valid references are not fetched or deployment-authorized by this resolver.
Deployment authorization of Parent/Blocked-By/Blocks applies when a node becomes
relevant. Valid unrelated graph declarations do not demand full validation.

Starting with the requested Issue, resolution expands to declared parents and
dependencies; incoming one-sided dependency declarations are discovered from
enumeration too. For every reached validated Phase, all enumerated Issues whose
body Parent points to it become candidates, including children in another
configured repository. Expansion continues until the relevant graph is complete.
Native SubIssues is not a complete child contract: projection may be absent or
stale. Native Parent, SubIssues, BlockedBy, and Blocking are never used to add,
remove, or reject declarative edges. A native child without a matching body
Parent is not an authoritative child.

Each candidate is refetched through `observe.Processor.ReadPrimary`, reusing
observation's deployment, resource-kind, repository-ID, number, and node identity
checks. Projects are unnecessary for this facts-only boundary. Current body is
parsed again; an enumerated participant whose body or node ID changed fails
with ErrChanged. Disappearance fails with ErrMissing rather than silently
removing a listed child or treating a blocker as closed. A fresh subsequent call
can rebuild context. Reads are not a transactionally atomic GitHub snapshot;
changes to unrefetched unrelated Issues or newly created Issues during pagination
cannot be detected as a single global instant. No cache or historical webhook
state substitutes for current reads.

`IssueNode.State` is typed `IssueState`, with open/closed represented by constants
`Open` (OPEN) and `Closed` (CLOSED). It is a repository-native observable fact.
Closed blockers and their edges remain in the graph; no Done or active-dependency
policy is computed.

## Relationship identities and edges

Parent belongs to the child. `ParentEdge{Child, Parent}` records that current
explicit intent independently of dependencies. A Phase body need not list
children. Parent does not imply Blocked-By, and dependencies do not imply Parent.

Both dependency forms normalize to `DependencyEdge{Blocker, Blocked}`:

```text
A Blocked-By B → B → A
B Blocks A    → B → A
```

Reciprocity is unnecessary. Mirrored declarations and repeated semantic edges
deduplicate by resource pair. Local `#45` is relative to the body-owning
repository. `parametron-io/parametron-engine#45` preserves the other owning
repository. Equal Issue numbers across repositories remain distinct.

Every relevant relationship reference must match a repository in
`ResolvedConfig.Repositories` case-insensitively; observation uses its discovered
identity and spelling. Syntax acceptance is not deployment authorization.
Unauthorized references fail with ErrDeployment before any read of that target.
The root is also checked before enumeration. No arbitrary repository is fetched.

Resource identity is lowercase owner, lowercase repository, Issue kind, and
number. NodeID is current validation evidence, not semantic identity. Output
nodes sort by owner, repository, number, then node ID. Parent edges sort by child
then parent; dependency edges sort by blocker then blocked, using the same
resource comparison. Public representation contains structs and slices, no
canonical maps. Returned slices and parsed intent belong to that context;
classification labels are copied. Local intent retains parser list order, while
graph edge ordering is independent of directive/discovery order. Ten repeated
resolutions with shuffled discovery order prove byte-stable encoding/json output.

## Semantic acceptance and failures

`semanticflow.NewAcceptedLoader(AcceptedStore)` constructs a read-only loader.
`AcceptedIssue(ctx, resource)` returns `(semanticpolicy.AcceptedIssue, bool,
error)` without any Runner or write interface. Semanticflow retains ownership
of the completion namespace, keys, private envelope decoding, canonical-byte
validation, domain/provenance validation, and originating event binding checks.
The coordinator and loader share the same private validation helpers. Accepted
label slices are caller-owned. Reading the originating event verifies identity
only; its body/relationships/state never enter context.

After parsing the current body, Type authority follows this precedence:

```text
accepted semantic completion, when present
    ↓ otherwise, only when effective Classification is disabled
validated current native Issue Type
    ↓ otherwise
ErrIncomplete
```

Accepted completion wins even when Classification is now disabled or native
Type has drifted. Without completion, effective Classification enabled fails
ErrIncomplete and never trusts native Type. With effective Classification disabled
(including Automation: false), current human-managed native Type must match both
the exact canonical Phase/Task/Feature/Bug name and its deployment-resolved node
ID. Missing, unknown, noncanonical, or mismatched Type fails ErrInvalid. New
validates nonblank, distinct canonical IDs and retains a defensive copy of only
those Issue Type bindings alongside organization identity.

IssueNode exposes validated Type and TypeSource (`accepted_semantic` or
`manual_native`), plus an optional AcceptedClassification. The accepted path
contains the real validated classification with copied labels; the manual path
has no accepted classification and fabricates no Priority, Effort, Labels, or
provenance. This preserves deterministic graph/lifecycle input under
Automation: false / Classification: false. Native relationships remain
non-authoritative. No model execution or reclassification occurs. Initial
accepted classification remains durable across ordinary edits, as #27 defines.
Corrupt completion fails with existing
semanticflow.ErrCompletion; persistence failures retain semanticflow's bounded
categories. GitHub and observation errors propagate through their existing typed
boundaries.

Local `*engineeringcontext.Error` unwraps stable categories for errors.Is:
ErrInvalid, ErrMissing, ErrIncomplete, ErrParentCycle, ErrDependencyCycle,
ErrDeployment, and ErrChanged. Errors contain no body text. Self Parent,
Blocked-By, and Blocks fail ErrInvalid. Conflicting directives use the existing
parser rejection rather than last-declaration-wins. Parent and dependency cycles
are checked independently using iterative Kahn traversal, including long cycles,
without recursion proportional to graph depth.

## Pure lifecycle consumer

#35 now implements Backlog/Blocked/Ready, Phase selection/activation, parent
gating, execution eligibility, Create-Branch defaults, and Set-Status policy in
[engineering-lifecycle-policy.md](engineering-lifecycle-policy.md). It consumes
this Context plus an explicit validated PhaseOrder; #36 now produces that order
through [engineering-ordering.md](engineering-ordering.md).
None is computed by this resolver. #36 orders Phase and parentless Task/Feature
roots, supplies a Phase-only projection to #35, and projects retained roots/children
to numeric Roadmap Order without Project-position input. Relationship projection, Project field writes,
branches, Start Date, Assignees, and later lifecycle synchronization remain later work.
There are no storage/schema, app/runtime, CLI, dependency, or Nix changes.
The default CLI and all Phase #3 behavior remain unchanged; this resolver is not
wired into production execution yet.
