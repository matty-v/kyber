# Thin A2A 1.0 HTTP+JSON edge adapter

**Status:** Accepted design
**Date:** 2026-08-30
**Tracker:** [MAT-26](https://linear.app/matty-v/issue/MAT-26/designplatform-thin-a2a-httpjson-edge-adapter)
**Depends on:** [MAT-19](2026-08-30-durable-agent-tasks.md), [MAT-20](2026-08-30-task-progress-typed-results.md), [MAT-21](2026-08-30-cooperative-task-cancellation.md), [MAT-22](2026-08-30-resumable-multi-turn-tasks.md), [MAT-23](2026-08-30-principal-scoped-task-authorization.md), [MAT-24](2026-08-30-public-agent-capability-manifest.md), and [MAT-25](2026-08-30-durable-task-event-streams.md)
**Origin:** [MAT-6](https://linear.app/matty-v/issue/MAT-6/spikeplatform-what-formal-a2a-protocol-support-would-require-for-kyber), [A2A gap study](2026-08-30-a2a-protocol-support.md), and gap-study PR #183
**Normative input:** A2A specification 1.0.0 at commit
`173695755607e884aa9acf8ce4feed90e32727a1` and official Go SDK v2.4.0 at
commit `5736cc7c76905476840257b2c3b0f84a6fea8134`

## 1. Decision

Expose Kyber agents through one pinned A2A 1.0 HTTP+JSON/REST binding. Bundle
G10 Agent Card generation into the same adapter so discovery and runtime
behavior cannot drift into separate capability sources.

Use the official Go SDK only at a bounded edge: normative A2A wire types,
HTTP+JSON routing/codecs, request validation, protocol version handling, SSE
framing, and error representation. By Matt's decision on 2026-08-30, Kyber
implements a narrow service adapter behind that edge. It does not use the SDK's
task store, queue, agent executor, push sender, capability producer, or
authorization policy.

MAT-19 through MAT-25 remain the only sources of task identity/state, typed
content, cancellation, continuation, authorization, capabilities, and events.
Claude Code and Codex remain execution engines. No A2A type enters core
repositories or runtime envelopes.

Version one advertises Bearer authentication and streaming. Push
notifications (G8), OAuth2/OIDC (G9), JSON-RPC, gRPC, extended Agent Cards,
extensions, and outbound A2A client behavior are unsupported and omitted.

## 2. Verified protocol baseline

The A2A project identifies 1.0.0 as the latest stable specification. A2A 1.0
separates its common data/operation model from JSON-RPC, gRPC, and
HTTP+JSON/REST bindings. It requires `A2A-Version` negotiation using major.minor
semantics, defines task polling/list/cancel/subscribe operations, and requires
the current Task as the first Subscribe-to-Task event.

The official Go SDK v2.4.0 at commit
`5736cc7c76905476840257b2c3b0f84a6fea8134` supports A2A 1.0 and REST server
handlers. SDK version and protocol version are separate. The specification tag
resolves to `173695755607e884aa9acf8ce4feed90e32727a1`. Implementation pins the
module checksum too; upgrades go through MAT-27 rather than floating on
`latest` or upstream main.

The checked-in normative sources are:

- [A2A 1.0.0 specification](https://a2a-protocol.org/v1.0.0/specification)
- [A2A release repository](https://github.com/a2aproject/A2A)
- [official Go SDK releases](https://github.com/a2aproject/a2a-go/releases)

## 3. Architecture boundary

```text
A2A HTTP+JSON client
        |
        v
official SDK REST parser / validator / encoder
        |
        v
Kyber A2A translator (protocol types end here)
        |
        +--> MAT-23 principal/action policy
        +--> MAT-24 capability projection
        +--> MAT-19 task service
        +--> MAT-20 parts/results
        +--> MAT-21 cancel service
        +--> MAT-22 continuation service
        +--> MAT-25 event replay/live stream
        |
        v
Claude Code or Codex through existing native dispatch
```

The translator depends on small native service interfaces and pure conversion
functions. Core packages do not import the A2A SDK. The SDK does not start a
second work queue, persist task objects, call a harness, or authorize a caller.
This avoids dual state and makes protocol removal/upgrade an edge change.

## 4. Routing and agent resolution

Use one installation origin from trusted configuration, never `Host` or
forwarded headers without trusted-proxy validation. A versioned per-agent base
keeps names and tenants explicit:

```text
https://<installation>/a2a/v1/agents/{agent-resource-id-or-slug}/
```

The SDK mounts its normative HTTP+JSON routes under that base. The exact child
paths come from the pinned SDK/spec and are locked by contract tests rather
than duplicated in Kyber core. Agent Card discovery is exposed at the
spec-defined well-known location resolved to this base. If a human slug is
used, lookup resolves it to the immutable agent resource ID before policy or
data access.

Multi-tenant installations resolve the authenticated MAT-23 tenant first, then
the agent within that tenant. No global name lookup or redirect confirms an
agent in another tenant. Card URLs, task URLs, and file references are built
from trusted installation origin plus immutable resource identity.

Only HTTPS is advertised outside local development. Gateways must preserve
`A2A-Version`, `Authorization`, `Accept`, content type, SSE flushes, request
IDs, and response status. Redirects are avoided for authenticated operations
because clients may drop credentials.

## 5. Version and content negotiation

Support protocol version `1.0`, backed by specification artifact `1.0.0`.
Patch versions do not change wire compatibility. Every 1.0 operation requires
`A2A-Version: 1.0`; query-parameter negotiation is accepted only where the
normative binding requires it, and conflicting header/query values fail.

The specification assigns an absent version to legacy 0.3 semantics. Since
Kyber does not implement 0.3, an absent or unsupported version receives the
normative `VersionNotSupportedError`, not an attempt to parse the request as
1.0. The Agent Card advertises only the actual 1.0 HTTP+JSON interface.

Require the SDK-defined JSON media type and `Accept` values. Reject unknown
content encodings, duplicate ambiguous headers, unsupported charsets,
oversized bodies, excessive JSON depth, duplicate JSON keys if the decoder can
enforce it, and trailing content. SSE routes require the binding's event-stream
acceptance rules. Kyber never sniffs content.

## 6. Authentication and authorization

The Agent Card declares one HTTP Bearer security scheme backed by MAT-23
machine/service principals. API keys remain bearer credentials at the edge;
the card does not pretend they are OAuth2 tokens. G9 may later add OIDC to the
same MAT-23 principal envelope.

Authentication runs before agent/task lookup. Each operation maps to an exact
MAT-23 scope:

| A2A operation | Native scope |
| --- | --- |
| Send Message / Send Streaming Message | `tasks:create` or `tasks:continue` |
| Get Task | `tasks:read` |
| List Tasks | `tasks:list` |
| Cancel Task | `tasks:cancel` |
| Subscribe to Task | `task-events:read` plus result metadata policy |
| Get public Agent Card | installation discovery policy |

Tenant, owner, and agent-resource checks remain mandatory. `TaskNotFoundError`
is used for absent and unauthorized object IDs so A2A does not weaken MAT-23
non-enumeration. An A2A message cannot assert a principal, tenant, owner,
scope, admin override, connector identity, or internal attempt token through
metadata.

Authenticated operator override is not exposed through A2A v1. Administrators
use explicit native endpoints so protocol clients cannot trigger silent
ownership bypass.

## 7. Agent Card projection (G10)

Generate the card read-only and deterministically from:

- MAT-24's validated operator-approved public identity and capabilities;
- deployed native feature flags for MAT-19 through MAT-25;
- the pinned protocol/binding version;
- trusted public route configuration; and
- the MAT-23 Bearer security description.

Mapping:

| MAT-24/native field | A2A card field |
| --- | --- |
| display name/description/documentation URL | corresponding safe identity fields |
| capability ID/name/description | skill ID/name/description |
| input/output MIME modes | supported input/output modes |
| MAT-25 enabled and available | `capabilities.streaming=true` |
| G8 deferred | `pushNotifications=false`/omitted |
| no extended card | `extendedAgentCard=false`/omitted |
| HTTP+JSON base and version | interface URL, binding, protocol version |
| MAT-23 Bearer profile | security schemes/requirements |

Only Available MAT-24 declarations are projected as callable skills. The card
may expose a stable declared skill as temporarily unavailable only if A2A has a
normative representation that clients can safely interpret; otherwise omit it
and change the card ETag/revision. Never publish private evidence, skill paths
or bodies, prompts, tool schemas, model/runtime/pod data, connector accounts,
internal endpoints, or secrets.

Card generation is pure and covered by golden fixtures. Cache using the
MAT-24 digest plus feature/security/routing policy version. Public-card
exposure is installation policy; authenticated card views still do not invent
additional capabilities in v1. Signed Agent Cards are deferred until identity
key management and normative signing behavior receive their own design.

## 8. Operation mapping

| A2A operation | Kyber behavior |
| --- | --- |
| Send Message, new task | Validate parts/capability, create MAT-19 task with A2A message ID as scoped idempotency input |
| Send Message, existing task | Route to the current MAT-22 interaction/continuation when allowed |
| Send Streaming Message | Create/continue, then project MAT-25 snapshot and events until interrupted/terminal |
| Get Task | Fetch authorized MAT-19 snapshot and project bounded history/results |
| List Tasks | Owner-filtered MAT-23 SQL list and cursor translation |
| Cancel Task | Invoke MAT-21 and return the freshly committed task snapshot |
| Subscribe to Task | Emit current Task first, then translate MAT-25 ordered events |
| Push configuration operations | Normative `PushNotificationNotSupportedError` |
| Get Extended Agent Card | `UnsupportedOperationError` |

Kyber always returns a Task for accepted work, including trivial work. It does
not add a direct-Message fast path because native execution is durable and may
outlive the request. Blocking Send Message waits for terminal, input-required,
or auth-required state as A2A 1.0 requires. A server or gateway deadline does
not turn a working task into a successful blocking response: if the response
deadline expires, the adapter returns a retryable transport error when it can
still write one, while the durable task continues and remains available through
Get/Subscribe. A client disconnect likewise does not cancel the task.
Non-blocking mode returns immediately after durable create and dispatch receipt
semantics permit.

Send Streaming Message and Subscribe share the MAT-25 event authority. They do
not hold a direct harness connection. The former creates/continues first; the
latter attaches to an existing non-terminal task. Both begin with an A2A Task
snapshot as required, eliminating the snapshot/subscription race.

## 9. Identity, context, and idempotency mapping

- A2A Task ID is the opaque MAT-19 task ID without an alternate lookup table.
- A2A `contextId` is the opaque MAT-19/MAT-22 context ID.
- A2A `messageId` is retained with the native task-visible message and
  participates in idempotency under principal + tenant + agent + operation.
- MAT-19 task/version and MAT-25 event sequence stay native metadata unless a
  normative A2A field exists; do not overload arbitrary fields.
- Trace/request IDs are transport correlations only and never task authority.

For a new task, duplicate identical `messageId`/content returns the canonical
Task. Reuse with different normalized content conflicts. For continuation, the
message must reference the authorized non-terminal task/context and satisfy
the one-outstanding-interaction MAT-22 rules. Client-provided context IDs are
accepted only when they resolve to an authorized existing context or meet the
native create policy; rejection never substitutes a new context silently.

## 10. State mapping

| Kyber state | A2A 1.0 state |
| --- | --- |
| `queued`, `dispatched` before active receipt | `TASK_STATE_SUBMITTED` |
| accepted/running/progressing | `TASK_STATE_WORKING` |
| `input_required` | `TASK_STATE_INPUT_REQUIRED` |
| `auth_required` | `TASK_STATE_AUTH_REQUIRED` |
| `completed` | `TASK_STATE_COMPLETED` |
| `failed`, including honest ambiguous failure | `TASK_STATE_FAILED` |
| `canceling` | `TASK_STATE_WORKING` with bounded cancellation status message |
| `canceled` | `TASK_STATE_CANCELED` |
| `rejected` | `TASK_STATE_REJECTED` |

No state is inferred from transcript text or process liveness. Safe native
reason codes may become bounded status messages or error details where the
spec permits. Internal attempt/delivery/lease states stay private.

Terminal mapping is immutable. A late stale harness update rejected by native
state cannot generate an A2A reversal. `canceling` is not falsely projected as
canceled because MAT-21 cancellation is cooperative.

## 11. Messages, Parts, history, and Artifacts

Inbound A2A user Messages become MAT-20/MAT-22 task-visible caller messages.
Only supported Part variants and MIME modes advertised by the selected MAT-24
capability are accepted:

| A2A part | Native mapping |
| --- | --- |
| text | bounded text part |
| structured data | validated JSON part |
| inline file bytes | controlled upload staging, size/type/digest checks |
| file URI/reference | approved fetch/reference policy only; no arbitrary server-side URL fetch in v1 |

Reject unsupported or ambiguous parts with `ContentTypeNotSupportedError` or
field validation detail before task creation. Metadata is allowlisted and
bounded; it cannot carry instructions outside the message, credentials,
authorization decisions, internal paths, or arbitrary nested payloads.

MAT-20 results project to A2A Artifacts. Text, JSON, images/audio, and
object-backed files preserve declared MIME type, safe name, size, and digest.
Object content remains behind a freshly authorized Kyber download URL or
spec-supported file reference; never embed durable presigned bearer URLs.
Artifacts are outputs, not status Messages.

History length maps to the bounded MAT-22 task-visible log. Zero omits history;
positive values return the most recent allowed entries up to native caps.
Unset uses a documented conservative default, never unbounded transcript
history. Hidden reasoning, raw tool output, terminal bytes, provider events,
and credentials do not enter history.

## 12. Event translation and SSE

The A2A stream is a projection of MAT-25, not a second event log:

- current native snapshot -> first A2A Task;
- task state/progress/interaction event -> `TaskStatusUpdateEvent`;
- result-added event -> `TaskArtifactUpdateEvent`; and
- native terminal event -> final status update and stream close.

Events preserve native per-task order. Several native events may collapse into
one A2A event only when the projection records the highest included sequence
and semantics remain complete; one native event may not be reordered across
another. Unknown native future event types fail closed or are safely skipped
without advancing a resumable A2A cursor until compatibility policy says how
to handle them.

The SDK owns normative SSE encoding. The adapter owns snapshot construction,
MAT-25 cursor/replay, authorization rechecks, backpressure, heartbeats, and
terminal behavior. Resume metadata maps to the binding's supported mechanism;
if A2A 1.0 does not normatively expose Kyber's durable cursor, reconnect begins
with the required current Task snapshot and then live events. Kyber must not
claim stronger protocol replay semantics than the pinned binding defines.

## 13. List and pagination

A2A List Tasks delegates to MAT-23 owner-filtered repository queries. Sort by
status timestamp descending as required by A2A. Filters map only where native
semantics are exact. History/result expansion applies bounded per-item caps so
one page cannot multiply into unbounded work.

Translate A2A page tokens to signed native cursors bound to principal, tenant,
agent, normalized filters, order, requested history length, and protocol
version. Tokens are opaque and cannot cross an Agent Card interface. Bad or
expired tokens return the normative invalid-argument error without disclosing
hidden tasks.

## 14. Errors

Use the SDK's normative HTTP error encoder after translating typed native
errors:

| Native condition | A2A error |
| --- | --- |
| absent/unauthorized task | `TaskNotFoundError` |
| terminal/stale continuation | `UnsupportedOperationError` or exact task-state error |
| non-cancelable task | `TaskNotCancelableError` |
| unsupported input/output mode | `ContentTypeNotSupportedError` |
| G8 push operation | `PushNotificationNotSupportedError` |
| G9/extended card/unknown optional operation | `UnsupportedOperationError` |
| wrong/absent A2A version | `VersionNotSupportedError` |
| invalid field/limit/idempotency conflict | binding-native validation/conflict detail |
| transient store/capacity failure | safe retryable binding-native error/status |

Authentication challenges retain standard HTTP `401`/`WWW-Authenticate` and
authorization semantics while task object denial remains non-enumerating.
Error messages are stable, bounded, and content-free. SDK errors are reviewed
so no Go error, SQL detail, internal URL, stack trace, or provider response
crosses the edge.

## 15. Unsupported features

The Agent Card declares only implemented behavior. In v1:

- push notification capability is false/absent and all configuration
  operations return `PushNotificationNotSupportedError`;
- OAuth2/OIDC security schemes are absent;
- extended Agent Card and extensions are false/absent;
- JSON-RPC and gRPC interfaces are absent;
- direct caller webhooks, signed cards, outbound A2A client calls, and
  client-to-client delegation are absent; and
- provider/harness-specific fields are never exposed as custom extensions.

Routes for unsupported bindings should not be mounted. Operations mandated by
the selected binding but gated by an optional advertised capability use the
normative unsupported error rather than generic 404.

## 16. Limits and abuse resistance

Apply limits before expensive conversion or task creation: request/header/body
bytes, JSON depth and collection counts, part count/bytes, inline file total,
metadata size, history length, list page size, concurrent blocking requests,
SSE connections, reconnect rate, per-principal task create rate, and total
outstanding tasks.

Normalize exactly once for digest/idempotency and reject decompression bombs,
malformed Unicode, non-finite JSON numbers, unsafe file names, server-side
request forgery through file URLs, and URLs outside trusted origins. Stream
slow-consumer behavior inherits MAT-25: disconnect and resume/re-snapshot,
never unbounded buffering or silent event loss.

The adapter has request and blocking-wait deadlines that do not become task
deadlines unless explicitly requested and accepted by MAT-19. Client
disconnect does not cancel a durable task. Cancellation requires the normative
authenticated operation.

## 17. Observability and audit

Metrics cover requests by operation/outcome/protocol version, translation
failures, unsupported operations, content modes, blocking wait duration, SSE
connections/events/lag, card generation/cache, SDK panics, native latency, and
rate-limit rejection. Labels exclude agent/task/principal IDs, messages,
metadata, file names, and unbounded error strings.

Trace the edge request through native task operation with safe request/task
correlation. Logs include operation, protocol/binding version, safe reason,
latency, and response class—not Authorization, Parts, Artifacts, cursor/page
tokens, or task content. MAT-23 audit records creation, continuation, read,
list, cancel, event subscription, and denied access with the A2A entry point.

## 18. SDK boundary and upgrade policy

Pin `github.com/a2aproject/a2a-go/v2` at v2.4.0, resolved commit
`5736cc7c76905476840257b2c3b0f84a6fea8134`, plus its `go.sum` checksum.
MAT-27 records the same commit in evidence. Before implementation, reverify the
tag against the official repository and security advisories without changing
the pin silently. Wrap SDK construction in one package and expose only
Kyber-owned interfaces.

Allowed SDK responsibilities:

- A2A structs/enums and JSON codecs;
- REST route/method/content/version validation;
- normative errors and SSE record encoding; and
- stateless protocol handler plumbing.

Forbidden SDK responsibilities:

- task/event/result persistence or caching;
- worker/work queue/executor lifecycle;
- push delivery;
- authorization, tenant resolution, or agent selection;
- capability generation from executor state; and
- direct Claude Code/Codex integration.

Adapter contract tests snapshot every used SDK behavior. SDK upgrades are
deliberate dependency PRs with generated diff, translator tests, TCK evidence,
security review, and rollback. No automatic major/minor dependency update may
change the public protocol edge.

## 19. Threat model

Defend against task-ID enumeration, cross-tenant agent confusion, metadata
identity injection, replayed message IDs with changed content, page/cursor
swapping, oversized recursive Parts, malicious file references, SSRF, content
type confusion, stale-event access after revocation, cache poisoning through
Host/forwarded headers, credential leakage through URLs/logs, slow streams,
and dependency behavior changes.

The card is an allowlisted projection, not a dump of CRD/runtime state. Public
discovery does not grant task access. Bearer credentials are accepted only over
HTTPS and are never forwarded to agent pods or file origins. Native attempt
tokens stay confined to internal sidecar routes.

## 20. Rollout

1. Add the isolated SDK wrapper and pure translation fixtures without routes.
2. Implement card projection and authenticated Get/List against test native
   services; run MAT-27 translator requirements.
3. Add create/continue/cancel and typed Parts/Artifacts behind an installation
   feature flag.
4. Add Send Streaming Message and Subscribe using MAT-25, with multi-replica,
   restart, retention, revocation, and slow-client tests.
5. Deploy only to the dev environment, exercise the official CLI plus an
   independent client, and run the pinned TCK/applicability suite.
6. Enable selected agents only after their MAT-24 manifests are valid and all
   native dependency feature flags are available.
7. Publish the MAT-27 evidence-backed support matrix; do not claim
   certification.

Disablement removes cards/routes for new traffic but does not delete native
tasks. Existing A2A-created tasks remain accessible through authorized native
APIs and return safe unsupported/unavailable behavior at the disabled edge.

## 21. Tests

Pure mapping tests cover every state, operation, Part, Artifact, message/history
case, capability/card field, error, idempotency outcome, pagination token, and
unknown enum. Golden tests use official 1.0 examples plus independently encoded
fixtures so SDK encode/decode is not testing itself.

HTTP tests cover routes/methods, `A2A-Version`, media negotiation, Bearer
challenges, non-enumeration, limits, malformed/duplicate JSON, blocking and
non-blocking send, first stream snapshot, ordering, terminal close, reconnect,
and unsupported operations. Security tests cover all MAT-23 boundaries and the
threat model above.

Deployed tests use real native task/event stores and disposable Claude Code and
Codex agents. They verify the adapter does not bypass the sidecar, create a
second task store, or leak raw runtime events. MAT-27 owns the full normative
ledger, TCK, independent-client smoke, artifact retention, and release gate.

## 22. Out of scope

- implementing MAT-19 through MAT-25 native services;
- G8 push notifications and G9 OAuth2/OIDC;
- JSON-RPC, gRPC, A2A 0.3 compatibility, extensions, extended/signed cards;
- outbound A2A client/delegation or a general agent gateway;
- raw harness events, transcripts, reasoning, prompts, tools, or model details;
  and
- G12 conformance automation beyond defining its seams.

## 23. Estimate

Estimated implementation is **4–7 engineer-weeks** after native dependencies:
SDK boundary/card/routing (1–1.5 weeks), operations and translations (1.5–2.5
weeks), SSE/security/limits (1–1.5 weeks), and integration/hardening (0.5–1.5
weeks). MAT-27 conformance infrastructure is separate.

## 24. Acceptance criteria

- Kyber exposes exactly one pinned A2A 1.0 HTTP+JSON binding and advertises no
  unsupported binding or capability.
- The official SDK is confined to stateless normative edge mechanics; all
  durable and security authority stays in MAT-19 through MAT-25.
- Agent Cards are deterministic read-only projections from MAT-24 plus deployed
  features and trusted routes.
- Every supported operation, state, Part, Artifact, error, page token, and
  stream event has a tested native mapping.
- Bearer principals retain MAT-23 tenant/owner/resource policy and
  non-enumeration.
- A2A streams start with a current Task and project only MAT-25 normalized
  ordered events.
- G8/G9/JSON-RPC/gRPC/extended-card operations are omitted or rejected with the
  normative unsupported error.
- MAT-27 can test the deployed adapter without special protocol behavior in
  core services.
