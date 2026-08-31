# MAT-22 design plan: resumable multi-turn tasks

**Status:** Accepted
**Issue:** [MAT-22](https://linear.app/matty-v/issue/MAT-22/designplatform-resumable-multi-turn-agent-tasks)

## Decisions

- [x] Pursue one-outstanding-interaction multi-turn tasks.
- [x] Support typed clarification, choice, confirmation, JSON, and registered
  authorization requests.
- [x] Resume the same task with a fresh delivery attempt for every turn.
- [x] Require restart-safe replay from bounded task-visible context.
- [x] Never persist raw credentials, transcripts, or hidden reasoning.
- [x] Defer arbitrary chat participants, branching, and streaming.

## Investigation

- [x] Trace task, result, cancellation, receipt, sidecar, and harness seams.
- [x] Define task and interaction state machines and pause acknowledgment.
- [x] Define ordered immutable messages and typed interaction shapes.
- [x] Define same-session continuation and deterministic recovery capsules.
- [x] Define credential-safe registered authorization flow references.
- [x] Define concurrency, idempotency, stale reply, cancel/complete, expiry,
  restart, and context-cap behavior.
- [x] Define persistence, APIs, MCP, channel presentation, security, limits,
  retention, observability, A2A projection, rollout, tests, and estimate.

## Deliverable

- [x] Check in the MAT-22 design referencing MAT-6, PR #183, and MAT-19/20/21.
- [x] Run repository checks.
- [x] Commit, push, and open stacked PR #187. Linear is updated separately
  because its comment is external state.

## Progress log

- 2026-08-30: Matt accepted MAT-21 and selected MAT-22 as the next gap.
- 2026-08-30: Matt selected restart-safe replay. The draft keeps the normal
  continuation in the existing Claude Code/Codex session and uses a bounded,
  typed, task-visible recovery capsule only when that session is unavailable.
