# Runtime + Model Management — Design

**Date:** 2026-05-29
**Status:** Approved design, ready for implementation planning
**Author:** Dave (off-cluster agent) with Matt
**Reviewed:** independent design-review pass 2026-05-29 (findings incorporated below)

## Problem

Adding a new Claude Code (CC) CLI version or a new Claude model to Kyber agents
currently requires editing Kyber's Go code (or a shell script) and rebuilding images:

- **Models** are a hardcoded Go slice — `knownModels` in `pkg/tokenreport/limits.go:27`.
  Adding a model means editing that slice + its test, building, and deploying the
  control-plane. The list feeds the `/api/v1/config` endpoint
  (`pkg/api/routes_config.go:73`, the `Models: tokenreport.Models()` field; route
  registered in `pkg/api/server.go:442`) which the PWA Create-Agent dropdown reads,
  plus the token-usage context-window math.
- **Model 1M-context opt-in** is _also_ hardcoded, in a shell `case` —
  `images/claude-code/start-claude.sh:386-395`. Models known to support a 1M window
  get a `[1m]` suffix; a new model that matches no arm runs in its **200K** variant,
  and Kyber's budget card then overreports headroom ~5× (per the comment at
  start-claude.sh:382-385). So adding a new 1M model today needs a shell edit too.
- **CC version** is baked into the agent image at build time —
  `images/claude-code/Dockerfile:34` (`npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}`,
  ARG default at :33, surfaced as `KYBER_RUNTIME_DEFAULT_VERSION` env at :35), pinned
  via the chart's `runtime.claudeCode.version` (`deploy/helm/kyber/values.yaml:485`,
  build-time only via CI/yq → build-arg). Changing it means a chart-value bump + CI
  image rebuild + ArgoCD roll.

There is also a **silent-failure gap**: nothing validates that the model an agent
is asked to run is actually supported by the CC version installed in its image.
A mismatch surfaces as a crash or odd behavior, not a clear signal — the same class
of failure as the kyber#358 / R2-D2 incident (2026-05-29).

## Goal

Make it easy to **detect** new CC versions and new models and **apply** them to
agents **without changing Kyber code** (Go _or_ the runtime shell script). Detection
should be automatic; application is operator-initiated (a PWA click), not auto-upgrade.

"Without changing Kyber code" means: an operator adopts a newly-released CC version
or model by interacting with the PWA / setting a config value — never by editing
source and cutting a release. This explicitly includes the 1M-context decision,
which today lives in `start-claude.sh` and must become data-driven (§3, §5).

## Non-goals (YAGNI)

- **Auto-upgrade / version policies.** Detection surfaces options; a human applies them.
- **Staged / active fleet rolls.** Fleet apply is lazy (§4) — no orchestrated rolling
  restart machinery. (But see "Known interactions" for how pod recreation from other
  causes opportunistically adopts the new default.)
- **Pre-baked per-version images.** CC is installed at runtime (§3).
- **Offline operation.** Agents require network regardless.
- **Integrity/signature verification** of the npm package beyond what npm itself does
  (operators can pin any published version; trust assumption stated in §3).

## Architecture overview

```
            ┌────────────────────── control-plane ──────────────────────┐
            │   Detection poller ──► cache (Redis) ──► GET /api/v1/available
            │     • npm registry (CC versions)                           │
            │     • Anthropic Models API (models, operator-supplied key)  │
            │                                                            │
            │   Agent reconciler:                                        │
            │     resolve spec.model / spec.runtimeVersion vs fleet      │
            │       defaults; look up context window for the model       │
            │     ──► pod env: CLAUDE_MODEL, KYBER_REQUESTED_CC_VERSION,  │
            │            KYBER_MODEL_CONTEXT_WINDOW                       │
            │     ◄── runtime report: installedVersion, currentModel,    │
            │            requestedSatisfied, modelSupported              │
            │     ──► status conditions (mismatch safety net)            │
            └────────────────────────────────────────────────────────────┘
                              │ pod spec                  ▲ status report
                              ▼                           │
            ┌──────────────────── agent pod ─────────────────────────────┐
            │  start-claude.sh:                                          │
            │   1. validate + (if != baked-in) npm install CC@<req>      │
            │   2. pre-flight probe: claude --model <m> (timeout)        │
            │   3. apply [1m] iff KYBER_MODEL_CONTEXT_WINDOW >= 1M        │
            │   4. report installed version + model + satisfied/supported │
            └────────────────────────────────────────────────────────────┘

 PWA: reads /available + per-agent requested-vs-installed + mismatch badges;
      "apply" → PATCH agent spec (one) or bump fleet default (all, lazy).
```

