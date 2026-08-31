# Durable resumable task event streams

**Status:** Proposed
**Date:** 2026-08-30
**Tracker:** [MAT-25](https://linear.app/matty-v/issue/MAT-25/designplatform-durable-resumable-task-event-streams)
**Depends on:** [MAT-19](2026-08-30-durable-agent-tasks.md), [MAT-20](2026-08-30-task-progress-typed-results.md), [MAT-21](2026-08-30-cooperative-task-cancellation.md), [MAT-22](2026-08-30-resumable-multi-turn-tasks.md), and [MAT-23](2026-08-30-principal-scoped-task-authorization.md)
**Origin:** [MAT-6](https://linear.app/matty-v/issue/MAT-6/spikeplatform-what-formal-a2a-protocol-support-would-require-for-kyber), [A2A gap study](2026-08-30-a2a-protocol-support.md), and gap-study PR #183

## 1. Decision

Add one durable, ordered event log per task and expose it through a resumable
native Server-Sent Events endpoint. The same database transaction that accepts
a task mutation appends its public event. PostgreSQL is the source of truth for
replay and recovery; PostgreSQL notifications or Redis may wake API replicas
but never determine event existence or order.

Version one is SSE-only by Matt's decision on 2026-08-30. Native task
WebSockets are deferred. The stream publishes only Kyber-accepted task state,
bounded progress, interaction, cancellation, and result metadata. It never
mirrors transcripts, hidden reasoning, token deltas, raw tool calls, or
provider-specific harness events.

Normal task Get remains the authoritative current snapshot. The stream is a
bounded change feed that reduces polling and supports reconnect; it is not a
second task store.

## 2. Why this belongs in Kyber

Claude Code and Codex can emit rich runtime events, but those streams describe
provider execution rather than Kyber's durable task contract. They differ by
runtime/version, may contain sensitive data, and disappear with a pod or
session. Kyber already accepts the normalized state, progress, result, and
interaction updates that external callers should observe.

The thin-adapter boundary is therefore:

```text
harness-native activity
        |
        v
status sidecar + MAT-19/20/21/22 validation
        |
        v
task mutation + normalized public event (one DB transaction)
        |
        +--> task snapshot
        +--> durable replay / live SSE
```

Kyber does not interpret or republish the rest of the harness stream.

## 3. Current state and gap

Kyber's existing `EventBus` serves a fleet-oriented WebSocket feed. It is
process-local, has no persistence or replay cursor, and intentionally drops
messages when subscriber channels fill. Multiple control-plane replicas do not
share its history. It is suitable for best-effort UI refresh, not task delivery
semantics.

Redis is already deployed for caches, metrics, deduplication, and request
state, but it is optional operational infrastructure and not a transactional
peer of the future PostgreSQL task store. Making Redis Streams or Pub/Sub the
task event authority would introduce a dual-write gap between current task
state and observable events.

MAT-19 through MAT-22 define the events worth exposing, but no retained
per-task sequence, replay/live handoff, cursor expiry, slow-consumer policy, or
principal-scoped subscription currently exists.

## 4. Event model

Each event has:

```text
event_id          globally unique opaque ID
task_id           immutable task ID
tenant_id         MAT-23 tenant partition
sequence          monotonically increasing uint64 within the task
task_version      task snapshot version after the mutation
type              closed versioned event type
occurred_at       database transaction time
payload_version   schema version for this event type
payload           bounded normalized JSON
trace_id          safe correlation ID, optional
```

The primary key is `event_id`; `(task_id, sequence)` is unique. Sequence begins
at 1 and has no committed gaps. It establishes order only inside one task.
Consumers must not compare sequences across tasks or use timestamps as order.

`task_version` lets a consumer identify several events from one accepted
mutation and compare the feed with a later task snapshot. Event payloads are
immutable. Corrections append a new event or update the authoritative task
snapshot; stored events are never rewritten in place.

## 5. Event vocabulary

Version one uses a closed set:

| Type | Public payload |
| --- | --- |
| `task.created` | task/context IDs, initial state, timestamps |
| `task.state_changed` | prior/new state, safe reason code, version |
| `task.progress` | MAT-20 bounded status update and progress sequence |
| `task.result_added` | result ID, kind, media type, size/digest, safe metadata |
| `task.interaction_requested` | MAT-22 interaction ID/type and public prompt parts |
| `task.interaction_resolved` | interaction ID and non-secret resolution metadata |
| `task.cancellation_requested` | cancellation ID and safe status |
| `task.terminal` | final state, result summary, safe failure/cancel code |

Create one event per semantically accepted public change, not per SQL row or
harness callback. Duplicate idempotent mutations return their canonical result
without appending another event. Internal delivery receipts, leases,
heartbeats, retries, pod names, attempt tokens, and row-lock activity are not
public task events unless a separate normalized task-state change occurs.

Result events carry metadata and an authorized API reference, never inline
object content or signed storage URLs. Authorization completion events contain
only an opaque non-secret connection reference/status. Failure messages retain
the safe bounded redaction rules from the owning task design.

## 6. Atomic append

Every repository mutation that changes public task state:

1. opens a database transaction and locks the task row;
2. validates MAT-23 actor or internal current-attempt authority;
3. verifies state, task version, idempotency key, and mutation invariants;
4. updates task/result/interaction rows;
5. obtains the next task sequence while holding the task lock;
6. inserts the normalized event row; and
7. commits once.

There is no state-without-event or event-without-state outcome. A retry after an
unknown commit result uses the operation idempotency key and returns the
already-committed event sequence. The application must not publish a live
notification until the transaction commits.

`task.created` is inserted in the task-creation transaction. Backfilled legacy
tasks may begin with a synthetic `task.snapshot_imported` event at sequence 1,
clearly marked as migration-generated; Kyber never fabricates historical
intermediate events.

## 7. Durable storage and wakeups

Use a partitionable PostgreSQL table indexed by `(task_id, sequence)` and by
retention time. Payloads use bounded JSONB or typed columns plus JSONB; every
write validates size and schema before entering the transaction.

After commit, signal interested replicas through PostgreSQL `NOTIFY` or Redis
Pub/Sub. The signal contains only task ID and latest sequence. It is a hint:
signals may be duplicated, delayed, reordered, or lost. Each subscriber always
queries PostgreSQL from its last delivered sequence, so correctness survives a
missed signal, Redis outage, replica restart, or reconnect.

Use periodic database catch-up polling even while notifications are healthy.
Do not create one database LISTEN connection or Redis subscription per client;
replicas multiplex wakeups by task and keep bounded subscriber registries.

## 8. Native SSE API

```http
GET /api/v1/agents/{agent}/tasks/{task}/events
Accept: text/event-stream
Last-Event-ID: <opaque cursor>
```

The endpoint requires MAT-23 `task-events:read`, tenant, owner, and allowed
agent checks before revealing existence. Query `?cursor=` may be supported for
clients that cannot set `Last-Event-ID`; sending both with different values is
`400 invalid_cursor`. `Last-Event-ID` wins only when the values agree.

Each record is encoded:

```text
id: <opaque signed cursor>
event: task.progress
data: {"schemaVersion":"v1","taskId":"...","sequence":7,...}

```

The cursor binds task ID, tenant, owner/policy context, last delivered
sequence, and cursor version with authenticated encoding. It contains no task
content or credentials. A bare numeric sequence is not accepted from public
clients. Cursors are portable across API replicas and deploys while their
version and retained event range remain supported.

Responses set `Content-Type: text/event-stream`, disable proxy buffering and
content transformation, use private/no-store cache controls, and emit an
initial comment promptly so intermediaries establish the stream.

## 9. Gap-free replay and live handoff

For an authorized connection:

1. Decode the cursor and determine `after_sequence` (0 for a fresh stream).
2. In a short repeatable-read transaction, read the task's retained floor and
   current high-water sequence.
3. Reject an expired cursor before streaming headers.
4. Replay `(after_sequence, high_water]` in bounded pages.
5. Register the local subscriber for the task.
6. Query PostgreSQL again for `(last_sent, latest]` before waiting.
7. On every wakeup or poll tick, query and deliver all unseen rows in order.

Register-before-second-read closes the replay/live race. A commit between any
steps appears either in the second read, a wakeup, or the periodic poll. Since
all paths query `sequence > last_sent`, duplicates are harmless and gaps are
not.

The server updates `last_sent` only after the complete SSE record is written to
its buffered writer. Network ambiguity can still cause a reconnect to receive
the last event again. Consumers must apply `(task_id, sequence)` idempotently.

Terminal tasks send retained events, a terminal event if not already included,
then close after a short flush. Clients may reconnect until retention expires.

## 10. Snapshot and cursor expiry

Task Get is always the recovery snapshot. A new stream without a cursor begins
at the retained floor by default; clients that need only future changes first
GET the task and connect with the snapshot's `eventCursor` high-water value.
This avoids a race between snapshot and subscription.

If a cursor is older than the retained floor, return before opening SSE:

```http
410 Gone
{
  "code": "event_cursor_expired",
  "taskId": "...",
  "snapshotRequired": true,
  "currentCursor": "..."
}
```

The response discloses this only after authorization. The client fetches the
current task snapshot, replaces local derived state, and reconnects using its
cursor. Unknown cursor versions, bad signatures, wrong tasks, principals, or
filters return `400 invalid_cursor` without revealing another resource.

## 11. Retention and cleanup

Retain events for the task retention period with a configurable minimum replay
window after each event, default seven days. A terminal task's event rows may
be deleted with the task after its retention expires. Active task events must
not be removed merely because they are old; compact high-frequency progress
events only through an explicitly versioned policy that advances the retained
floor and preserves the authoritative snapshot.

Cleanup operates in small indexed batches and records the new retained floor.
It does not block task writers for long periods. Existing connections may
finish rows already loaded; reconnect follows the recorded floor. Retention
changes never make an old cursor point at a different event.

Per-task and installation quotas limit event count and bytes. Progress
coalescing occurs before acceptance under MAT-20 rules; once an event is
accepted, live fanout cannot silently drop it while claiming the cursor
advanced.

## 12. Heartbeats, backpressure, and limits

Send an SSE comment heartbeat every 15–30 seconds with jitter and no cursor
advance. It keeps intermediaries alive but is not durable data. Detect request
context cancellation and stop all work promptly.

Each connection has a small bounded outbound byte/event budget and a maximum
write deadline. Database pages are pulled only when capacity is available. If
a client remains slow, close the connection; it reconnects from its last
successfully processed cursor. Never grow an unbounded goroutine, channel, or
event slice and never skip an event to keep a connection alive.

Enforce per-principal, per-task, per-tenant, and installation connection caps;
connection-rate and reconnect-rate limits; replay page/byte limits; maximum
stream age with jittered reconnect; and query timeouts. Return `429` with a
bounded `Retry-After` before streaming headers when possible. Capacity limits
are policy, not cursor advancement.

## 13. Authorization and revocation

MAT-23 authorization runs before event lookup and again periodically for
long-lived streams. The stream binds the authenticated principal, credential
generation, tenant, task owner, agent resource, and policy version. Credential
revocation, ownership repair, resource-policy removal, or task deletion closes
the stream within a bounded revocation interval.

Every database query includes tenant/task ownership predicates. Event payloads
cannot refer to unauthorized result content. A result reference still requires
a fresh `task-results:read` check when followed. Administrator subscriptions
require explicit override mode and produce audit records for open/replay/close.

CORS follows the existing exact-origin policy. Browser cookie sessions require
the MAT-23 session/revocation model; bearer credentials must not appear in URLs
or cursor values. Logs and metrics never include cursor tokens or payloads.

## 14. Multi-replica and failure behavior

- **API replica restart:** client reconnects to any replica and replays from
  PostgreSQL.
- **Redis/notification outage:** catch-up polling continues; latency rises but
  events are not lost.
- **PostgreSQL unavailable:** existing streams stop after bounded retries and
  clients reconnect; no replica invents events from cache.
- **Unknown task commit:** mutation idempotency resolves the canonical event.
- **Duplicate/out-of-order wakeup:** sequence query deduplicates and orders.
- **Slow client:** connection closes without advancing the client's stored
  cursor.
- **deploy/load-balancer timeout:** maximum stream age and resume semantics make
  reconnect normal.
- **task deletion/retention:** authorized stream receives a safe final event
  when possible, then closes; subsequent lookup is non-enumerating.

Availability favors honest reconnect over indefinite buffering. SSE delivery
is at-least-once across network failure; the durable task mutation itself keeps
its owning idempotency semantics.

## 15. Harness audit

| Runtime signal | Public mapping |
| --- | --- |
| Claude Code/Codex turn start, token delta, thinking, raw message | None |
| Native tool invocation/result | None unless a Kyber task MCP call is accepted |
| MAT-19 receipt/completion through sidecar | Normalized state event if public state changes |
| MAT-20 progress/result call | Bounded progress/result event in same transaction |
| MAT-21 cancel acknowledgment | Normalized cancellation/state event |
| MAT-22 interaction request/response | Normalized interaction event |
| Runtime crash/restart | Only a public task state event when task policy changes state |

Both harness adapters receive the same task MCP and attempt envelope. No
provider-specific event becomes public merely because it is available. This
keeps behavior consistent across Claude Code and Codex and prevents raw stream
formats from becoming Kyber compatibility obligations.

## 16. A2A projection seam

G11 can translate the native ordered event vocabulary to A2A SSE wire events,
map its resume token to the native cursor, and apply A2A error shapes. It must
not query harness streams directly or add event types absent from the native
task contract. Projection fixtures pin native payload versions to supported
A2A versions.

The native endpoint remains independently supported. A2A transport-specific
headers, media types, JSON-RPC shapes, and discovery live in G10/G11 rather
than leaking into the event store.

## 17. Migration and rollout

1. Add the task-event table, sequence/high-water fields, repositories, limits,
   and cleanup without serving streams.
2. Dual-write normalized events in the same task transactions and verify event
   high-water against task versions. There is no best-effort async dual write.
3. Backfill one synthetic snapshot event for pre-existing tasks and record the
   migration floor.
4. Add authenticated replay API, then live notifications/polling, heartbeats,
   and limits behind a feature flag.
5. Exercise multiple replicas, Redis disabled, replica termination, database
   failover, slow consumers, expired cursors, and revocation in staging.
6. Enable per installation, monitor gaps/lag/capacity, then expose the feature
   through MAT-24 capability declarations.

Rollback disables new subscriptions and notifications but keeps atomic event
appends until the release is safely rolled back. Dropping event writes while
clients may hold cursors would create silent gaps.

## 18. Tests and observability

Required tests include transaction rollback, unknown commit replay,
idempotency, per-task ordering under concurrent writers, no committed gaps,
replay/live boundary races, missed/duplicate notifications, replica handoff,
cursor tampering/cross-task reuse/expiry, terminal closure, retention cleanup,
and snapshot recovery.

Security tests cover cross-owner/tenant/agent denial, result-reference follow,
revocation during a stream, admin override audit, CORS, cache headers, and
payload redaction. Load tests cover many idle streams, burst fanout, long
replay, slow readers, reconnect storms, database failover, and Redis loss.

Metrics include active/rejected connections, replayed/live events, database
query and delivery lag, wakeup-to-send latency, polling fallback, slow-consumer
disconnects, cursor expiry, auth revocation closes, event bytes/count,
retention floor, cleanup duration, and transaction/event invariant failures.
Labels stay bounded; task/principal IDs and payload content are excluded.

## 19. Out of scope

- native WebSocket task subscriptions;
- outbound webhooks or caller-owned callback delivery (G8);
- A2A routes, wire types, auth schemes, and discovery (G10/G11);
- raw transcript, reasoning, token, tool, terminal, or provider event streaming;
- replacing the existing fleet/CRD EventBus; and
- exactly-once network delivery or an unbounded audit ledger.

## 20. Estimate

Estimated implementation is **3–5 engineer-weeks**: transactional event store
and migration (1–1.5 weeks), replay/SSE and multi-replica wakeups (1–1.5
weeks), authorization/retention/limits (0.5–1 week), and failover/load/security
testing (0.5–1 week). A2A projection is separate.

## 21. Acceptance criteria

- Every published event corresponds to a committed public task mutation, and
  every such mutation appends its event in the same transaction.
- Per-task sequence and opaque cursor provide ordered, at-least-once replay
  across reconnects and replicas without relying on notifications for truth.
- Replay-to-live handoff has no gap; slow consumers are disconnected rather
  than buffered without bound or silently skipped.
- Expired cursors produce an authorized snapshot-recovery response.
- MAT-23 protects initial replay and long-lived access, including revocation.
- Only normalized task-contract data is published; harness-private activity
  never enters the log.
- PostgreSQL remains authoritative with Redis unavailable.
- Native v1 exposes SSE only and preserves a clean later A2A projection seam.

