# Curated public agent capability manifest

**Status:** Proposed
**Date:** 2026-08-30
**Tracker:** [MAT-24](https://linear.app/matty-v/issue/MAT-24/designplatform-curated-public-agent-capability-manifest)
**Depends on:** [MAT-19](2026-08-30-durable-agent-tasks.md), [MAT-20](2026-08-30-task-progress-typed-results.md), [MAT-21](2026-08-30-cooperative-task-cancellation.md), [MAT-22](2026-08-30-resumable-multi-turn-tasks.md), and [MAT-23](2026-08-30-principal-scoped-task-authorization.md)
**Origin:** [MAT-6](https://linear.app/matty-v/issue/MAT-6/spikeplatform-what-formal-a2a-protocol-support-would-require-for-kyber), [A2A gap study](2026-08-30-a2a-protocol-support.md), and gap-study PR #183

## 1. Decision

Add a versioned, operator-curated public capability manifest to each Agent.
The desired contract lives in `Agent.spec.publicCapabilities`; its validation,
availability, and drift live in `Agent.status.publicCapabilities`.

Only explicit declarations become public promises. By Matt's decision on
2026-08-30, Kyber never auto-promotes a discovered skill, MCP tool, prompt,
model claim, or harness feature. Observations can support validation and show
operators what changed, but publication always requires an operator write.

The native manifest remains useful without A2A. A later G10 adapter projects a
validated manifest into an A2A Agent Card. Claude Code and Codex remain the
execution engines; Kyber supplies the stable, safe discovery envelope.

## 2. User outcomes

- An API client can discover stable capability IDs, descriptions, accepted
  input modes, emitted output modes, and supported task features before
  submitting work.
- An operator can review and approve the claims exposed for an agent without
  publishing private skill instructions or runtime internals.
- A runtime upgrade or missing skill changes availability and drift status; it
  does not silently rewrite the public contract.
- The same native manifest can back Kyber UI/API clients and later A2A
  discovery without two sources of truth.

## 3. Current state and gap

Kyber already has useful operational evidence:

- `kyber-skills` scans identity, vendored, and platform skills;
- the status sidecar posts bounded reports containing skill name, frontmatter
  description, source, linked runtimes, and health issues;
- `skillstore` durably keeps the latest report and
  `GET /api/v1/agents/{name}/skills` serves it to the operator UI; and
- Agent status reports lifecycle, current runtime/model, and other observed
  state.

Those reports answer “what files appear loadable right now,” not “what stable
service does this agent promise callers.” Skill descriptions are mutable repo
content. Skills may be private, administrative, unsafe for remote invocation,
or too broad to serve as contracts. A harness can also support task mechanics
that are not represented by a skill. Conversely, a declared business
capability may depend on several skills, connectors, and platform features.

There is no versioned public schema, approval boundary, feature vocabulary,
availability model, or compatibility rule. Publishing the raw skills endpoint
would expose paths and implementation detail while creating accidental API
promises from unreviewed content.

## 4. Three separate truths

The system must keep these distinct:

| Layer | Owner | Meaning |
| --- | --- | --- |
| Declared contract | Operator, in Agent spec | Stable public promise |
| Observed evidence | Runtime/platform reporters | Current facts used to validate the promise |
| Availability | Controller, in Agent status | Whether Kyber currently believes the promise can be served |

An observation can never create or broaden a declaration. Missing or changed
evidence may make a declaration unavailable or degraded, but does not delete
it. This separation makes drift visible and prevents a compromised repo or
model from advertising a new public capability.

## 5. Native schema

Proposed CRD shape:

```yaml
spec:
  publicCapabilities:
    schemaVersion: v1alpha1
    identity:
      displayName: Deployment assistant
      description: Plans and reviews bounded application deployments.
      documentationUrl: https://docs.example.com/agents/deployer
    capabilities:
      - id: deployment-plan
        version: "1.0"
        name: Plan a deployment
        description: Produces a reviewed deployment plan from repository context.
        inputModes: [text/plain, application/json]
        outputModes: [text/markdown, application/json, application/octet-stream]
        taskFeatures: [durable, progress, typed-results, files, cancellation, multi-turn]
        evidence:
          requiredSkills: [deploy]
          requiredConnectors: [github]
```

The public API returns a normalized form with immutable agent resource ID,
manifest revision/digest, generated timestamp, and URLs selected from trusted
installation configuration. It does not echo internal evidence details unless
the authenticated caller has operator-read authority.

### Identity

`displayName` and `description` are bounded plain text. `documentationUrl` is
optional and must use an installation-approved HTTPS origin. Agent names,
namespace names, pod addresses, and internal service URLs are not public
identity fields.

### Capability IDs and versions

IDs are unique within an agent, lowercase ASCII slugs, 1–64 characters, and
stable across wording changes. Removing or semantically narrowing a capability
is a contract change. `version` is an operator-managed opaque semantic version
string; Kyber also creates a monotonically changing manifest revision and
content digest so clients can cache and compare exact representations.

### Media modes

Use registered or valid vendor MIME types, lower-cased and parameter-free in
the declaration. Version one allows a bounded registry supported by MAT-20:

- `text/plain`, `text/markdown`;
- `application/json` with separately registered, bounded schema references;
- approved image/audio MIME types when runtime and result storage support
  them; and
- `application/octet-stream` only for controlled MAT-20 file results.

A declared output mode is a promise that Kyber can represent and deliver that
result, not a claim that every invocation emits it. Files are explicitly
included in v1 by the MAT-20 decision. Inline data, object-backed file results,
size limits, and content safety retain MAT-20's rules.

### Task features

Use a closed, versioned registry rather than free-form booleans:

```text
durable, progress, typed-results, files, cancellation, multi-turn,
authorization-request, event-replay
```

Each feature maps to one implemented Kyber contract from MAT-19 through MAT-22
and later work. A feature cannot become Available until the installation,
agent configuration, runtime adapter, and required dependencies all support
it. Harness marketing or model prose is not evidence.

### Private evidence

Optional `evidence` is operator-only configuration that tells reconciliation
what observations must hold. V1 supports exact required skill IDs, connector
kinds, platform features, and compatible runtime adapters. It never contains
skill bodies, prompts, arbitrary shell probes, credentials, or model-generated
attestations.

## 6. Validation

Admission and API validation reject:

- unknown schema versions, duplicate/invalid IDs, unsupported feature names,
  invalid MIME types, unsafe URLs, and values beyond count/length limits;
- public descriptions containing control characters or markup outside the
  documented plain-text subset;
- declared task features disabled at the installation level;
- impossible runtime/feature combinations known statically; and
- secret references, filesystem paths, internal hosts, raw tool schemas, or
  prompt-shaped fields anywhere in the public contract.

Some checks depend on observations and cannot reject the spec write reliably.
The controller accepts the declaration, sets a precise NotAvailable condition,
and reports the mismatch. Validation is deterministic and fail-closed for
publication: an invalid manifest is not partially served.

Limits for v1: at most 50 capabilities per agent, 16 modes and 16 task
features per capability, 1 KiB short descriptions, and 8 KiB total normalized
public payload. Larger catalogs require pagination and a separate product
design.

## 7. Harness capability audit

| Evidence | Claude Code | Codex | Safe public use |
| --- | --- | --- | --- |
| Identity/platform skill report | linked skill + health | linked skill + health | Exact skill ID may satisfy private evidence only |
| Runtime version/model report | observed at pod boot | observed at pod boot | Compatibility input, not a public capability claim |
| Native conversation | multi-turn session | multi-turn session | MAT-22 adapter feature when Kyber envelope is enabled |
| Tool/MCP invocation | supported through harness integration | supported through harness integration | Specific Kyber-managed MCP feature only |
| Runtime tool list/schema | provider/runtime dependent | provider/runtime dependent | Never auto-published in v1 |
| Model self-description | untrusted prose | untrusted prose | Never accepted as evidence |

The adapter exposes a versioned internal support matrix such as “this adapter
can receive MAT-19 tasks and call the MAT-20 task MCP.” It does not claim that
the model will perform a business capability correctly. Operator-declared
required skills bridge that remaining intent, and availability stays
conservative when a report is absent, stale, broken, or unlinked to the
selected runtime.

## 8. Reconciliation and status

For each Agent generation, the controller normalizes and validates the
declaration, loads the installed platform-feature registry and runtime-adapter
matrix, and joins the latest observed skill/connector/runtime reports. It
writes:

```yaml
status:
  publicCapabilities:
    observedGeneration: 17
    manifestRevision: "sha256:..."
    conditions:
      - type: Valid
        status: "True"
      - type: Available
        status: "False"
        reason: RequiredSkillBroken
        message: capability deployment-plan requires healthy skill deploy
    capabilities:
      - id: deployment-plan
        availability: unavailable
        reason: required-skill-broken
```

Availability values are `available`, `degraded`, `unavailable`, and `unknown`.
Unknown is used when evidence has never arrived or is too old for a requirement
with a declared freshness bound. Staleness never presents as healthy.

Status updates are idempotent and use observed generation. Transient stores or
report failures retain the last observation plus age and mark unknown; they do
not erase the stable declaration. The skills endpoint remains an operator
diagnostic and is not merged into the public response.

## 9. API and caching

Native authenticated endpoint:

```http
GET /api/v1/agents/{agent}/capabilities
```

It returns the normalized valid contract, per-capability availability, schema
version, immutable agent ID, revision/digest, and safe links. MAT-23 requires
principal authorization and agent-resource permission. Whether a deployment
also exposes a deliberately unauthenticated discovery view is a G10/security-
scheme decision; it is not enabled implicitly here.

The digest is an ETag. Cache keys include agent resource ID, manifest revision,
visibility class, and policy version. Stable declarations may use short
revalidation caching; availability has a shorter freshness window and its own
observed timestamp. An invalid declaration returns a stable safe error to
operators and is absent from any public projection.

Writes use the normal Agent update endpoint and Kubernetes generation/conflict
semantics. The PWA provides a structured editor, preview of the exact public
payload, validation errors, observed-evidence suggestions, drift warnings, and
an explicit publish/update action. Suggestions never preselect publication.

## 10. Change and compatibility policy

Additive capabilities, modes, or optional task features are backward-
compatible but still change the manifest revision. Removing a capability,
changing its semantic meaning, dropping a media mode, or adding a new required
input is breaking and requires an operator-incremented capability version.

The API schema version describes Kyber's envelope, not the business capability
version. Kyber supports at least the current and previous readable manifest
schema during migration. Unknown future fields are never interpreted as
permissions or automatically projected into another protocol.

Availability changes do not change the stable contract version, but do change
the availability timestamp/ETag. Clients decide whether to submit to degraded
capabilities; unavailable capabilities reject new task creation with a stable
reason while existing tasks follow their durable lifecycle.

## 11. Security and privacy

- Public fields are an allowlisted projection, never serialization of Agent
  spec/status or skill reports.
- Skill names can appear publicly only as explicitly declared capability IDs
  or prose; private evidence IDs are stripped.
- Prompt text, skill bodies/paths, tool schemas, connector account metadata,
  model names, pod/node data, health details, secrets, and internal endpoints
  are excluded.
- Descriptions render as text, not HTML or Markdown with active links.
- URLs are built from trusted installation origin configuration or checked
  against an allowlist; forwarded headers do not define them.
- MAT-23 policy controls authenticated discovery and operator evidence views.
- Audit records capture actor, prior/new digest, capability IDs, validation
  result, and timestamp without storing secret-bearing source content.

The manifest describes capability, not authorization. Seeing a capability does
not grant task creation, result access, a connector credential, or permission
to bypass harness approvals.

## 12. A2A projection seam

G10 can deterministically map:

- safe identity to Agent Card name/description/documentation;
- input/output MIME modes to the corresponding A2A modes;
- capability ID/name/description to A2A skill metadata;
- Kyber task features to A2A capability booleans only when semantics match;
  and
- trusted installation routes/security references to adapter-owned fields.

The A2A adapter must not publish private evidence or invent protocol claims.
Fields without a faithful mapping are omitted. A projection test fixture pins
each supported native schema version to the normative A2A representation so a
Kyber-native change cannot silently alter wire discovery.

## 13. Rollout

1. Add CRD spec/status types, structural schema, validation, feature registry,
   adapter matrix, and generated code.
2. Add reconciliation using existing durable skill reports and runtime status;
   expose conditions to operators only.
3. Add authenticated native read API with ETag and MAT-23 enforcement.
4. Add PWA structured editor, exact public preview, evidence suggestions, and
   drift display.
5. Opt agents in with empty manifests by default. Nothing is inferred or
   published during upgrade.
6. Seed example manifests only in documentation/test fixtures. Operators
   explicitly approve production declarations.
7. Let G10 add the A2A projection and any public discovery exposure after its
   security and caching design is accepted.

Rollback stops serving the new API but preserves unknown CRD fields and
declarations. Removing controller support must not erase operator-authored
manifests.

## 14. Tests and observability

Required tests cover schema limits, duplicate IDs, unsupported modes/features,
unsafe URLs/text, secret-shaped/private fields, CRD round trips, normalization,
digest stability, optimistic concurrency, and backward schema reading.

Reconciliation tests cover absent/stale/broken skills, wrong runtime linkage,
runtime upgrades, platform-feature disablement, connector loss, store outage,
agent stop/start, declaration changes, and recovery without auto-promotion.
Contract tests ensure the public response contains no paths, prompts, private
evidence, internal endpoints, model/pod metadata, or unapproved skills.

Metrics include manifests by validity, capability availability/reason, drift,
observation age, validation failures, public read latency/cache result, and
declaration changes. Avoid labels containing descriptions, user content, or
unbounded capability IDs. Logs identify agent and safe reason codes; detailed
evidence stays in authorized operator views.

## 15. Out of scope

- A2A Agent Card projection, discovery route, transport, or auth scheme;
- automatic promotion of skills, tools, prompts, or model claims;
- marketplace, ranking, semantic search, dynamic capability negotiation, or
  per-task generated capability claims;
- implementation of MAT-19 through MAT-23 features; and
- guarantees about model quality or downstream service availability.

## 16. Estimate

Estimated implementation is **2–4 engineer-weeks**: CRD/schema and validation
(0.5–1 week), reconciliation/adapter matrix (0.5–1 week), native API and PWA
(0.5–1 week), and security/contract testing plus rollout (0.5–1 week). G10's
A2A projection is separate.

## 17. Acceptance criteria

- Public capabilities originate only from explicit operator declarations.
- Declared contract, observed evidence, and current availability are separate
  and cannot broaden one another.
- Claude Code/Codex support is represented by a versioned adapter matrix and
  existing bounded observations, not raw harness metadata.
- The native API exposes a stable versioned manifest without private evidence
  or implementation details and is protected by MAT-23.
- Drift, missing evidence, and unsupported combinations fail closed with
  precise operator status.
- Upgrading an existing agent publishes nothing by default.
- A later A2A projection can be deterministic without making A2A the native
  source of truth.

