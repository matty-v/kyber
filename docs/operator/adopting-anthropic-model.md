# Adopting a new Claude model

This how-to walks through the workflow an operator follows after Anthropic
ships a new Claude model: get it into the per-agent model picker, set its
context-window so the budget card and `[1m]` opt-in are accurate, and apply
it to one agent or the whole fleet — all without rebuilding any Kyber image.

There are two model levers, and they source differently:

- **Per-agent** — the agent detail page's **Set Model** dropdown, sourced
  from the **agent's own authenticated model catalog**
  (`GET /api/v1/agents/{name}/models`), which the running runtime reports.
  It reflects what that agent's subscription actually offers.
- **Fleet-wide** — **Settings → Fleet defaults**, the model new agents (and
  any agent with no explicit `spec.model`) inherit. Agent creation has no
  model picker: a new agent starts on the fleet default.

The detection poller (`runtimeDetect`, fed by the `kyber-anthropic-key`
Secret / `PUT /api/v1/settings/anthropic-key`) still matters, but for
**context windows and harness-version pickers** (`GET /api/v1/available`),
not for the per-agent model dropdown — see
`docs/architecture/model-onboarding.md`.

## I want a new model in the picker

Most of the time you don't have to do anything. The Set Model dropdown is
populated from the catalog the agent's authenticated runtime reports, so a
model becomes selectable once the provider offers it to that account and
the agent's runtime has reported its catalog. A freshly created or
restarted agent reports shortly after boot; until the first report the
models endpoint returns `409` and the dialog says no authenticated catalog
is available yet.

There is no manual free-text override input in the picker any more. If you
know a model ID the catalog doesn't list yet, set it via the API
(`POST /api/v1/agents/<name>/set-model` accepts any string) or as the
fleet default. Boot fails visibly if the installed Claude Code version
doesn't recognize the model (the `ModelUnsupported` badge surfaces this).

## I want to set a model's context-window

