# A2A protocol support for Kyber agents

**Status:** Proposed
**Date:** 2026-08-30
**Tracker:** MAT-6
**Protocol baseline:** A2A protocol 1.0, verified against the corrected
`a2aproject/A2A` `v1.0.1` release artifacts on 2026-08-30. A2A wire-version
negotiation uses `1.0`; patch versions are distribution corrections, not wire
versions.

## Recommendation

Do not treat A2A as one protocol feature to approve or reject. Work through the
gap register in dependency order and decide whether each missing capability is
worth building for Kyber on its own merits. Features that improve Kyber's
native request, automation, and integration model can land independently;
protocol-only adapters should wait until the underlying capabilities and a
real A2A caller both exist.

The first decision is a general-purpose durable agent task model. It is the
load-bearing gap for polling, cancellation, multi-turn work, progress,
artifacts, streaming, and restart recovery. If that model is not independently
valuable to Kyber, formal inbound A2A support is not worth pursuing. If it is,
the later A2A transport becomes a much smaller compatibility layer over a
Kyber-native feature.

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

## 2. Gap register

### Architecture guardrail: thin adapters, one platform envelope

Kyber must not reimplement the agent loop already provided by Claude Code and
Codex. Runtime adapters should pass through or project each harness's native
prompt, approval, tool-progress, file, continuation, and interruption
primitives wherever they exist. Each pursued design starts with a
harness-capability audit; custom runtime machinery needs a documented gap in
both supported harnesses.

Kyber owns the smaller contract that must be uniform across runtimes and remain
valid when a harness session or pod disappears: external task identity,
durability and retention, idempotency, caller authorization, bounded state and
result projection, and auditable capability claims. Harness sessions and
events feed that envelope but are not its source of truth. The estimates below
are planning ranges until the relevant capability audit identifies what can be
adapted directly.

Each gap below is an independent product decision. “A2A requirement” means the
capability is needed for the future state of a formally conformant inbound
HTTP+JSON server. “Native Kyber value” asks whether the feature earns its place
without that protocol. A decision of `pursue` authorizes a separate design and
implementation issue; it does not authorize the later gaps automatically.

