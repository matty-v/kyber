# MAT-7 — agent harness contract execution plan

Status: draft contract/test baseline and architecture review complete; awaiting
approval of the material refactoring proposal. MAT-7 remains in progress.
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
- [x] Run appropriate checks and record exact results/limits. Live-harness
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

## Validation checkpoint

Code/tests commit: `d4a6b83442cd9afefb12580a5dc8b24b560022f8`.
Draft PR: https://github.com/matty-v/kyber/pull/231

- Local `go build -p 2 ./...` passed with Go 1.26.0.
- Local `go test ./pkg/runtimes/... ./pkg/tokenreport ./pkg/taskdispatch ./images/agent-base` passed.
- Local `go vet ./pkg/runtimes/... ./images/agent-base` passed.
- Local integration-tagged Codex suite passed after the cron/receipt fixture fix.
- Local Claude integration suite had one host-PATH absence-fixture failure;
  the corrected `TestStartClaude_SkillReport` group passed on rerun. Other
  cases completed in the original run; no complete local rerun is claimed.
- CI [test gate](https://github.com/matty-v/kyber/actions/runs/34060782526)
  passed, including full lint and repository tests with envtest and Helm.
- CI [integration](https://github.com/matty-v/kyber/actions/runs/34060782523)
  passed with its integration test step actually executed.
- CI [build workflow](https://github.com/matty-v/kyber/actions/runs/34060782520)
  agent-base integration and control-plane build jobs passed.
- The duplicate local full repository run was stopped after the full CI gate
  passed, to avoid continuing redundant tests. Its completed command/API and
  other package checks had passed; it is not recorded as a complete local pass.
- Documentation links resolve and `git diff --check` passes.

The shared checker covers both auth modes and two Agent names for both real
adapters, a minimal registry fixture and negative cases. Receipt recovery tests
execute the production hook for both runtime IDs across seven scenarios plus
local conflicts. No real CLI hook semantics, live auth or pod recovery were
certified by these fixtures.

Two existing tests were corrected: independent task receipts no longer fail
an all-cron-hooks-absent assertion, and the absent skill reporter fixture now
actually excludes the host binary. Production behavior is unchanged.

Next action: Matt reviews the incremental registry/descriptor/auth strategy
proposal in the linked architecture review. On approval, make its first schema
slice concrete, implement the agreed extension points and capability gates,
then prove a fully bootable fake harness and both real integrations in dev.
No capability API, CRD, auth policy or runtime behavior has changed yet. The
unrelated `scripts/__pycache__/` directory is preserved.

This branch is a docs/tests checkpoint; no deployable behavior is changed.
Final live certification follows the approved architectural migration; do not
roll dev/prod pods merely to validate source/test additions.
