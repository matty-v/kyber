# Cooperative task cancellation

**Status:** Proposed
**Date:** 2026-08-30
**Tracker:** [MAT-21](https://linear.app/matty-v/issue/MAT-21/designplatform-cooperative-task-cancellation)
**Depends on:** [MAT-19](2026-08-30-durable-agent-tasks.md) and [MAT-20](2026-08-30-task-progress-typed-results.md)
**Origin:** [MAT-6](https://linear.app/matty-v/issue/MAT-6/spikeplatform-what-formal-a2a-protocol-support-would-require-for-kyber), [A2A gap study](2026-08-30-a2a-protocol-support.md), and gap-study PR #183

## 1. Decision

Add an idempotent native Cancel Task operation with honest cooperative
semantics:

- queued work is canceled atomically before dispatch;
- dispatched work enters `canceling` and records a durable cancellation
  request;
- an adapter may actively interrupt only when it can target the exact native
  task turn and observe a structured terminal acknowledgment;
- otherwise Kyber notifies the agent and waits for an explicit task-scoped
  acknowledgment; and
- timeout becomes `failed/cancel_unconfirmed`, never a false `canceled`.

Do not send generic Escape/Ctrl+C keystrokes to the current Claude Code or
Codex TUI adapters in production. A live MAT-21 prototype proved that an exact
foreground marker narrows the target, but the TUIs do not provide the
acknowledgment and input cleanup required for a safe task contract.

Cancellation stops Kyber from intentionally continuing future task work. It
does not roll back filesystem writes, network calls, payments, deployments,
messages, or other external effects that already occurred.

## 2. Current state

Kyber request expiry removes or expires control-plane state but does not stop a
prompt already delivered to a long-lived harness. Stopping an Agent or
restarting its session interrupts the whole shared runtime and can disrupt
operator conversations, channel messages, scheduled jobs, and unrelated
background work. It is not task cancellation.

MAT-19 supplies durable task identity, execution-attempt identity, delivery
receipts, row locking, and honest ambiguous outcomes. MAT-20 supplies the
task-scoped loopback MCP/internal update path. MAT-21 extends those boundaries;
it does not add another agent loop or parse transcripts.

## 3. Runtime capability evidence

### Supported native primitives

The official [Codex app-server contract](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md)
exposes `turn/interrupt(threadId, turnId)`. A successful call
targets the exact turn and is followed by `turn/completed` with
`status: interrupted`. The same official contract warns that interruption does
not terminate background terminals; those require separate explicit cleanup.
Kyber's current Codex integration is a long-lived TUI, not app-server.

Claude Code's official [interactive-mode reference](https://code.claude.com/docs/en/interactive-mode)
documents Escape and Ctrl+C as interruption controls for the
current response or tool call. Its current TUI integration does not expose a
stable programmatic task-turn interrupt request plus structured terminal event.
A future Claude Agent SDK adapter may provide a stronger capability, but it is
not silently assumed by this design.

### MAT-21 live prototype

The disposable prototype consists of:

- [`mat21-task-marker.sh`](prototypes/mat21-task-marker.sh), managed
  `UserPromptSubmit`/`Stop` hooks that store only exact task, attempt, session,
  optional turn, timestamp, and interrupt status;
- [`mat21-guarded-interrupt.sh`](prototypes/mat21-guarded-interrupt.sh), which
  takes the same lock, checks the exact non-stale foreground marker, sends one
  adapter key, and makes duplicates idempotent; and
- [`mat21-guarded-interrupt_test.sh`](prototypes/mat21-guarded-interrupt_test.sh),
  which proves exact-attempt, duplicate, stopped, non-task, and stale-marker
  behavior without a harness.

The local test passed. Live tests ran in guarded `kyber-dev` purpose-built
agents using Codex 0.151.0 and Claude Code 2.1.251:

- Codex Escape visibly interrupted the foreground conversation, but left the
  spawned background terminal running and did not yield a usable managed Stop
  acknowledgment. The marker remained `interrupt_sent` until a later non-task
  prompt safely cleared it.
- Claude Escape interrupted the exact marked task before model output, but the
  canceled prompt remained in the editor. A subsequent dispatcher paste was
  appended to that stale task prompt and re-submitted its task header, rearming
  the marker. This is an unsafe redelivery shape.
- A Claude cancel racing normal completion found no marker and sent no key,
  which is the safe result.
- Both purpose-built test agents were explicitly deleted; the unrelated
  `echo` agent was not modified.

The result is decisive: a foreground marker prevents a known wrong-target
interrupt, but it cannot make a TUI keystroke atomic with harness turn state,
clear the input editor safely, terminate detached work, or produce a durable
acknowledgment. Production TUI adapters are therefore `notify_only`.

## 4. State model

MAT-21 adds `canceling` and `canceled` to the native public task lifecycle:

```text
queued ─────────────── cancel ───────────────> canceled
  │
  └─ dispatch ─> dispatched ─ cancel request ─> canceling
                       │                           │
                       ├─ complete/fail ──────────┤─> completed | failed
                       │                           ├─ exact/agent ack ─> canceled
                       │                           └─ deadline ─> failed(cancel_unconfirmed)
                       └─ deadline ──────────────────> failed(deadline_exceeded)
```

`canceling` means a valid request is durable and Kyber will not intentionally
start another execution attempt. It does not mean the harness or external
effects have stopped.

`canceled` means one of:

1. the task never crossed the dispatch ambiguity boundary; or
2. the exact active attempt acknowledged cancellation through the task MCP
   path; or
3. an exact native adapter interrupt produced its structured terminal event.

It never means rollback. The terminal record exposes `cancelScope:
future_task_work` so API and A2A clients cannot infer compensation of prior
side effects.

If completion or failure locks and transitions the task before the cancel
transaction, Cancel returns the terminal task and `applied: false`. If Cancel
commits `canceling` first, a later valid completion/failure from the same
attempt may still win because the work may have finished before observing
cancellation. A cancellation acknowledgment racing completion is serialized by
the task row lock; the first valid terminal transition wins, and the loser
receives the canonical task.

## 5. Public API

```http
POST /api/v1/agents/{agent}/tasks/{task}/cancel
Idempotency-Key: <caller-generated key>
Content-Type: application/json

{
  "reason": "superseded by a newer deployment request"
}
```

The reason is optional, caller-visible, UTF-8, and bounded. It is never treated
as a trusted instruction and is not pasted raw into the harness. The response
returns the canonical task plus:

```json
{
  "cancel": {
    "requestedAt": "...",
    "requestedBy": "principal-ref",
    "reason": "superseded by a newer deployment request",
    "status": "requested",
    "applied": true
  }
}
```

Rules:

- identical replay returns the same result;
- the same idempotency key with different normalized input returns `409`;
- canceling or canceled returns the current task idempotently;
- an already completed/failed task returns `200`, `applied: false`, and the
  terminal task rather than pretending cancellation occurred;
- missing or unauthorized uses MAT-23's non-enumerating behavior; and
- store outage returns `503` without claiming acceptance.

MAT-23 defines final ownership and scopes. Until then, use MAT-19's provisional
creator/admin authorization and reserve `tasks:cancel` as a distinct action.

## 6. Persistence and concurrency

Add cancellation fields to the task and a delivery row for retries:

```sql
ALTER TABLE agent_tasks
  ADD COLUMN cancel_requested_at TIMESTAMPTZ,
  ADD COLUMN cancel_requested_by TEXT,
  ADD COLUMN cancel_reason TEXT NOT NULL DEFAULT '',
  ADD COLUMN cancel_deadline_at TIMESTAMPTZ,
  ADD COLUMN cancel_acknowledged_at TIMESTAMPTZ,
  ADD COLUMN cancel_ack_source TEXT NOT NULL DEFAULT '',
  ADD CONSTRAINT task_state_check
    CHECK (state IN ('queued','dispatched','canceling','canceled','completed','failed'));

CREATE TABLE agent_task_cancel_deliveries (
  task_id          TEXT PRIMARY KEY REFERENCES agent_tasks(id) ON DELETE CASCADE,
  attempt_id       TEXT NOT NULL,
  status           TEXT NOT NULL,
  adapter_mode     TEXT NOT NULL,
  delivery_count   INTEGER NOT NULL DEFAULT 0,
  next_delivery_at TIMESTAMPTZ NOT NULL,
  lease_owner      TEXT,
  lease_until      TIMESTAMPTZ,
  last_safe_error  TEXT NOT NULL DEFAULT '',
  updated_at       TIMESTAMPTZ NOT NULL,
  CHECK (status IN ('pending','delivering','notified','interrupted','acknowledged','closed')),
  CHECK (adapter_mode IN ('notify_only','exact_interrupt'))
);
```

Cancel locks the task row. Queued cancellation atomically marks the task
`canceled` and closes its dispatch row. Dispatched cancellation atomically
marks `canceling`, fixes the current attempt ID, inserts the cancel delivery,
increments version, and appends the MAT-20 safe update record.

Workers claim cancel deliveries with `FOR UPDATE SKIP LOCKED`. Cancellation
delivery has priority over new task dispatch for the same agent. It retries
notification idempotently until acknowledgment, terminal task outcome, or the
cancel deadline. It never starts a replacement task attempt.

## 7. Runtime contract

Extend the adapter seam:

```go
type TaskCancellationMode string

const (
    NotifyOnly     TaskCancellationMode = "notify_only"
    ExactInterrupt TaskCancellationMode = "exact_interrupt"
)

type TaskCancellationAdapter interface {
    CancellationMode() TaskCancellationMode
    RequestCancellation(context.Context, TaskAttemptReceipt) (CancelReceipt, error)
}
```

`ExactInterrupt` is advertised only when the adapter accepts the exact opaque
turn receipt and returns a structured terminal acknowledgment for that same
turn. Codex app-server can satisfy this after an explicit adapter migration.
Generic tmux keys, process signals, transcript strings, and global harness
idle status cannot.

`NotifyOnly` writes the durable cancel request and makes it available through
the loopback `kyber-task` MCP server:

```text
get_control(task_id, attempt_id) -> {cancel_requested, reason, requested_at}
ack_cancel(task_id, attempt_id, acknowledgment_id, note?)
```

The task envelope tells the agent to check control before each material phase
and after long tools. Managed pre-tool hooks may additionally block new tool
starts when a matching local cancel request exists, but that is defense in
depth, not terminal acknowledgment. Hook coverage differs by runtime and does
not stop model generation, already-running tools, or background processes.

The sidecar may steer a sanitized fixed message such as "Kyber cancellation
requested for the active task; stop future work and call ack_cancel" only when
the harness exposes an exact same-turn steering API. It does not paste the
caller's reason or queue ambiguous TUI input.

## 8. Signals, tools, and side effects

- Never restart the Agent pod/session, SIGTERM the harness, or SIGKILL the pod
  for one task.
- Never send a signal based only on task state, tmux pane text, or global
  activity.
- An exact native foreground interrupt may stop generation or a foreground
  tool, but detached/background processes require separate ownership and
  cleanup. Until Kyber can attribute them to the exact task, their existence
  prevents an adapter from claiming complete process cancellation.
- MCP/tool servers should accept context cancellation when the harness passes
  it, but external services may have committed work already.
- Agents must not claim rollback. Workflows needing compensation model that as
  a new explicit task or domain operation.

## 9. Timeouts and restart recovery

Default cancel acknowledgment deadline is five minutes, capped at one hour and
never later than the task deadline. Installations may lower it.

On control-plane restart, the reconciler resumes pending/delivering rows.
`ExactInterrupt` queries the exact native turn when supported; `NotifyOnly`
redelivers only through its idempotent exact control path. Pod restart does not
turn `canceling` into `canceled`. A stale attempt cannot acknowledge after a
replacement attempt or task terminal transition.

At the cancel deadline:

- if the task has another known terminal outcome, keep it;
- if exact evidence proves interruption, mark `canceled`;
- otherwise mark `failed/cancel_unconfirmed` and retain cancel metadata.

`cancel_unconfirmed` means Kyber stopped scheduling future task work but could
not prove the prior harness work stopped. Operators can then inspect and take
domain-specific action.

## 10. Security, limits, and observability

- Reason default 512 bytes, compiled maximum 2 KiB.
- One live cancellation record per task; repeated requests do not grow storage.
- Default cancel rate 30/minute per caller/agent, maximum 300/minute.
- Authenticate every public request and every pod acknowledgment.
- Bind acknowledgment to task agent and exact current attempt.
- Never log cancel reasons, prompts, transcripts, tmux contents, or external
  side effects. Log safe IDs, actor reference, state transition, adapter mode,
  attempts, latency, and outcome.
- Metrics cover requests by outcome, queued immediate cancels, canceling age,
  delivery retries, exact interrupts, acknowledgments, unconfirmed timeouts,
  and terminal races. Task IDs are trace/log fields, not metric labels.
- Audit records distinguish request accepted, notification delivered,
  interrupt requested, interrupt acknowledged, agent acknowledged, completion
  won, and timeout. They must never collapse those events into one "canceled"
  log line.

## 11. Failure matrix

| Cut point or race | Required result |
| --- | --- |
| Cancel before dispatch claim | Atomic `canceled`; no prompt delivery |
| Cancel races dispatch lease before ambiguity | Row lock decides; either no dispatch or durable `canceling` |
| Cancel after task receipt | `canceling`; capability-gated adapter action |
| Duplicate cancel | Canonical idempotent response; one delivery record |
| Completion before cancel lock | Completed wins; `applied: false` |
| Completion after `canceling`, before ack | Valid completion may win honestly |
| Ack before completion | Canceled wins; later completion conflicts |
| Wrong/stale attempt ack | Reject; remain `canceling` |
| TUI foreground marker absent/mismatch | Send no key or prompt |
| TUI marker matches | Production still sends no key; prototype evidence only |
| Native interrupt response lost | Query exact turn; never guess |
| Background process survives interrupt | Do not claim process rollback; surface limitation |
| Control plane/pod restarts | Resume durable delivery; no implicit canceled |
| Cancel deadline expires without proof | `failed/cancel_unconfirmed` |

## 12. A2A projection

The later A2A edge maps native cancel to Cancel Task. Queued immediate or
acknowledged cancellation maps to A2A `canceled`. While native state is
`canceling`, the adapter returns the current task/status honestly; it must not
invent an A2A canceled terminal state. `cancel_unconfirmed` maps to failed with
a stable safe error description.

The A2A binding cannot strengthen the runtime guarantee. Agent Card capability
publication must state whether cancellation is supported at all, while Kyber's
native diagnostics may separately expose `notify_only` versus
`exact_interrupt` for operators.

## 13. Rollout, tests, and estimate

1. Add schema, queued cancellation, idempotency, API, audit, and race tests.
2. Add `canceling`, timeout reconciliation, and loopback get/ack tools.
3. Ship both TUI adapters as `notify_only`; validate no generic keys or process
   signals are emitted.
4. Prototype Codex app-server adoption separately. Enable `exact_interrupt`
   only after exact-turn, response-loss, background-process, restart, and
   completion-race tests pass.
5. Evaluate a structured Claude adapter separately; do not infer parity from
   interactive Escape behavior.

Required tests include repository transition/property tests, API
authorization/idempotency, multi-replica leases, every failure-matrix row,
managed-hook coverage, purpose-built live agents, and assertions that
unrelated terminal/channel/scheduled turns are never interrupted.

Estimated implementation is three to five engineer weeks after MAT-19/MAT-20:
approximately two weeks for durable native cancellation and notify/ack,
one week for failure/security/runtime testing, and one to two weeks for the
first exact native adapter if pursued. Codex app-server migration beyond the
cancellation surface may increase that estimate.

## 14. Recommendation

Approve the native cancellation state machine and ship current TUI adapters as
`notify_only`. The guarded-interrupt prototype did its job by falsifying the
unsafe shortcut: exact task correlation alone does not make terminal keystrokes
a reliable cancel API. Preserve the capability seam so native Codex and Claude
turn-control adapters can later provide exact interruption without changing the
public contract.
