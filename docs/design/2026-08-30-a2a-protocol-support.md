# A2A protocol support for Kyber agents

**Status:** Proposed
**Date:** 2026-08-30
**Tracker:** MAT-6
**Protocol baseline:** A2A protocol 1.0, verified against the corrected
`a2aproject/A2A` `v1.0.1` release artifacts on 2026-08-30. A2A wire-version
negotiation uses `1.0`; patch versions are distribution corrections, not wire
versions.

## Recommendation

Do not claim formal A2A support yet. There is no identified caller that
justifies the server-side task system Kyber would need, and the existing
bounded request/reply API is deliberately too small and short-lived to be
presented as an A2A task service.

If a real inbound caller appears, build **server support as its own project**,
starting with an explicitly experimental HTTP+JSON endpoint that serves an
honest Agent Card and bridges one bounded text request to the existing
request/reply path. That first phase is useful for interoperability discovery
and synchronous delegation, but it is not formal support. Promote it to formal
support only after Kyber has a persistent A2A task service, implements the
mandatory operation behavior for the declared binding, and passes the official
A2A Technology Compatibility Kit (TCK) for all declared capabilities.

Treat **outbound client support as a separate project**. It shares protocol
types and an SDK choice with the server, but not the server's public ingress,
task ownership, persistence, authentication, or runtime-completion boundary.
The smallest useful client is a loopback MCP sidecar or packaged CLI that lets a
Kyber agent discover and call an allowlisted external A2A endpoint. It does not
require Kyber itself to be an A2A server.

For a formal server, use the official Go SDK's transport-neutral handler and
HTTP+JSON binding, while keeping Kyber's task state in PostgreSQL and its
runtime bridge behind a Kyber-owned interface. Do not put A2A tasks in Agent
CRD status. Use Redis only for bounded dispatch and live event fan-out, never as
the authoritative long-lived A2A task record.

## 1. What “formal support” means

A2A 1.0 defines eleven binding-independent operations: Send Message, Send
Streaming Message, Get Task, List Tasks, Cancel Task, Subscribe to Task, four
push-notification configuration operations, and Get Extended Agent Card. A
server must publish an Agent Card and implement at least one of the standard
JSON-RPC, gRPC, or HTTP+JSON bindings. Streaming, push notifications, and an
extended card are optional capabilities, but an implementation that declares
them false must return the specified errors when their operations are called.
The card and every supported binding must describe the same behavior honestly.

The protocol also requires:

- the `A2A-Version: 1.0` negotiation contract and
  `VersionNotSupportedError` behavior;
- the submitted, working, completed, failed, canceled, input-required,
  auth-required, and rejected task states;
- authenticated-client isolation for task reads, listing, cancellation, and
  updates;
- structured binding-specific errors and defined idempotency behavior;
- HTTPS/TLS in production and authentication matching the card's declared
  OpenAPI-shaped security schemes;
- message history, output artifacts, cursor-based task listing, and consistent
  ordering for addressable tasks; and
- exact capability behavior for streaming, push notifications, and extended
  cards whether enabled or disabled.

