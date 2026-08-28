# Atomic harness install and repair — execution plan

**Status:** In progress
**Date:** 2026-08-28
**Tracker:** MAT-11

## Goal

Keep a working runtime harness available across interrupted upgrades, distinguish
an unusable harness from an authentication failure, and give operators one
runtime-neutral repair action that works even when the agent pod is absent.

## Current findings

- Codex and Claude Code both replace the live root-owned global npm package
  during boot. npm renames the working package before it finishes downloading
  the replacement, so ENOSPC, preemption, or termination can persist a partial
  install in rootfs mode.
- Codex compares a concrete installed version with the literal request
  `latest`, causing a fresh global install on every boot. Claude Code already
  avoids reinstalling when a concrete request matches the baked-in version,
  but still uses the same destructive npm operation for other requests.
- Codex performs `codex login status` without first proving that the harness can
  execute. A truncated or missing binary therefore reaches the credential
  failure path and is reported as `NeedsAuth`.
- `DiskExhausted` established a maintenance-pod lifecycle that keeps an agent's
  persistent root mounted without a running harness. Runtime repair should
  reuse that controller boundary instead of depending on exec into a live agent
  pod.

## Delivery rules

- Deliver three independently reviewable PRs in dependency order.
- Keep package installation policy in one shared, shell-testable helper used by
  both runtime images.
- Never remove or overwrite the last verified harness until its replacement is
  installed and verified.
- Keep runtime-specific paths, packages, and version parsing behind the runtime
  adapter contract; keep lifecycle and Kubernetes I/O in the controller.
- Update Go types, generated CRDs/OpenAPI, PWA wire types, changelog, and
  `pwa-views` version together for wire/UI changes.
- Validate destructive and interruption cases against disposable agents in
  `kyber-dev-gcp`; never test repair against a durable operator agent.

## PR A — atomic shared installer

**Branch:** `sol/mat-11-atomic-installer`

- [x] Characterize the global npm layouts and executable/version output for
  both Codex and Claude Code.
- [x] Add a shared installer that installs into a unique staging prefix,
  verifies the staged executable and parseable version, then swaps it into the
  live global package location with a rollback copy.
- [x] Make cleanup and rollback idempotent across signals and stale staging or
  backup directories; preserve the previous verified install on every failure.
- [x] Replace both runtime-specific in-place install blocks with the helper.
- [x] Resolve `latest` to a concrete registry version and skip installation
  when that version is already live.
- [x] Add shell/integration tests for successful install, failed verification,
  interrupted swap rollback, stale artifacts, and two consecutive `latest`
  boots.
- [x] Run focused image tests and repository-required gates.
- [x] Deploy to `kyber-dev-gcp`, interrupt an upgrade, and verify the prior
  harness still boots and runs.

## PR B — broken-runtime lifecycle and diagnosis

**Branch:** `sol/mat-11-broken-runtime` (from merged PR A)

- [x] Add a stable broken-runtime phase/condition and a bounded diagnostic
  payload naming the runtime and failed executable/version probe.
- [x] Probe harness usability before any authentication command and report a
  harness failure through the internal status path.
- [x] Add pure state-machine transitions and reconciler handling that cannot
  fall through to `NeedsAuth` or an unbounded restart loop.
- [x] Surface the real cause and recovery guidance in Agent Detail and suppress
  unusable authentication actions.
- [x] Update generated API/CRD contracts, PWA types, lifecycle docs, and tests.
- [x] Verify a deliberately truncated disposable harness reaches the new state
  with its actual failure reason.

Implementation started 2026-08-28 from merged PR A (`a09f166`). The wire
contract uses phase `BrokenRuntime`, condition `RuntimeUnusable`, and exit code
43 after a bounded executable/version probe that runs before authentication.

## PR C — runtime-neutral operator repair

**Branch:** `sol/mat-11-runtime-repair` (from merged PR B)

- [ ] Add the runtime repair contract to `runtimes.Adapter`, with Codex and
  Claude Code implementations describing package paths, cleanup, install, and
  verification.
- [ ] Add `POST /api/v1/agents/{name}/repair-runtime` under `lifecycle:write`
  with phase validation, conflict handling, and audit-safe errors.
- [ ] Reuse the maintenance-pod/PVC path to repair an agent with no running pod,
  then remove maintenance state and deliberately restart the agent.
- [ ] Add the Agent Detail `Repair runtime` action only for the broken-runtime
  state, with confirmation and progress/error feedback.
- [ ] Cover both adapters, absent-pod repair, failed repair retention, API
  authorization, and successful restart with focused Go/PWA tests.
- [ ] Document diagnosis and recovery in
  `docs/operator/wedged-agent-recovery.md`.
- [ ] Verify Codex and Claude Code repair end to end on disposable dev agents.

## Progress log

- 2026-08-28: Matt approved the three-PR plan. Updated from `origin/main` after
  MAT-10's `DiskExhausted` lifecycle merged as PR #164; PR A begins from commit
  `4ade768`.
- 2026-08-28: Added the shared staged installer, signal/SIGKILL rollback,
  legacy npm-staging cleanup, atomic global-bin repair, and concrete `latest`
  resolution for both runtimes. Focused shared, Codex, and Claude Code
  integration tests passed at the implementation checkpoint.
- 2026-08-28: Deployed immutable dev images
  `worktree-20260828153953-c45a55f`. A zero-grace kill immediately after the
  staging marker left Codex `0.148.0` executable while `0.149.0` was
  interrupted, and left Claude Code `2.1.250` executable while `2.1.249` was
  interrupted. Recovery/consecutive boots resolved `latest` and skipped the
  download at Codex `0.150.1` and Claude Code `2.1.250`. The existing Claude
  test agent returned to `Running`; disposable `sol-test-mat11-codex` was
  deleted. PR #168's required test, integration, build, and security checks all
  pass.
- 2026-08-28: Deployed PR B as immutable images
  `worktree-20260828183417-bcce284`. A disposable Codex image with a truncated
  executable exited 43 and reached `BrokenRuntime` with condition
  `RuntimeUnusable=True` and diagnostic `codex executable produced no version`.
  The pod UID and restart count stayed unchanged after the failure; the test
  agent was deleted and the normal Codex dev image restored.
