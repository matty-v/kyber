# MAT-31 implementation plan: cooperative task cancellation

**Status:** In progress
**Issue:** [MAT-31](https://linear.app/matty-v/issue/MAT-31/a2a-39-implement-cooperative-task-cancellation)
**Design:** [Cooperative task cancellation](../design/2026-08-30-cooperative-task-cancellation.md)

## Scope guardrails

- Current Claude Code and Codex TUI adapters are `notify_only`; production code
  must not emit generic terminal keys or process signals.
- A queued task may become `canceled` atomically. A dispatched task becomes
  `canceling` and becomes `canceled` only after an acknowledgment bound to its
  exact current attempt.
- Completion or failure may honestly win a race with cancellation. Cancellation
  never promises rollback of external effects.
- Missing cancellation evidence expires as `failed/cancel_unconfirmed`.

## Checkpoints

- [x] Extend the task model, limits, persistence schema, response projection,
  and in-memory contract test double with cancellation metadata and states.
- [x] Add idempotent public cancellation with queued/dispatched row-lock race
  semantics and one durable notify-only delivery record.
- [x] Add exact-attempt loopback `get_control` and `ack_cancel` operations.
- [x] Add cancellation delivery/reconciliation, including restart-safe retries
  and `cancel_unconfirmed` deadlines.
- [x] Cover API, store, stale-attempt, repeat, terminal-race, deadline, and
  no-generic-interrupt behavior with tests.
- [x] Run focused and repository verification, deploy to `kyber-dev`, and
  exercise cancellation end to end with purpose-built test agents.
- [ ] Open the PR, resolve review and CI findings, and merge when green.

## Progress log

- 2026-09-01: Started from clean `main` after MAT-30. Reconfirmed the accepted
  notify-only safety boundary and created `feat/mat-31-cooperative-task-cancellation`.
- 2026-09-01: Implemented queued atomic cancellation, dispatched `canceling`,
  durable notify-only control delivery, exact-attempt acknowledgment, honest
  completion races, and `cancel_unconfirmed` reconciliation. Focused taskstore,
  API, dispatcher, and status-sidecar suites pass.
- 2026-09-01: Deployed the worktree to `kyber-dev`. Verified the six-state
  migration against the existing PostgreSQL schema and exercised queued cancel,
  closed dispatch, exact idempotent replay, and conflicting-key rejection over
  the public API. Added PostgreSQL restart, stale-attempt, and timeout coverage.