There is now an official
[A2A Technology Compatibility Kit](https://github.com/a2aproject/a2a-tck).
The checked revision (`5996b79`, 2026-06-29) embeds the A2A 1.0.0 source,
contains 129 requirement definitions, exercises all three standard bindings,
and produces machine-readable, HTML, pytest, and JUnit reports. It has no
tagged release and contains an active backlog, so it is evidence rather than a
certification authority. No official certification registry, badge program, or
third-party approval process was found in the specification, governance,
roadmap, or TCK repositories.

Kyber's concrete bar for “formally supports A2A 1.0” should therefore be:

1. serve an Agent Card whose interfaces, skills, security, and optional
   capability flags match deployed behavior;
2. pass every applicable TCK `MUST` requirement for every declared binding,
   with no unexplained failures and a committed CI report tied to the tested
   protocol and TCK revisions;
3. document every applicable `SHOULD` result, including justified exceptions;
4. run Kyber-owned integration tests for authorization isolation, restart
   recovery, cancellation races, prompt injection, and resource limits that a
   generic protocol TCK cannot know about; and
5. publish the exact protocol minor, SDK, and TCK revisions in release notes.

That is self-attested, repeatable conformance. It is not certification, and
Kyber must not describe it as certified.

Primary protocol references are the
[A2A 1.0 specification](https://a2a-protocol.org/v1.0.0/specification),
[v1.0.1 release](https://github.com/a2aproject/A2A/releases/tag/v1.0.1), and
[normative protobuf schema](https://github.com/a2aproject/A2A/blob/v1.0.1/specification/a2a.proto).

## 2. Direction: two projects, not one

### Kyber agents as A2A servers

Inbound support is a control-plane product. The public endpoint authenticates
an external principal, resolves a particular Kyber agent, creates and owns an
addressable task, dispatches work into a long-lived runtime, persists task
updates and artifacts, and lets the same principal retrieve or cancel it. It
needs public routing, multi-tenant authorization, retention policy, task
storage, runtime signaling, audit logs, abuse limits, and protocol
conformance.

This is where Kyber's architecture has meaningful gaps and where most of the
cost lies.

### Kyber agents as A2A clients

Outbound support is an agent capability. The agent discovers a remote card,
selects a binding, supplies remote credentials, submits or follows a remote
task, and treats all returned card text, messages, URLs, and artifacts as
untrusted. The remote server owns the task lifecycle. Kyber needs bounded
egress, secret delivery, SSRF controls, and a usable runtime tool, but no Kyber
task store or public ingress.

The clean Kyber shape is the same one used by Telegram and Discord: a
loopback-only MCP sidecar that owns credentials and network calls, plus a
runtime skill describing safe use. A lighter first experiment can package the
official Go SDK's `a2a` CLI in a skill, using existing `USER_*` secrets, but it
would expose remote credentials to the runtime and offer weaker endpoint and
payload controls.

Server and client work may share a pinned `a2a-go` module and test fixtures.
They should have separate specs, feature gates, threat models, and delivery
decisions.

## 3. Binding choice

Choose **HTTP+JSON** for the first and formal server binding.

| Binding | Fit for Kyber | Decision |
| --- | --- | --- |
| HTTP+JSON | Matches the control plane's existing `net/http` server, route and middleware model, REST operational tooling, and SSE support model. The official Go SDK provides `NewRESTHandler`. | First binding |
| JSON-RPC | Mature in the official SDK and historically the reference-style binding, but adds an RPC envelope and method dispatcher without reducing Kyber's task or runtime work. Streaming still uses SSE. | Add only for a demonstrated client requirement |
| gRPC | Strong typed contract and native streaming, but adds HTTP/2/gRPC serving, proxy and load-balancer validation, generated service wiring, and another operational surface. | Defer |

The binding decision does not materially change the task-service cost. The
official [A2A Go SDK v2.5.0](https://github.com/a2aproject/a2a-go/releases/tag/v2.5.0)
supports clients and servers for all three bindings behind transport-neutral
interfaces and requires Go 1.25 or newer; Kyber is already on Go 1.26. Using it
avoids maintaining protocol serialization, error mappings, version handling,
and SSE framing by hand. It would be a new dependency and must receive the
normal explicit dependency review before implementation.

Do not declare multiple bindings merely because the SDK can mount them. Each
declared interface multiplies conformance and operational testing, and the spec
requires equivalent behavior across them.

## 4. Current Kyber foundation and exact gaps

### Existing inbound webhooks are delivery, not A2A

[`pkg/api/routes_inbound.go`](../../pkg/api/routes_inbound.go) accepts a
binding-specific HMAC-signed body, filters and renders it, enqueues it, and
returns an acknowledgement. Its generic non-leaking authentication failures,
bounded body, rate limits, per-agent queue, and Running-phase gate are useful
defense patterns.

It cannot be adapted directly into an A2A endpoint:

- its HMAC body signature is not an Agent Card security scheme and authenticates
  a binding, not a reusable client principal;
- the caller receives delivery acceptance, never a Message or Task;
- the binding selects fields into a static operator-owned action, while A2A
  content is caller-authored and may contain files, data, task references, and
  multi-turn input; and
- it has no task authorization, retrieval, cancellation, history, artifacts,
  or version negotiation.

Keep `/webhooks/inbound/*` unchanged. A2A should be a separate public route
family and authentication boundary.

### MAT-9 created a reusable bridge, not an A2A task model

The ticket's original statement that Kyber has no addressable request state is
now partly stale. Since it was written, MAT-9 added:

- authenticated scoped submit/read routes in
  [`pkg/api/routes_agent_requests.go`](../../pkg/api/routes_agent_requests.go);
- a Redis-backed atomic store in
  [`pkg/requeststore`](../../pkg/requeststore) with in-memory development
  fallback;
- typed jobs through the existing bounded per-agent inbound queue; and
- a loopback `kyber-request-reply.respond(request_id, response)` MCP tool whose
  sidecar-authenticated internal handler is
  [`pkg/api/internal_request_reply.go`](../../pkg/api/internal_request_reply.go).

Those are exactly the trust boundaries an A2A runtime bridge should preserve:
the caller never sees terminal history, the runtime gets no control-plane
credential or callback URL, the pod can act only as itself, delivery and
completion are separate, and completion is explicit and single-assignment.

The current request model is intentionally incompatible with formal A2A task
semantics:

| Kyber bounded request | A2A task requirement |
| --- | --- |
| 60-second default, five-minute hard lifetime | Retained addressable tasks with deployment-defined history and pagination |
| `queued`, `dispatched`, `completed`, `failed`, `expired` | Full eight-state lifecycle, including cancellation, input-required, auth-required, and rejection |
| One prompt and one text response | Multi-turn Messages plus typed Parts and output Artifacts |
| GET one request only | Get and cursor-paginated List, scoped to the authenticated principal |
| No cancellation path into the runtime | Idempotent Cancel Task with a real attempt to stop work |
| No incremental updates | Status/artifact events for streams and subscriptions |
| Redis TTL and bounded terminal ring | Durable source of truth suitable for restart recovery and ordered listing |

Do not stretch `requeststore.Request` until it becomes an accidental A2A
schema. Reuse its queue integration, pod-identity boundary, metrics patterns,
and explicit response-tool lesson behind a new task-executor interface.

### Agent completion is now explicit, but richer states are not

MAT-6 originally named unreliable Codex turn boundaries as a blocker. A2A
should not use a generic harness “turn ended” signal as task completion in
either runtime. MAT-9 established the stronger cross-runtime pattern: the agent
must explicitly call a platform tool for the request it is completing.

Extend that pattern with an A2A task MCP surface such as:

```text
a2a-task.update(task_id, state, status_message?)
a2a-task.publish_artifact(task_id, artifact)
a2a-task.complete(task_id, artifacts?)
```

The sidecar supplies agent identity, validates task ownership and transitions,
and forwards to the internal API. A cancellation request must also be delivered
to the runtime as a typed control job. The harness boundary is still
cooperative: an agent may ignore or delay cancellation, so Kyber must represent
“cancellation requested” internally until a valid canceled or terminal update
arrives or policy times out.

### Agent skills are observable but not an honest public contract

[`pkg/api/routes_agent_skills.go`](../../pkg/api/routes_agent_skills.go) serves
the last filesystem-backed skills report. The scanner records name,
description, source, runtime links, and health issues. This is useful input for
an Agent Card, especially because it measures what is actually loadable.

It is not enough to generate A2A skills automatically. A2A also requires stable
IDs, tags, examples, input/output media types, and potentially per-skill
security. Many Kyber skills are operational instructions such as restart or
memory saving that must never become public promises. A healthy loadable skill
may still require credentials or operator approval unavailable to an external
caller.

## 5. Task model and persistence

Put the authoritative A2A task model in an API-owned **PostgreSQL store**.

PostgreSQL is the best fit because A2A requires durable multi-row state,
principal-scoped queries, descending update-time ordering, cursor pagination,
history, artifacts, atomic transitions, and restart recovery. Kyber already
operates PostgreSQL for session briefs and fleet metadata. A task store can use
the official SDK's `taskstore.Store` interface at the transport edge while
retaining Kyber-owned migrations, authorization fields, limits, and retention.

Suggested logical records are:

- `a2a_tasks`: task ID, context ID, agent, authenticated principal/tenant,
  current state and status timestamp, cancellation request, created/updated
  timestamps, expiry/retention timestamp, and optimistic version;
- `a2a_messages`: immutable ordered client/agent messages and Parts;
- `a2a_artifacts`: artifact metadata and ordered Parts; and
- `a2a_task_events`: append-only transition/artifact events used to recover SSE
  subscriptions and audit invalid transitions.

Keep small text and structured-data Parts in PostgreSQL initially. Before
supporting arbitrary raw/file Parts, define a hard inline limit and place large
blobs in the existing object-storage abstraction with opaque, authorized
references. Never persist remote credentials or bearer headers in task
metadata.

Use Redis for:

- the existing bounded per-agent dispatch queue;
- cross-replica live event publication for SSE subscribers; and
- short-lived idempotency/dedup acceleration where PostgreSQL remains
  authoritative.

Do not use Agent CRD status. Tasks are external-client resources, not desired
or observed Kubernetes agent state. A task-per-CRD design would create API
server/watch churn, expose task content to broad Kubernetes readers, make
artifacts and history awkward, and couple retention to cluster control-plane
storage. Do not use the current TTL-only Redis request store as the source of
truth; its deliberate expiry and list-free interface conflict with A2A.

## 6. Agent Card source and honesty

Use an **operator-authored per-agent A2A configuration with validated,
selective derivation**:

- operator-authored: public name and description, provider, public version,
  documentation/icon URLs, enabled interfaces, security requirement, exposed
  skill IDs, tags, examples, media modes, and retention/size policy;
- platform-derived: actual endpoint URLs, protocol version, binding identifiers,
  and optional capability flags from enabled server components; and
- skills-report-validated: every exposed skill must match a current,
  non-broken, runtime-linked Kyber skill with the configured name.

Do not publish every reported Kyber skill. Do not derive capability flags from
the presence of a file. The server configuration and running components are
authoritative for streaming, push notifications, and extended-card support.

The comms API in
[`pkg/api/routes_agent_comms.go`](../../pkg/api/routes_agent_comms.go) is the
configuration pattern to copy: one per-agent protocol surface with idempotent
PUT, readback that omits secret material, validation before mutation, and clean
DELETE. An eventual shape could be:

```text
GET    /api/v1/agents/{name}/a2a
PUT    /api/v1/agents/{name}/a2a
DELETE /api/v1/agents/{name}/a2a
```

The public card should be served from a tenant-scoped URL as well as the
well-known resolver arrangement selected for the installation. A fleet cannot
serve one ambiguous `/.well-known/agent-card.json` for many agents without a
documented host or tenant routing rule.

Fail closed: if the skills report is absent/stale beyond policy, an exposed
skill is broken, the configured auth scheme is unavailable, or a declared
component is unhealthy, do not serve a card that overclaims. Return an
operator-visible configuration error while retaining the last valid config for
diagnosis.

## 7. Authentication and authorization

For the first server binding, declare **HTTP Bearer authentication** and map it
to Kyber named callers with new A2A-specific scopes. This is a standard A2A
HTTP security scheme and fits the existing `Authenticator` and `Caller`
boundary in [`pkg/api/auth.go`](../../pkg/api/auth.go).

Add scopes such as:

```text
a2a:send
a2a:tasks:read
a2a:tasks:cancel
```

Every task stores the authenticated caller identity (and tenant when enabled).
Get, List, Cancel, follow-up Send Message, subscribe, and push-config operations
must filter by that identity before distinguishing missing from inaccessible.
The legacy full-scope operator key should not be advertised in an Agent Card;
issue dedicated referenced secrets and enable enforcement for an A2A surface.

Keep the three auth domains separate:

- `/webhooks/inbound/*`: binding-specific shared-secret HMAC;
- `/api/v1/*`: operator/PWA Bearer or browser session; and
- public A2A routes: declared A2A Bearer credentials and A2A-specific scopes.

OAuth2/OIDC is the right longer-term scheme for cross-organization use, but
Kyber's current named static callers do not provide token issuance, discovery,
audience validation, subject mapping, or delegated scopes. Do not advertise
OAuth2 or OIDC until those exist. API keys and mTLS may be added for concrete
deployments, not spec completeness.

Treat all A2A content as untrusted prompt input. Preserve the existing
operator/caller boundary by rendering a fixed platform instruction separately
from caller Messages and Parts. Validate media types, byte counts, URL schemes,
redirects, and file fetches before dispatch. A card or message description must
never become privileged instructions merely because it came through a standard
protocol.

## 8. Phased delivery and cost

Sizes are one experienced Kyber engineer, including design updates, tests,
review fixes, and dev-environment verification but excluding queue time for
review or production rollout.

### Phase 0: demand validation — 2–3 days

- name the first real caller, required binding, authentication, media types,
  response latency, and task retention;
- confirm whether direct Message responses are sufficient or the caller needs
  addressable tasks; and
- decline the project if no caller will integrate within the next release
  horizon.

**Value:** prevents building a public task platform for hypothetical demand.

### Phase 1: experimental discovery and bounded send — 1–2 weeks

- add opt-in per-agent A2A config and an honest static Agent Card;
- mount the official SDK's HTTP+JSON handler for Send Message only;
- accept text/plain Parts within MAT-9's current bounds, dispatch through the
  existing request/reply path, and return a direct Message response;
- declare streaming, push notifications, and extended card false; return
  specified unsupported errors; and
- use dedicated Bearer callers, limits, audit logs, and an explicit
  `experimental` product label.

**Value:** a real external A2A client can discover and synchronously delegate a
small text request to a Kyber agent. **Not formal support:** mandatory task
operations and full TCK conformance are absent. This phase must not be marketed
as A2A-compatible or conformant.

### Phase 2: formal non-streaming A2A server — 6–10 weeks

- add PostgreSQL tasks, messages, artifacts, transitions, retention, ownership,
  listing, and pagination;
- add the explicit runtime task-update MCP bridge, follow-up input, cancellation
  delivery, failure recovery, and bounded text/data artifacts;
- implement Send Message, Get Task, List Tasks, and Cancel Task completely;
- implement the required error behavior for disabled streaming, subscription,
  push configuration, and extended-card operations;
- enforce version negotiation and A2A caller isolation; and
- pass all applicable HTTP+JSON TCK `MUST` checks plus Kyber restart,
  authorization, race, and prompt-injection suites.

**Value:** honest formal A2A 1.0 server support for non-streaming,
poll-or-cancel task clients.

### Phase 3a: streaming and subscriptions — 3–5 weeks

- add cross-replica event fan-out with replay from the task event log;
- implement SSE Send Streaming Message and Subscribe to Task, disconnect and
  resume behavior, backpressure, and terminal closure; and
- declare `streaming: true` only after TCK and proxy/load-balancer tests pass.

**Value:** interactive progress and efficient long-running task monitoring.

### Phase 3b: push notifications and extended card — 3–5 weeks

- persist push configurations and implement all four operations;
- add webhook egress allow/deny policy, DNS rebinding and private-address
  defenses, redirects, authentication credentials, retry/idempotency policy,
  cleanup, and delivery observability; and
- add an authenticated extended card only if a real private-skill use case
  exists.

**Value:** disconnected consumers. This is optional and should remain off
unless callers need it; webhook SSRF and credential handling make it a poor
checkbox feature.

### Separate client project — 2–4 weeks bounded, 6–10 weeks full

A bounded text-only HTTP+JSON MCP sidecar with card discovery, endpoint
allowlisting, Bearer secret references, Send/Get/Cancel, payload limits, and
untrusted-output labeling is approximately 2–4 weeks. Supporting all card
security schemes, bindings, streaming, files/artifacts, interactive
input/auth-required states, and interoperability testing is another 4–6 weeks.

The Phase 2 server plus hardening is therefore roughly **two to three engineer
months** before Kyber should make a formal claim. A full server with streaming
and push is roughly **three to five engineer months**. The bounded outbound
client can ship independently and earlier if that is where demand appears.

## 9. Risks and decisions that remain load-bearing

- **No caller:** the dominant product risk. Phase 0 is a real gate, not
  paperwork.
- **Agent cooperation:** explicit task updates are more reliable than turn
  inference but remain model-driven. Timeouts, invalid transitions, and missing
  completion must be first-class failed outcomes.
- **Cancellation:** tmux prompt injection cannot guarantee interruption of a
  tool or shell process already running. Formal cancellation needs a defined
  cooperative contract and honest timeout behavior.
- **Content size:** arbitrary A2A files can bypass the bounded-memory discipline
  established throughout Kyber. Start text/data-only and add object-backed
  artifacts deliberately.
- **Multi-tenancy:** task IDs are not authorization. Every store query and event
  subscription must carry caller ownership.
- **Card drift:** derived data can become stale; hand-authored data can lie.
  Validated selective derivation is intentionally stricter than either alone.
- **SDK drift:** the protocol and Go SDK are active projects. Pin versions,
  isolate SDK types at an adapter boundary, and rerun the pinned TCK on every
  upgrade.
- **TCK maturity:** it is official and useful but unversioned. Pin its commit,
  inspect skipped tests, and keep Kyber-owned coverage for behavior the TCK
  cannot observe.

## 10. Acceptance decision

Matt can accept this recommendation by choosing one of three outcomes:

1. **Recommended: not yet.** Close MAT-6 with this design, wait for a named
   inbound or outbound caller, and rerun Phase 0 when one exists.
2. **Experimental server.** Approve Phase 0 and Phase 1 only, with no formal
   support claim and a decision checkpoint before building the task service.
3. **Formal server.** Fund Phase 0 through Phase 2 as a two-to-three engineer
   month project; streaming and push remain separately justified options.

Outbound need should create a separate client issue rather than changing this
choice.

## Sources checked

- [A2A protocol 1.0 specification](https://a2a-protocol.org/v1.0.0/specification)
- [A2A normative schema at corrected v1.0.1 release](https://github.com/a2aproject/A2A/blob/v1.0.1/specification/a2a.proto)
- [A2A v1.0.1 release notes](https://github.com/a2aproject/A2A/releases/tag/v1.0.1)
- [Official A2A TCK](https://github.com/a2aproject/a2a-tck)
- [A2A project roadmap](https://github.com/a2aproject/A2A/blob/main/docs/roadmap.md)
- [A2A Go SDK v2.5.0](https://github.com/a2aproject/a2a-go/releases/tag/v2.5.0)