| Order | Gap | Current state | Future state | Native Kyber value | Depends on | Rough size | Decision |
| --- | --- | --- | --- | --- | --- | --- | --- |
| G1 | Durable agent tasks | Bounded MAT-9 request records expire in at most five minutes and retain one response | Addressable, restart-safe task lifecycle with retention, Get, and List | High: long-running API automation, reliable job tracking, and a common request surface | None | L, 4–6 weeks | Pursue design: [MAT-19](https://linear.app/matty-v/issue/MAT-19/designplatform-durable-externally-addressable-agent-tasks) |
| G2 | Explicit progress and typed results | Runtime can submit one final text response | Validated task transitions, status messages, and bounded typed artifacts, including an explicit multimodal policy | High: structured progress and results without transcript scraping | G1 | M, 2–3 weeks | Pursue design: [MAT-20](https://linear.app/matty-v/issue/MAT-20/designplatform-task-scoped-progress-and-typed-multimodal-results) |
| G3 | Cooperative cancellation | No request cancellation path reaches a running agent | Idempotent cancel request, runtime delivery, acknowledgment, and honest terminal outcome | Medium/high: operator and API control over runaway or obsolete work | G1, runtime contract | M, 2–3 weeks | Pursue design: [MAT-21](https://linear.app/matty-v/issue/MAT-21/designplatform-cooperative-task-cancellation) |
| G4 | Multi-turn task continuation | Each bounded request is independent | Follow-up Messages on a task plus input-required, auth-required, rejected, and failed states | Medium: resumable approvals, clarification, and credential handoff for automations | G1, G2 | L, 3–5 weeks | Pursue design: [MAT-22](https://linear.app/matty-v/issue/MAT-22/designplatform-resumable-multi-turn-agent-tasks) |
| G5 | Principal-scoped task authorization | Named request scopes exist, but records are keyed only by agent and request ID | Every create/read/list/update/cancel is scoped to an authenticated caller or tenant | High: safe delegated automation and least-privileged integrations | G1 | M, 1–2 weeks | Pursue design: [MAT-23](https://linear.app/matty-v/issue/MAT-23/designplatform-principal-scoped-task-authorization) |
| G6 | Machine-readable public capability contract | Skills are observable internally; no curated public capability document exists | Per-agent validated capability manifest with stable skill IDs, media modes, endpoints, and honest health | Medium/high: discovery for gateways, catalogs, and operators even without A2A | Skills reporting | M, 2–3 weeks | Pursue design: [MAT-24](https://linear.app/matty-v/issue/MAT-24/designplatform-curated-public-agent-capability-manifest) |
| G7 | Live event subscriptions | PWA WebSocket carries CRD events; requests expose polling only | Ordered task event log and resumable per-task subscription, with SSE at the A2A edge | Medium: efficient progress UIs and API consumers | G1, G2 | L, 3–5 weeks | Pursue design: [MAT-25](https://linear.app/matty-v/issue/MAT-25/designplatform-durable-resumable-task-event-streams) |
| G8 | Outbound task webhooks | Kyber sends selected alerts; no caller-configured task callbacks | Persisted callback configs, authenticated delivery, retries, idempotency, SSRF defense, and cleanup | Medium/low until a disconnected consumer asks for it | G1, G2, G5 | L, 3–5 weeks | Defer; revisit for a concrete disconnected consumer |
| G9 | External identity suitable for cross-organization agents | Static Bearer callers and optional unenforced scopes | Enforced per-principal task scopes; OAuth2/OIDC only if federation is required | High for any serious external API consumer; OAuth federation itself is demand-specific | G5 | M for enforced Bearer; L for OIDC | Defer OAuth2/OIDC; G5 is the initial security profile |
| G10 | A2A Agent Card projection | No public card | Map G6 into the normative Agent Card and well-known/tenant discovery rules | Low without an A2A caller; protocol adapter over G6 | G6, G9 | S, 3–5 days | Bundle with G11; no separate native issue |
| G11 | A2A HTTP+JSON binding | No A2A routes, version negotiation, data model, or error mapping | One complete declared binding backed by G1–G6 and optional flags matching reality | Low without an A2A caller; interoperability value when one exists | G1–G6, G9–G10 | M, 2–4 weeks | Pursue design: [MAT-26](https://linear.app/matty-v/issue/MAT-26/designplatform-thin-a2a-httpjson-edge-adapter) |
| G12 | Conformance and release discipline | Normal Kyber tests only | Pinned SDK/TCK, applicable MUST pass, SHOULD review, security/restart tests, and published support matrix | Medium: repeatable compatibility discipline; specific suite is A2A-only | G10–G11 | M, 1–2 weeks | Review next and final |

The sizes are intentionally not additive estimates for one project. Each gap
needs its own scope after the previous decision. Some work overlaps: for
example, G1 plus G2 replaces much of the earlier monolithic Phase 2 estimate,
while G10 and G11 stay small only if the native foundations already exist.

### G1 decision brief: durable agent tasks

This is the first review because it is both the largest dependency and the
clearest standalone Kyber feature.

**Current Kyber capability:** MAT-9 accepts a short authenticated prompt,
queues it to a Running agent, and stores one explicit text response in Redis.
The default lifetime is 60 seconds, the hard maximum is five minutes, at most
two requests are outstanding per agent by default, and there is no list route.
This is intentionally a bounded request/reply channel, not durable work.

**A2A-required future state:** tasks survive control-plane restarts, remain
addressable through a retention window, carry a context and reserved origin
reference, expose current state, support stable descending pagination, and
provide atomic state transitions. G5 defines external-principal ownership and
access enforcement; later gaps add artifacts, cancellation, continuation, and
events.

**Standalone Kyber value:** this would let trusted systems submit long-running
agent work without holding a terminal or polling ephemeral request IDs. Jobs,
websites, other agents, and operator automation could share one durable status
model. It would also give Kyber a safe result surface that remains narrower
than transcripts. The feature is valuable only if Kyber wants to become an
automation API, not merely an interactive agent host.

**Cost and risk:** approximately four to six engineer weeks for the store,
migrations, API, retention, limits, restart behavior, observability,
and integration tests before progress/artifacts or cancellation. PostgreSQL is
the proposed source of truth; Redis remains dispatch infrastructure. The main
risks are retention cost, a false guarantee of execution durability while the
runtime is unavailable, and allowing a generic task API to become an unbounded
prompt or data-ingestion surface.

**Decision:** pursue the design in
[MAT-19](https://linear.app/matty-v/issue/MAT-19/designplatform-durable-externally-addressable-agent-tasks).
Matt made this decision on 2026-08-30 based on the feature's native automation
value. MAT-19 is design-only and explicitly excludes G2–G12.

### G2 decision brief: explicit progress and typed results

G2 asks whether a durable Kyber task should expose structured progress and
results instead of only a final text response. The decision can be made now,
but implementation remains conditional on an approved G1 design.

**Current Kyber capability:** the request/reply MCP tool accepts one bounded
string and completes the request in a single atomic transition. Agent activity
status can say that the whole runtime is working or idle, and transcripts may
contain intermediate work, but neither is tied to one request or safe to expose
as that request's result. A caller cannot distinguish “started,” “halfway,” or
“produced one result while continuing.”

**A2A-required future state:** the runtime can make validated, task-scoped
status transitions and attach status Messages or output Artifacts. A2A
Artifacts contain typed Parts that can carry text, structured data, inline raw
bytes, or URL-referenced media with a media type. Kyber does not need to clone
that schema as its native contract, but its result envelope must be extensible
enough to project the supported modes honestly. The task service rejects
invalid transitions, cross-agent updates, oversized content, and updates after
a terminal state. Streaming delivery remains G7; G2 only records queryable
current state and results.

**Standalone Kyber value:** API consumers and operator UIs can show meaningful
progress for long-running work without reading a transcript or inferring state
from global runtime activity. Typed JSON results let automations consume agent
output without parsing prose. Multiple named results support reports, patches,
or summaries while keeping internal reasoning private.

**Cost and risk:** approximately two to three engineer weeks after G1 for the
task-update contract, loopback MCP tools, sidecar/internal API forwarding,
transition validation, bounded persistence, read shapes, and both-runtime
tests. The main risk is asking a model-driven runtime to report progress
reliably: updates are cooperative, not proof of real-world side effects.
Multimodal results can also become an unbounded or unsafe file-transfer feature
without strict byte/count caps, ownership, retention, media validation, and
SSRF controls.

**Proposed native cut:** keep an extensible media-typed result envelope. Start
with bounded text and structured JSON; the design must decide whether the
first delivery also admits controlled object-backed media references. Those
references would carry a name, media type, size, checksum, authorization, and
expiry rather than exposing runtime filesystem paths or accepting arbitrary
remote URLs. Inline arbitrary blobs, incremental event delivery, cancellation,
and follow-up input are not part of the starting recommendation.

**Decision:** pursue the design in
[MAT-20](https://linear.app/matty-v/issue/MAT-20/designplatform-task-scoped-progress-and-typed-multimodal-results).
Matt made this decision on 2026-08-30. The issue makes the supported multimodal
subset and staging a load-bearing design question; implementation remains
conditional on the G1 design in MAT-19.

### G3 decision brief: cooperative cancellation

G3 asks whether callers and operators should be able to stop queued or running
task work without stopping the entire agent.

**Current Kyber capability:** a bounded request can expire or be removed from
the request/reply surface, but that does not reliably stop work already
dispatched to the runtime. Agent restart and stop controls operate on the whole
runtime, not one task, and may interrupt unrelated interactive work.

**A2A-required future state:** cancellation is an idempotent task operation.
Queued work can become canceled immediately; dispatched work records a
cancel-requested state, delivers a typed control message to the runtime, and
reaches a terminal state only after runtime acknowledgment or another known
terminal outcome. Kyber must describe this as cooperative cancellation, not a
guarantee that already-started side effects were undone.

**Standalone Kyber value:** callers can abandon obsolete work, operators can
limit runaway API jobs, and the platform can avoid further model and tool cost
without killing the long-lived agent. This becomes increasingly valuable as
G1 permits longer-running automation.

**Cost and risk:** approximately two to three engineer weeks after G1 for the
API and state transitions, dispatch control path, runtime integration,
timeouts, observability, and both-runtime tests. A prompt delivered through
tmux cannot reliably interrupt an active shell command or tool call. Correct
semantics therefore depend on runtime cooperation, and external side effects
may already have occurred before cancellation is observed.

**Proposed native cut:** cancel queued tasks immediately. For dispatched work,
record `cancel-requested`, notify the runtime, and publish `canceled` only on
acknowledgment or a known exit. Do not use whole-agent `SIGKILL` as task
cancellation, and do not promise rollback of side effects.

**Decision question:** is cooperative, task-scoped cancellation worth
designing as a Kyber feature for cost control and operator/API ergonomics,
independent of A2A?

**Decision:** pursue the design in
[MAT-21](https://linear.app/matty-v/issue/MAT-21/designplatform-cooperative-task-cancellation).
Matt made this decision on 2026-08-30. MAT-21 is design-only and must align
with the durable task and runtime update contracts in MAT-19 and MAT-20.

### G4 decision brief: multi-turn task continuation

G4 asks whether a durable task should pause for more input and then resume,
rather than forcing every API request to be complete and independent.

**Current Kyber capability:** Telegram and an attached terminal support natural
multi-turn conversation with the long-lived agent, but bounded API requests do
not. A request carries one prompt and one final response; a caller cannot answer
a clarification, approve a proposed action, or supply newly requested input on
the same task.

**A2A-required future state:** a task can enter `input-required` or
`auth-required`, expose a safe prompt explaining what it needs, accept a
follow-up Message in the same task and context, then resume or reject it.
Terminal failure and rejection remain distinguishable from a resumable pause.
Every continuation is authorized, bounded, ordered, and idempotent.

**Standalone Kyber value:** API automations could support approvals,
clarifying questions, and human-in-the-loop workflows without falling back to
transcript scraping or inventing a new task for every turn. It also creates a
channel-independent primitive shared by a future UI, chat adapters, and other
agents.

**Cost and risk:** approximately three to five engineer weeks after G1 and G2
for pause/resume states, message persistence, ordering and idempotency, runtime
wakeup, expiry, authorization, UI/API shapes, and both-runtime tests. The main
risks are indefinite paused tasks, duplicate or stale replies, prompt injection
across participants, and accidentally treating credentials as ordinary task
messages. `auth-required` must request a credential reference or authorization
flow; secrets must not be persisted in task history.

**Proposed native cut:** support one authorized follow-up at a time for an
explicit `input-required` pause, with a bounded prompt, expiry, and idempotency
key. Keep credential acquisition as a typed `auth-required` flow that passes
references, never raw secrets. Defer arbitrary participant chat and concurrent
branches.

**Decision question:** is resumable, human-in-the-loop task continuation worth
designing as a native Kyber automation feature, independent of A2A?

**Decision:** pursue the design in
[MAT-22](https://linear.app/matty-v/issue/MAT-22/designplatform-resumable-multi-turn-agent-tasks).
Matt made this decision on 2026-08-30. MAT-22 is design-only and depends on the
durable task and task-update contracts in MAT-19 and MAT-20.

### G5 decision brief: principal-scoped task authorization

G5 asks whether every task operation should be owned and authorized by a
stable caller identity instead of treating possession of an agent-level token
and task ID as sufficient access.

**Current Kyber capability:** API access can use named static Bearer tokens with
declared request scopes, but the scopes are not an enforced authorization model
for task ownership. Request records are addressed by agent and request ID. The
service does not persist a caller principal on each record or guarantee that
create, read, list, continue, cancel, and result access are filtered to that
principal.

**A2A-required future state:** authentication resolves a stable principal and
tenant boundary before task handling. Every task stores its owner; every
operation enforces both action scope and resource ownership; list and event
surfaces cannot reveal another caller's tasks. Operator access is explicit,
least-privileged, and audited. G9 can later add federated OAuth/OIDC identities
without changing this native authorization model.

**Standalone Kyber value:** multiple automations, services, users, or future
agents can safely share one Kyber installation without task IDs acting as
bearer secrets. Per-principal revocation and auditability make the task API
viable beyond a single trusted integration. This is a prerequisite for safely
shipping G1 as a broadly reachable API.

**Cost and risk:** approximately one to two engineer weeks after G1 for the
principal model, persisted ownership, enforced scopes, list/query filters,
operator override, migration, audit events, and adversarial tests. The largest
risk is an incomplete check on a secondary surface such as artifacts,
continuations, events, or cancellation. Existing tokens also need an explicit
migration policy so legacy access does not silently become over-privileged.

**Proposed native cut:** keep static Bearer credentials initially, but resolve
each to a stable principal with explicit task action scopes. Persist owner and
tenant on creation; require ownership plus scope for all subsequent operations;
make operator override a separate audited permission. Defer federation and
third-party identity claims to G9.

**Decision question:** is principal-scoped ownership and authorization worth
designing as a native Kyber security boundary for the task API, independent of
A2A?

**Decision:** pursue the design in
[MAT-23](https://linear.app/matty-v/issue/MAT-23/designplatform-principal-scoped-task-authorization).
Matt made this decision on 2026-08-30 and clarified that Kyber should expose a
unified platform contract over harness-native concepts rather than reimplement
them. MAT-23 is design-only and applies that thin-adapter boundary explicitly.

### G6 decision brief: machine-readable public capability contract

G6 asks whether each Kyber agent should publish a curated, machine-readable
description of what external callers can ask it to do, independent of the A2A
Agent Card wire format.

**Current Kyber capability:** the platform can observe runtime skill reports
and knows operational facts such as runtime type, model, endpoints, and health.
Those facts are internal and dynamic. There is no stable, operator-approved
public contract describing skill IDs, input/output media modes, supported task
features, or whether a capability is currently available.

**A2A-required future state:** G10 needs enough validated native data to project
an honest Agent Card: identity and description, stable skills, endpoints,
security schemes, input/output modes, supported optional features, and version.
Claims must not be inferred solely from a model's prose or the presence of a
tool; advertised capabilities must match the selected harness adapter and
deployed platform features.

**Standalone Kyber value:** gateways, UIs, automation catalogs, and other Kyber
agents can discover compatible agents without scraping prompts or coupling to
Kubernetes internals. Operators can curate the supported public surface while
Kyber validates it against observed runtime and platform capabilities.

**Cost and risk:** approximately two to three engineer weeks for the native
manifest schema, CRD/API storage, validation, runtime-capability reconciliation,
health projection, caching, UI/operator workflow, and tests. A thin adapter
should reuse harness-native tool/skill metadata where available. The main risk
is confusing discovered capabilities with approved public promises, producing
stale or unsafe advertisements, or exposing private skill instructions.

**Proposed native cut:** add an operator-authored per-agent public manifest with
stable capability IDs, short descriptions, bounded media modes, and declared
task features. Validate claims against Kyber's runtime-adapter capability
matrix and deployment state; expose availability separately from the stable
contract. Do not publish raw prompts, skill files, tool schemas, or
model-generated claims automatically.

**Decision question:** is a curated, validated capability manifest worth
designing as a native Kyber discovery feature for gateways, UIs, and agent
catalogs, independent of A2A?

**Decision:** pursue the design in
[MAT-24](https://linear.app/matty-v/issue/MAT-24/designplatform-curated-public-agent-capability-manifest).
Matt made this decision on 2026-08-30. MAT-24 is design-only and begins with a
Claude Code/Codex capability audit under the thin-adapter guardrail.

### G7 decision brief: live event subscriptions

G7 asks whether task clients should receive ordered progress and result events
as they happen instead of repeatedly polling the current task representation.

**Current Kyber capability:** the PWA receives control-plane and CRD changes on
a WebSocket, and each harness has its own live execution behavior. Bounded API
requests expose only polling and a final response. Kyber has no durable,
task-scoped event sequence that an external caller can resume after disconnect.

**A2A-required future state:** every accepted task transition, status Message,
and Artifact update produces an ordered event authorized exactly like the
task. A subscriber can reconnect from a cursor, receive retained missed events,
then follow live updates without gaps between replay and fanout. The A2A edge
can project that native log through SSE while normal task reads remain the
authoritative current snapshot.

**Standalone Kyber value:** operator UIs, API automations, and other agents can
show long-running progress promptly and efficiently. Resumability avoids
fragile polling loops and lets callers recover after mobile or network
disconnects. The value is strongest once G1 and G2 produce durable, meaningful
task updates.

**Cost and risk:** approximately three to five engineer weeks after G1 and G2
for a transactional event sequence, retention and cursor rules, replay/live
handoff, authorization, backpressure, connection limits, SSE/API integration,
and restart tests. Harness-native events should feed G2 through thin adapters;
G7 must not become a second transcript or token-streaming system. Slow clients,
unbounded retention, leaked cross-principal events, and replay gaps are the
main risks.

**Proposed native cut:** persist one monotonic event sequence per task for
state, bounded status, and result metadata already accepted by G2. Expose a
principal-scoped resumable subscription with bounded retention, heartbeats,
backpressure, and explicit cursor-expired behavior. Exclude raw transcript
chunks, hidden reasoning, token deltas, and arbitrary harness events.

**Decision question:** is a durable, resumable task event stream worth
designing as a native Kyber feature for responsive UIs and API consumers,
independent of A2A?

**Decision:** pursue the design in
[MAT-25](https://linear.app/matty-v/issue/MAT-25/designplatform-durable-resumable-task-event-streams).
Matt made this decision on 2026-08-30. MAT-25 is design-only and limits the
stream to normalized platform task events under the thin-adapter guardrail.

### G8 decision brief: outbound task webhooks

G8 asks whether a task caller should be able to register a callback URL and
have Kyber deliver later task updates without keeping a subscription open.
This is an optional A2A capability: Kyber can remain conformant to the baseline
while declaring push notifications unsupported.

**Current Kyber capability:** Kyber sends selected operator-configured alerts
to chat channels, but a request caller cannot supply a per-task callback. There
is no persisted delivery queue, caller-specific signing secret, retry ledger,
or destination-validation policy for task notifications.

**A2A-required future state if advertised:** callback configuration is
authorized and persisted with the task. Delivery is signed or otherwise
authenticated, retryable, deduplicable by the receiver, observable, and
cleaned up with task retention. Destination handling defends against SSRF,
DNS rebinding, redirect abuse, credential leakage, and internal-network access.

**Standalone Kyber value:** disconnected automation platforms could receive
completion or progress without polling or maintaining SSE. The value is lower
while Kyber has no concrete webhook consumer; G7 already covers connected and
reconnecting clients with a simpler trust boundary.

**Cost and risk:** approximately three to five engineer weeks after G1, G2,
and G5 for callback registration, encrypted credentials, an outbox/worker,
signing, retries and dead-letter handling, destination policy, observability,
rate limits, and adversarial network tests. The main risk is turning Kyber into
a privileged network-pivot and reliable-delivery service for arbitrary URLs.
This work is platform infrastructure, not a harness adapter.

**Proposed native cut if pursued:** completion-only webhooks to pre-approved
HTTPS destinations, with per-destination authentication, signed event IDs,
bounded retry/backoff, an outbox, and no redirects or private-network targets.
Defer caller-supplied arbitrary destinations and per-token progress callbacks.

**Recommendation and decision question:** defer G8 until a real disconnected
consumer requires it. Does Kyber have enough standalone need to pursue the
webhook design now, or should the future A2A server declare push notifications
unsupported initially?

**Decision:** defer G8. Matt made this decision on 2026-08-30. The initial A2A
surface will declare push notifications unsupported. Revisit when a concrete
disconnected consumer needs callbacks strongly enough to justify the delivery
and network-security infrastructure.

### G9 decision brief: external identity for cross-organization agents

G9 asks whether Kyber needs federated OAuth2/OIDC identity now, beyond the
stable static-token principals and authorization boundary being designed in
G5. A2A can advertise multiple security schemes and does not require OIDC for
baseline interoperability.

**Current Kyber capability:** installations can authenticate named static
Bearer credentials, but task ownership and scopes are not yet fully enforced.
G5 closes that authorization gap by resolving credentials to stable principals
and applying ownership plus action scopes on every task surface.

**A2A-required future state:** the Agent Card truthfully advertises a supported
security scheme and every A2A operation applies G5 authorization. High-entropy,
rotatable, revocable Bearer credentials over TLS can satisfy the initial
machine-to-machine deployment. OAuth2/OIDC becomes necessary when callers from
other organizations must bring identities issued by their own authorization
servers or require delegated user consent and short-lived claims.

**Standalone Kyber value:** federation would improve enterprise SSO,
short-lived credentials, centralized revocation, workload identity, and
cross-organization delegation. Those benefits are substantial only with a
known identity provider, trust topology, tenant mapping, and consumer; a
generic provider integration designed in advance is likely to encode the wrong
flows.

**Cost and risk:** the enforced static-principal baseline belongs to MAT-23.
Adding production OAuth2/OIDC is a separate large effort, roughly three to five
engineer weeks for issuer discovery, JWKS caching and rotation, audience and
claim validation, principal/tenant mapping, scopes, clock skew, revocation
expectations, configuration, observability, and adversarial tests. Multi-issuer
or delegated user flows can make it larger. Authentication bugs directly
compromise the task authorization boundary.

**Proposed native cut:** finish G5 with rotatable named service principals,
hashed credential storage, explicit scopes, audit, and no query-string tokens.
Let G10 advertise that supported scheme. Defer OAuth2/OIDC until a concrete
cross-organization or enterprise identity requirement names the issuer,
grant/credential flow, audience, tenant mapping, and revocation expectations.

**Recommendation and decision question:** treat G5 as sufficient for the
initial A2A security profile and defer federated OAuth2/OIDC. Is there a known
cross-organization identity use case that warrants pursuing G9 design now?

**Decision:** defer federated OAuth2/OIDC. Matt made this decision on
2026-08-30. MAT-23's scoped, rotatable service principals define the initial
A2A machine-to-machine security profile. Revisit G9 only for a concrete issuer,
flow, audience, tenant mapping, and revocation requirement.

### G10 decision brief: A2A Agent Card projection

G10 asks whether Kyber should publish the normative A2A Agent Card and
discovery routes by projecting the native capability manifest from G6. Unlike
G6, this is protocol adapter work with little independent Kyber value.

**Current Kyber capability:** there is no public Agent Card. MAT-24 will design
the curated native contract containing stable public capabilities, media modes,
task features, availability, and the security information needed for safe
discovery.

**A2A-required future state:** an A2A client can retrieve a valid Agent Card at
the applicable discovery location and learn the agent's identity, skills,
declared interfaces, security schemes, input/output modes, and optional
features. Every claim is derived from the operator-approved G6 contract and
actual deployed A2A features; unavailable or unsupported capabilities are not
invented from harness metadata.

**Standalone Kyber value:** negligible beyond the native manifest. Kyber-owned
UIs, gateways, and catalogs should consume G6 directly. The Agent Card earns
its place only as the standards-compatible representation used by external A2A
clients.

**Cost and risk:** approximately three to five engineer days once G6 and the
initial G5 security profile exist, covering deterministic projection,
well-known and tenant/agent routing, caching, version/media type, validation,
and tests. The primary risk is advertising optional features before their
backing gaps are implemented or allowing deployment details to drift from the
card.

**Proposed cut:** keep the Agent Card a read-only deterministic projection of
G6 plus deployed adapter capabilities. Do not create a second operator-edited
source of truth. Advertise the G5 Bearer scheme, omit deferred webhooks and
OIDC, and validate generated cards against the pinned A2A schema/TCK fixtures.

**Recommendation and decision question:** do not open an independent native
feature design. Bundle G10 into the eventual A2A adapter issue with G11 and its
conformance work. Is that the right disposition, or should Agent Card
projection get a separate design issue now?

**Decision:** bundle G10 with the A2A adapter. Matt made this decision on
2026-08-30. No separate native design issue will be opened; MAT-24 remains the
single capability source of truth and G11 owns its normative projection.

### G11 decision brief: A2A HTTP+JSON binding

G11 is the actual standards adapter: whether Kyber should expose the approved
native task, capability, and authorization contracts through one declared A2A
HTTP+JSON binding. It has little independent product value, but it is the
feature that delivers formal A2A interoperability.

**Current Kyber capability:** Kyber has its own authenticated agent request
routes and runtime adapters, but no A2A URLs, wire types, method semantics,
version negotiation, Agent Card, streaming projection, or normative error
mapping.

**A2A-required future state:** an external A2A client can discover a Kyber
agent, authenticate with the advertised G5 security scheme, send and retrieve
Messages/Tasks, continue or cancel when supported, retrieve typed Artifacts,
and subscribe to events through the declared HTTP+JSON interface. Unsupported
optional capabilities such as push notifications and OIDC are omitted from the
card and rejected according to the specification. Native Kyber services remain
the source of truth.

**Standalone Kyber value:** low for existing Kyber clients, which should use
the native API. The value is standards interoperability: third-party agents,
gateways, and tooling can call Kyber agents without a Kyber-specific
integration. This is the explicit outcome of MAT-6 rather than another native
foundation.

**Cost and risk:** approximately two to four engineer weeks after the required
native designs are implemented, including G10 projection, HTTP routing and
media types, official SDK integration where it reduces translation code,
version/error mapping, security wiring, SSE projection, limits, observability,
and interoperability tests. The estimate is conditional on a mapping audit;
the adapter must stay thin. The main risks are semantic drift between native
and A2A states, accidentally advertising deferred options, and leaking
cross-principal tasks or internal harness details.

**Proposed cut:** support one pinned A2A 1.0 HTTP+JSON binding backed entirely
by G1–G7 and the G5 security profile. Bundle the deterministic G10 Agent Card.
Do not create a parallel task store or agent loop. Declare webhooks and OIDC
unsupported, map only normalized platform updates, and keep protocol types at
the edge behind translation tests.

**Recommendation and decision question:** pursue a design-only A2A adapter
issue if formal interoperability remains the goal. It should reference every
native dependency, bundle G10, and leave G12 to define the independent
conformance/release gate. Pursue G11 design?

**Decision:** pursue the design in
[MAT-26](https://linear.app/matty-v/issue/MAT-26/designplatform-thin-a2a-httpjson-edge-adapter).
Matt made this decision on 2026-08-30. MAT-26 bundles G10, references the native
dependency designs, and explicitly excludes a parallel task store or agent
loop.

### G12 decision brief: conformance and release discipline

G12 asks whether formal A2A support should have a repeatable compatibility gate
and published support profile instead of relying only on ordinary endpoint
tests and a claim that Kyber follows the specification.

**Current Kyber capability:** normal unit, integration, security, and release
tests protect Kyber behavior, but CI does not pin the A2A SDK or TCK, inventory
the applicable normative requirements, test another implementation, or publish
which binding/version/options Kyber actually supports.

**A2A-required future state:** every release claiming A2A support runs a pinned
conformance suite against the deployed edge, passes all applicable MUST
requirements, records its disposition of SHOULD requirements, and exercises
authorization, restart, retention, limits, streaming, cancellation, and
unsupported-option behavior. Kyber publishes a support matrix naming the wire
version, binding, security profile, optional capabilities, known limitations,
and test artifact versions.

The official TCK is useful but currently untagged and there is no formal A2A
certification program. Kyber can claim tested or self-attested conformance with
evidence; it must not claim certification.

**Standalone Kyber value:** the A2A-specific suite has no value without G11,
but the discipline is broadly useful: dependency pins, compatibility matrices,
negative/security tests, deployed restart tests, and evidence-backed release
claims reduce protocol drift and make upgrades reviewable.

**Cost and risk:** approximately one to two engineer weeks after G11 for the
requirement inventory, pinned TCK harness, test deployment and fixtures,
cross-implementation smoke tests, CI/release gates, artifact retention,
support-matrix automation, and upgrade runbook. The main risks are treating an
untagged upstream TCK as stable truth, silently skipping inapplicable tests, or
letting flaky network tests either block releases or be ignored.

**Proposed native cut:** pin the A2A spec, Go SDK, and exact TCK commit; check in
an applicability ledger for all normative requirements; run unit translation
tests plus a deployed HTTP+JSON/TCK suite; add focused security, restart, and
one independent-client smoke test; publish a generated support matrix. Upgrade
pins deliberately, never floating on upstream main.

**Recommendation and final decision question:** pursue G12 as a separate
design-only release/conformance issue. It is required for Kyber to call G11
formal support rather than merely an A2A-shaped endpoint. Pursue G12 design?

## 3. Direction: two projects, not one

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

## 4. Binding choice

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

## 5. Current Kyber foundation and exact gaps

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

## 6. Task model and persistence

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

## 7. Agent Card source and honesty

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

## 8. Authentication and authorization

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

## 9. Cost interpretation

The register sizes assume one experienced Kyber engineer and include design,
implementation, tests, documentation, and dev-environment verification. They
are comparison ranges, not approved delivery estimates. The dedicated issue
for a pursued gap must re-estimate it against the exact native use case.

Building every inbound gap remains roughly three to five engineer months, but
that total is no longer the proposed plan. The proposed plan is to spend only
on gaps that pass their individual product decision. G10–G12 are the point at
which accepted Kyber-native capabilities become formal A2A support.

Outbound client support remains separately costed: approximately two to four
weeks for a bounded text-only HTTP+JSON MCP sidecar with discovery,
allowlisting, Bearer secret references, Send/Get/Cancel, payload limits, and
untrusted-output labeling; another four to six weeks for broader bindings,
security schemes, streaming, files/artifacts, and interrupted task states.

## 10. Risks and decisions that remain load-bearing

- **No caller:** the dominant risk for G10–G12, but not a reason to reject a
  native gap that has a concrete Kyber consumer.
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

## 11. Gap-by-gap decision process

Review exactly one gap at a time, beginning with G1. For each gap:

1. confirm the current and required future state against current code and the
   pinned protocol;
2. identify at least one concrete non-A2A Kyber consumer and the user outcome;
3. reject protocol-driven scope that the native consumer does not need;
4. decide `pursue`, `defer`, or `A2A-only` and record the reason in this table;
5. if pursued, create a dedicated design/implementation issue with its own
   acceptance criteria and cost; and
6. update dependent gaps before reviewing the next one.

Stopping is a valid outcome at every step. Formal A2A server work begins only
when the prerequisite native gaps have been accepted and a real A2A caller
justifies G10–G12. Outbound client demand remains a separate issue because it
does not need this inbound task stack.

## Sources checked

- [A2A protocol 1.0 specification](https://a2a-protocol.org/v1.0.0/specification)
- [A2A normative schema at corrected v1.0.1 release](https://github.com/a2aproject/A2A/blob/v1.0.1/specification/a2a.proto)
- [A2A v1.0.1 release notes](https://github.com/a2aproject/A2A/releases/tag/v1.0.1)
- [Official A2A TCK](https://github.com/a2aproject/a2a-tck)
- [A2A project roadmap](https://github.com/a2aproject/A2A/blob/main/docs/roadmap.md)
- [A2A Go SDK v2.5.0](https://github.com/a2aproject/a2a-go/releases/tag/v2.5.0)