## Components

### 1. Public harness detection + authenticated per-agent model discovery

A goroutine in the control-plane periodically (configurable, default ~1h) queries
the public npm upstreams and caches the result **in Redis** (Kyber already runs Redis for the
token-budget feature, and the control-plane may run multiple replicas — a per-replica
in-memory cache would make `/available` answer differently per replica and flicker
the PWA picker; Redis is the default, in-memory only as a single-replica fallback):

- **CC versions:** the npm registry endpoint for `@anthropic-ai/claude-code`
  (`https://registry.npmjs.org/@anthropic-ai/claude-code`), extracting `latest` and
  the N most-recent published versions (keep the picker sane — not all history).
- **Codex versions:** the npm registry endpoint for `@openai/codex`, with the same
  bounded version policy.

The public harness lists are exposed via **`GET /api/v1/available`**. No provider
credential is required for this control-plane poller.

**Model authentication:** after an agent authenticates, its in-pod reporter queries
the provider using that agent's existing credential. Claude Code calls Anthropic's
Models API with its OAuth access token (or its configured API key); Codex calls the
authenticated app-server `model/list` method. The reporter sends only model metadata
through the status sidecar. Credentials never reach the control plane or browser.
The control plane stores catalogs by agent and exposes only the requested agent's
list at **`GET /api/v1/agents/{name}/models`**.

Anthropic includes `max_input_tokens` in the authenticated response, so Claude
picker entries carry authoritative windows. Codex app-server `model/list` does
not expose a context-window field; those picker entries remain explicitly
unknown, while the active model's rollout `token_count.context_window` drives
its budget gauge. No static or guessed fallback is applied.

Before authentication, Change model reports that authentication is required. A
discovery failure never blocks agent operation or changes its current model.

**Does not:** mutate any agent. Detection is strictly read-only.

### 2. Requested config — per-agent CR fields + fleet defaults

> Correction from review: there is **no existing fleet-default mechanism**. Today
> `agent.Spec.Model` is plumbed directly to the pod (`pkg/runtimes/claudecode/adapter.go:103`)
> and is a **required** field (`pkg/api/v1/agent_types.go:463`). Both the
> default-resolution layer and the version field are new work.

- New Agent CR field **`spec.runtimeVersion`** (string; requested CC version).
  `+optional`, with `+kubebuilder:validation:Pattern` constraining it to a
  semver-ish shape (`^[0-9A-Za-z.\-]+$`) and a sane `MaxLength` — both correctness
  and the injection guard in §3. Empty → use the fleet default.
- **`spec.model`** becomes `+optional` (today required). Empty → use the fleet default.
- **Fleet defaults** `defaultRuntimeVersion` and `defaultModel`: control-plane
  config (chart value → ConfigMap, also settable via the PWA). Fresh installs use
  `latest` for the runtime version and an empty model, meaning the harness chooses
  its own default. Operators can explicitly pin either value in Settings.
- The reconciler resolves `(spec.X || default.X)` for both fields and, for the model,
  looks up its context window (§5) to decide the 1M opt-in. It injects into the pod:
  - `CLAUDE_MODEL` (already wired — adapter.go:103),
  - `KYBER_REQUESTED_CC_VERSION` (new),
  - `KYBER_MODEL_CONTEXT_WINDOW` (new — the resolved context window in tokens, so the
    `[1m]` decision is data-driven in the runtime instead of a hardcoded shell `case`).

