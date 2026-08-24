# Dependabot PR triage — 2026-08-24

**Status:** Complete

## Goal

Assess every open Dependabot PR for compatibility, operational risk, and merge
readiness; repair its own branch when necessary; and obtain Matt's explicit
approval before merging anything.

## Checkpoints

- [x] Inventory all open Dependabot PRs, branch state, changed files, and CI.
- [x] Review upstream compatibility and release notes for each update.
- [x] Reproduce failures and apply the smallest branch-local fixes needed.
- [x] Run targeted local verification and confirm required GitHub checks.
- [x] Report per-PR risk and recommendation to Matt; wait for yes/no approval.
- [x] Merge the fourteen approved PRs and verify the resulting main branch.
- [x] Coordinate Node 26 across development, CI, publishing, and the production
  PWA builder on #86; validate and request a separate merge approval.

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
- Matt approved and all fourteen recommended PRs (#126–#139) were merged.
- Main is green at `79a655e59c5a75755cc5ce2511373e3199b1168b` (test, build,
  CodeQL, and pwa-views auto-publish all succeeded).
- Matt chose to adopt Node 26 early. #86 is being expanded into a coordinated
  change: `.nvmrc`, the package engine contract, control-plane PWA builder, and
  holocron publishing job will agree on Node 26, with a CI drift guard and the
  Node 26 test-environment compatibility fix discovered during validation.
- #86 is cleanly mergeable at `82b488497f425641886a872fe8d212f4ef1d3430`.
  Its PWA build, full Go suite, integration test, production control-plane image
  build, CodeQL, design lint, and GitGuardian checks all pass.
- Matt approved and #86 merged as `ce24359c111b97d7b7fabe32d24571339a668e74`;
  its post-merge build, test, and CodeQL workflows all passed on main.
- The coordinated Holocron follow-up merged as matty-v/holocron#103 at
  `0634e966422e32f8370071f3ebe152d03e47a34b`; its post-merge Node 26
  install, type-check, tests, build, and Firebase production deploy passed.

## Safety

- Do not merge without explicit approval.
- Keep fixes isolated to the affected Dependabot branch.
- Treat major runtime/toolchain updates as higher risk even when CI is green.
