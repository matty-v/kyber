# MAT-85: switch an agent's harness on its existing disk

## Contract

`POST /api/v1/agents/{name}/switch-runtime` accepts a registered target
`runtime`. The route requires lifecycle-write scope. It accepts stable
Running, Stopped, Failed, and NeedsAuth agents; transient phases are rejected.
The route validates the target image, current auth mode and all enabled
channels before changing anything. A same-runtime request is rejected.

For runtimes with a repair contract, the existing bounded same-node maintenance
runner prepares the target executable on the existing PVC. Runtimes without a
repair contract use their image's normal startup installation. A failed
preparation leaves the Agent spec and old pod untouched. Once preparation
succeeds, one optimistic-lock spec patch changes the runtime, clears the
runtime-scoped model and version, and requests NeedsAuth. The controller removes
the old pod and moves to NeedsAuth. Reauthorization (or an explicit retry with
an existing target credential) starts a fresh target session. The previous
credential Secret and transcript trees remain on the PVC.

The route does not migrate conversations across harnesses. The first target
pod must not resume the source harness's session. Scheduled jobs remain in the
Agent spec; the UI calls out job hooks that the target contract does not offer.
The target credential is created when its authorization flow completes:
Claude OAuth can now create a missing Secret, Codex device login already
creates its placeholder Secret, and the generic API-key flow creates or
updates the runtime-owned Secret for Claude Code, Codex, or Hermes.

## Checkpoints

- [x] API route: preflight, target preparation, one spec commit, audit event,
  runtime observation reset and scoped authorization.
- [x] Controller/internal reporter: reliable NeedsAuth transition and rejection
  of stale source-harness reports after the spec changes.
- [x] Agent Detail: target choice, preflight explanation, switch action, and
  appropriate target reauthorization controls.
- [ ] Contract, tests and docs: both directions, image/auth/channel failures,
  PVC preservation, jobs and skills semantics, and full repository gates.
- [ ] Canary: after code is reviewable and Matt names/approves the test Agent,
  verify old/new skills, local files, reauthentication and one working turn.

## Risks to verify

The target maintenance pod shares a node and PVC with the live source pod.
Target executable paths must remain separate. A source runtime-version report
arriving during the switch must not restore stale status. Codex device auth
currently needs a placeholder Secret; the switch must expose a path to start
that flow without creating a fake healthy agent.

## Checkpoint: 2026-09-22 22:02 UTC

The route, controller guard, public read projection, Agent Detail action,
OpenAPI shape, and operator/architecture docs are implemented. Focused API
tests cover both Claude/Codex directions, old Secret retention, image/auth/
channel failure, and failed preparation. The controller envtest confirms the
same PVC and source Secret remain through the NeedsAuth transition. The PWA
switch action test, API transport test, TypeScript lint, and PWA build pass.

Next: run the complete Go/PWA/embedded gates, fix failures, push the branch and
open the review PR. Then arrange canary validation after standard deployment;
do not deploy by hand or choose a real agent without Matt's direction.

## Checkpoint: 2026-09-22 22:15 UTC

Review found that the old Claude OAuth endpoint returned 404 when the target
Secret did not exist, and that `/auth` had no API-key branch. Both first-switch
authorization cases now create the target Secret, with focused tests and an
Agent Detail API-key control. Draft PR #272 is open; these follow-up changes
still need a checkpoint commit and CI.

## Checkpoint: 2026-09-22 22:29 UTC

Full PWA suite passed (88 files, 817 tests) with two workers; the default
parallel run timed out several otherwise passing UI tests under simultaneous
Go compilation. The first manual integration workflow passed on PR #272 head
4865a6f. Full Go tests found one legacy fixture with no `spec.runtime`; the
stale-report guard now applies only when that field is configured. Focused
runtime report, switch, OAuth, and API-key tests pass. The API-key handler also
rejects auth attempts outside NeedsAuth and re-reads the Agent before its
optimistic spec patch so recovery-gate status writes cannot cause a false
conflict. Full Go, embedded PWA, and final-head CI checks remain.

## Checkpoint: 2026-09-22 22:33 UTC

The focused switch, OAuth, and API-key authorization tests pass after the
recovery-gate and optimistic-lock fixes. PWA lint/build, embedded PWA lint/build
and tests, and the complete PWA test suite pass. The first manual integration
workflow passed; the manual test workflow exposed only the legacy empty-runtime
fixture, now fixed locally. The full local Go run is still completing. Next:
push these fixes, run both workflows on the final head, then request a named
canary agent for live acceptance after standard deployment.
