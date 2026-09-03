# Durable tasks and typed results

## Purpose and source of truth

Kyber's durable task path gives operators a PostgreSQL-backed execution handle
that survives control-plane restarts and can expose cooperative progress plus
typed outputs without scraping an agent transcript. Authoritative code lives in
`pkg/taskstore/`, `pkg/taskdispatch/`, `pkg/taskobject/`,
`pkg/api/routes_agent_tasks.go`, `pkg/api/internal_tasks.go`, and
`cmd/status-sidecar/request_mcp.go`.

## Control flow

1. An authenticated caller creates a task at
   `/api/v1/agents/{agent}/tasks`. PostgreSQL atomically stores the task and its
   dispatch intent.
2. A leased worker delivers the task through the existing bounded per-agent
   queue. The runtime's managed submit hook records the exact random attempt
   receipt before the task becomes `dispatched`.
3. Claude Code or Codex invokes the sidecar's loopback `kyber-task` MCP tools.
   The sidecar validates basic shape and forwards progress, results, or
   completion using its pod credential. The control plane repeats validation
   and binds every mutation to the current agent, task, attempt, and state.
4. Public Get exposes current progress and detailed typed result metadata. List
   exposes bounded summaries. File bytes are available only from the authorized
   Kyber download route; object keys never appear in public JSON.
5. Every accepted public mutation appends one normalized event in the same
   PostgreSQL transaction. `GET /api/v1/agents/{agent}/tasks/{task}/events`
   replays those rows in per-task sequence order and then follows them as SSE.
   PostgreSQL polling is the correctness path across replicas and restarts;
   cursors are opaque, signed, and bound to the task, tenant, principal,
   credential generation, and agent resource.

Task Get includes an `eventCursor` at the snapshot's current event high-water
mark. A client that needs future changes only first reads the snapshot, then
connects with that cursor in `Last-Event-ID`. A fresh stream replays retained
history. Expired cursors return `410 event_cursor_expired` and require snapshot
recovery; bad signatures and cross-task or cross-principal reuse return the
non-enumerating `400 invalid_cursor` response.

The event vocabulary is closed and task-contract-only: creation, state,
progress, result metadata, interactions, cancellation, and terminal state.
Result bytes, cancellation reasons, interaction responses, transcripts, token
deltas, raw tools, attempt receipts, leases, pod identity, and harness-native
events are never published. Terminal streams close after replay; active streams
heartbeat, poll for catch-up, periodically reauthorize, and reconnect after a
bounded lifetime.

## Public authorization boundary

Every task is created transactionally with a stable tenant ID, owner principal
ID, and normalized agent resource ID. Public repository reads, list pages,
continuations, cancellations, and result downloads require that complete
envelope; filters run in PostgreSQL before result or message loading, ordering,
limits, and cursor generation. Cursors are bound to the tenant, principal,
agent resource, state filter, and sort contract.

Callers also need the exact action scope (`tasks:create`, `tasks:read`,
`tasks:list`, `tasks:continue`, `tasks:cancel`, or `task-results:read`) and the
agent must appear in their exact resource allowlist. Scope and resource failures
on object-addressed routes are indistinguishable from an absent task. Credential
rotation preserves ownership through the stable principal ID, while browser
sessions are revalidated against the current credential ID and generation on
every request. Internal sidecar updates remain a separate agent/task/attempt
boundary and never receive public-owner authority.

## Cooperative cancellation

An authenticated task owner may request cancellation through
`POST /api/v1/agents/{agent}/tasks/{task}/cancel`. A queued task becomes
`canceled` atomically and its dispatch intent is closed. Once delivery may have
started, the task instead becomes `canceling`: the exact active attempt observes
the durable control request through `get_control` and confirms it with
`ack_cancel`. Acknowledgments from stale attempts are rejected.

The Claude Code and Codex TUI adapters are deliberately `notify_only`. Kyber
does not synthesize Escape, Ctrl+C, or process signals, and does not claim that
external effects are rolled back. Completion or failure may honestly win a
race with cancellation. If the deadline passes without exact evidence, the
task becomes `failed` with `cancel_unconfirmed`, preserving cancellation
metadata for operator follow-up. Safe audit logs identify the actor, task,
attempt, transition, and adapter mode; cancellation reasons and task content
are never logged.

## Typed result boundary

Results are immutable and contain ordered `text`, canonical `json`, or `file`
parts. A repeated result ID with the same public-content digest is an
idempotent replay; a different digest conflicts. Legacy string completion
synthesizes one deterministic text result so older runtimes remain compatible.

Files must be opened beneath `/persist/task-results` using component-by-
component `openat` calls with no symlink following. Only single-link regular
files within the configured cap are accepted. The streaming reader verifies
size, inode, link count, timestamps, and absence of trailing bytes as the final
declared byte is consumed. The control plane re-sniffs content, sanitizes the
filename, and falls back to `application/octet-stream` on a MIME disagreement.

