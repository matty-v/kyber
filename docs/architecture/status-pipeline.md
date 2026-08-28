# Status pipeline — agent pod observability

This doc describes how status / health / activity / metric signals flow
from inside agent pods to the Kyber control plane and out to the PWA.
Read it before adding a new runtime (Codex, OpenClaw, Hermes, etc.) or
a new in-pod signal source.

Spec: kyber#247 epic. Phase A foundation: kyber#248. Phase B (Claude
Code activity): kyber#249.

## The shape

```
┌────────────────────── Agent Pod ───────────────────────┐
│                                                        │
│  ┌─ runtime container ─────────────┐                  │
│  │                                 │                  │
│  │  Claude Code / Codex /          │                  │
│  │  OpenClaw / Hermes              │                  │
│  │                                 │                  │
│  │  ┌──────────────────────────┐   │                  │
│  │  │ runtime-specific         │   │                  │
│  │  │ in-pod binary            │   │                  │
│  │  │  (e.g. kyber-token-      │   │                  │
│  │  │   reporter for Claude)   │   │                  │
│  │  │                          │   │                  │
│  │  │  - tails the runtime's   │   │                  │
│  │  │    transcript / log /    │   │                  │
│  │  │    state file            │   │                  │
│  │  │  - emits statusEvent     │   │                  │
│  │  │    JSON to localhost     │   │                  │
│  │  └──────┬───────────────────┘   │                  │
│  │         │                       │                  │
│  └─────────┼───────────────────────┘                  │
│            │ POST http://127.0.0.1:8091/event         │
│            ▼                                           │
│  ┌─ kyber-status-sidecar (platform) ────────────┐    │
│  │                                              │    │
│  │  - runs heartbeat loop (15s)                 │    │
│  │  - samples pod CPU/memory + /persist usage   │    │
│  │  - listens on :8091 for in-pod events:       │    │
│  │      POST /event          → status-event     │    │
│  │      POST /token-usage    → token-usage      │    │
│  │      POST /refresh-token  → refresh-token    │    │
│  │      POST /runtime-catalog → runtime-catalog │    │
│  │  - applies agent identity + pod-token auth   │    │
│  │  - forwards to control plane                 │    │
│  └────────────┬─────────────────────────────────┘    │
│               │                                       │
└───────────────┼───────────────────────────────────────┘
                │ POST /internal/agents/{name}/{status-event|token-usage|refresh-token}
                ▼
        ┌─ control plane ──────────────┐
        │                              │
        │  - patches Agent.status.     │
        │    activity.{state,          │
        │    lastActivityAt,           │
        │    lastHeartbeatAt,          │
        │    resources}                │
        │                              │
        │  - serializes to wire on     │
        │    GET /api/v1/agents/{name} │
        └─────────┬────────────────────┘
                  │
                  ▼
            PWA (AgentList dot, AgentDetail badge)
```

## Two trust boundaries

1. **Runtime container**: arbitrary code. Has filesystem access to its
   own transcripts / logs. Pushes events to the local sidecar over a
   loopback HTTP call. Does NOT know the control-plane URL or pod token.

2. **Status sidecar (platform)**: Kyber-built, pinned image. Owns the
   onward connection to the control plane (URL, auth, retry, future
   batching). The only outbound HTTP from the pod's status pipeline.

