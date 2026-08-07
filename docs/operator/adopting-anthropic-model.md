# Adopting a new Claude model

This how-to walks through the workflow an operator follows after Anthropic
ships a new Claude model: tell Kyber about it, set its context-window so
the budget card and `[1m]` opt-in are accurate, and apply it to one agent
or the whole fleet — all without rebuilding any Kyber image. PR-D of
kyber#374 (PR #378 onwards) makes the picker data-driven and the
context-window operator-editable.

> **Prerequisite:** the detection poller (PR-A, kyber#375) must be
> reachable. That means the `kyber-anthropic-key` Secret holds a valid
> Anthropic API key — see `docs/runtime-detection.md` § "Setup".

## I want to add a new model to the picker

Most of the time you don't have to do anything. The control-plane poller
queries the [Anthropic Models API](https://docs.anthropic.com/en/api/models)
every `runtimeDetect.cadenceSeconds` (default 1 hour) and the new model
shows up in the PWA's Create-Agent picker on the next refresh. To force
a refresh sooner, restart the control-plane pod.

If detection is unavailable (no API key configured, air-gapped install,
upstream outage), or the model hasn't been added to the Anthropic API yet
but you already know its ID, use the **Manual model override** input on
the picker — type the model ID directly. Kyber's API accepts any string;
the model becomes the agent's `spec.model` and boot will fail visibly if
the installed Claude Code version doesn't recognize it (PR-E's
`ModelUnsupported` badge surfaces this).

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
`docs/2026-05-29-runtime-model-management-design.md` § "Known
interactions" for the kyber#358 convergence interaction). The budget %,
by contrast, is not pod-env-bound and updates on the next read.

To persist the change across `helm upgrade`, mirror the new entry into
`runtimeDetect.contextWindows` in your kyber-deploy values.yaml.

## I want to apply a model to one agent

Open the agent's detail page in the PWA → **More** → **Set Model**. The
dropdown is sourced from `/api/v1/available` (live detection + override
map). Pick the new model and click **Apply**.

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
version picker is sourced from the same `/available` data. See
`docs/operator/adopting-cc-version.md` for the per-agent CC version
workflow.

## I want to bump the fleet default

Open **Settings** → **Fleet defaults** in the PWA. Change `Default
model` (and optionally `Default Claude Code version`) and click **Save**.

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
`docs/2026-05-29-runtime-model-management-design.md` § "Known
interactions". If you want to avoid this, set per-agent `spec.model`
explicitly before bumping the default.

## Failure modes

| Condition | What you see |
|---|---|
| Anthropic API key not entered | Picker still works (uses /config knownModels fallback); new models won't appear until the key is set. |
| Detection upstream down | `/available` serves last-good cache; picker is stale, not broken. |
| Model not in override map | Picker shows "context unknown" indicator; budget card under-reports; `[1m]` not applied. Fix by editing the ConfigMap. |
| Manual-entry model not recognized by installed CC | Boot fails; PR-E badge `ModelUnsupported` lights up; apply a newer CC version (see `adopting-cc-version.md`). |
| `helm upgrade` reverts ConfigMap | Mirror operator edits to `runtimeDetect.contextWindows` in kyber-deploy values.yaml to survive upgrades. |