### 3. Runtime-install + model handling at startup

`images/claude-code/start-claude.sh` gains, before launching `claude`:

1. **Validate** `KYBER_REQUESTED_CC_VERSION` against `^[0-9A-Za-z.\-]+$` before any
   shell interpolation (mirrors the existing identity-repo slug guard at
   start-claude.sh:108-112) — prevents command injection via a crafted version string.
2. If it is non-empty and differs from the baked-in version
   (`KYBER_RUNTIME_DEFAULT_VERSION`), `npm install -g @anthropic-ai/claude-code@<req>`.
   On failure, **keep the baked-in version** and record `requestedSatisfied=false`.
   Never block startup on a failed upgrade.
3. **Replace the hardcoded `[1m]` `case`** (start-claude.sh:386-395): append `[1m]`
   iff `KYBER_MODEL_CONTEXT_WINDOW >= 1000000`. The family aliases (`sonnet`/`opus`/
   `haiku`) that some callers still emit can stay as a small compatibility map, but a
   concrete model ID no longer needs a code arm — its context window comes from config.
4. **Pre-flight model probe** (the feasible `ModelUnsupported` mechanism): run a
   short, bounded `claude --model <resolved> --print 'ping'` (or equivalent
   non-interactive one-shot) with a timeout. Capture exit code / stderr. If it fails
   in a way attributable to an unknown/unsupported model, record `modelSupported=false`.
   This is necessary because the real session launches **detached** in tmux
   (`tmux new-session -d ... "claude $CLAUDE_ARGS"`, start-claude.sh:562) and its exit
   code is not observable — so we cannot infer model support from the session itself.
