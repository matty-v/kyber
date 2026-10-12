# Model discovery and context windows

Kyber discovers model choices from each authenticated Agent runtime. The runtime reports its catalog through the status sidecar, and `GET /api/v1/agents/{name}/models` serves that Agent's choices. A new Agent returns `409` until its runtime reports a catalog. Entitlements may differ between Agents, so the model picker does not borrow another Agent's catalog.

The public version poller fetches npm harness versions. A legacy Anthropic platform-key detection path remains in the codebase but is not wired in production; see [runtime discovery](../runtime-detection.md).

## Context-window resolution

The context-window gauge and Claude `[1m]` runtime opt-in need a model's token limit. Kyber resolves the limit from, in order:

1. The operator's `kyber-model-context-windows` override ConfigMap.
2. The Agent runtime's authenticated model catalog.
3. The detection snapshot, when available.

An unknown window is shown as unknown rather than as a confirmed capacity. The pod-construction path uses a memoized, bounded snapshot lookup so a slow cache does not stall reconciliation. A blank `spec.model` lets the harness choose; a transcript-backed observation updates `status.currentModel` without pinning `spec.model`.

## Usage metadata

Model onboarding does not require a price table. Harness reporters record observed model IDs, token classes, configured routes, and any known upstream provider as local per-call evidence. The authenticated [usage API](metrics-data-flow.md#token-usage) exposes that data for downstream analysis. Kyber does not calculate dollar costs or install a LiteLLM dataset.

## Code references

- `pkg/api/routes_agent_models.go` — Agent-scoped model catalog.
- `pkg/runtimedetect/` — version detection and bounded snapshot reads.
- `pkg/contextwindowmap/` — operator context-window overrides.
- `pkg/runtimes/claudecode/adapter.go` — Claude runtime context-window environment.
- `pkg/tokenreport/` and `pkg/usagestore/` — local usage evidence and durable storage.

See [the operator telemetry guide](../operator/telemetry.md) for API and retention behavior.
