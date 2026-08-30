# MAT-19 durable agent tasks — design plan

**Status:** In progress
**Date:** 2026-08-30
**Tracker:** [MAT-19](https://linear.app/matty-v/issue/MAT-19/designplatform-durable-externally-addressable-agent-tasks)
**Parent spike:** [MAT-6](https://linear.app/matty-v/issue/MAT-6)
**Parent PR:** [#183](https://github.com/matty-v/kyber/pull/183)
**Branch:** `docs/mat-19-durable-agent-tasks`

## Goal

Design a durable, externally addressable Kyber task envelope for long-running
agent work. Preserve Claude Code and Codex as the execution engines and reuse
Kyber's existing sidecar-authenticated request handoff wherever it remains
sound. Add only the platform guarantees that cannot live in a harness session:
stable task identity, durable lifecycle state, restart-safe dispatch intent,
retention, Get, and List.

## Scope guardrails

- Begin with a capability audit of both supported harness adapters and the
  existing MAT-9 request/reply path. Custom runtime machinery requires a gap in
  both harnesses.
- PostgreSQL is the proposed task source of truth. Redis may carry dispatch
  leases, wakeups, or fanout but may not own durable task state.
- A persisted `dispatched` state means Kyber handed work to a runtime boundary;
  it does not prove that a model or tool is still executing after a crash.
- Do not design progress/artifacts (MAT-20), cancellation (MAT-21), continuation
  (MAT-22), principal authorization (MAT-23), live events (MAT-25), or A2A wire
  types (MAT-26), except to reserve stable extension seams.
- This task produces design and review artifacts only, not implementation.

## Investigation

- [x] Trace request creation, validation, dispatch, completion, expiry, and
  failure across the control plane, Redis store, pod exec/queue boundary,
  status sidecar, and both runtime adapters.
- [x] Audit Claude Code and Codex primitives for session identity, queued input,
  turn completion, resume/restart behavior, and any stable correlation fields.
- [x] Trace existing PostgreSQL ownership, migration, backup, availability, and
  multi-replica patterns that a task repository should reuse.
- [x] Define the minimal task aggregate, lifecycle, atomic transitions,
  idempotency rules, pagination contract, retention classes, and cleanup model.
- [x] Define dispatch leasing, retry/reconciliation, pod/control-plane restart,
  runtime-unavailable, and ambiguous-delivery semantics without promising
  exactly-once execution.
- [x] Define native create/get/list API shapes and the internal runtime envelope,
  leaving explicit seams for MAT-20–23 and MAT-25.
- [x] Threat-model unbounded prompts, task enumeration, retention growth,
  duplicate execution, stale dispatch, and transcript leakage.
- [x] Produce implementation slices, migration/compatibility strategy,
  observability, rollout/rollback, test matrix, and revised estimates.

## Deliverable

- [x] Write `docs/design/2026-08-30-durable-agent-tasks.md` with current-state
  evidence, decisions, alternatives, schemas/state machine, failure semantics,
  API boundaries, security, operations, rollout, and open questions.
- [ ] Verify material code claims with file references and distinguish observed
  harness behavior from assumptions requiring implementation prototypes.
- [ ] Run documentation and repository checks appropriate to the changed files.
- [ ] Commit and push the focused branch, open a stacked PR based on #183 while
  it remains unmerged, and link the result from MAT-19.

## Progress log

- 2026-08-30: Matt selected MAT-19 as the first design after completing the
  MAT-6 gap review. Created the separate stacked branch and confirmed the
  existing request store is intentionally ephemeral: a 60-second default,
  five-minute hard lifetime, one Get operation, and terminal records bounded by
  count. The status sidecar exposes one loopback `respond` tool for an explicit
  request ID. These are foundations to adapt, not a durable task model.
- 2026-08-30: Audited the shared tmux delivery and runtime adapter boundary.
  Neither current adapter exposes a stable native turn receipt or task query to
  the control plane. Drafted the PostgreSQL task model, durable dispatch lease,
  conservative ambiguous-delivery behavior, native APIs, limits, rollout, and
  test strategy. Five load-bearing review questions remain before approval.
