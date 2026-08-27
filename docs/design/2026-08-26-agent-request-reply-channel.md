# Bounded agent request/reply channel

Status: proposed on 2026-08-26

## Problem

Kyber can accept authenticated webhook events and dispatch their rendered
envelopes into an agent's tmux session. That path intentionally acknowledges
delivery, not completion: the caller cannot correlate an agent response with
the request. The authenticated exec API can attach to or capture a terminal,
but exposing terminal history to an external application would reveal unrelated
operator and agent activity.

An external application needs a narrow way to ask a running agent to perform a
bounded task and collect its explicit response. The first consumer is a public
Kyber information kiosk shared by `voget.io` and the Kyber marketing site. The
platform contract must remain useful outside that showcase and must not encode
public-web abuse policy in the control plane.

## Goals

- Submit one authenticated request to one running agent and receive a stable
  correlation ID immediately.
- Let the agent explicitly complete that request through a platform-provided
  tool available in both Claude Code and Codex.
- Return only the response associated with that request, never raw terminal or
  transcript content.
- Bound request size, response size, lifetime, concurrency, and retained state.
- Preserve the existing inbound dispatcher, agent lifecycle, pod isolation, and
  status-sidecar trust boundaries.
- Support an external gateway that exposes a stricter skill allowlist to
  untrusted browser clients.

## Non-goals

- A public or unauthenticated Kyber control-plane endpoint.
- A general browser terminal, free-form public prompt box, or transcript API.
- Durable job execution or guaranteed delivery while an agent is unavailable.
- Streaming arbitrary model tokens in v1.
- Executing a named skill inside the control plane. The agent runtime remains
  responsible for interpreting the bounded prompt and invoking its installed
  skill.
- Public cluster inventory. Any showcase metadata is curated by the external
  gateway and its kiosk skill.

## Decisions

### Authenticated asynchronous API

Kyber adds an operator API surface:

```text
POST /api/v1/agents/{agent}/requests
GET  /api/v1/agents/{agent}/requests/{requestID}
```

`POST` accepts a short prompt and an optional caller correlation value. It
returns `202 Accepted` with a cryptographically random request ID, status, and
expiry. It never waits for model work:

```json
{
  "prompt": "Run the kyber-features skill and answer this kiosk request.",
  "correlation": "gateway-generated-opaque-value"
}
```

```json
{
  "id": "req_...",
  "status": "queued",
  "createdAt": "2026-08-26T22:00:00Z",
  "expiresAt": "2026-08-26T22:01:00Z"
}
```

`GET` returns one of `queued`, `dispatched`, `completed`, `failed`, or
`expired`. Only `completed` includes `response`. Terminal text, intermediate
reasoning, tool calls, and unrelated activity are never returned. Errors use a
small stable vocabulary and do not include pod, tmux, Kubernetes, or provider
details.

The API uses new `requests:write` and `requests:read` scopes. They do not imply
lifecycle access, and lifecycle scopes do not imply request access. The legacy
full-scope key remains compatible with the existing authorization contract.

### Delivery reuses the bounded dispatcher

The handler creates a structured platform envelope containing the request ID,
caller prompt, expiry, and a fixed instruction to finish through the response
tool. It enters the same per-agent bounded queue and Running-phase gate used by
inbound webhooks. It does not issue a loopback HTTP request to its own webhook
route and does not duplicate HMAC verification, event matching, or external
binding configuration.

The delivery envelope contains no callback URL or credential. A request can be
completed only through the local platform tool, so prompt content cannot redirect
the response to an attacker-controlled destination.

Dispatch success changes status to `dispatched`; it does not mean the agent
completed the task. Queue-full, unavailable-agent timeout, and tmux delivery
failure produce terminal `failed` state.

### Explicit response tool

Both supported runtimes register one platform-owned MCP tool:

```text
kyber-request-reply.respond(request_id, response)
```

The tool is reachable only inside the agent pod. It sends the response to the
status sidecar over loopback. The sidecar authenticates onward to an internal
control-plane endpoint using the existing per-agent pod token. Runtime code
never receives a control-plane credential or destination.

The control plane verifies that the pod identity matches the request's agent,
the request is still `dispatched`, and the response is within the byte limit.
Completion is single-assignment: the first valid response wins; repeats return
an idempotent success only when their content hash matches and otherwise return
conflict. A different agent cannot discover or complete the request.

This extends the existing status-pipeline trust boundary rather than adding a
direct runtime-to-control-plane path. The concrete MCP transport may be a small
shared in-runtime bridge that posts to a new loopback status-sidecar route; it
must not require a separate credential-bearing sidecar.

### Bounded ephemeral state

Redis stores request metadata and completed responses with TTL. The in-memory
store implements the same interface for local development and tests. Requests
are intentionally ephemeral and are not added to the Agent CRD status:

- default lifetime: 60 seconds;
- maximum request body: 2 KiB;
- maximum response body: 8 KiB;
- maximum outstanding requests per agent: 2;
- maximum retained terminal requests per agent: 20;
- no extension of the original expiry after dispatch or completion.

Concrete defaults are configuration, but chart values may only lower or raise
them within defensive hard caps compiled into the control plane. Expiry is a
terminal state. A late tool response is rejected and cannot recreate data.

