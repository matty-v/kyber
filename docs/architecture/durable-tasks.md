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
