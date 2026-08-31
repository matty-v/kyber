# MAT-21 design plan: cooperative task cancellation

**Status:** Complete; awaiting design decision
**Issue:** [MAT-21](https://linear.app/matty-v/issue/MAT-21/designplatform-cooperative-task-cancellation)

## Decisions

- [x] Pursue task-scoped cancellation independently of A2A.
- [x] Prototype guarded TUI interruption before choosing the adapter contract.
- [x] Cancel queued work immediately and persist dispatched work as
  `canceling`.
- [x] Require exact native terminal evidence or agent acknowledgment before
  `canceled`.
- [x] Disable generic TUI interruption in production; current TUI adapters are
  `notify_only`.
- [x] Never equate cancellation with rollback or whole-Agent termination.

## Investigation

- [x] Trace current expiry, dispatch, restart, and sidecar boundaries.
- [x] Verify official Claude Code and Codex interruption capabilities.
- [x] Build and locally test an exact foreground marker/lock prototype.
- [x] Exercise the prototype against purpose-built Claude Code and Codex agents
  in guarded `kyber-dev`.
- [x] Capture missing acknowledgment, surviving Codex background work, and
  Claude stale-editor redelivery risk.
- [x] Define state machine, API, persistence, concurrency, retries, timeouts,
  adapter capabilities, security, observability, A2A projection, rollout,
  tests, and estimate.

## Deliverable

- [x] Check in the design and disposable prototype assets.
- [x] Run repository checks.
- [ ] Commit, push, open a stacked PR, and update Linear.

## Progress log

- 2026-08-30: Matt accepted MAT-20 and selected MAT-21 as the next gap.
- 2026-08-30: Matt selected the guarded-interrupt prototype. Local marker race
  tests passed.
- 2026-08-30: Live Codex 0.151.0 interrupt stopped the foreground conversation
  but left a background terminal and no usable Stop acknowledgment. Live
  Claude Code 2.1.251 interrupt stopped an exact marked task, but left its
  prompt in the editor; a later paste appended and re-submitted the task
  header. Both test agents were deleted. The design therefore capability-gates
  active interruption and ships current TUIs as notify-only.
