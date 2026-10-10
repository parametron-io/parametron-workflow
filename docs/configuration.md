# Deployment configuration

`internal/config` owns strict JSON parsing, deterministic validation and pure
schema binding. Workflow semantics remain Go-owned. The supported profiles are
`engineering` (Parametron Engineering) and `bug_tracker` (Bug Tracker).

A deployment example is below. Project numbers are illustrative; replace them
with the organization's actual Project numbers. All names match exactly,
including case and spaces. No name defaults or coercions are applied.

```json
{
  "organization": "parametron-io",
  "repositories": ["parametron-workflow", "parametron"],
  "projects": {
    "engineering": {
      "number": 1,
      "fields": {
        "status": "Status",
        "priority": "Priority",
        "effort": "Effort",
        "estimate": "Estimate",
        "start_date": "Start date"
      },
      "status_options": {
        "backlog": "Backlog",
        "ready": "Ready",
        "in_progress": "In Progress",
        "in_review": "In Review",
        "blocked": "Blocked",
        "done": "Done"
      }
    },
    "bug_tracker": {
      "number": 2,
      "fields": {
        "status": "Status",
        "priority": "Priority",
        "effort": "Effort",
        "estimate": "Estimate",
        "start_date": "Start date",
        "priority_score": "Priority Score"
      },
      "status_options": {
        "backlog": "Backlog",
        "to_triage": "To Triage",
        "ready": "Ready",
        "in_progress": "In Progress",
        "in_review": "In Review",
        "done": "Done"
      }
    }
  }
}
```

Organization is an organization login; repository values are unqualified names
within that organization. At least one repository and both Projects are required.
Projects are selected by organization owner and positive Project number; the
profiles must bind distinct Projects.

Every shown field and status binding is required. Engineering does not accept
`to_triage` or `priority_score`; Bug Tracker does not accept `blocked`. Unknown
profile, field and status roles are rejected. Status, Priority and Effort bind
single-select fields; Estimate and Priority Score bind numeric fields; Start
Date binds a date field. Status is represented as a single-select field in
normalized discovery data. Priority/Effort semantic allowlists are validated by
semanticreconcile at construction; config retains options generically.
Native Sub-issues progress is a GitHub UI
projection, not a controller-written custom field binding.

`Parse(io.Reader)` rejects malformed JSON, unknown or duplicate properties, trailing JSON,
blank required values, duplicate repository names, and contradictory field or
status names. Missing/null required objects, collections and values fail
validation; there are no optional deployment bindings in this version. Independent
source validation errors are sorted for stable diagnostics. Map keys select
fixed code-owned roles, never new workflow semantics.

`SourceConfig` contains names and numbers only. `Resolve(source, schema)` first
validates source again, then matches organization, repositories, Projects,
fields and Status options against explicit transport-independent `Schema` data.
Schema includes organization IDs/logins, repository IDs/owners/names, Project
IDs/owners/numbers, field IDs/names/kinds, option IDs/names, and organization
Issue Type ID/name records. Blank/duplicate Issue Type records are rejected.
Discovery must
provide complete relevant schema, including all options; pagination belongs to
the discovery adapter.

Every binding requires exactly one matching record. Zero matches and multiple
matches fail explicitly; input ordering never selects a winner. A unique field
with an incompatible kind fails. Discovered empty IDs and reused field/Status
option IDs within a Project are rejected. Resolution reports the first failure
in fixed profile/role and source repository order and returns no partial result.

`ResolvedConfig` contains the discovered organization and repository identities,
and separate Engineering/Bug Tracker Project bindings containing Project IDs,
field IDs keyed by canonical roles and Status option IDs keyed by canonical
statuses, FieldOptions[FieldRole][optionName] for all single-select fields, and
IssueTypes[name] for organization Issue Types. Nested maps are copied.
The semantic reconciler requires unique live Phase/Task/Feature/Bug IDs and all
canonical Priority/Effort options for both Projects. SourceConfig is unchanged.
Later consumers need no repeated name lookup. Its maps/slices are
owned by the returned value; consumers should treat it as read-only.
GitHub IDs are runtime discovery results, never durable user-authored bindings.
Changing a GitHub field name requires updating the deployment configuration.

Issue #9 provides the authentication boundary, GitHub transport and live schema
discovery. `github.Client.DiscoverSchema` translates current API data into
`Schema`; `app.Prepare` composes validated source, discovery, and resolution.
See [github-integration.md](github-integration.md). The command strictly parses
`--config` before constructing the runtime. `app.Run` always performs live schema
discovery and resolution before opening ingress; a pre-resolved configuration
cannot bypass startup validation. Secrets and operational settings remain
separate from SourceConfig. See [runtime.md](runtime.md).