Per kyber#257 (Phase C2 of the observability epic), every runtime
in-pod path rides this same conduit: activity events on `/event`,
context-budget snapshots from `kyber-token-reporter` on `/token-usage`,
OAuth credential rotations from the credential syncer on `/refresh-token`,
and authenticated model catalogs on `/runtime-catalog`. Claude Code queries
Anthropic with its own OAuth token or API key; Codex asks its authenticated
app-server. Only non-secret model metadata crosses the sidecar. One egress,
one auth path, one place to add future
cross-cutting concerns (rate-limiting, batching, signature verification).
Claude Code refreshes the upstream catalog hourly but replays the cached
metadata every 30 seconds, allowing an in-memory control-plane cache to recover
quickly after a restart without multiplying provider traffic.
Claude catalog entries must include a provider-reported, positive context
window below Kyber's defensive upper bound. Codex app-server's `model/list`
schema does not publish context windows, so its catalog keeps them explicitly
unknown; the active model's rollout `token_count.context_window` is the
authoritative source for its token budget. Kyber never substitutes a guessed
window. Catalogs are stored under separate per-agent keys so concurrent reports
cannot overwrite public harness discovery or leak one user's entitlements to
another.

The only remaining direct-to-control-plane path from an agent pod is
`start-claude.sh`'s boot-time OAuth push, which runs before the sidecar
is reachable; that migration is tracked separately.

The boundary matters because runtime images can update independently
from the platform; an old runtime binary should keep working with a new
sidecar, and vice versa, as long as the wire shape (statusEvent) is
preserved.

## The wire shape — `statusEvent`

Every event — heartbeat from the sidecar itself, activity from the
runtime binary, future health/metric events — is the same JSON shape:

```json
{
  "type": "heartbeat" | "activity" | <future kind>,
  "at":   "2026-05-03T22:45:00Z",   // RFC3339, when the event was generated
  "state": "working" | "idle" | "unknown"  // only for type=activity (kyber#249)
}
```

The control plane patches `Agent.status.activity` based on type:

- `heartbeat` → `lastHeartbeatAt`
- `activity` → `lastHeartbeatAt` + `lastActivityAt` + `state`
- `resource_usage` → `lastHeartbeatAt` + latest CPU/memory/disk sample
- unknown type → silently dropped (forward-compat: newer runtimes can
  emit new event kinds without breaking older control planes)

Token snapshots also carry the concrete model parsed from the runtime's
transcript. The control plane persists that observation as
`Agent.status.currentModel`. This is especially important when `spec.model` is
empty: the spec continues to mean “use the harness default,” while status and
the PWA can still show which model the harness actually selected.

Resource usage is best-effort and sampled on the sidecar heartbeat tick. The
pod-level cgroup provides CPU and memory usage/limits. Disk total always comes
from the Agent's requested allocation, passed to the sidecar as
`KYBER_AGENT_DISK_BYTES`. For a whole-filesystem `/persist` mount, used bytes
come from `statfs`. For a bind-mounted directory (including k3s `local-path`),
the sidecar walks only `/persist` every five minutes in a throttled background
goroutine and reuses the cached result between walks. `diskUsageMethod`,
`diskUsageState`, and `diskUsedSampledAt` expose that distinction and staleness.
Unreadable paths are skipped, logged, and reported as a `partial` sample; that
usable lower bound may assert the 90% reserve but cannot clear an existing
reserve because skipped paths may contain the missing bytes. A pending or
failed walk never blocks heartbeat delivery or clears an existing
disk-exhaustion marker. The control plane keeps only the latest sample in
`status.activity.resources`, while the sidecar emits the same values as OTel
gauges. Sampling failures do not affect heartbeat or readiness.

See `pkg/api/internal_status.go` for the handler; `pkg/api/v1/agent_types.go`
for the CRD shape.

## Adding a new runtime

For a new runtime (say Codex), three places change:

### 1. Runtime image gains an activity binary

Build a small Go binary in the runtime's image. Mirrors
`cmd/token-reporter/` and `pkg/tokenreport/activity.go` for Claude Code.
Expected behavior:

- On a tight tick (1s recommended), inspect the runtime's transcript /
  log / state file to determine the current activity state
- Debounce: only POST when the state changes from the last-pushed value
- POST to `http://127.0.0.1:8091/event` with the `statusEvent` shape
  and `type: "activity"`
- Never crash the agent pod on errors — log + continue

