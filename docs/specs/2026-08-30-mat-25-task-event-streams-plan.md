# MAT-25 design plan: durable resumable task event streams

**Status:** Complete; awaiting design decision
**Issue:** [MAT-25](https://linear.app/matty-v/issue/MAT-25/designplatform-durable-resumable-task-event-streams)

## Decisions

- [x] Persist one ordered normalized event log per durable task.
- [x] Append task mutation and public event in the same PostgreSQL transaction.
- [x] Use notifications only as wakeups; PostgreSQL remains source of truth.
- [x] Ship a native resumable SSE endpoint only in v1; defer task WebSockets.
- [x] Exclude transcripts, reasoning, token deltas, raw tools, and harness event
  formats.

## Investigation

- [x] Audit the process-local WebSocket EventBus, Redis usage, task seams, and
  Claude Code/Codex event boundaries.
- [x] Define event identity, vocabulary, ordering, idempotency, atomic append,
  storage, and wakeups.
- [x] Define cursor, snapshot, replay/live handoff, expiry, terminal, retention,
  cleanup, heartbeat, backpressure, and capacity semantics.
- [x] Define MAT-23 authorization/revocation, failure behavior, rollout,
  A2A projection seam, tests, observability, and estimate.

## Deliverable

- [x] Check in the MAT-25 design referencing MAT-6, PR #183, and MAT-19–23.
- [x] Run repository checks.
- [ ] Commit, push, and open a stacked draft PR. Linear is updated separately
  because its comment is external state.

## Progress log

- 2026-08-30: Matt accepted MAT-24 and advanced to MAT-25 / G7.
- 2026-08-30: Matt selected SSE-only v1. PostgreSQL is authoritative and
  notification infrastructure is an optional latency optimization.
