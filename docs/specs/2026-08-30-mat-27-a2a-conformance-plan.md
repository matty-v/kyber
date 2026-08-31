# MAT-27 design plan: evidence-backed A2A conformance gate

**Status:** Complete; awaiting design decision
**Issue:** [MAT-27](https://linear.app/matty-v/issue/MAT-27/designrelease-evidence-backed-a2a-conformance-gate)

## Decisions

- [x] Require a pinned normative applicability ledger and layered evidence.
- [x] Block adapter-affecting PRs on the deployed official HTTP+JSON TCK and
  required Kyber supplemental suites.
- [x] Require all applicable MUSTs; review and expire every SHOULD deviation.
- [x] Pin exact spec, SDK, TCK, independent client, and Kyber/image inputs.
- [x] Publish “tested/self-attested conformance,” never “certified.”

## Investigation

- [x] Verify official TCK transports, RFC 2119 levels, Agent Card discovery,
  report formats, and evolving untagged status.
- [x] Define applicability ledger, change detection, merge/release gates,
  translator/integration/TCK/security/lifecycle/client test layers.
- [x] Define hermetic deployment, secrets, cleanup, timeouts, flake/retry/
  quarantine policy, and upstream-TCK defect handling.
- [x] Define immutable evidence bundle, generated support matrix, claim language,
  release/withdrawal flow, upgrade policy, ownership, observability, and cost.

## Deliverable

- [x] Check in the MAT-27 design referencing MAT-6, PR #183, and MAT-26.
- [x] Run repository checks.
- [x] Commit, push, and open stacked draft PR #198. Linear is updated
  separately because its comment is external state.

## Progress log

- 2026-08-30: Matt accepted MAT-26 and advanced to MAT-27 / G12.
- 2026-08-30: Matt selected the stronger merge gate. Adapter-affecting PRs must
  pass the deployed TCK rather than waiting for release validation.
