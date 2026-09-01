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
