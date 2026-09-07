# Release documentation and dead-code audit

## Authorization and baseline

Matt requested a comprehensive code-grounded documentation audit, removal of
confirmed dead code, and release preparation on 2026-09-06 (Telegram 980).
Baseline: origin/main b8c47b9; latest release v1.4.1. Work branch:
`sol/release-docs-audit`, worktree `/home/kyber/dev/kyber-release-audit`.

## Checkpoints

- [x] Fetch main, isolate worktree, inspect repository and release instructions.
- [ ] Inventory current docs and compare product/architecture/operator guidance
  with API, controllers, runtimes, UI, chart, scripts, and CI.
- [ ] Correct stale claims and links; preserve dated design history as history.
- [ ] Remove proven unused code; record evidence and verify affected behavior.
- [ ] Run documentation, release, build, and affected code verification; review diff.
- [ ] Push consolidated cleanup PR and check CI.
- [ ] Prepare version recommendation, release notes, and precise release gates.
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
