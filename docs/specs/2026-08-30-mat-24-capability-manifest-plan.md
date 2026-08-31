# MAT-24 design plan: curated public agent capability manifest

**Status:** Complete; awaiting design decision
**Issue:** [MAT-24](https://linear.app/matty-v/issue/MAT-24/designplatform-curated-public-agent-capability-manifest)

## Decisions

- [x] Pursue a Kyber-native, versioned public capability manifest.
- [x] Publish only explicit operator declarations; never auto-promote observed
  skills, tools, prompts, or model claims.
- [x] Store the stable contract in Agent spec and validation/availability/drift
  in Agent status.
- [x] Use existing skill/runtime reports only as bounded private evidence.
- [x] Keep G10's A2A Agent Card as a deterministic later projection.

## Investigation

- [x] Audit Agent spec/status, skill scanning/reporting/store/API, and the
  Claude Code/Codex runtime boundary.
- [x] Define native identity, capability ID/version, media mode, task feature,
  and private evidence schemas.
- [x] Define admission validation, reconciliation, availability, drift,
  caching, compatibility, operator UX, and failure behavior.
- [x] Define privacy, authorization, audit, rollout, rollback, tests,
  observability, A2A projection seam, and estimate.

## Deliverable

- [x] Check in the MAT-24 design referencing MAT-6, PR #183, and MAT-19–23.
- [x] Run repository checks.
- [x] Commit, push, and open stacked draft PR #195. Linear is updated
  separately because its comment is external state.

## Progress log

- 2026-08-30: Matt accepted MAT-23 and advanced to MAT-24 / G6.
- 2026-08-30: Matt accepted the recommendation that v1 uses explicit operator
  declarations only. Runtime observations may validate but never publish.
