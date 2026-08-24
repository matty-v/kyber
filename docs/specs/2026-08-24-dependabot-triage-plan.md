# Dependabot PR triage — 2026-08-24

**Status:** Awaiting merge approval

## Goal

Assess every open Dependabot PR for compatibility, operational risk, and merge
readiness; repair its own branch when necessary; and obtain Matt's explicit
approval before merging anything.

## Checkpoints

- [x] Inventory all open Dependabot PRs, branch state, changed files, and CI.
- [x] Review upstream compatibility and release notes for each update.
- [x] Reproduce failures and apply the smallest branch-local fixes needed.
- [x] Run targeted local verification and confirm required GitHub checks.
- [ ] Report per-PR risk and recommendation to Matt; wait for yes/no approval.
- [ ] Merge only the PRs Matt approves, then verify the resulting main branch.

## Current evidence

- Fifteen Dependabot PRs are open: #86 and #126–#139.
- All are currently reported mergeable by GitHub.
- #126–#134 and #86 had green required checks at inventory time.
- #135–#139 initially failed the pwa-views release guard before builds ran.
- #135 repairs the guard for devDependency-only updates and fixes the router
  boundary test for Vitest 4.1.11; local builds, type checks, and 721 tests pass.
- #136 lowers the jsdom worker pool from four to two after jsdom 30 caused four
  deterministic timeouts; local builds, type checks, and 721 tests pass.
- #137 builds and type-checks cleanly with the Node 26 type definitions.
- #138 builds, type-checks, and passes all 721 tests with jest-dom 7.
- #139 now carries the required pwa-views 0.32.2 version and changelog entry;
  local builds, type checks, and 721 tests pass after hardening the slow router
  boundary test.
- All fifteen PRs are mergeable and all required GitHub checks are green.
- The exact next action is Matt's yes/no merge decision; #86 is recommended
  against because it moves the production frontend image from the repository's
  supported Node 22 line directly to Node 26.

## Safety

- Do not merge without explicit approval.
- Keep fixes isolated to the affected Dependabot branch.
- Treat major runtime/toolchain updates as higher risk even when CI is green.
