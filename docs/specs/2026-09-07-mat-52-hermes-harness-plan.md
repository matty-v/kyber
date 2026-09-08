# MAT-52 — Hermes harness implementation plan

**Issue:** [MAT-52](https://linear.app/matty-v/issue/MAT-52/add-hermes-as-a-supported-kyber-agent-harness)
**Dependency:** [MAT-7](https://linear.app/matty-v/issue/MAT-7), merged in PR #231
**Status:** Slice A implemented; first live hardening checkpoint complete 2026-09-08

## Live hardening checkpoint — skill-report runtime contract

The first live Hermes agent reached `Running` on `kyber-dev`, cloned its
GitHub identity repo, and linked its skills into all three runtime homes. Its
boot-time skill report was nevertheless rejected with HTTP 400 because the
internal API's closed `linked` runtime allowlist still admitted only
`claude-code` and `codex`, while the scanner now correctly emits `hermes`.

Acceptance for this checkpoint:

- the internal skills endpoint accepts `hermes` as a linked runtime while
  continuing to reject unknown runtime identifiers;
- a focused API regression test exercises one skill linked into Claude Code,
  Codex, and Hermes;
- the relevant API and scanner tests pass; and
- after a dev control-plane reload, the live Hermes agent's report is stored
  and served by `GET /api/v1/agents/hermes/skills`.

Implementation checkpoint:

- extended the internal API allowlist with `skillscan.RuntimeHermes`;
- updated the store-and-serve regression fixture to require all three runtime
  identifiers;
- `go test ./pkg/api ./pkg/skillscan` passes; and
- `go test ./pkg/runtimes/hermes ./pkg/runtimes/... ./pkg/api/...` passes.

Live verification:

- commit `58ce671` was built and deployed to the dev control plane as
  `control-plane:worktree-20260908173454-58ce671`;
- the public health check recovered after the rollout;
- a temporary link from the live Hermes runtime home to the bundled
  `telegram-messaging` skill produced a report with `linked: ["hermes"]`;
- `GET /api/v1/agents/hermes/skills` returned HTTP 200 and served that exact
  runtime link, proving the previously rejected payload is now accepted; and
- the temporary link was removed and the agent was returned to its original
  state.

## Live operator feedback backlog

The first Hermes test also exposed four operator-facing parity gaps. Keep
these explicit rather than implying support through generic runtime UI:

- Kyber cannot switch the model used by an existing Hermes agent.
- Kyber does not show the Hermes agent's active provider/model.
- Kyber does not show Hermes context-window usage or remaining context budget.
- Kyber cannot browse or select available Hermes harness versions.

The first two gaps belong with provider/model discovery in Slice C. Context
budget visibility requires native, bounded Hermes telemetry before Kyber can
render it truthfully. Harness-version browsing should use the runtime registry
and image/version metadata rather than extending the legacy Claude/Codex
catalogs.

## Slice C checkpoint — operator model, context, and version visibility

The approved next checkpoint closes the four parity gaps with data native to
the configured Hermes/OpenRouter runtime:

- add a bounded in-image Hermes reporter that reads only strict finalized API
  call records from Hermes's agent log, posts the observed provider/model and
  prompt-token context usage through the existing token-usage sidecar route,
  and never reads or forwards prompt/message text;
- have that reporter publish the OpenRouter model list from Hermes's own
  provider cache, enriched with Hermes's cached OpenRouter display names and
  context lengths, through the existing per-agent runtime-catalog route;
- declare Hermes model-catalog and usage-reporting support only after those
  reports exist, enabling the existing validated model picker and set-model
  pod-roll path without a Hermes-specific API;
- add a GitHub Releases client to runtime detection and expose recent stable
  Hermes package versions through an additive `hermesVersions` field and the
  descriptor-driven harness-version picker; GitHub Releases is authoritative
  here because Hermes 0.21.x is published there while PyPI remains at 0.19.0;
  and
- keep arbitrary Hermes source-version installation out of this checkpoint.
  The image is commit-pinned and its native updater targets a branch rather
  than a package version, so browsing must not be represented as proof that a
  selected historical source release can be installed safely. That requires a
  separate atomic source-install/rollback design.

Acceptance for this checkpoint:

- a live Hermes agent reports a non-empty, OpenRouter-scoped model catalog with
  known positive context windows, and Kyber's Model action can select one;
- after a model change and pod roll, the observed current model replaces the
  requested fallback in the agent detail view;
- after a completed Hermes API call, the agent detail context card shows the
  native prompt-token count, resolved limit, and percentage;
- the harness-version dialog lists current stable Hermes GitHub releases even
  though PyPI is stale; and
- focused Go, shell/image, OpenAPI contract, PWA type/lint, and component tests
  pass before the worktree is deployed to `kyber-dev` for live verification.

Implementation checkpoint:

- added a bounded Hermes reporter with strict finalized-call parsing, native
  OpenRouter catalog conversion, retry-until-present cache discovery, and
  provider/model/context reporting through the existing sidecar endpoints;
- enabled the Hermes model-catalog and usage-reporting contract features, so
  the existing per-agent model validation and pod-roll action apply without a
  runtime-specific API;
- added stable Hermes GitHub release discovery and the additive
  `hermesVersions` Go/OpenAPI/TypeScript contract;
- made the Hermes version dialog explicitly browse-only, because the pinned
  source runtime does not yet have an atomic historical-version installer;
- bumped `@matty-v/kyber-pwa-views` to 0.42.0 and documented the UI change; and
- verified the affected Go suites, image integration fixtures, OpenAPI
  contract, TypeScript lint, 786-test PWA suite, and focused provider/version
  component regressions.

First rollout finding:

- the 517 MB Hermes image built successfully, but the controller deleted the
  first dev pod after 387 seconds while its durable-root merge was still in
  progress; the reporter and Hermes process had not started, so this was not a
  reporter failure;
- the deletion aligned with the 300-second Hermes liveness delay plus its
  failure-period allowance, proving the previous budget remained marginal for
  a cold image pull and full durable-root merge; and
- Hermes now receives a 600-second initial liveness grace while readiness
  remains strict, giving the bounded merge room to complete without exposing a
  half-started runtime.

## Live hardening checkpoint — Hermes native skill discovery

Hermes reports 59 usable skills in the live Linux agent: five identity skills
linked at the top level of `~/.hermes/skills`, plus 54 Linux-compatible skills
from its manifest-backed nested category tree. Kyber currently reports only
the five flat identity links and treats the category directories as unmanaged
state, so its Skills tab cannot match the runtime's native view.

Acceptance for this checkpoint:

- the scanner recognizes Hermes's `.bundled_manifest` as the authority for
  image-bundled native skills and recursively discovers their `SKILL.md`
  packages;
- platform gating excludes macOS-only bundled skills from a Linux report;
- each included native skill is reported as image-provided and linked to
  `hermes`, without false unmanaged-category warnings;
- flat identity, vendor, and Kyber capability skill behavior remains
  unchanged; and
- a rebuilt Hermes dev image reports the same 59 skills visible inside the
  runtime, and Kyber serves them from the agent Skills endpoint.

Implementation and live verification:

- commit `1e63442` teaches the scanner to discover manifest-backed nested
  Hermes skills, filters `platforms: [macos]` on Linux, suppresses false
  unmanaged-category findings, and gives platform skills a runtime-neutral PWA
  label;
- focused scanner, API, runtime, PWA component, and TypeScript lint checks pass;
- the rebuilt dev Hermes image is
  `hermes:worktree-20260908180700-1e63442` (digest
  `sha256:03edfab550ffa34b8e16b2fad563442c56633a0ab21f19d1852827696642a32f`);
- that image's scanner reports exactly 59 usable skills: five identity skills,
  54 image-provided Hermes skills, all linked to `hermes`, and zero findings;
- the dev control plane stores that exact 59-skill report for the Skills tab;
  and
- the live agent is `Running` on Hermes 0.21.0 with both runtime containers
  ready and zero restarts.

Rebuilding the 512 MB Hermes image triggered the durable-root base-image
merge, which exceeded Hermes's original 90-second effective liveness allowance.
Kubernetes repeatedly terminated the agent before the merge could finish.
Commit `e74dbac` gives Hermes a 300-second initial liveness grace period while
readiness continues to hold it out of service. Commit `f124030` makes the
controller's Starting timeout honor each pod's declared liveness budget instead
of preempting it at a fixed 120 seconds. The final migration completed on one
stable pod after roughly six and a half minutes, within its bounded probe
budget. The corresponding control-plane image is
`control-plane:worktree-20260908184524-f124030` (digest
`sha256:e1c16e3d41c71a4d62553bd189c2c5ebb59fe8a8041376f7c914a302412becc5`).

Exact next action: deploy the extended Hermes startup budget to the dev control
plane, restart the failed dev agent on the already-built Hermes image, and
verify live model selection, observed provider/model, context budget, and
release browsing.

## Outcome

Add `hermes` as a registry-driven Kyber runtime using the
`interactive-tmux-v1` harness contract. The first supported configuration uses
an OpenRouter API key, a pinned Hermes source release, Kyber-owned channel MCP
sidecars, native Hermes session persistence, and capability declarations that
omit behavior the integration cannot prove.

Hermes must not add a runtime branch to the Agent CRD or lifecycle state
machine. Runtime-specific behavior belongs in the runtime adapter, its image,
and its native configuration.

## Upstream baseline

- Repository: `https://github.com/NousResearch/hermes-agent`
- Release tag: `v2026.8.31`
- Package version: `0.21.0`
- Commit: `29112bef099274229cadff79cdff7bf7b99c4b77`
- Python contract: `>=3.11,<3.14`

The image builds from the pinned source and lockfile. Hermes's official
Debian+s6 image is not used as the base because Kyber runtimes must extend the
shared durable-root entrypoint contract.

## Capability contract

The preview declares:

- session restart;
- native session resume across pod restart; and
- in-place context compaction.

The preview does not declare task receipts, task tools, job-turn hooks, model
catalog, usage reporting, or in-place runtime repair. Those features remain
absent until their native wiring and live evidence exist.

Hermes 0.21.0's `pre_llm_call` hook is context-only and fail-open. It cannot
block a model turn when Kyber's receipt persistence fails, so it cannot satisfy
HC-07. Durable task delivery must stay gated until Hermes supplies a fail-closed
pre-model admission hook or Kyber carries a reviewed, pinned patch with the same
semantics.

## Delivery slices

### Slice A — bootable OpenRouter preview

- Add `pkg/runtimes/hermes` with registration, descriptor, authentication,
  adapter, lifecycle probes, session commands, and reusable contract tests.
- Add binary imports and Helm image discovery so the control plane can create a
  Hermes pod only when an image is pinned.
- Add `images/hermes` based on `runtime-base`, install the pinned upstream
  source with `uv`, and report the installed runtime version.
- Configure `HERMES_HOME=/home/kyber/.hermes`, OpenRouter auth, classic CLI,
  non-interactive hook acceptance, and the Kyber tmux launch/watchdog contract.
- Generate Hermes MCP configuration for enabled Kyber loopback services while
  keeping channel credentials in their sidecars.
- Declare channel support per authentication mode so the API and creation
  wizard can offer Telegram, Discord, and Slack to Hermes API-key agents.
- Implement fresh session restart with `/persist/last-hermes-launch.sh`, native
  pod-restart resume, and `/compress` delivery.
- Add focused Go and shell fixture tests, image build/release wiring, and chart
  validation.

### Slice B — supported beta evidence

- Add Hermes skills to the identity linker and scanner without creating a
  second unsynchronized identity authority.
- Add normalized append-only transcript output for platform recall/archive;
  the SQLite session database is not a transcript input.
- Add bounded activity and usage reporting sourced from native Hermes
  state/hooks.
- Exercise startup prompt, later prompt, busy turn, restart, resume, compaction,
  shutdown, credential failure, request/reply, and channel MCP flows in
  `kyber-dev` with a disposable agent.
- Record exact image digest, Hermes version, auth mode, and outcomes in the
  harness conformance matrix.

### Slice C — parity follow-ups

- Add Nous Portal device OAuth through the generic authentication route and
  synchronize refreshed opaque credentials safely.
- Add provider/model discovery and version selection without extending the
  legacy two-runtime catalogs.
- Add advanced job hooks after correlation and cleanup evidence.
- Add durable tasks only after the fail-closed hook requirement is met.
- Generalize runtime repair for source/`uv` installations if operator demand
  warrants it.

## Slice A acceptance checks

- `hermes` appears in runtime discovery only when its package is registered,
  and pod creation rejects an unpinned Hermes image.
- `runtimeAuth.openrouterApiKey` creates only the selected agent's
  `<agent>-openrouter` Secret and injects it as `OPENROUTER_API_KEY`.
- The descriptor exposes the OpenRouter auth field and only the three supported
  session features.
- The adapter passes the requested model through `HERMES_INFERENCE_MODEL`, sets
  the OpenRouter provider, and does not expose another provider credential.
- Liveness proves the Hermes process exists. Readiness proves both the process
  and selected credential are present from inside the durable root.
- The start script writes a permission-bounded relaunch script, starts Hermes
  in the managed `agent` tmux session, and distinguishes intentional fresh
  restart from configured pod-restart resume.
- MCP configuration covers each enabled Kyber loopback endpoint and contains no
  channel provider credential.
- Compaction uses Hermes's `/compress` command through the shared non-blocking
  tmux delivery helper.
- Runtime, image, chart, and affected API/PWA tests pass. The image build is
  included in PR and release workflows.

## Verification

Run during Slice A:

```sh
go test ./pkg/runtimes/hermes ./pkg/runtimes/... ./pkg/api/...
go test -tags=integration ./images/hermes/...
make helm-lint helm-template
npm run lint --workspace=packages/pwa-views
npm run test --workspace=packages/pwa-views
```

Then build the runtime-base and Hermes images and run the version-pinned live
matrix in `kyber-dev`. Fixture success is not native Hermes certification.