The detector lives in the runtime image rather than the platform sidecar
because the runtime's transcripts are inside the agent's
chroot/overlay-mounted filesystem; the sidecar can't see them from a
separate container.

### 2. (Optional) Runtime adapter implements `runtimes.Probe`

If the runtime emits cross-runtime signals that DON'T need filesystem
access (process health from `/proc`, OTel metrics, etc.), put them in
the sidecar's Probe via `pkg/runtimes/<type>/probe.go`. The sidecar's
runtime registry (kyber#250) dispatches via blank import.

For runtimes that only need filesystem-bound signals (transcript
parsing), the in-pod-binary path (1) is sufficient — the Probe
implementation can be a no-op.

### 3. PWA — usually no change

PWA renders `Agent.status.activity` directly via `AgentActivityDot` and
`AgentActivityBadge`. As long as the runtime emits standard `working` /
`idle` states, the PWA picks them up automatically.

## Why this shape

Earlier designs in #247/#249 had the sidecar's `Probe` directly tail
the runtime's JSONL transcript. That fell apart on contact with reality:

- Containers in the same pod have independent image filesystems by default
- The agent runtime runs in a chroot/overlay (whole-disk-persistence,
  kyber#135). Mounting the PVC in the sidecar permits volume-level `statfs`,
  but does not reproduce PID 1's chroot/bind namespace or expose transcript
  paths as the harness sees them without `nsenter` + privileged

Pivoting to "runtime-specific binary in the runtime image, pushing to a
runtime-agnostic sidecar over localhost" (Matt 2026-05-03) gave us:

- Filesystem access happens in the right namespace
- New runtimes can ship their own detector without the sidecar caring
  what runtime they are
- Single egress path from the pod (sidecar) for future auth /
  rate-limit / batching logic
- Wire-shape stability: the sidecar's heartbeat already used
  `statusEvent`; runtime binaries reuse it verbatim

## Runtime harness usability

Runtime startup reports an executable/version probe through the existing
`/runtime-version` sidecar route before it runs any authentication command. A
failed probe sends the runtime name, `usable:false`, and a sanitized diagnostic
capped at 300 bytes, then exits 43. The controller maps that exit to
`BrokenRuntime` instead of `NeedsAuth` or the generic restart path. A successful
probe reports `usable:true` and clears `RuntimeUnusable`.

## Failure modes and what they look like

| Failure | Symptom |
|---|---|
| Runtime binary not running | `Agent.status.activity.state` stays unset; PWA shows no dot |
| Runtime binary running, sidecar localhost listener down | Runtime binary's POST to `127.0.0.1:8091` fails with connection refused; runtime binary logs error and retries on next tick |
| Sidecar localhost listener up, control plane unreachable | Runtime binary POST returns 502 from the sidecar's `/event` handler; sidecar's heartbeat loop also logs failures; PWA shows stale `lastHeartbeatAt` |
| Control plane up, agent CR missing | Control plane returns 404; sidecar logs + drops the event |
| Agent is not authenticated | No per-agent catalog is reported; Change model asks the operator to authenticate the agent |

## Related

- **kyber#247** — parent observability epic
- **kyber#248** — Phase A: sidecar foundation (heartbeat + container injection + wire endpoint)
- **kyber#249** — Phase B: Claude Code activity probe + PWA indicator (closes kyber#174)
- **kyber#250** — runtime registry refactor (`pkg/runtimes/<type>/`); the `Probe` interface mentioned above lives here
- `pkg/api/internal_status.go` — control-plane endpoint
- `pkg/api/v1/agent_types.go` — `ActivityStatus` CRD field
- `cmd/status-sidecar/main.go` — sidecar binary
- `cmd/token-reporter/main.go` + `pkg/tokenreport/activity.go` — Claude Code activity detector
- `packages/pwa-views/src/components/AgentActivityDot.tsx`, `AgentActivityBadge.tsx` — PWA renderers
