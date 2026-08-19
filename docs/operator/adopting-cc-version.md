# Adopting a new Claude Code version

This how-to walks through the two ways an operator pins a Claude Code (CC)
CLI version on Kyber agents: per-agent via `spec.runtimeVersion`, and
fleet-wide via the `defaultRuntimeVersion` setting. As of kyber#374 PR-C
neither path requires editing Kyber source or rebuilding the
`kyber-claude-code` image — both pins are runtime-resolved and the version
is installed at agent pod boot.

> **Heads-up:** the picker that surfaces *which* CC versions are available
> ships in PR-A. Until then, you can still pin any published version of
> `@anthropic-ai/claude-code` on npm; you just need to know the version
> string ahead of time. See the npm package page for the list.

## Per-agent: pin one agent to a specific version

Use this when you want to try a new CC version on a single agent before
rolling it out fleet-wide — e.g., qualifying a `2.2.x` build on a
non-critical agent.

```bash
# Set the Agent CR's spec.runtimeVersion via the PWA API.
# (The PWA's agent-detail page will get a UI field for this in PR-D.)
# A running agent is automatically flipped to Restarting so the new value
# takes effect on the next pod boot — no separate restart call needed.
curl -X POST "$KYBER_URL/api/v1/agents/<agent-name>/set-runtime-version" \
  -H "Authorization: Bearer $KYBER_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"runtimeVersion": "2.2.0"}'

# For a Stopped/Suspended agent the value is stored but the pod is not
# rolled; start (or restart) it to run the boot-time install.
curl -X POST "$KYBER_URL/api/v1/agents/<agent-name>/restart" \
  -H "Authorization: Bearer $KYBER_API_KEY"
```

What happens on the next boot:

1. The reconciler resolves `spec.runtimeVersion` (or falls back to the
   fleet default), injecting `KYBER_REQUESTED_CC_VERSION` into the pod
   env.
2. `start-claude.sh` validates the value against the charset pattern
   (`^[0-9A-Za-z.\-]+$`, max 64 chars), then runs
   `npm install -g @anthropic-ai/claude-code@<version>` iff the value
   differs from the baked-in pin. If it matches the baked-in version,
   no install runs.
3. If the install **succeeds**, Claude Code runs the requested version.
4. If the install **fails** (registry outage, version doesn't exist,
   network unreachable), the pod **does not crash-loop** — it continues
   on the baked-in version. PR-E adds a `RuntimeVersionMismatch` status
   condition + PWA badge so you see the failure without grepping logs.

## Fleet-wide: bump the default for all agents

Use this when you've qualified a new CC version on one agent and want
the rest of the fleet to adopt it on next pod recreation.

1. Open the PWA → **Settings** → **Fleet defaults**.
2. Set **Default Claude Code version** to the qualified version string.
3. Click **Save**.

The new default applies on the next pod recreation per agent. Lazy adopt
is deliberate — there's no orchestrated rolling restart machinery (see
kyber#374 § Known interactions for why). Per-agent immediate adoption is
the per-agent restart in the section above.

Choose **Latest** to track the current upstream harness release at each pod
creation, or enter a concrete version to pin the fleet. Fresh installs start
on Latest. PWA-selected values are operator-owned and survive Helm upgrades.

## Charset rules + error messages

`spec.runtimeVersion` is validated server-side by the CRD's
`kubebuilder:validation:Pattern` (rejection at `kubectl apply` or PATCH
time) and re-validated by `start-claude.sh` at boot (defense in depth
against a future CRD change that could open a shell-injection path).

Allowed characters: `0-9 A-Z a-z . -`. Max length: 64.

Examples:

| Input | Result |
|---|---|
| `2.1.119` | Accepted |
| `latest` | Accepted |
| `2.1.119-rc.1` | Accepted |
| `2.1.119;rm -rf /` | Rejected (charset) — pod continues on baked-in |
| `$(whoami)` | Rejected (charset) — pod continues on baked-in |
| 65+ char string | Rejected (length cap) — pod continues on baked-in |

Rejections at boot are logged with `[kyber] WARNING:` and the pod
continues on the baked-in version. PR-E will surface this as a
`RuntimeVersionMismatch` condition.

## What if it doesn't work?

PR-E (kyber#379) lights up two PWA badges on the agent detail page so you
never have to grep pod logs to see a silent install or model failure:

- **"Runtime version mismatch"** (warning, yellow). Fires when the agent's
  installed Claude Code version differs from what `spec.runtimeVersion`
  (or the fleet default) asked for. Almost always means the boot-time
  `npm install` failed and the pod fell back to the baked-in version.
  The badge shows both versions so you can diagnose at a glance. Common
  causes:
  - Charset/length-cap rejection (you'll see a `[kyber] WARNING:`
    rejection line in the pod's logs from start-claude.sh, and
    `spec.runtimeVersion` will need to be edited to match
    `^[0-9A-Za-z.\-]+$`, ≤ 64 chars).
  - npm registry outage (transient; restart the agent once registry is
    healthy).
  - Version doesn't exist on npm (the requested string is well-formed
    but no such `@anthropic-ai/claude-code` version is published).
  Remedy: fix the cause, then restart the agent so start-claude.sh
  re-attempts the install.

- **"Model not supported by installed Claude Code"** (danger, red).
  Fires when start-claude.sh's pre-flight probe
  (`claude --model <resolved> --print 'ping'`) reported the model as
  unsupported by the installed CC. The probe times out at 10s by default
  (override with `KYBER_PROBE_TIMEOUT_SECONDS`); a timeout reports as
  unknown — NOT as unsupported — so a flaky network blip won't flip the
  badge.
  Remedy: apply a newer Claude Code version that supports the model.
  Set `spec.runtimeVersion` per-agent, OR bump
  `defaultRuntimeVersion` fleet-wide via the PWA Settings panel.
  Restart the agent.

Both badges clear within one report cycle (≈ next pod boot) once the
underlying signal resolves. If a badge is stuck after a restart, check
`agent.status.runtime` via
`kubectl get agent <name> -o yaml` — the `requestedSatisfied`,
`modelSupported`, `installedVersion`, and `requestedVersion` fields show
exactly what the pod reported.

## How the install path interacts with the fleet-default model

This how-to focuses on the CC version. Model selection has the same
shape — `spec.model` plus the fleet `defaultModel` (kyber#376 PR-B). The
two settings are independent. A common pattern is to:

- Pin `defaultModel` to a stable model the whole fleet runs.
- Pin `defaultRuntimeVersion` to the CC version known to support that
  model.
- Override per-agent (`spec.model` or `spec.runtimeVersion`) only when
  qualifying a new version on a single agent.

Once PR-E ships, an agent whose installed CC version can't run its
configured model surfaces as `ModelUnsupported` in the PWA — the cue
to bump `spec.runtimeVersion` for that agent.

## Related

- `docs/design/2026-05-29-runtime-model-management-design.md` — authoritative
  architecture for the whole runtime + model management story.
- kyber#374 — epic tracker.
- kyber#376 — PR-B (fleet-default resolution layer; `defaultModel`).
- kyber#377 — PR-C (this PR; `spec.runtimeVersion` + boot-time install).
- kyber#379 — PR-E (mismatch safety net; ships the failure-mode badges
  referenced above).