Task objects use a private `task-results/` namespace in the configured GCS or
S3-compatible store. They do not use public URLs or durable presigned links.
Downloads re-check task visibility and authorization, support one bounded byte
range, and force safe attachment/CSP/nosniff headers.

## Transaction and cleanup invariants

- PostgreSQL is the visibility authority; only `ready` objects referenced by a
  committed result are public.
- Before object upload starts, the control plane records a `pending` row with a
  short lease. A failed or crashed upload is therefore discoverable rather than
  an untracked orphan.
- Publication promotes that exact pending row and inserts result, part, update,
  and task-version changes in one transaction. On an ambiguous commit response,
  the API reads the task back before abandoning the upload and never infers a
  rollback from the transport error alone.
- Expired pending objects and retained terminal-task objects become `deleting`.
  Cleanup replicas claim rows with `SKIP LOCKED` leases. Provider failures are
  recorded with exponential backoff and do not starve later rows. Metadata is
  removed only after provider deletion succeeds; terminal task deletion waits
  until no object rows remain.

## Configuration and failure posture

`api.durableTasks.enabled` remains opt-in and requires PostgreSQL. Progress,
result, part, and byte limits are rendered from Helm and capped by compiled hard
limits. Task object configuration may override the archive provider settings;
otherwise it reuses the provider/bucket credentials under its separate prefix.
An unsupported or unavailable configured object backend leaves inline task
results available but fails file publication and download closed; it never
degrades file results to local disk. Malware scanning is explicit
metadata (`not_configured` in v1); Kyber does not imply that unscanned content
was inspected.

### A2A protocol edge

`api.a2a.enabled` exposes the A2A 1.0 HTTP+JSON binding under
`/a2a/v1/agents/{agent}/` and is disabled by default. It also requires
`api.durableTasks.enabled`; enabling the edge without the native task service
returns a service-unavailable protocol error. Every operation requires a Kyber
Bearer principal, an exact MAT-23 task scope, agent-resource access, and
`A2A-Version: 1.0`.

The adapter uses the official Go SDK only for 1.0 wire types, REST routing,
SSE framing, and normative errors. Native Kyber services remain authoritative
for persistence, dispatch, ownership, idempotency, cancellation, multi-turn
continuation, results, and event replay. Agent Cards are deterministic
projections of reconciled, currently available MAT-24 public capabilities;
they never expose private evidence, runtime details, prompts, tools, or
credentials. Push notifications, extended cards, JSON-RPC, gRPC, and protocol
extensions are intentionally unsupported in v1.

### Outbound A2A client boundary

Supported runtimes receive a separate loopback `kyber-a2a` MCP server owned by
the status sidecar. Operators configure named destinations in
`Agent.spec.a2aPeers`; a peer binds an HTTPS base URL to one Kubernetes Secret
key. The controller injects that key only into the sidecar, while the runtime
sees the peer name and bounded task operations. Destination dialing resolves
and checks addresses at connection time to prevent DNS-rebinding bypasses;
loopback, link-local, and metadata ranges are always blocked, and private
address space requires explicit operator opt-in. Redirects fail closed.

The remote A2A service remains the durability and ownership authority for the
delegated task. Kyber returns the peer/task/context handle and can recover work
after either a sidecar or coordinator restart through `get_task` or the
owner-scoped remote `list_tasks`; it does not create a second mutable copy of
the remote task in local task tables. Bounded `await_task` reconnects from the
remote snapshot before following live events, so correctness does not depend
on a process-local stream or cursor.

## Multi-turn interactions

A dispatched attempt may request exactly one typed interaction (`text`,
`choice`, `confirm`, bounded `json`, or a registered `authorization` flow).
Kyber atomically records the task-visible request and moves the task to
`input_required` or `auth_required`. The attempt must then stop.

The task owner answers through the interaction-specific public endpoint with
an idempotency key. Kyber validates the response type, appends immutable caller
and platform messages, and queues a fresh attempt. The continuation envelope
contains only the original instruction and bounded task-visible interaction
context. It never persists or replays a harness transcript, hidden reasoning,
raw tool output, environment state, or credentials. The authorization
interaction type and registered-flow persistence are reserved for a future
platform connector boundary. No public authorization-continuation route is
advertised until a production producer can durably complete and bind the flow;
ordinary typed responses cannot impersonate that completion.

PostgreSQL enforces one live interaction per task. Interaction expiry,
cancellation, completion, rejection, stale attempts, and duplicate responses
are resolved transactionally against the canonical task row. A resumed attempt
consumes the answered interaction; a later interaction is a new durable turn,
not a mutation of the prior attempt.
