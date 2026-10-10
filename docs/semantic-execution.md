# Bounded semantic execution

Issue #25 establishes `internal/semantic`, independently usable with only the Go
standard library. It is not wired into the controller. Semantic output is
untrusted input, not workflow authority: LLM interprets semantic content, Go owns
policy and mutations, and GitHub records observable state. Observation is not
authorization.

## Runner and provider contracts

Workflow callers use `Runner.Run(context.Context, Request) (Result, error)`.
The only supported, code-owned cheap capabilities are `ClassifyIssue`
(`classify_issue`), `ClassifyPR` (`classify_pr`), and `EstimateIssue`
(`estimate_issue`). Unknown capabilities, including heavy-agent capabilities,
fail before adapter execution.

`Request{Capability, Input{Title, Body}}` contains selected semantic content only.
This deliberately small envelope leaves normalized planning-context design to
#26. It has no provider/model choice, credentials, URLs, commands, headers,
environment, callbacks, or GitHub client. Callers are responsible for selecting
content; the type cannot prevent a secret being pasted into ordinary prose.

`Provider.Execute(context.Context, ExecutionRequest) (json.RawMessage, error)`
receives capability, configured model, selected prompt/schema assets, and Input.
Adapters must respect context and perform one attempt without retries or fallback.
Provider secrets, if future adapters need them, belong to adapter deployment
construction. No production adapter, HTTP transport, shell execution, environment
reading, secret loading, GitHub access, or logging is implemented here.

`Result{Output, Provenance}` returns an owned copy of one syntactically valid JSON
object. Empty responses, prose, invalid JSON, scalar/array/null responses, and
trailing values are rejected. The runner does not enforce schema fields, duplicate
output properties, semantic allowlists, or workflow policy. Even an empty object
or out-of-domain estimate passes this execution check; #26 must reject unsuitable
output before it can be accepted. Schema assets describe the intended field
shape, not implemented semantic validation.

## Deployment selection and construction

`ParseConfig(io.Reader)` strictly accepts this separate deployment contract:

```json
{"cheap":{"provider":"adapter-name","model":"model-name"}}
```

Provider and model are explicit, exact, nonblank identifiers of at most 128
printable ASCII characters without whitespace. There is no default, name
normalization, per-capability routing, fallback, or credentials field. Unknown
properties, duplicate properties (including escaped key spellings), malformed
JSON, trailing values, missing selections, and invalid direct construction fail
deterministically. Parse errors never echo supplied values or properties.

`config.SourceConfig` continues to contain GitHub organization/repository/Project
bindings only; it has not been extended. Semantic configuration contains no
GitHub bindings. There are no semantic command flags or runtime loaders yet.

`NewRunner(config, catalog, providers, timeout)` requires a positive explicit
`time.Duration` and an explicitly injected `map[string]Provider`. An unknown,
nil, or typed-nil selected adapter fails at construction. All three capabilities
use the same selected provider/model. Construction retains only the selected
adapter and snapshots the catalog; later registry mutations do not change it.
There are no global registries, init hooks, discovery, or plugin loading.

## Canonical assets

`LoadCatalog(fs.FS)` loads these fixed paths in capability order:

| Capability | Prompt | JSON Schema |
| --- | --- | --- |
| classify_issue | `prompts/cheap/classify_issue-v1.txt` | `schemas/model/classify_issue-v1.json` |
| classify_pr | `prompts/cheap/classify_pr-v1.txt` | `schemas/model/classify_pr-v1.json` |
| estimate_issue | `prompts/cheap/estimate_issue-v1.txt` | `schemas/model/estimate_issue-v1.json` |

Each selected asset has explicit version `v1` and identity equal to its canonical
path. Prompt/schema contents are immutable strings in a typed Catalog. Empty or
missing prompts and missing/invalid JSON object schemas fail with `ErrAssets`,
without filesystem diagnostics or content in errors. There is no automatic
working-directory lookup, duplicated asset tree, or generated embedding workaround.
Composition must explicitly supply a filesystem with these paths; final production
packaging belongs to later runtime wiring. Tests explicitly load the repository
assets. No installed controller dependency on a developer checkout is introduced.

Schemas use JSON Schema draft 2020-12, required named fields, and
`additionalProperties: false`. Issue classification names type/labels/priority/
effort; PR classification names labels; estimation names estimate. Semantic
value restrictions and detailed prompt refinements remain #26 work.

## Cancellation, timeout, and errors

Each call derives `context.WithTimeout(parent, configuredTimeout)` and invokes
exactly one provider attempt. An already cancelled/expired parent prevents work.
The provider receives the derived context, preserving parent cancellation and
an earlier parent deadline. On return, parent and derived context state take
precedence over late output or adapter errors. The runner neither spawns a
cancellation goroutine nor forces termination of adapters that ignore context;
such adapters violate the Provider contract. Durable retry scheduling remains
outside this package.

Stable errors support `errors.Is`:

| Error | Meaning |
| --- | --- |
| ErrConfiguration | Invalid deployment selection, timeout, or selected adapter |
| ErrRequest | Invalid request context |
| ErrCapability | Unsupported capability |
| ErrAssets | Missing or invalid catalog assets |
| ErrCancelled | Cancellation; also matches `context.Canceled` |
| ErrTimeout | Runner or parent deadline; also matches `context.DeadlineExceeded` |
| ErrProvider | Adapter execution failure |
| ErrResponse | Malformed structured response |
| ErrUnconfigured | Fake capability function not configured |

Adapters may return `ProviderError{Kind, Cause}` with `rate_limited`, `transient`,
or `permanent`; other kinds normalize to `unknown`. Runner failures expose
`*ExecutionError` through `errors.As`, preserving the category and underlying
cause for programmatic inspection. Unclassified errors remain unknown, without
substring matching. Neither error type formats arbitrary categories, causes,
prompts, input, or output into its public message. Explicitly unwrapping a cause
can expose adapter diagnostics; callers must not log or persist those diagnostics.
No category implies a retry decision here. No hidden retry, model switching,
provider switching, backoff, or jitter occurs.

## Provenance and test fakes

Every successful result includes capability, provider, model, prompt identity,
prompt version, schema identity, and schema version. Metadata includes no raw
prompt, input, output, credentials, GitHub identifiers, or timestamps. Output is
returned separately for Go validation. Provenance is not persisted by this package.

`semantic.Fake` implements Runner with explicit `ClassifyIssueFunc`,
`ClassifyPRFunc`, and `EstimateIssueFunc` fields. Functions receive context and
Input and return configured results/errors. Unconfigured calls fail loudly.
Closures can record requests deterministically; owners synchronize fixtures if
used concurrently, following the GitHub fake convention. Fake adds no mutable
recording, sleeps, network, or credentials. Focused runner tests use a separate
function-backed Provider test double to verify forwarding, deadlines, cancellation,
error categories, response rejection, and single-attempt behavior.

## Remaining phase work

Cheap inference is separate from future Triage, Audit, Validation, Review,
reproduction, and repository-aware agent tasks. No heavy-agent interface is
implemented or added to the cheap capability set.

#26 combines parsed explicit intent with semantic judgement and implements exact
schema/allowlist validation, normalized planning context, and prompt refinements.
This package does not import `internal/intent`.
#27 owns runtime/durable integration, classification pending, current-state
revalidation, stale-result rejection, retries/restarts, and accepted completion.
#28 owns authorized GitHub metadata and Project-routing convergence.
No app/worker/webhook wiring, SQLite schema, jobs, decisions, lifecycle policy,
relationship edges, branch authorization, or GitHub mutations are introduced.
