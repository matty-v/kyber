# MAT-26 design plan: thin A2A HTTP+JSON edge adapter

**Status:** Complete; awaiting design decision
**Issue:** [MAT-26](https://linear.app/matty-v/issue/MAT-26/designplatform-thin-a2a-httpjson-edge-adapter)

## Decisions

- [x] Bundle G10 Agent Card projection with the G11 adapter.
- [x] Pin A2A 1.0.0 and one HTTP+JSON/REST binding.
- [x] Use official Go SDK v2.4.0 only for bounded normative edge mechanics.
- [x] Keep MAT-19–25 as the only task/event/auth/capability sources of truth.
- [x] Advertise Bearer authentication and streaming only; omit deferred G8/G9,
  JSON-RPC, gRPC, extensions, and extended cards.

## Investigation

- [x] Verify official stable specification, HTTP+JSON binding requirements,
  version negotiation, stream snapshot rule, and Go SDK REST support.
- [x] Define SDK boundary, trusted routing, tenant/agent resolution, security,
  version/content negotiation, limits, and threat model.
- [x] Map Agent Card, operations, IDs/context/idempotency, states, messages,
  Parts, history, Artifacts, events, pagination, and errors.
- [x] Define unsupported behavior, observability/audit, upgrade/rollback,
  rollout, MAT-27 handoff, tests, and estimate.

## Deliverable

- [x] Check in the MAT-26 design referencing MAT-6, PR #183, and MAT-19–25.
- [x] Run repository checks.
- [x] Commit, push, and open stacked draft PR #197. Linear is updated
  separately because its comment is external state.

## Progress log

- 2026-08-30: Matt accepted MAT-25. Previously deferred G8/G9 are skipped and
  the approved bundled G10+G11 design begins.
- 2026-08-30: Matt selected the bounded official SDK edge. SDK task stores,
  queues, executors, push, authorization, and capability generation are
  explicitly excluded.