5. Report `installedVersion`, `currentModel`, `requestedSatisfied`, `modelSupported`
   to the control-plane (extends the kyber#175 report — see §6 / S1).

The baked-in pin stays as the fast-path baseline and fallback. A brand-new CC version
is adoptable with zero image rebuild: set the requested version → restart → it installs
on boot.

**Trust assumption:** an operator with apply rights can pin any published
`@anthropic-ai/claude-code` version, installed at boot from the public npm registry on
a pod that holds OAuth/identity tokens. The version string is validated to a safe
charset (step 1) but not signature-verified beyond npm's own integrity checks.

### 4. Apply (PWA, one-click)

The PWA reads `/available`, each agent's resolved requested values, and
`status.runtime.installedVersion` / `status.currentModel` (reported), and shows
**requested-vs-installed** per agent.

- **Apply to one agent:** PATCH `spec.runtimeVersion` / `spec.model`, then restart that
  agent. The new pod installs/uses the requested values.
- **Apply to fleet:** bump `defaultRuntimeVersion` / `defaultModel`. Agents adopt on
  their next restart — no active rolling (operator decision: lazy). Immediate adoption
  for one agent is the per-agent restart.

### 5. Models list moves out of Go

`knownModels` (the hardcoded slice) is replaced as the source of truth by the detected
list from §1. The one datum the Models API does **not** return is **context-window
size** (needed both for the token-usage `%` and now for the §3 `[1m]` decision):

- Maintain a small **operator-editable context-window map** (`modelId → tokens`), e.g.
  a ConfigMap, with `contextWindowKnown=false` + a safe **floor** default (200K) when a
  model isn't listed. Adding the number is a config edit, not code. Note the default is
  a _floor_: an unmapped new 1M model will under-report usage and not get `[1m]` until
  its real window is added — acceptable degraded behavior, not a crash, and fixable
  without a release.
- Keep a **manual model-entry override** (operator types a model ID the API didn't
  surface) as a safety valve; the API/CRD already accept any model string.

`pkg/tokenreport/limits.go` keeps a minimal hardcoded **fallback** list so the system
degrades gracefully when detection is down, but it is no longer the path operators
touch to add a model.

### 6. Safety net — mismatch surfacing

**Report-path extensions (called out per review S1):** the kyber#175 runtime report
(`POST /internal/agents/{name}/runtime-version`, handler `pkg/api/internal.go:522`,
status struct `AgentRuntimeStatus` at `pkg/api/v1/agent_types.go:137`) currently
accepts only `{version, reportedAt}` and writes only `InstalledVersion`. This design
**extends** the report body and the CRD status to also carry `requestedVersion`,
`requestedSatisfied`, and `modelSupported`. These are explicit work items.

The reconciler then raises visible Agent **status conditions** (`status.conditions`
already exists, agent_types.go:564):

- **`RuntimeVersionMismatch`** — `True` when `installedVersion != requestedVersion`
  (e.g. boot-time install failed → fell back to baked-in). Message names both.
- **`ModelUnsupported`** — `True` when the §3 pre-flight probe reported
  `modelSupported=false`. The remedy (apply a newer CC version) is in the same UI.

Both surface in `GET /api/v1/agents/{name}` and the PWA agent view.

## Known interactions / caveats

- **kyber#358 sidecar-convergence vs "lazy adopt" (review S2).** `convergeSidecarImage`
  (`pkg/controllers/agent/reconciler.go:1972`) unconditionally deletes any agent pod
  whose status-sidecar image is stale, with no idle/stability gate. Because pod
  recreation re-runs `start-claude.sh` against the controller's _current_ resolved
  config, **any** pod recreation — including #358 convergence or an unrelated sidecar
  bump — will opportunistically adopt the new fleet-default model and trigger a
  boot-time CC install. So "lazy adopt" is really "adopt on next recreation from any
  cause," which can be fleet-wide and involuntary. This is acceptable given the lazy
  decision, but implementers must know it, and the kyber#371 convergence-hardening
  (idle gate / concurrency cap) directly bounds the blast radius. Do not build a
  separate roll mechanism on top.

## Data flow (adopt a new model, end-to-end)

1. Anthropic ships `claude-opus-4-9`. An authenticated agent's reporter picks it up;
   `/api/v1/agents/{name}/models` lists it.
2. Operator selects it for that agent in the PWA → PATCH `spec.model` → restart.
3. New pod resolves the model + its context window; `start-claude.sh` applies `[1m]`
   iff the window ≥ 1M, runs the pre-flight probe, launches, and reports
   `currentModel` + `modelSupported`.
4. If the installed CC is too old to know the model, the probe fails → `ModelUnsupported`
   lights up in the PWA. Operator applies a newer CC version (same mechanism) + restarts.
   No Go/shell code, no release. (Context-window accuracy for the new model is a config
   map entry, also no release.)

## Error handling

- **Detection down / key missing or revoked:** `/available` serves last-good cache or
  the fallback list; PWA shows "detection unavailable"; agents unaffected.
- **Boot-time CC install fails:** agent keeps baked-in version, comes up, raises
  `RuntimeVersionMismatch`. No crash-loop.
- **Requested model unsupported by installed CC:** pre-flight probe → `ModelUnsupported`
  condition; remedy in the same UI.
- **Malformed version string:** rejected by the charset validation (§3 step 1) before
  any npm call; reported as a mismatch with the validation error.

## Testing

- **Detection:** parse npm + Models API responses from recorded fixtures; Redis cache
  TTL + the key-absent/poll-failure fallback; multi-replica returns a consistent list.
- **Reconciler:** `spec.runtimeVersion` / `spec.model` (+ fleet-default fallback) render
  the expected `KYBER_REQUESTED_CC_VERSION` / `CLAUDE_MODEL` / `KYBER_MODEL_CONTEXT_WINDOW`
  env; `RuntimeVersionMismatch` / `ModelUnsupported` conditions set/clear from reported
  status (envtest).
- **start-claude.sh** (extend `images/claude-code/start_claude_test.go`): version
  charset validation rejects injection; install-vs-baked-in branch; fall-back-on-failure
  does not block startup and emits `requestedSatisfied=false`; `[1m]` applied iff
  `KYBER_MODEL_CONTEXT_WINDOW >= 1M`; pre-flight probe sets `modelSupported`.
- **/available endpoint:** contract test pinning the response shape the PWA consumes.
- **Runtime report:** round-trip test for the extended body ↔ CRD status fields.
- **PWA:** picker renders from `/available` (not a hardcoded list); apply issues the
  expected PATCH / default-bump; mismatch badges render from status conditions.

## Rollout / compatibility

- `spec.runtimeVersion` and `spec.model` are optional and fall back to fleet defaults.
  Fresh fleet defaults request the latest harness and delegate model choice to it;
  concrete operator pins remain stable across upgrades.
- An untouched fleet setting leaves `spec.model` empty so the harness chooses its
  upstream default; an explicit per-agent selection is persisted in `spec.model`.
- Ships behind the existing chart/ArgoCD flow. Poller + `/available` are control-plane
  only; the start-claude.sh changes ship in the kyber-claude-code image (one rebuild to
  introduce the data-driven runtime; subsequent version/model adoption needs none).

## Suggested implementation breakdown (multi-PR)

Re-scoped per review (C1) — fleet-defaults and report-schema split out so no single PR
is overloaded:

- **PR-A — detection + `/available`:** public npm poller, Redis cache, and endpoint.
- **PR-B — fleet-default resolution layer:** `spec.model`→optional, `defaultModel` /
  `defaultRuntimeVersion` config + ConfigMap + reconciler resolution. (No new runtime
  behavior yet; pure plumbing + the default layer the rest builds on.)
- **PR-C — `spec.runtimeVersion` + runtime-install + data-driven 1M:** CRD field
  (+pattern/validation), `KYBER_REQUESTED_CC_VERSION` / `KYBER_MODEL_CONTEXT_WINDOW`
  env, start-claude.sh install-at-boot + charset guard + data-driven `[1m]`.
- **PR-D — models out of Go:** `/available` models drive the picker; context-window map
  + manual override; retain fallback list.
- **PR-E — safety net:** extend the runtime report body + CRD status
  (`requestedSatisfied`, `modelSupported`); pre-flight probe in start-claude.sh;
  `RuntimeVersionMismatch` / `ModelUnsupported` conditions + PWA badges.

Dependencies: PR-A and PR-B are independent. PR-C depends on PR-B (needs the default
layer). PR-D depends on PR-A. PR-E depends on PR-C. Repo convention notes this is
`complexity:l` → the Plan agent should sequence these before implementation.

## Open questions for the implementer

- **npm version list scope:** `latest` + N most-recent published versions (lean), or all?
- **Models API context-window:** re-confirm at impl time the API still omits context
  window; if Anthropic adds it, the §5 override map becomes optional.
- **Pre-flight probe cost:** confirm a one-shot `claude --model X --print` is cheap and
  fast enough to run on every boot, or gate it behind "model changed since last boot."

## Related

- `pkg/tokenreport/limits.go:27` — current hardcoded `knownModels` (source of truth being replaced).
- `pkg/api/routes_config.go:73` / `pkg/api/server.go:442` — `/api/v1/config` (today's model surface).
- `pkg/runtimes/claudecode/adapter.go:103` — `spec.Model → CLAUDE_MODEL` (extend for version + context window).
- `pkg/api/v1/agent_types.go` — `spec.Model` (:463, required→optional), `AgentRuntimeStatus.InstalledVersion` (:143), `status.currentModel` (:508), `status.conditions` (:564).
- `pkg/api/internal.go:522` — runtime-version report handler (extend body + status).
- `images/claude-code/start-claude.sh` — model `case` (:386-395), launch (:562), version report (:421-468), identity-slug validation precedent (:108-112).
- `images/claude-code/Dockerfile:33-35` — baked-in CC pin (fallback baseline).
- `deploy/helm/kyber/values.yaml:485` — `runtime.claudeCode.version` (build-time pin).
- kyber#175 — runtime-version reporting this builds on.
- kyber#371 — sidecar-convergence hardening (bounds the §"Known interactions" blast radius).
- `docs/design/2026-04-18-user-secrets-design.md` — operator-supplied-secret pattern for the Anthropic key.
