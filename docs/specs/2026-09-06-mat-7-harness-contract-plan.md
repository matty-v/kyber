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
- [ ] Publish normative requirements, evidence matrix, and onboarding checklist.
- [ ] Establish reusable adapter conformance tests, negative fixtures, and a
  minimal fake integration; map behavioral coverage to existing tests and
  identify uncovered requirements explicitly.
- [ ] Run appropriate checks and record exact results/limits. Live-harness
  verification requires the dedicated dev environment and disposable agents.
- [ ] Review registration, authentication, dispatch, lifecycle, sidecars,
  API/UI and packaging against the contract; produce a prioritized coupling
  map and concrete refactoring proposal.
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

Go is absent from the current PATH. Next: install the pinned Go toolchain in
user-owned tooling, draft the contract, implement the first conformance suite,
and verify it. Preserve the unrelated scripts/__pycache__/ directory.

This branch is a docs/tests checkpoint; no deployable behavior is changed.
Commit and push each verified checkpoint. Stop for the architecture decision,
not after routine intermediate checkpoints.
