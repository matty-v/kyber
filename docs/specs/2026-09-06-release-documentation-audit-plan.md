# Release documentation and dead-code audit

## Authorization and baseline

Matt requested a comprehensive code-grounded documentation audit, removal of
confirmed dead code, and release preparation on 2026-09-06 (Telegram 980).
Baseline: origin/main b8c47b9; latest release v1.4.1. Work branch:
`sol/release-docs-audit`, worktree `/home/kyber/dev/kyber-release-audit`.

## Checkpoints

- [x] Fetch main, isolate worktree, inspect repository and release instructions.
- [x] Inventory current docs and compare product/architecture/operator guidance
  with API, controllers, runtimes, UI, chart, scripts, and CI.
- [x] Correct stale claims and links; preserve dated design history as history.
- [x] Remove proven unused code; record evidence and verify affected behavior.
- [x] Run documentation, release, build, and affected code verification; review diff.
- [x] Push consolidated cleanup PR and check CI.
- [x] Prepare version recommendation, release notes, and precise release gates.
- [ ] Follow prepare-release workflow after required release approval, then verify
  image/chart publication and report operator-controlled rollout status.

## Initial findings / next action

Release runbook describes removed push-deploy jobs, eight images instead of
nine, and a functioning test-tag dry run that dependency skipping prevents.
The actual release chart stamping loop omits the new Slack image, despite
building that image. Verify chart wiring and add it to the existing release
invariant coverage. Audit recent features: avatars, Slack, navigation, and
harness contract. No release dispatch has occurred.

Release preparation must leave a concrete version/notes/checks proposal before
requesting the operator's pre-tag approval described by the release workflow.

## Cleanup checkpoint

Evidence: [audit record](2026-09-06-release-documentation-audit-evidence.md).
Release notes drafted at `docs/releases/v1.5.0.md`: minor recommendation for
Slack, avatars, navigation, and harness integration since v1.4.1. Focused chart
and skills tests, 81 product-doc checks, and 15 release guards passed. Full Go
build/vet/test pass is running. Next: final consistency review, record final
checks, push cleanup PR and inspect CI before the concrete pre-tag approval.

PR: https://github.com/matty-v/kyber/pull/232. Final source review confirmed
avatars are API-only; notes and docs explicitly exclude a console upload control.

## Approved release checkpoint — 2026-09-07

Cleanup merged as PR #232 / 10fb298. Local full Go build, focused chart/skills,
Helm rendering/lint, product docs (81), release guards (15), and PWA tag tests
(16) passed. Full Go lint/test, integration, A2A and analyses passed in CI at
99ae991. The node-agent image was confirmed published at that SHA while its
non-required build job finished after publication. Duplicate local vet/full-suite
execution was stopped after CI covered those checks.

Matt approved v1.5.0 on Telegram message 992. Standard prepare-release run
34074770319 opened PR #233; final approved notes/changelog are included in that
preparation commit. Next: verify its merge/tag, release images/chart and final
GitHub Release notes. No live cluster upgrade is authorized by this release cut.
