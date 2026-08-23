# Agent startup prompt

**Status:** Implemented — live dev acceptance complete
**Owner:** sol  
**Branch:** `sol/agent-startup-prompt`  
**Last updated:** 2026-08-23 UTC

## Goal

Let an operator configure an optional prompt for an Agent through the Kyber
API and PWA. Every supported runtime receives that prompt as its initial user
turn when a new harness session starts. Agents without a configured prompt
continue to start exactly as they do today.

## Proposed behavior

- Add optional `Agent.spec.startupPrompt` with a 32 KiB UTF-8 limit.
- Accept `startupPrompt` during agent creation.
- Return `startupPrompt` from agent list/detail responses.
- Allow `PATCH /api/v1/agents/{name}` to set, replace, or clear it. A JSON
  string sets/replaces it; an empty string clears it; omission leaves it
  unchanged.
- Editing a running Agent does not inject text into its live session. The spec
  generation advances, so the existing dirty/restart-required UI tells the
  operator that a restart is needed.
- Inject the value into the runtime container as `KYBER_STARTUP_PROMPT` from
  the runtime-neutral pod builder.
- Claude Code and Codex append the prompt as one safely quoted positional CLI
  argument. Newlines, quotes, shell metacharacters, and leading dashes must be
  preserved as data and must never be interpreted by a shell.
- Empty or absent prompts preserve the current launch command byte-for-byte.
- Apply the configured prompt to every new harness
  session, including a pod boot, an explicit in-pod session reset, or the
  guarded relaunch after an unexpected harness exit. Matt approved these
  semantics on 2026-08-23.

## Non-goals

- Injecting a newly edited prompt into an already-running harness session.
- Treating the prompt as a secret. It is visible in the Agent CR and API to
  authorized operators.
- Fleet-wide default startup prompts.
- Prompt templating, variables, attachments, or per-runtime prompt variants.
- Supporting arbitrary unregistered runtimes without a runtime-image change;
  the contract is runtime-neutral, but each runtime launcher must consume it.

## API and storage contract

The Agent CR is the source of truth:

```yaml
spec:
  startupPrompt: |-
    Review open work and continue from the highest-priority task.
```

Validation:

- Optional and empty by default for backward compatibility.
- Maximum length: 32,768 bytes/characters as enforced by the Kubernetes CRD
  schema and API handler. The implementation must choose one precise unit and
  use it consistently; Kubernetes `MaxLength` is character-oriented, so the
  API should match that behavior.
- No content filtering beyond the length limit. The value is user-authored
  agent input, not a shell fragment.

Wire-shape changes must stay synchronized across:

1. `pkg/api/routes_agents.go`
2. `test/contract/openapi.yaml`
3. `packages/pwa-views/src/lib/types.ts`

## UI contract

- The Create Agent wizard includes an optional multiline **Startup prompt**
  field with concise copy explaining that it becomes the first user turn on
  each new session and that it is not secret.
- The Review step shows either a compact preview or “none”; it must not dump a
  multi-page prompt into the review screen.
- Agent Detail shows the configured value and provides edit/clear controls.
- Saving a change uses the existing PATCH endpoint and then presents the
  existing dirty/restart-required state. The UI must not silently restart the
  agent.
- The field displays its 32 KiB limit and prevents over-limit submission while
  the server remains authoritative.

## Runtime delivery design

`BuildPodSpec` owns the common `KYBER_STARTUP_PROMPT` environment variable so
the Agent-to-pod contract does not fork by runtime. The Claude Code and Codex
start scripts consume the same variable and add it to their argument arrays.

The implementation must not concatenate the raw prompt into generated shell
source. For Claude Code, the current string-based `CLAUDE_ARGS` launch path
needs a safely shell-quoted command representation (or an equivalent argument
array serialized with `printf %q`) shared by initial launch and generated
relaunch script. Codex already builds its command from an array with
`printf %q`; extend that path and retain the single command definition.

Prompt delivery must be covered for:

- initial pod boot;
- `/restart-session` generated launch script;
- automatic harness relaunch;
- both empty and non-empty prompt values;
- multiline content containing single quotes, double quotes, `$()`, backticks,
  semicolons, and leading `-` characters.

## Checkpoints and restart protocol

Each checkpoint is independently resumable. At the end of every checkpoint:

1. update the status table below with the commit SHA and verification result;
2. commit the coherent change;
3. push `sol/agent-startup-prompt`;
4. send Matt a short Telegram update;
5. after any restart, fetch the branch, read this document, inspect `git
   status`, and resume at the first incomplete checkpoint. Never assume an
   unrecorded command completed.

| Checkpoint | Status | Exit criteria | Commit |
|---|---|---|---|
| 0. Plan and decision | Complete | Design committed; Matt approved every-session semantics and the CRD change; durable identity copy records the checkpoint. Kyber push remains blocked on missing repo credential. | `08dc0fe` + follow-up |
| 1. CRD and API contract | Complete | Generated CRD includes the limit; focused API tests passed. | `f00e142` |
| 2. Pod and runtime delivery | Complete | Common env injection and all three launch paths implemented; pod-builder and launcher prompt tests passed. | `7ecb41f` |
| 3. PWA create and edit surfaces | Complete | Wizard and Agent Detail implemented; 19 focused tests, TypeScript lint, and both builds passed; package bumped to 0.29.0. | `463010e` |
| 4. Documentation and focused verification | Complete | Product capability docs explain behavior; changed Go/package tests, generation checks, TypeScript lint, PWA tests, and builds pass. The broad Go suite passed except the controller package exceeded its global 10-minute envtest timeout; focused feature tests pass. | `9991de3` |
| 5. GCP dev deployment and operator test | Complete | Full rollout completed to `datawire-dev` / zonal `kyber-dev` with CRD, control plane, Claude Code, and Codex images tagged `worktree-20260823144052-9991de3`. Public health, rollout, runtime pins, and live CRD schema passed. Purpose-built live Claude Code and Codex agents both consumed exact marker prompts on initial launch and `restart-session`; test agents and auth state were cleaned up. | Dev rollout and acceptance 2026-08-23 |
| 6. Full verification and handoff | In progress | Branch is clean and pushed; PR #114 is open with test evidence and rollout notes. CI is running. Luna received the webhook but skipped because the current inbound binding envelope omits header metadata required by its review policy; Matt has the exact manual-review prompt and the platform contract gap is durably recorded. | PR #114 |

