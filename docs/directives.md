# Deterministic body intent

`internal/intent.Parse(body, Context{Organization, Repository}) (Intent, error)`
parses a current Issue/PR body without GitHub reads/writes, semantic judgement,
resource-type inference, lifecycle decisions, or runtime integration. It uses
only the standard library. Context requires organization `parametron-io` and a
valid current repository name. No existence lookup or authorization is implied.

## Line recognition

Directives must start at column one with an exact, case-sensitive allowlisted
name immediately followed by `:`. Values may have surrounding ASCII spaces or
tabs; list tokens may be separated by one or more ASCII spaces/tabs. LF and CRLF
are accepted. No other whitespace normalization occurs. A recognized name with
an empty or invalid value fails. Additional colons are part of the value and
fail validation. Unknown names and near matches are ordinary prose.

Indented lines, blockquotes, list-prefixed lines, inline code, and directive
names embedded in prose are not directive lines. Backtick and tilde fences
suppress all enclosed lines: an opener has at least three identical markers,
with zero to three leading spaces and an optional info suffix. Backtick info
suffixes cannot contain backticks. A closer uses the same marker with at least
the opening run length and only spaces/tabs afterward. An unclosed fence
suppresses the rest of the body. This is a small lexical rule, not a Markdown
parser; there is no interpretation of HTML blocks/comments or other Markdown
containers. Put examples in fences, indentation, or explicit `>` quotes. A
column-one directive outside a fence is intent regardless of surrounding prose.

## Boolean intent and defaults

The only accepted values are lowercase `true` and `false`.

| Name | Global effective default |
| --- | --- |
| Automation | true |
| Classification | true |
| Triage | false |
| Audit | false |
| Validation | false |
| Review | false |
| Set-Status | true |
| Set-Position | true |
| Create-Branch | unresolved |

`Intent.Explicit` contains named `Boolean{Value, Explicit}` fields, with Go
field names `SetStatus` and `SetPosition`. `Value` is meaningful only when
`Explicit` is true. Identical repetitions are accepted; contradictory values
return `ErrConflict`. Absent directives leave explicit presence false.

`Intent.Effective()` derives global defaults and applies the master switch.
`Automation: false` suppresses Classification, Triage, Audit, Validation, Review,
and `AutomaticEstimate`, including explicitly enabled children. The explicit
values are preserved. `AutomaticEstimate` indicates master-switch permission
only; estimation eligibility/execution is future policy. SetStatus, SetPosition,
CreateBranch and all relationships are unaffected by Automation. Deterministic
dependency, membership, close/reopen and lifecycle processing are outside the
parser and are not disabled by this switch.

`Intent.CreateBranch` is a separate explicit `Boolean`. Absence means
unresolved/default-needed, never an effective false. #35 Engineering policy
resolves Phase false and Task/Feature true; Bug defaults belong to Bug Tracker
policy, and PR is not applicable.
The parser accepts explicit true/false without determining applicability.

## Relationship grammar

Only these names are recognized:

| Name | Cardinality |
| --- | --- |
| Parent | One semantic reference |
| Target | One semantic reference |
| Blocked-By | One or more whitespace-separated references |
| Blocks | One or more whitespace-separated references |
| Refs | One or more whitespace-separated references |

Local references are `#N`. Cross-repository references are
`parametron-io/repository#N`, with the literal case-sensitive organization prefix.
Qualified references naming the current repository fail as noncanonical; use
`#N` instead. Repository comparisons are case insensitive, and output repository
names are lowercase. Repository tokens contain 1–100 ASCII letters, digits,
`-`, `_`, or `.`, excluding `.` and `..`. This validates syntax only, not whether
a repository exists. Numbers contain only ASCII decimal digits, range from 1
to 9223372036854775807, and have no leading zeros. Leading zeros fail as
noncanonical rather than creating an alias.

`IssueRef{Repository string, Number int64}` contains a resolved lowercase
repository name even for a local reference. All refs belong to the fixed
organization grammar. It is comparable and contains no URL or GitHub node ID.
Parent/Target use pointers for presence; lists use ordered slices (nil when
absent). Parent/Target repetitions, including identical tokens, normalize to one
semantic value; conflicting values fail. Repeated list lines append new
identities in first-seen order. Duplicates within/across lines are removed by
semantic equality, including repository-case variants. Returned pointers and
slices belong to the caller; there are no shared mutable defaults.

Comma/semicolon lists, URLs, arbitrary owners, bare numbers, negative/zero
numbers, missing `#`, fragments, punctuation, and trailing prose fail. No
separator correction or external resolution occurs.

Accepted standalone lines:

```text
Automation: false
Audit: true
Parent: #10
Parent: #10
Blocked-By: #9 #10
Blocked-By: #10 #11
Refs: #22 parametron-io/parametron-engine#41
```

Here Audit is explicitly true but effectively false; BlockedBy is #9, #10, #11.

Rejected recognized directives:

```text
Automation: True
Review: yes
Parent:
Parent: https://github.com/parametron-io/repo/issues/1
Parent: #10
Parent: #11
Blocked-By: #9,#10
Blocked-By: #9, #10
Refs: #1;#2
Refs: #1; #2
Target: #1 trailing prose
```

Ignored prose/examples include `Set Automation: false when needed`,
`The Parent: field is useful`, `automation: false`, `Unknown: value`,
`Automation : false`, and `> Automation: false`. A body without recognized
lines is valid default intent with no relationships.

## Errors and deferred work

Issue #34 consumes current Parent/Blocked-By/Blocks through this parser in a
standalone read-side resolver. Children are discovered from body Parent across
configured repositories, independent of native SubIssues. Target/Refs do not
construct its graph. See [engineering-context.md](engineering-context.md).

Errors support `errors.Is` with `ErrContext`, `ErrDirective`, `ErrConflict`,
`ErrReference`, and `ErrNoncanonical`. Body errors additionally support
`errors.As` to `*intent.Error` with one-based Line, allowlisted Directive, and
Kind. Messages never include body/value text. Parsing returns the first error
in body order and a zero Intent, so partial intent cannot be used accidentally.
Invalid context is rejected before body parsing.

#25 owns semantic runner/provider execution; #26 owns validated classification
and estimation; #27 owns durable semantic integration, stale-result rejection
and classification-pending safety; #28 owns GitHub metadata/Project convergence.
None is implemented or wired by this parser package. #35 now computes pure
pre-development lifecycle, Create-Branch defaults, execution eligibility, and
Set-Status ownership in
[engineering-lifecycle-policy.md](engineering-lifecycle-policy.md). Canonical
Status and eligibility are independent of Set-Status ownership and Automation
suppression. Relationship projection, branch creation, and lifecycle mutation
remain later work.
