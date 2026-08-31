# MAT-23 design plan: principal-scoped task authorization

**Status:** Complete; awaiting design decision
**Issue:** [MAT-23](https://linear.app/matty-v/issue/MAT-23/designplatform-principal-scoped-task-authorization)

## Decisions

- [x] Give authenticated callers stable principal and tenant IDs.
- [x] Make task ownership owner-only in v1; defer sharing and delegation.
- [x] Require exact action scope, task ownership, tenant, and agent-resource
  permission on every public task operation.
- [x] Keep internal pod mutations on assigned-agent/current-attempt authority.
- [x] Require explicit, separately scoped and audited administrator override.
- [x] Preserve principal identity across credential rotation and enforce
  revocation across API replicas and browser sessions.

## Investigation

- [x] Trace the current Caller, API-key, scope, authenticator, and browser
  session model.
- [x] Define principal envelope, scope vocabulary, resource constraints, and
  route authorization matrix.
- [x] Define repository-level filtering, idempotency ownership, pagination, and
  non-enumerating errors.
- [x] Define result download, event replay, interaction, cancellation, and
  internal sidecar boundaries.
- [x] Define key rotation, revocation, admin override, channel identity, audit,
  migration, rollout, adversarial tests, and estimate.
- [x] Explain the A2A projection while retaining Kyber's thin-adapter boundary.

## Deliverable

- [x] Check in the MAT-23 design referencing MAT-6, PR #183, and MAT-19–22.
- [x] Run repository checks.
- [x] Commit, push, and open stacked draft PR #188. Linear is updated
  separately because its comment is external state.

## Progress log

- 2026-08-30: Matt accepted MAT-22 and selected principal-scoped task
  authorization as the next gap.
- 2026-08-30: Matt selected owner-only v1. Same-tenant sharing and delegation
  remain deferred; explicit audited administrator override is retained.