The `kyber-model-context-windows` ConfigMap is the **single source of
truth** for a model's context window — it drives **both** the model
picker / detection list **and** the Token Usage budget percentage
(kyber#396). Correct a window in this one place and every consumer
reflects it; there is no second table to edit and no Kyber release needed.

By default every model surfaced through `GET /api/v1/available` gets the
200K **floor** with `contextWindowKnown=false` — the PWA renders a
"context unknown" indicator next to its entry in the picker, **and** the
Token Usage card renders that model's budget % as an **estimate**
(`≈ {pct}%` + a "· unverified window" hint, no "over budget" warning)
rather than a confidently-wrong number. That estimate state is the nudge
to "add this model to the ConfigMap," not a bug. While unknown, the model
is treated as 200K (a safe under-report) and `start-claude.sh` does NOT
enable the `[1m]` opt-in.

To set the real window, edit the `kyber-model-context-windows` ConfigMap
in-cluster:

```bash
kubectl edit cm kyber-model-context-windows -n kyber-system
```

Add a row for the new model (key = model ID, value = tokens):

```yaml
data:
  claude-opus-4-7: "1000000"
  claude-opus-5-0: "1000000"   # new entry
  ...
```

Within `ResolveCacheTTL` (30s) the control-plane re-reads the ConfigMap
and:

- `/api/v1/available` starts returning the model with the configured
  window + `contextWindowKnown=true`. The picker drops the "context
  unknown" badge.
- The Claude Code adapter feeds the configured window into the pod env
  as `KYBER_MODEL_CONTEXT_WINDOW`, so `start-claude.sh` will set the
  `[1m]` suffix when the value is ≥ 1,000,000 on the next pod boot.
- **The Token Usage budget % corrects immediately, even for
  already-running agents** (kyber#396). The limit is resolved
  server-side at serve-time from this ConfigMap on each token-usage read,
  so the next time the card refreshes it shows the corrected % and drops
  the estimate flag — **no pod restart needed** for the budget number.
  (The `[1m]` pod-env opt-in above is the one piece that still waits for a
  pod recreation.)

The token-usage GET response carries `contextWindowKnown` (additive) so
the card knows whether to show the estimate flag. The `[1m]`/`opus`/
`sonnet`/`haiku` normalization the old in-pod table did is preserved
server-side, so suffixed and family-alias model ids resolve correctly.

Existing pods continue running with the previously-resolved
`KYBER_MODEL_CONTEXT_WINDOW` env until their next recreation (lazy adopt —
same posture as fleet defaults; see
`docs/design/2026-05-29-runtime-model-management-design.md` § "Known
interactions" for the kyber#358 convergence interaction). The budget %,
by contrast, is not pod-env-bound and updates on the next read.

To persist the change across `helm upgrade`, mirror the new entry into
`runtimeDetect.contextWindows` in your kyber-deploy values.yaml.

## I want to apply a model to one agent

Open the agent's detail page in the PWA → **More** → **Set Model**. The
dropdown is sourced from the agent's authenticated catalog
(`GET /api/v1/agents/<name>/models` — **not** `/api/v1/available`), so it
lists what this agent's subscription offers; it returns `409` until the
runtime has reported. Pick the new model and click **Apply**.

What happens server-side:

1. PWA POSTs `/api/v1/agents/<name>/set-model` with the new ID.
2. The control-plane sets `spec.model = <new id>` and flips
   `DesiredPhase=Restarting` if the agent is currently Running/Starting/
   Restarting (Stopped agents keep their disk state; the new model takes
   effect on the next manual start).
3. The next pod boot picks up the new model via `CLAUDE_MODEL` env, sizes
   `KYBER_MODEL_CONTEXT_WINDOW` from the override map (or the floor),
   and `start-claude.sh` applies `[1m]` when the window is ≥ 1M.

If you also want to bump the Claude Code version on that one agent, use
**More** → **Set Claude Code Version** in the same dialog flow — the
version picker, unlike the model dropdown, is sourced from `/available`
(detection). See
`docs/operator/adopting-cc-version.md` for the per-agent CC version
workflow.

## I want to bump the fleet default

Open **Settings** → **Fleet defaults** in the PWA. Change `Default
model` (and optionally `Default Claude Code version`) and click **Save**.
Choose **Default** to let the harness select its current model; this is the
fresh-install setting. Enter a concrete model identifier to pin it.

What happens:

1. PWA PUTs `/api/v1/fleet-defaults` with the new values.
2. The control-plane updates the `kyber-fleet-defaults` ConfigMap in
   place.
3. **Running agents are not restarted.** The default takes effect on
   each agent's next pod recreation (lazy adopt — same as the model
   workflow above). Pick an agent and use **Restart** if you want
   immediate adoption.

Caveat: `kyber#358`'s sidecar-image convergence can recreate every
agent pod when the sidecar image rolls. Because pod recreation
re-resolves `(spec.model || defaultModel)` against the *current*
fleet default, that opportunistic recreation will adopt the new
default fleet-wide. This is documented behavior — see
`docs/design/2026-05-29-runtime-model-management-design.md` § "Known
interactions". If you want to avoid this, set per-agent `spec.model`
explicitly before bumping the default.

## Failure modes

| Condition | What you see |
|---|---|
| Agent runtime hasn't reported a catalog yet | Set Model dialog says no authenticated catalog is available (the models endpoint returns `409`). Wait for the agent to boot and report, then reopen the dialog. |
| Anthropic API key not entered | The Set Model dropdown is unaffected (it reads the agent's catalog). Context-window *detection* is off, so windows for models the catalog doesn't cover fall back to the override map. |
| Detection upstream down | `/available` serves last-good cache; version pickers and detected windows are stale, not broken. The per-agent model catalog is unaffected. |
| Model window in neither catalog, override map, nor detection | "context unknown" indicator; budget card under-reports; `[1m]` not applied. Fix by editing the ConfigMap. |
| API-set model not recognized by installed CC | Boot fails; the `ModelUnsupported` badge lights up; apply a newer CC version (see `adopting-cc-version.md`). |
| `helm upgrade` reverts ConfigMap | Mirror operator edits to `runtimeDetect.contextWindows` in kyber-deploy values.yaml to survive upgrades. |
