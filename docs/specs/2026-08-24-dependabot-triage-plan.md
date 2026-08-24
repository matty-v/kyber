# Dependabot PR triage — 2026-08-24

**Status:** In progress

## Goal

Assess every open Dependabot PR for compatibility, operational risk, and merge
readiness; repair its own branch when necessary; and obtain Matt's explicit
approval before merging anything.

## Checkpoints

- [x] Inventory all open Dependabot PRs, branch state, changed files, and CI.
- [ ] Review upstream compatibility and release notes for each update.
- [ ] Reproduce failures and apply the smallest branch-local fixes needed.
- [ ] Run targeted local verification and confirm required GitHub checks.
- [ ] Report per-PR risk and recommendation to Matt; wait for yes/no approval.
- [ ] Merge only the PRs Matt approves, then verify the resulting main branch.

## Current evidence

- Fifteen Dependabot PRs are open: #86 and #126–#139.
- All are currently reported mergeable by GitHub.
- #126–#134 and #86 have green required checks.
- #135–#139 fail `pwa-build`; investigation is the next action.

## Safety

- Do not merge without explicit approval.
- Keep fixes isolated to the affected Dependabot branch.
- Treat major runtime/toolchain updates as higher risk even when CI is green.
