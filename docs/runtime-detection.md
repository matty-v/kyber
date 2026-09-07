# Runtime version and model discovery

Kyber separates public harness-version discovery from authenticated per-agent
model discovery. The production wiring is in `cmd/control-plane/main.go`;
`pkg/runtimedetect` provides the caches and public npm poller.

## What the poller does

When `runtimeDetect.enabled` is true, the control plane polls npm for stable
Claude Code and Codex versions. `runtimeDetect.cadenceSeconds` defaults to 3600
and `versionLimit` to 20. Failed upstream fetches preserve previously cached
version lists. Redis shares the snapshot between replicas; the development
fallback is process-local memory.

Production does not wire the poller's legacy platform-level Anthropic client or
key source. The legacy client, settings endpoint, and chart key configuration
still exist for compatibility, but saving a platform Anthropic key does not
activate model discovery in the current production wiring.

## API: `GET /api/v1/available`

This authenticated route returns `claudeCodeVersions`, `codexVersions`, `models`,
and `codexModels`. The version lists support harness pickers. The model lists
are compatibility projections of reported catalogs, not a replacement for the
agent-specific endpoint. If detection is disabled or its cache is unavailable,
the route returns empty lists rather than an upstream error.

## Authenticated model catalogs

`GET /api/v1/agents/{name}/models` returns only that agent's reported catalog.
The runtime discovers models after authentication and submits bounded non-secret
metadata through its status sidecar to the internal `runtime-catalog` route.
Provider credentials stay in the pod.

- Claude Code discovery uses the agent's Anthropic credential. Its report must
  carry an authoritative context window for each model.
- Codex discovery uses native app-server `model/list`. A model without an
  authoritative window remains unknown in that catalog; the active session's
  transcript-backed context window is authoritative for usage reporting.
- A missing first report returns `409 authentication_required`. Missing or
  unavailable catalog storage returns `503 catalog_unavailable`. Neither case
  borrows another agent's model list.
- The internal route checks the reported runtime against the Agent, limits the
  body to 128 KiB and catalog to 100 models, and validates known window sizes.

An empty Agent model setting lets the harness choose its own default. The first
observed concrete model is recorded in status without turning it into a spec pin.

## Context windows: auto-detect + optional override

The authenticated catalog and active-session observations above describe current
picker/usage behavior. The Claude pod-construction path also has an operator
context-window override map (`runtimeDetect.modelContextWindows`) and a cached
snapshot resolver. These support the explicit large-context launch option.
Compatibility resolvers retain legacy known-model/floor behavior; do not copy
that fallback into authenticated Codex catalog reporting.

### Pod runtime `[1m]` opt-in follows the same detection snapshot

The Claude adapter resolves `KYBER_MODEL_CONTEXT_WINDOW` from the override and
available snapshot before its legacy fallback. Its launch script uses the
resolved value to select the large-context suffix. Inspect
`pkg/runtimes/claudecode/adapter.go`, `pkg/contextwindowmap`, and
`pkg/runtimedetect/snapshot_resolver.go` when changing this path.

## Setup

Keep runtime detection enabled for npm version discovery. Authenticate each
agent and wait for its own model report before using its model picker. A
platform Anthropic key is not required for this path.

## Chart values reference

| Value | Default | Meaning |
|---|---|---|
| `runtimeDetect.enabled` | `true` | register public version polling/cache integration |
| `runtimeDetect.cadenceSeconds` | `3600` | polling interval |
| `runtimeDetect.versionLimit` | `20` | recent stable versions per harness |
| `runtimeDetect.modelContextWindows` | map | explicit context-window overrides |
| `runtimeDetect.anthropicApiKey` / `existingSecret` | empty | retained platform key configuration; not wired into production polling |

## Multi-replica installs

Redis provides shared snapshots and per-agent catalogs. In-memory fallbacks are
for development and do not coordinate independent replicas. Model reports are
scoped to an agent; a successful report from one agent does not authenticate
another agent or fill its catalog.

## Failure modes

| Symptom | Check |
|---|---|
| Version lists empty | detection enabled, npm access, snapshot cache |
| Agent model picker pending | that agent's authentication and runtime catalog report |
| Catalog unavailable | runtime-detection cache configuration and backend health |
| Codex context window unknown | active session token metadata; do not invent a catalog window |
| Saving platform Anthropic key changes nothing | current production discovery is per-agent |

See [runtimes](runtimes.md), [model onboarding](architecture/model-onboarding.md),
and [the harness contract](architecture/agent-harness-contract.md).
