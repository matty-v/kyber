# Durable externally addressable agent tasks

**Status:** Draft
**Date:** 2026-08-30
**Tracker:** [MAT-19](https://linear.app/matty-v/issue/MAT-19/designplatform-durable-externally-addressable-agent-tasks)
**Parent:** [MAT-6](https://linear.app/matty-v/issue/MAT-6)

## 1. Decision summary

Add a native Kyber task service backed by PostgreSQL. It gives trusted API
callers a stable task ID, durable lifecycle snapshot, idempotent creation,
restart-safe dispatch intent, configurable retention, Get, and cursor-paginated
List. PostgreSQL is authoritative; Redis and process memory may accelerate
wakeups but never own task state.

Keep Claude Code and Codex as the execution engines. The runtime adapter sends
one bounded task envelope through the existing per-agent delivery boundary and
the status sidecar retains the only control-plane credential. G1 does not add a
second agent loop, per-task harness process, transcript parser, or protocol
model.

The durable guarantee applies to the **task record and dispatch intent**, not
to uninterrupted model execution. The current adapters inject work into one
long-lived tmux conversation and expose no durable harness turn query that
Kyber can use after a crash. The
[MAT-28 receipt spike](2026-08-30-runtime-turn-receipt-spike.md) found a
promising common positive boundary: both pinned integrations have managed
`UserPromptSubmit` hooks, and Codex includes a native turn ID. Live restart and
task-envelope tests validated that boundary for both harnesses, and persisted
receipts survived session restarts. The sidecar recovery handshake and the
remaining failure cut points are still required before production reliance.
Kyber therefore does not
automatically redeliver an unresolved attempt; it records
`delivery_unknown`.

MAT-20 through MAT-23 and MAT-25 will extend this foundation with typed
updates/results, cancellation, continuation, principal authorization, and
events. This design reserves their keys and transition seams but does not
implement their behavior.

## 2. Goals and non-goals

### Goals

- Keep task state addressable across control-plane, Redis, sidecar, and agent
  pod restarts until an explicit retention deadline.
- Accept a task and its dispatch intent atomically so a successful create
  cannot be lost from an in-memory queue.
- Give callers deterministic idempotency, Get, and stable bounded List.
- Serialize task delivery per agent across control-plane replicas.
- Make every delivery outcome and ambiguity visible without claiming
  exactly-once execution.
- Preserve the existing pod-local MCP trust boundary and support both runtimes
  through one small adapter contract.
- Bound prompt size, outstanding work, retries, task age, retained rows, and
  query cost.

### Non-goals

- A2A wire types, routes, Agent Cards, or protocol state names.
- Progress, multiple results, artifacts, or multimodal content (MAT-20).
- Cancellation or rollback of side effects (MAT-21).
- Follow-up input, approvals, or credential acquisition (MAT-22).
- The final tenant/principal authorization model (MAT-23).
- Live subscriptions or a public event log (MAT-25).
- Exactly-once model/tool execution, task recovery inside an unknown harness
  turn, or transcript-derived completion.
- Replacing the existing short request/reply API in the first delivery slice.

## 3. Current state and capability audit

### Ephemeral request foundation

[`pkg/requeststore/store.go`](../../pkg/requeststore/store.go) defines the
current `queued`, `dispatched`, `completed`, `failed`, and `expired` request
lifecycle. Its default lifetime is 60 seconds, the compiled maximum is five
minutes, and terminal retention is a per-agent count. The interface has Create,
Get, dispatch, fail, and complete operations but no List.

[`pkg/requeststore/redis.go`](../../pkg/requeststore/redis.go) uses Lua for
atomic limits and transitions. Every record has a Redis expiry, so physical
deletion is part of normal lifecycle behavior. Redis is useful dispatch
infrastructure, but this data model cannot become durable merely by increasing
its TTL.

[`pkg/api/routes_agent_requests.go`](../../pkg/api/routes_agent_requests.go)
creates the record before enqueue, sends an envelope through the in-process
per-agent queue, and reports delivery through a callback. The queue in
[`pkg/inbound/queue.go`](../../pkg/inbound/queue.go) is bounded and serial per
agent within one control-plane process, but accepted jobs and callbacks do not
survive process termination and different replicas do not share its FIFO.

[`pkg/inbound/delivery.go`](../../pkg/inbound/delivery.go) waits for the Agent
to become Running and then executes one delivery attempt before the short
request expiry. Successful tmux injection is the current meaning of
`dispatched`; it is not a runtime receipt or model-turn identity.

### Runtime boundary

Both runtime adapters register the same pod-loopback request MCP URL from
[`pkg/runtimes/request.go`](../../pkg/runtimes/request.go). The status sidecar's
[`request_mcp.go`](../../cmd/status-sidecar/request_mcp.go) validates an opaque
request ID, bounds the response, and forwards it using the pod's internal
identity. The agent container never receives a caller token or control-plane
credential. This is the boundary to preserve.

Claude Code and Codex both persist their conversational files under `/persist`
and can resume a harness session after selected failures. Kyber's supported
adapter interface, however, exposes no durable per-turn handle, queued-input
receipt, or query for whether a particular injected envelope is still running.
Both adapters ultimately operate a long-lived interactive process in tmux.
Native harness behavior can help the agent continue, but it cannot serve as the
platform task source of truth.

### PostgreSQL foundation

The control plane already opens one PostgreSQL pool when
`KYBER_POSTGRES_URL` is configured, fails loudly if migrations cannot complete,
and shares the pool between durable stores in
[`cmd/control-plane/main.go`](../../cmd/control-plane/main.go). The brief and
skill stores demonstrate startup migration and no-silent-fallback behavior.
Their `CREATE TABLE IF NOT EXISTS` scaffolds are adequate for simple tables;
task evolution needs ordered, versioned migrations before implementation.

Production task service availability requires PostgreSQL. Unlike briefs and
skills, tasks must not fall back to memory when the database is absent. A
development-only memory repository may exist behind an explicit mode, but the
public task routes fail closed by default without PostgreSQL.

## 4. Native task contract

### Identity and input

A task ID is `task_` plus 128 bits from `crypto/rand`, encoded as lowercase
hex. IDs are opaque and never carry an agent, tenant, timestamp, or database
sequence. They are not authorization secrets.

Create accepts:

```json
{
  "prompt": "bounded UTF-8 text",
  "correlation": "optional caller reference",
  "deadlineAt": "optional RFC3339 timestamp"
}
```

G1 remains text-input compatible with MAT-9. The default execution deadline is
24 hours; operators may configure a value from 5 minutes through 7 days. A
caller may request an earlier deadline but never extend the installation cap.
The prompt defaults to an 8 KiB maximum and has a 32 KiB compiled ceiling.
Correlation defaults to 256 bytes and has a 1 KiB ceiling. MAT-20 owns future
typed input/output payloads.

Clients should send an `Idempotency-Key` on Create. The key is bounded to 128
bytes and scoped to the authenticated caller plus target agent. In the G1
transition period the caller's existing authenticated `Caller.Name` is stored;
MAT-23 replaces that provisional name with a durable principal/tenant key.

The store saves a SHA-256 hash of the canonical create input. Reusing the key
with the same hash returns the original task and never dispatches again.
Reusing it with different input returns `409 idempotency_conflict`. The key and
hash live at least as long as the task, so retention cannot resurrect a key
while its task is still addressable.

### Public lifecycle

G1 exposes four states:

```text
queued -> dispatched -> completed
   |           |
   +----------> failed
```

- `queued`: durable task and dispatch intent committed; no delivery attempt has
  begun.
- `dispatched`: the delivery adapter returned success after sending the
  envelope to the runtime boundary.
- `completed`: the existing explicit response tool accepted one bounded text
  response. MAT-20 replaces this compatibility result with typed updates.
- `failed`: stable failure code and safe message are available.

`completed` and `failed` are terminal. Repeating the same terminal transition
is idempotent; a different terminal outcome conflicts. Internal dispatch lease
states are not public lifecycle states.

Initial stable failure codes are:

- `agent_unavailable`: the agent did not become Running before its deadline;
- `delivery_failed`: the adapter proved the envelope was not delivered;
- `delivery_unknown`: a worker or control plane disappeared after an attempt
  began, so delivery cannot be ruled in or out;
- `deadline_exceeded`: the task remained non-terminal through its execution
  deadline; and
- `internal_error`: a bounded, non-sensitive fallback for an unrecoverable
  platform transition.

A deadline is not physical deletion. The reconciler moves non-terminal tasks
to `failed/deadline_exceeded`; the retention deadline controls later deletion.

### Read contract

Get returns the task snapshot by agent and task ID. G1 preserves the existing
agent route family:

```text
POST /api/v1/agents/{agent}/tasks
GET  /api/v1/agents/{agent}/tasks/{taskID}
GET  /api/v1/agents/{agent}/tasks?limit=&cursor=&state=
```

Responses include ID, agent, state, failure code when relevant, correlation,
created/updated/deadline/retention timestamps, and an integer version. Prompt
and compatibility response visibility follow the current request-read trust
boundary; MAT-23 will make per-principal ownership mandatory before the route
is broadly exposed.

List uses keyset pagination ordered by immutable `(created_at DESC, id DESC)`.
The cursor is opaque, versioned base64url containing the final tuple and a hash
of normalized filters. Default limit is 20 and maximum is 100. A cursor with
different filters is invalid. Immutable ordering avoids tasks moving between
pages as their state changes. Concurrent retention may remove an older item,
but cannot duplicate or reorder remaining rows.

List supports only indexed agent, state, and creation-time filters in G1. It
does not search prompts, responses, correlations, or transcripts.

## 5. Persistence model

Use versioned migrations for two G1 tables. Names are native and intentionally
not A2A-specific.

```sql
CREATE TABLE agent_tasks (
  id                 TEXT PRIMARY KEY,
  agent_namespace    TEXT NOT NULL,
  agent_name         TEXT NOT NULL,
  created_by         TEXT NOT NULL,
  prompt             TEXT NOT NULL,
  correlation        TEXT NOT NULL DEFAULT '',
  state              TEXT NOT NULL,
  failure_code       TEXT NOT NULL DEFAULT '',
  response           TEXT NOT NULL DEFAULT '',
  version            BIGINT NOT NULL DEFAULT 1,
  created_at         TIMESTAMPTZ NOT NULL,
  updated_at         TIMESTAMPTZ NOT NULL,
  deadline_at        TIMESTAMPTZ NOT NULL,
  retain_until       TIMESTAMPTZ NOT NULL,
  completed_at       TIMESTAMPTZ,
  CHECK (state IN ('queued', 'dispatched', 'completed', 'failed'))
);

CREATE TABLE agent_task_dispatches (
  task_id            TEXT PRIMARY KEY REFERENCES agent_tasks(id) ON DELETE CASCADE,
  status             TEXT NOT NULL,
  lease_owner        TEXT,
  lease_until        TIMESTAMPTZ,
  attempts           INTEGER NOT NULL DEFAULT 0,
  next_attempt_at    TIMESTAMPTZ NOT NULL,
  attempt_started_at TIMESTAMPTZ,
  last_error_code    TEXT NOT NULL DEFAULT '',
  updated_at         TIMESTAMPTZ NOT NULL,
  CHECK (status IN ('pending', 'leased', 'attempting', 'receipt_pending', 'delivered', 'closed'))
);
```

Idempotency records may be a third table or columns with a unique partial
index. The required uniqueness is `(created_by, agent_namespace, agent_name,
idempotency_key)`, with the request hash and task ID stored in the same
transaction. MAT-23 will migrate `created_by` into its principal/tenant model;
the schema must not rely on caller display names as globally permanent IDs.

Required indexes cover:

- `(agent_namespace, agent_name, created_at DESC, id DESC)` for List;
- `(agent_namespace, agent_name, state, created_at DESC, id DESC)` for filtered
  List and outstanding limits;
- dispatch `(status, next_attempt_at)` plus agent identity for claims; and
- `retain_until` and `deadline_at` for bounded reconcilers.

Prompt and response columns are G1 compatibility fields, not the future
artifact schema. MAT-20 migrates result data into its own bounded tables or
JSON representation without changing task identity.

## 6. Atomic transitions and concurrency

Every mutation runs in a database transaction and locks the task row with
`SELECT ... FOR UPDATE`. The repository takes expected version/state and
returns a typed conflict when they do not match. Database time sets all
timestamps.

Create performs these operations atomically:

1. validate the request and idempotency tuple;
2. enforce the configured outstanding-task limit for the agent/caller;
3. insert `agent_tasks` in `queued`;
4. insert `agent_task_dispatches` in `pending`; and
5. commit before returning `202`.

No process-memory enqueue is required for correctness. A commit notification
or short poll wakes dispatch workers. Losing a notification only adds polling
latency.

Complete continues to be single-assignment. The internal sidecar route checks
pod identity and task agent, locks the row, accepts only `dispatched`, writes
the terminal state/result, increments version, and closes dispatch state in one
transaction. An old pod cannot complete a task for a different agent. MAT-20
will add an execution-attempt token so a replacement pod cannot complete a
stale attempt; G1 should reserve that request field even if the initial MCP
tool sends only task ID.

## 7. Durable dispatch and ambiguous delivery

### Claiming and serialization

Each control-plane replica runs a bounded dispatcher only when PostgreSQL is
healthy. Workers claim eligible pending rows with `FOR UPDATE SKIP LOCKED`.
Before claiming, the transaction acquires a per-agent PostgreSQL advisory lock
or equivalent durable agent-lane lease. One agent has at most one active
delivery attempt across replicas, preserving the current FIFO property.

Within the claim transaction the worker writes a unique lease owner, lease
deadline, increments attempts, and sets `leased`. A bounded retry with jitter is
allowed while no external delivery attempt has begun: for example, waiting for
an Agent to become Running or a transient database wakeup failure.

Immediately before invoking the existing delivery adapter, the worker commits
`attempting` and `attempt_started_at`. After tmux submission it moves to
`receipt_pending`; `dispatched` requires the matching managed
`UserPromptSubmit` receipt described by MAT-28, not merely a successful paste.
From `attempting` onward, an error or worker death is ambiguous unless the
adapter or receipt handshake proves the prompt did not enter model processing.

### Outcomes

- A persisted, matching positive harness receipt atomically marks the task
  `dispatched` and dispatch row `delivered`.
- A proven pre-delivery failure may return to `pending` with bounded backoff if
  the task deadline and attempt cap permit.
- A known terminal unavailability at the deadline marks
  `failed/agent_unavailable`.
- Any expired `attempting` or `receipt_pending` lease that the receipt
  reconciler cannot resolve marks `failed/delivery_unknown`; it is never
  automatically redelivered.
- Completion racing the delivery callback wins if its explicit sidecar update
  is valid. The later callback observes terminal state and no-ops.

This is intentionally at-least-persisted and at-most-one-automatic-attempt
after the ambiguity boundary, not exactly-once execution. A caller may create
a replacement task after `delivery_unknown`; external side effects must carry
their own idempotency where required.

### Restart reconciliation

On control-plane startup and continuously thereafter, a reconciler processes
bounded batches:

- expired `leased` rows that never reached `attempting` return to `pending`;
- expired `attempting` and `receipt_pending` rows are queried through the
  adapter receipt seam and become `failed/delivery_unknown` if unresolved;
- queued/dispatched tasks past `deadline_at` become
  `failed/deadline_exceeded`; and
- terminal tasks past `retain_until` are deleted in bounded batches.

Agent pod restart does not delete task state. In G1, a dispatched task stays
dispatched until explicit completion or its deadline. Kyber cannot infer from
a pod UID change whether the harness resumed the same turn. A later design may
use an adapter-supplied stable execution receipt, but it must not guess from
transcript text or global activity.

## 8. Runtime adapter seam

Add a narrow task delivery capability beside the general runtime metadata:

```go
type TaskDeliveryAdapter interface {
    DeliverTask(context.Context, AgentRef, TaskEnvelope) (DeliveryReceipt, error)
    Capabilities() TaskDeliveryCapabilities
}
```

The initial Claude Code and Codex implementations can wrap the existing tmux
delivery helper. `Capabilities` states whether the harness integration can
provide a stable native turn ID, a positive queued-input receipt, or resume
status. The current audit finds no durable turn query in either pinned TUI
integration, but both expose a managed pre-model prompt hook; Codex's verified
payload also includes `turn_id`. MAT-28's live prototype validated positive
receipts and session-restart persistence on both harnesses. Its sidecar
handshake and remaining destructive failure matrix must finish before these
capabilities are treated as production guarantees.

`TaskEnvelope` contains the Kyber task ID, agent identity, deadline, bounded
prompt, and a random execution-attempt token reserved for stale-pod protection.
It does not carry public caller credentials, callback URLs, database addresses,
or A2A types. The prompt directs the agent to use the pod-local Kyber task MCP
tool. G1 may temporarily expose `complete(task_id, response)`; MAT-20 replaces
it with the richer task-update contract.

If a future harness adapter exposes a reliable native turn API, it may persist
that opaque receipt in dispatch metadata and implement stronger reconciliation.
It cannot weaken the native task state machine or leak harness-specific IDs to
public clients.

## 9. Retention, limits, and cleanup

Recommended defaults and compiled maxima are deliberately conservative:

| Limit | Default | Compiled maximum |
| --- | ---: | ---: |
| Prompt | 8 KiB | 32 KiB |
| Correlation | 256 B | 1 KiB |
| Compatibility response | 32 KiB | 128 KiB |
| Outstanding tasks per agent | 8 | 100 |
| Create rate per caller/agent | 30/min | 300/min |
| Execution deadline | 24 h | 7 d |
| Terminal retention | 7 d | 30 d |
| List page | 20 | 100 |
| Dispatch attempts before `attempting` | 3 | 10 |

The installation also has a global retained-task quota and database-size alert.
When the quota is reached, Create fails with `503 task_capacity_exhausted` and
a retry hint; Kyber never evicts non-expired tasks to make room silently.

Cleanup claims bounded rows with `SKIP LOCKED`, deletes terminal tasks only,
and exports duration, rows, and errors. Non-terminal deadline reconciliation
runs before retention deletion. A legal-hold or archive feature is not in G1.

## 10. Security and privacy

- Continue to require `requests:write` for Create and `requests:read` for Get
  and List during the G1 transition. MAT-23 must replace this agent-level check
  with ownership plus action scopes before multi-principal production use.
- Store provisional creator identity now; do not treat task IDs or
  idempotency keys as credentials.
- Keep all runtime completion traffic on the sidecar-authenticated internal
  route. Verify pod agent identity and, when introduced, execution-attempt token
  on every mutation.
- Never expose transcripts, hidden reasoning, shell output, environment,
  harness session IDs, or internal delivery errors through task reads.
- Log task ID, agent, caller name, transition, attempt number, and safe failure
  code. Never log prompt, response, idempotency key, or Bearer material.
- Enforce body limits before JSON decoding, normalize timestamps and filters,
  and return non-enumerating 404 behavior where authorization requires it.
- Encrypt PostgreSQL transport and backups using installation policy. Database
  readers can see prompts/results; document that trust boundary and avoid
  placing secrets in task prompts.

## 11. Availability and observability

Public task routes fail closed when PostgreSQL is unavailable. Create must not
return acceptance without a committed task and dispatch row. Get/List return a
bounded service-unavailable error rather than stale Redis state.

Metrics:

- create/get/list totals and latency by safe outcome;
- tasks by state and age bucket, not task ID;
- pending/leased/attempting dispatch counts and oldest age;
- delivery attempts, proven failures, unknown outcomes, and retries;
- deadline and retention reconciliation totals/duration/errors;
- idempotency replays/conflicts and capacity rejections; and
- PostgreSQL pool saturation and dispatcher lease contention.

Structured logs and traces carry task ID as a high-cardinality log/trace field,
not a metric label. Alerts cover oldest queued age, stuck attempting leases,
`delivery_unknown` rate, reconciliation failures, retained-row quota, and
database availability.

## 12. API compatibility and rollout

Ship G1 alongside `/requests`; do not silently reinterpret existing request
IDs, expiry, limits, or response shapes. Suggested slices are:

1. versioned task migrations, repository, memory test double, state-machine and
   pagination tests;
2. create/get/list routes behind an installation feature flag, with PostgreSQL
   hard requirement and dispatch disabled;
3. durable dispatcher/reconciler and task envelope reusing current runtime
   delivery;
4. sidecar `kyber-task.complete` compatibility tool and internal endpoint;
5. restart, replica, failure-injection, capacity, and retention tests;
6. opt-in agent configuration and production soak; then
7. later MAT-20–23 migrations before considering `/requests` deprecation.

Rollback first disables new Create and dispatch claims while preserving reads,
completion of already dispatched tasks, and retention. Schema rollback is
forward-only: old application versions ignore the new tables; destructive
table removal waits until retained tasks have expired and a later migration is
explicitly approved.

## 13. Test strategy

### Repository and state machine

- Create/idempotent replay/conflict, transition matrix, version conflicts, and
  single-assignment completion.
- Keyset pagination with equal timestamps, concurrent updates, filter-bound
  cursors, invalid cursors, and retention deletion.
- Limits, Unicode byte counts, deadlines, capacity, and cleanup batches.
- The same contract suite against memory and PostgreSQL repositories.

### Dispatch and failure injection

- Multiple workers/replicas preserve one per-agent lane and claim distinct
  agents concurrently.
- Crash after claim but before `attempting` retries safely.
- Crash after `attempting` becomes `delivery_unknown` without redelivery.
- Completion racing delivery callback is terminal and idempotent.
- Database outage before Create never returns acceptance; outage after commit
  leaves recoverable dispatch intent.
- Control-plane restart recovers pending work and reconciles leases.
- Agent unavailable, pod restart, sidecar restart, tmux delivery failure, and
  execution deadline retain queryable outcomes.

### Security and operations

- Cross-agent internal completion is rejected; stale attempt tokens are
  rejected when enabled.
- Scope, ID guessing, cursor tampering, body/query amplification, and log
  redaction tests.
- Retention and quota load tests at configured maxima.
- PostgreSQL migration retry/fail-loud behavior and backward-compatible rollout.
- Live behavioral tests for both Claude Code and Codex to confirm the audited
  receipt/resume capabilities and explicit completion tool.

## 14. Alternatives rejected

### Increase the Redis TTL

Rejected. It leaves accepted dispatch in process memory, provides no durable
List/index model, couples lifecycle to physical expiry, and makes migrations,
retention, and future ownership/artifacts harder.

### One Kubernetes resource per task

Rejected. Prompts and results would enter a broadly observable control-plane
store, task churn would burden watches and etcd, and pagination/retention would
be awkward. Agent CRDs represent hosted runtimes, not external work records.

### Treat harness sessions as tasks

Rejected. One Kyber agent has a long-lived interactive session shared with chat,
terminal, jobs, and API input. The current adapter exposes no stable per-turn
receipt across both harnesses, and session transcripts are neither an
authorization boundary nor a reliable state machine.

### Automatically redeliver after every crash

Rejected. A worker can die after tmux accepted the envelope but before Kyber
persisted success. Blind retry can duplicate model/tool side effects. An honest
`delivery_unknown` outcome is safer until an adapter supplies a durable native
receipt or downstream work is guaranteed idempotent.

### Replace the interactive harness with one headless process per task

Rejected for G1. It discards Kyber's long-lived agent identity and conversation,
duplicates harness orchestration, changes cost and credential behavior, and
violates the thin-adapter direction. It may be a separate execution mode if a
future product requirement values isolated batch tasks over conversational
agents.

## 15. Open review questions

1. Should the default execution deadline be 24 hours and terminal retention 7
   days, or should installations start with shorter defaults?
2. **Decided:** require MAT-28's harness receipt prototype before dispatch
   implementation. Keep `delivery_unknown` only as the conservative fallback
   for an attempt the receipt reconciler cannot resolve.
3. Should G1 store the current caller name for migration into MAT-23, or remain
   limited to the legacy full-scope operator until stable principal IDs exist?
4. Does the first slice need a compatibility text result, or may tasks remain
   `dispatched` until MAT-20 supplies the task-update tool?
5. Should `/tasks` require PostgreSQL in every environment, or may an explicit
   development-only memory mode exist for local tests and demos?

## 16. Revised estimate

The original four-to-six-week range remains reasonable for implementation,
but it is now conditional on one short harness prototype:

- repository, migrations, lifecycle, idempotency, Get/List: 1.5–2 weeks;
- dispatcher, leasing, reconciliation, and failure injection: 1.5–2 weeks;
- runtime/sidecar compatibility completion and both-harness tests: 0.5–1 week;
- limits, observability, rollout, soak, and review fixes: 0.5–1 week.

If either harness exposes a reliable native queued-turn receipt and query API,
the dispatcher may simplify. If neither does, the conservative
`delivery_unknown` design stays inside this estimate. Isolated per-task harness
processes or exactly-once execution are outside it.
