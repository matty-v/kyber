# MAT-20 design plan: task progress and typed multimodal results

**Status:** Complete; awaiting design decision
**Issue:** [MAT-20](https://linear.app/matty-v/issue/MAT-20/designplatform-task-scoped-progress-and-typed-multimodal-results)

## Decisions

- [x] Pursue G2 as a standalone Kyber capability.
- [x] Keep Claude Code and Codex as the content-producing engines.
- [x] Include controlled object-backed files in v1 alongside text and JSON.
- [x] Prohibit arbitrary remote URLs, raw filesystem references, and inline
  binary blobs in the public contract.
- [x] Keep progress cooperative and queryable; streaming belongs to MAT-25.

## Investigation

- [x] Trace the existing bounded text response and loopback MCP boundary.
- [x] Identify reusable `/persist` path-validation and object-store patterns.
- [x] Define progress semantics without treating it as proof of side effects.
- [x] Define result/part schemas, idempotency, persistence, and read shapes.
- [x] Define safe file ingestion, object transaction, download authorization,
  retention, scanning posture, and cleanup.
- [x] Define limits, failure matrix, migration, A2A projection, rollout, tests,
  and revised estimate.

## Deliverable

- [x] Check in the MAT-20 design with references to MAT-6, PR #183, and MAT-19.
- [x] Run repository documentation checks.
- [x] Commit, push, and open stacked PR #185. Linear is updated separately
  because its comment is external state.

## Progress log

- 2026-08-30: Matt accepted MAT-19 and selected MAT-20 as the next A2A gap.
- 2026-08-30: Matt explicitly included controlled object-backed file results
  in v1. Drafted the native progress/result model around a thin harness
  adapter: agents produce content, while Kyber validates, stores, authorizes,
  and retains it.
