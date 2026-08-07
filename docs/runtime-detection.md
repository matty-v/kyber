# Runtime detection — Claude Code versions + Claude models

The Kyber control-plane runs a **detection poller** that periodically queries
the npm registry and the Anthropic Models API to discover newly-released
Claude Code (CC) versions and Claude models. Results are cached in Redis
and surfaced via `GET /api/v1/available`. The PWA picker reads from that
endpoint so operators can adopt new versions/models **without a Kyber code
change or rebuild**.

This is PR-A of the broader runtime-management initiative (kyber#374).
PRs B–E layer per-agent default resolution, runtime-install at boot, the
operator-editable context-window override map, and the
`RuntimeVersionMismatch` / `ModelUnsupported` safety net on top.

See [`docs/2026-05-29-runtime-model-management-design.md`](2026-05-29-runtime-model-management-design.md)
for the full design.

## What the poller does

Every `runtimeDetect.cadenceSeconds` (default `3600` = 1h) the control-plane
goroutine:

1. Calls `https://registry.npmjs.org/@anthropic-ai/claude-code` and extracts
   the `dist-tags.latest` plus up to `runtimeDetect.versionLimit` most-recent
   stable versions (pre-releases are filtered out).
2. Calls `https://api.anthropic.com/v1/models` with the operator-supplied
   Anthropic API key (see below) and extracts the model `id`, `display_name`,
   and `max_input_tokens`. When `max_input_tokens` is present it becomes the
   model's `contextWindow` with `contextWindowKnown=true`; when absent the
   model falls back to the 200K floor (`contextWindowKnown=false`). See
   [Context windows](#context-windows-auto-detect--optional-override) below.
3. Writes the combined result into Redis under the key
   `runtimedetect:available` so every control-plane replica returns the same
   `/available` body.

**Failure handling is deliberately soft:** when either upstream fails (or
the Anthropic key is missing/revoked), the previously-cached snapshot keeps
serving. `/available` never 5xx's and agents are never disrupted by a
detection outage — the worst that happens is the PWA picker is one cycle
stale.

## API: `GET /api/v1/available`

Authenticated via the standard `Authorization: Bearer <kyber-api-key>` (same
middleware as the rest of `/api/v1/*`).

**Response shape:**

```json
{
  "claudeCodeVersions": ["2.1.119", "2.1.118", "2.1.117"],
  "models": [
    {
      "id": "claude-opus-4-8",
      "displayName": "Claude Opus 4.8",
      "contextWindow": 1000000,
      "contextWindowKnown": true
    }
  ]
}
```

- `claudeCodeVersions`: newest first; capped at `runtimeDetect.versionLimit`
  entries. The chart's baked-in CC version (`runtime.claudeCode.version`)
  remains the fallback and is unrelated to this list.
- `models[].contextWindow`: the model's context window. Auto-detected from
  the Models API's `max_input_tokens` (kyber#488) — present →
  `contextWindowKnown=true`; absent → the 200K floor with
  `contextWindowKnown=false`. An operator override (see below) takes
  precedence when configured. Treat `contextWindowKnown=false` as "estimate,
  not authoritative" — the PWA renders it as an estimate rather than a
  confident gauge.

## Context windows: auto-detect + optional override

A model's context window is resolved in two layers, highest precedence first:

1. **Operator override** — the `kyber-model-context-windows` ConfigMap
   (`KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP`), resolved by
   `pkg/contextwindowmap`. When it carries an entry for a model ID, that
   value wins and `contextWindowKnown=true`. This is now an **optional
   edge-case override** — no longer a required per-model edit.
2. **Auto-detected `max_input_tokens`** — read off the `/v1/models` list
   response the poller already fetches. When present (`> 0`) the model gets
   its real window with `contextWindowKnown=true`. A brand-new model gets its
   true window the moment Anthropic publishes it, with **no ConfigMap edit**.
3. **Floor fallback** — when neither layer supplies a value, the model lands
   on `DefaultContextWindowFloor` (200K) with `contextWindowKnown=false`.

Before kyber#488 the enrichment loop unconditionally overwrote every model
with the ConfigMap lookup — which returned the floor for any unmapped ID and
thereby *erased* the auto-detected window. The loop now overrides **only when
the ConfigMap has an entry**, so detection and override compose instead of
fighting.

### Pod runtime `[1m]` opt-in follows the same detection snapshot

The per-pod `KYBER_MODEL_CONTEXT_WINDOW` env var that drives start-claude.sh's
`[1m]` suffix (`>= 1_000_000` → append `[1m]`) is sized by the Claude Code
adapter (`pkg/runtimes/claudecode`) in **four layers**, highest precedence
first (kyber#492):

1. **Operator override** — the `kyber-model-context-windows` ConfigMap, via
   `contextwindowmap.Resolver`. Stays on top: explicit human intent, and its
   30s TTL reflects a `kubectl edit` faster than the hourly detection poll.
2. **Detection snapshot** — the same `runtimedetect` snapshot served at
   `/api/v1/available`, via `runtimedetect.SnapshotResolver` (a 30s in-process
   TTL memo + a 2s bounded read over the snapshot cache). A model auto-detected
   at 1M gets `[1m]` at runtime with **no ConfigMap edit and no start-claude.sh
   change**.
3. **`tokenreport.LimitFor`** — the in-Go `knownModels` table.
4. **200K floor** — `LimitFor`'s default for unknown IDs.

This mirrors the poller's own override-on-top-of-detection order, so the gauge
(`/available`) and the runtime (`[1m]`) agree for the new-model case.

**Cold-start / degradation.** The snapshot layer is best-effort and never
blocks pod construction: an empty snapshot (poller not yet run, detection
disabled, or the Anthropic key unconfigured), a cache error (Redis down), a
read timeout, or a model absent / `contextWindowKnown=false` all fall through
to `LimitFor` → floor — identical to the pre-#492 behavior. A brand-new model
gets the floor until the first successful poll populates the snapshot, then
picks up its real window on the next reconcile that re-renders the pod (a
restart or spec change). The bounded read (2s) + TTL memo (30s) keep at most
one cache read per window off the synchronous reconcile path.

**Empty body contract.** When detection is disabled
(`runtimeDetect.enabled=false`), the cache is empty (poller hasn't run
yet), or both upstreams are down on a fresh install, the endpoint returns:

```json
{ "claudeCodeVersions": [], "models": [] }
```

— never 5xx. The PWA renders a "detection unavailable" state from the
empty arrays.

## API: `/api/v1/settings/anthropic-key`

Operator-facing write endpoint for the Anthropic API key the poller uses
for the Models API call. **Same auth as `/api/v1/available`** (Kyber API
key). The key is **never** echoed in any response, even to an
authenticated session.

| Method | Body | Response |
|---|---|---|
| `GET` | — | `{ "configured": true \| false }` |
| `PUT` | `{ "key": "sk-ant-..." }` | `204 No Content` |
| `DELETE` | — | `204 No Content` (clears the key) |

`PUT` patches the `api-key` field of the K8s Secret named by the chart's
`runtimeDetect.existingSecret` (or `{release}-anthropic-key` by default).
Kubelet refreshes the mounted Secret file within ~60s, so a rotation
propagates to the next poll cycle without a control-plane restart.

The handler refuses oversized payloads (>2 KiB) and never logs the key
value. Errors return generic messages so a misformatted body cannot leak
the key into the response.

## Setup

### 1. Generate an Anthropic API key

Create the key in the [Anthropic Console](https://console.anthropic.com/).
The key only needs read access to the Models API — Kyber **never** uses
it for inference. Treat it like a billing credential: this key is held
by the **control-plane** (one per Kyber install, not per-agent), and
anyone who acquires it can list models on the parent Anthropic
organization.

### 2. Enter the key in the PWA

Open the Kyber PWA → **Settings** → **Anthropic API key** card → paste
the key → **Save**. The PWA POSTs `PUT /api/v1/settings/anthropic-key`
and the Settings card refreshes to show **configured**. The poller picks
up the new key on its next cycle (within ~60s of the Secret mount
refresh).

To rotate: enter the new key and click **Replace**. To stop using the
Anthropic API entirely: click **Clear** — `/available` keeps serving the
last known model list until the cache TTL elapses, then surfaces an
empty `models` array.

### 3. (Optional) Pre-seed the key via Helm

For automated installs, set `runtimeDetect.anthropicApiKey=sk-ant-...`
in your values file. The chart renders the key into
`{release}-anthropic-key` at install time; rotation post-install still
goes through the PWA / write API.

For installations that manage the Secret externally (External Secrets
Operator, Sealed Secrets, etc.), set `runtimeDetect.existingSecret` to
the Secret name. The chart will skip its own Secret render; the
operator owns the Secret's lifecycle.

## Chart values reference

```yaml
runtimeDetect:
  # When false, the poller is not registered and /api/v1/available
  # returns the empty contract. Use this on air-gapped installs.
  enabled: true
  # How often to refresh from upstream. Default 1 hour.
  cadenceSeconds: 3600
  # Cap on the number of recent CC versions surfaced through /available.
  versionLimit: 20
  # Operator-supplied Anthropic API key. Prefer entering this via the
  # PWA Settings panel (PUT /api/v1/settings/anthropic-key).
  anthropicApiKey: ""
  # Name of an external Secret holding the api-key field. When set,
  # the chart does NOT render its own anthropic-key Secret.
  existingSecret: ""
```

### Environment variables (read by the control-plane)

| Var | Purpose |
|---|---|
| `KYBER_RUNTIMEDETECT_ENABLED` | `false` disables the poller |
| `KYBER_RUNTIMEDETECT_CADENCE_SECONDS` | Poll interval in seconds |
| `KYBER_RUNTIMEDETECT_VERSION_LIMIT` | Cap on CC versions in `/available` |
| `KYBER_ANTHROPIC_KEY_SECRET_NAME` | Secret name patched by the write API |
| `KYBER_ANTHROPIC_KEY_PATH` | File path read by the poller (default `/etc/kyber-anthropic-key/api-key`) |
| `KYBER_ANTHROPIC_API_KEY` | Env-var override for dev/test (skips the file mount) |

## Multi-replica installs

The poller writes to a single Redis key (`runtimedetect:available`). Every
replica's `/available` handler reads from the same key, so the PWA sees a
consistent answer regardless of which replica it hit. The poller goroutine
runs on every replica; the cache write is idempotent (last-writer-wins).
There is no leader election — replicas converge on identical content
within one cadence.

When Redis is unavailable (dev/test fallback), the poller writes to an
in-memory map. Multi-replica installs MUST configure Redis — a
per-replica in-memory cache would flicker the PWA picker as different
replicas served different snapshots.

## Failure modes

| Condition | Behavior |
|---|---|
| Anthropic key not yet entered | `/available.models` empty; `claudeCodeVersions` populated |
| Anthropic key revoked / wrong | Last-good `models` keeps serving; logs `WARN` per cycle |
| npm registry unreachable | Last-good `claudeCodeVersions` keeps serving |
| Redis down (production) | `/available` returns empty contract on every request |
| Both upstreams down + empty cache (fresh install) | `/available` returns empty contract until first success |
| Poller goroutine panics | Goroutine restarts via controller-runtime manager; cache continues serving last-good |

In every case the response is `200 OK` with the documented shape — the
PWA never has to handle a detection-side 5xx.
