# MAT-7 — agent harness contract execution plan

Status: approved direction; contract and test baseline in progress.
Approval: Matt, Telegram message 936, 2026-09-06.
Issue: https://linear.app/matty-v/issue/MAT-7
Baseline: 20b21f987402204b33944af9135db53d4cbf494b.

## Outcome and boundaries

Publish the Kyber-specific v1 interactive harness contract. Preserve both
subscription and API-key authentication for Claude Code and Codex. Establish
contract tests before reviewing and refactoring the integration architecture.
V1 uses the existing packaged tmux agent model. Capabilities are explicit and
feature-specific; source inspection is not live certification. No third harness,
new execution transport, or production deployment is authorized by this plan.

## Checkpoints

- [x] Direction and corrected source-grounded scope approved by Matt.
- [x] Draft normative requirements, evidence matrix, and onboarding checklist.
  Final v1 publication remains gated on migration and live evidence.
- [x] Establish reusable adapter conformance tests, negative fixtures, and a
  minimal registry adapter fixture; map behavioral coverage and gaps.
  A fully bootable fake harness remains a migration acceptance requirement.
- [ ] Run appropriate checks and record exact results/limits. Live-harness
  verification requires the dedicated dev environment and disposable agents.
- [x] Review registration, authentication, dispatch, lifecycle, sidecars,
  API/UI and packaging; produce the [refactoring proposal](../design/2026-09-06-mat-7-harness-extensibility-review.md).
- [ ] Obtain approval for material architectural decisions, then implement
  capability discovery/enforcement and adapter migrations with tests.
- [ ] Publish final conformance evidence and maintenance/release guidance.

## Current evidence and next action

The baseline uses runtime adapters plus out-of-interface scripts and
runtime-specific API auth branches. Claude readiness is process presence;
Codex readiness is key presence/local login status. Both support subscription
and API-key modes. Cancellation is notify-only. Credential write-back has
failure windows; Claude bootstrap has a direct self-scoped pod-token path.
Telegram/API-key is rejected by current platform validation. These are explicit
baseline facts and review targets, not promises to silently change behavior.

Go 1.26.0 was installed in user-owned tooling and verified against the official
archive checksum. Envtest 1.31.0 binaries were installed for repository checks.

Verified so far:

- `go test ./pkg/runtimes/... ./pkg/tokenreport ./pkg/taskdispatch ./images/agent-base` passes.
- New shared adapter checker covers both auth modes and two Agent names for
  both real adapters, plus a minimal registry fixture and negative cases.
- New receipt recovery suite executes the production hook for both runtime IDs
  across seven scenarios plus local conflict checks. It does not certify real
  CLI hook semantics.
- A broader integration-tagged Codex test exposed an outdated all-hooks-absent
  assertion. It now checks cron and receipt capabilities independently with
  controlled command paths. Rerun in progress.
- Full `go test -p 2 ./...` (the Makefile test target's command, with bounded
  compile parallelism) and integration boot suites are in progress. `make`
  itself is absent; no claim of a full green gate yet.

Next action: finish checks, record failures/limits, commit/push tests and docs,
and present the concrete architecture proposal for approval. No capability API,
CRD, auth policy or runtime behavior has changed. The unrelated
`scripts/__pycache__/` directory is preserved.

This branch is a docs/tests checkpoint; no deployable behavior is changed.
Final live certification follows the approved architectural migration; do not
roll dev/prod pods merely to validate source/test additions.