Control-plane restart loses requests only when using the documented in-memory
development fallback. Redis-backed installations retain them until TTL. The
feature reports unavailable rather than silently accepting requests when its
configured durable store is unreachable.

### External showcase gateway

The public websites never call the Kyber API directly. A separately deployed
gateway holds a narrowly scoped Kyber credential and is the only request API
caller. The gateway:

- accepts an enum of `/joke`, `/features`, `/architecture`, or
  `/cluster-status`, never caller-supplied prompt text;
- maps the enum to a versioned fixed prompt;
- applies origin checks, per-IP and global rate limits, concurrency limits,
  bot mitigation, request deadlines, and a circuit breaker;
- polls the authenticated Kyber result route and returns a normalized response;
- limits and escapes response text before returning JSON;
- caches non-personal informational answers when the agent or cluster is
  unavailable; and
- emits only curated public cluster properties. It never asks the agent for raw
  diagnostics, capacity, resource names, IP addresses, namespaces, or secrets.

`voget.io` and the Kyber marketing site consume the same versioned gateway API
and UI behavior. Their visual components may differ. Public policy remains in
the gateway so a future trusted request/reply consumer is not constrained to
the showcase allowlist.

### Terminal-peek presentation is structured, not captured

The kiosk may show harness, runtime, high-level progress, elapsed time, and the
final answer. Those fields come from the request state and allowlisted agent
status metadata. The website does not attach to tmux or render captured pane
content. "Terminal peek" is a presentation of this structured exchange, not a
terminal access mode.

Harness identity is sourced from the Agent spec/status and normalized to
`Claude Code` or `Codex`. The gateway does not expose model entitlement data,
credential state, transcript paths, or arbitrary environment variables.

## Security and abuse boundaries

- The Kyber routes remain behind API authentication and explicit scopes.
- Request IDs are random, non-sequential, agent-bound, and still require
  authorization to read.
- Prompts and responses are treated as untrusted data in logs and UIs. Normal
  logs record request ID, caller name, agent, status, sizes, latency, and stable
  error code—not full content.
- The platform tool accepts no URL, headers, target agent, or arbitrary metadata.
- Outstanding-request and byte ceilings apply before allocation and dispatch.
- Expiry, completion, and cancellation races are resolved atomically in the
  store.
- The gateway credential is server-side only and scoped to request read/write;
  it has no lifecycle, exec, logs, secrets, or fleet-management access.
- CORS is not an authentication mechanism. Browser origins are enforced by the
  gateway in addition to rate limits and bot mitigation.
- The kiosk agent itself receives a minimal identity repository, no operator
  credentials, no deployment permissions, and only the tools needed by its four
  information skills.

## Failure behavior

| Failure | Caller-visible result |
|---|---|
| agent not Running before bounded wait | `failed` / `agent_unavailable` |
| per-agent queue full | `429` on submission |
| outstanding-request limit reached | `429` on submission |
| tmux dispatch fails | `failed` / `delivery_failed` |
| agent does not call response tool | `expired` |
| response too large | tool error; request remains open until expiry |
| request store unavailable | `503`; request is not accepted |
| duplicate identical tool response | idempotent success |
| conflicting or late tool response | conflict/expired tool error |
| gateway deadline reached | cached fallback or bounded unavailable response |

## Rollout plan

Each checkpoint is independently reviewable and keeps the feature dark until a
scoped caller and agent are explicitly configured.

1. Add the request store interface, Redis/in-memory implementations, limits,
   atomic transitions, and unit tests.
2. Add authenticated submit/read routes, scopes, OpenAPI shapes, hand-written
   PWA wire types, and contract tests. Do not expose a PWA control yet.
3. Adapt the inbound dispatcher for internal request envelopes and cover queue,
   lifecycle, timeout, and delivery outcomes.
4. Add the loopback response path, internal authenticated handler, shared MCP
   bridge, runtime registration, and cross-agent/late/duplicate tests.
5. Add Helm configuration, metrics, structured audit logs, and a disabled-by-
   default feature gate. Verify in the full local stack with both runtimes.
6. Deploy one least-privileged kiosk agent and the external gateway in a
   non-production environment. Load-test rate, concurrency, timeout, and store
   failure behavior before public exposure.
7. Connect the `voget.io` preview, run an abuse review, then enable production
   behind a circuit breaker and cached fallback.
8. Reuse the gateway contract on the Kyber marketing site after production
   behavior and cost are understood.

## Acceptance criteria

- A scoped caller can submit a request, receive `202`, and retrieve only that
  request's explicit agent response.
- Claude Code and Codex agents can complete requests through the same tool
  contract without holding control-plane credentials.
- Requests cannot reveal terminal history, reasoning, unrelated replies, or
  another agent's data.
- Limits and TTLs remain enforced across success, queue pressure, store failure,
  agent unavailability, duplicate replies, and control-plane restart.
- The public gateway exposes only four fixed skills and remains useful to both
  websites without giving browsers a Kyber credential or direct cluster route.
- The kiosk degrades to a clearly labeled cached/unavailable state when the live
  agent cannot respond.