### Checkpoint 1 — CRD and API contract

Expected files:

- `pkg/api/v1/agent_types.go`
- `pkg/api/v1/zz_generated.deepcopy.go` (generated only)
- `deploy/helm/kyber/crds/kyber.io_agents.yaml` (generated only)
- `pkg/api/routes_agents.go`
- `pkg/api/routes_agents_test.go`
- `test/contract/openapi.yaml`
- relevant contract fixtures/tests if required

Focused verification:

```bash
make generate
go test ./pkg/api/... ./test/contract/...
git diff --check
```

### Checkpoint 2 — pod and runtime delivery

Expected files:

- `pkg/controllers/agent/pod_builder.go`
- `pkg/controllers/agent/pod_builder_test.go`
- `images/claude-code/start-claude.sh`
- `images/codex/start-codex.sh`
- existing or new runtime boot-script tests under the corresponding image test
  surfaces

Focused verification:

```bash
go test ./pkg/controllers/agent/... ./pkg/runtimes/claudecode/... ./pkg/runtimes/codex/...
# Run the repository's existing Claude/Codex boot-script test targets located
# during implementation; record exact commands in the checkpoint table.
git diff --check
```

Security assertions:

- A prompt such as ``$(touch /tmp/pwned)`` remains literal and creates no file.
- Quotes/newlines survive as one CLI argument.
- Prompt content is not emitted into bootstrap logs.

### Checkpoint 3 — PWA surfaces

Expected files:

- `packages/pwa-views/src/lib/types.ts`
- `packages/pwa-views/src/pages/CreateAgent.tsx`
- `packages/pwa-views/src/components/wizard/types.ts`
- `packages/pwa-views/src/components/wizard/BasicsSection.tsx` or a dedicated
  prompt section
- `packages/pwa-views/src/components/wizard/ReviewSection.tsx`
- `packages/pwa-views/src/pages/AgentDetail.tsx`
- relevant hooks/tests
- `packages/pwa-views/package.json`
- `packages/pwa-views/CHANGELOG.md`

Focused verification:

```bash
npm run lint --workspace=packages/pwa-views
npm run test --workspace=packages/pwa-views
npm run build --workspace=packages/pwa-views
npm run lint --workspace=apps/embedded-pwa
npm run test --workspace=apps/embedded-pwa
npm run build --workspace=apps/embedded-pwa
```

### Checkpoint 4 — docs and integration review

Update `docs/product/capabilities/agents-and-persistence.md` and, if the CLI
behavior needs runtime-specific explanation, `docs/product/capabilities/runtimes.md`.
Review API, CRD, PWA, and launch commands side by side for naming and semantics.

### Checkpoint 5 — GCP dev deployment and operator test

Use the `deploy-kyber-dev-gcp` workflow's guarded `--full` mode, which builds
the control plane plus Claude Code and Codex runtimes, applies the Agent CRD,
and updates the dev runtime pins. Never target the regional cluster where sol
runs. The 2026-08-23 rollout published tag
`worktree-20260823144052-9991de3`; public health recovered, rollout completed,
runtime pins matched, and the live v1 CRD reported `startupPrompt` as a string
with `maxLength: 32768`. Matt's create/edit/restart acceptance test remains.

### Checkpoint 6 — final gates and delivery

Run the repository-required gates from `AGENTS.md` in order:

```bash
make build
make lint
make test
make generate
npm run build --workspace=packages/pwa-views
npm run build --workspace=apps/embedded-pwa
npm run lint --workspace=packages/pwa-views
npm run lint --workspace=apps/embedded-pwa
npm run test --workspace=packages/pwa-views
npm run test --workspace=apps/embedded-pwa
git diff --check
git status --short --branch
```

Open one consolidated PR. The PR must call out that rollout updates the Agent
CRD and both runtime images, that existing Agents remain compatible, and that
a prompt edit requires an Agent restart.

## Risks and mitigations

- **Shell injection or argument splitting:** build commands from arrays and
  shell-quote once; test hostile content against both launchers.
- **Repeated work after a crash:** make the chosen every-session behavior
  explicit in UI/docs. If Matt chooses first-boot-only, stop and redesign with
  durable acknowledgement before implementation.
- **Oversized pod environment:** cap the field at 32 KiB, well below the Agent
  object and process environment limits.
- **Accidental live interruption:** PATCH only changes spec and uses the
  existing dirty/restart-required workflow.
- **Cross-runtime drift:** keep one CRD/API/env name and test both runtime
  launch commands from the same behavioral fixture set where practical.
- **Holocron missing the UI:** bump the shared package version and changelog;
  include the normal publish/tag follow-up in the PR handoff.

## Decision log

- 2026-08-23: Proposed persistent `startupPrompt`, optional, max 32 KiB,
  editable through create/PATCH/UI, activated after restart.
- 2026-08-23: Matt approved delivery on every harness session start, including
  pod boot, explicit session reset, and guarded relaunch.
