# MAT-52 — Hermes harness implementation plan

**Issue:** [MAT-52](https://linear.app/matty-v/issue/MAT-52/add-hermes-as-a-supported-kyber-agent-harness)
**Dependency:** [MAT-7](https://linear.app/matty-v/issue/MAT-7), merged in PR #231
**Status:** Slice A implemented; live hardening in progress 2026-09-08

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

Exact next action: reload the dev control plane and verify the live endpoint.

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
