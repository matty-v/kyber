# MAT-7 — harness extension boundary review

Status: proposed refactoring; awaiting Matt's architectural decision.
Date: 2026-09-06. Source baseline: 20b21f9.
Contract: [v1 draft](../architecture/agent-harness-contract.md).

## Finding

The existing registry and pod adapter are useful extension points, but adding
an adapter is insufficient to integrate a new harness. Authentication,
transcript handling, receipt admission, catalogs and UI policy still select
Claude Code or Codex in shared code. The new reusable adapter checks pass for
a minimal third adapter, while the production receipt hook would reject its
runtime ID. That is a concrete boundary gap, not a request to replace the
architecture wholesale.

## Coupling map

| Priority | Evidence | Impact | Proposed boundary |
|---|---|---|---|
| P1 | `images/agent-base/scripts/kyber-task-receipt` explicitly permits only `(claude-code|codex)` | A registered new harness cannot submit a valid acceptance receipt through the shared hook | Validate syntax in the shared hook; authenticate and resolve runtime against the Agent and enabled registry at the receiving boundary. Never accept arbitrary runtime claims merely by removing the whitelist |
| P1 | `pkg/api/routes_agents.go` auth validation/Secret creation, `routes_oauth.go`, `routes_codex_auth.go`, `routes_codex_device_status.go`; `reconciler.go:codexDeviceAuthPending` | New auth flows require controller/API branches; pending login and successful auth differ | Runtime-owned auth descriptor/strategy consumed by generic orchestration; retain existing endpoints and wire values as compatibility facades |
| P1 | `runtime.go` optional nil commands, boot sentinels, Jobs UI gating, `taskdispatch/cancellation.go` | No common distinction between supported and available | Versioned declared feature vocabulary plus bounded observed availability and reason codes; shared consumers gate operations from it |
| P2 | `reconciler.go:isOAuthRefreshFailure`, Claude boot exit 2 for provider/rotation failures | Network/synchronization failures can become human reauth conditions | Adapter-specific signal translation into existing platform failure categories; add explicit sync/service failures where approved; no lifecycle state-machine rewrite |
| P2 | `transcript_tailer.go:transcriptRoots`, `session_saver.go` hardcoded roots and jq parsers | New harness silently inherits Claude paths/parsing | Runtime-owned transcript/recall descriptors or bounded parser implementations; platform retains scheduling/storage and forwarding |
| P2 | `pkg/api/internal.go` catalog accepts only two names and has Claude-specific context rules; `model_validation.go`; PWA `lib/models.ts` | Registry additions do not extend model discovery | Registered runtime catalog semantics and validation; unknown context remains unknown |
| P2 | PWA `wizard/AuthSection.tsx`, `wizard/validation.ts`, `CreateAgent.tsx`, `AgentDetail.tsx` | UI selects flows by runtime name | Declarative auth flow metadata with bounded known UI interaction kinds; do not invent an arbitrary server-driven form engine |
| P2 | `pod_builder.go` token mount/refresh URL and Claude boot direct credential push | Documented sidecar-only ideal has a bootstrap exception | Preserve current path initially; separately decide sidecar readiness/boot ordering before removing the direct rotation path |
| P3 | `RuntimeRepair`, `HelmImageKey`, runtime default version switches, image/build wiring | Existing npm/image conventions leak beyond the adapter | Move runtime metadata to registry entries; keep explicit imports/build/release wiring as deliberate integration steps |
| Decision | `routes_agent_comms.go:validateTelegramAuth` rejects API-key + Telegram | Existing compatibility restriction may outlive its original transport rationale | Preserve until a scoped test/design decision establishes desired support; no incidental policy change during refactor |

Names in provider-owned implementations are expected. Names in generic
admission, lifecycle, dispatch and UI decision paths are the extension targets.
Moving strings to a different shared switch is not sufficient.

## Recommended implementation slices

1. **Declare support without changing behavior.** Introduce small runtime-owned
   descriptors for identity/profile, supported auth modes and feature semantics.
   Keep adapters for pod assembly. Add common capability status with explicit
   unknown/unavailable reasons, version compatibility and negative tests. The
   exact API/schema needs review; the test fixture format is not that schema.
2. **Make feature consumers generic.** Use declarations plus boot observations
   for session/job/task operations and operator discovery. Replace receipt
   name-whitelisting with authenticated registered-runtime validation. Prove a
   bootable fake harness can traverse dispatch/acceptance and fail safely when
   missing hooks, using the same path as both real integrations.
3. **Move auth knowledge behind an owned strategy.** Preserve both auth modes
   and existing endpoints/Secret layouts while relocating provider-specific
   provisioning and pending-login interpretation. Test credential rotation,
   stale-bootstrap protection, reauth, outages, and no billing fallback for
   both modes. Additive discovery can drive UI without breaking older clients.
4. **Extract observation/packaging metadata.** Move transcript roots/parser
   selection, model catalog rules, image/default-version and repair metadata
   behind declared interfaces. Keep transport/storage/authorization shared.
5. **Verify migration and publish.** Run contract regression checks per slice,
   then exact-version live dev tests for both auth modes and both harnesses.
   Publish v1 only after required gaps close; optional unsupported states remain
   honest. Keep old fields/endpoints through a documented deprecation period.

## Decisions requested

Approve this incremental registry/descriptor/strategy approach, retaining the
current tmux profile and existing public API compatibility. This adds explicit
extension points without a new plugin loader, universal SDK or generic model
execution loop. Detailed wire/CRD changes will be made concrete in the first
slice before approval where required by repository policy.

Keep Telegram/API-key policy and the Claude direct bootstrap credential path
unchanged during the initial structural migration. Resolve each in its own
scoped decision with behavioral evidence; do not equate preserving an exception
with satisfying the final normalized failure/availability contract.

## Evidence and limits

The new adapter suite covers both real adapters and a registry fixture; the new
receipt suite executes actual shared script behavior for both real runtime IDs.
Existing credential/dispatch tests supply regression coverage. These are
fixture tests, not live CLI certification. Full validation results live in the
execution plan. This review does not assert complete removal of coupling or
completion of MAT-7; those require approved implementation and live evidence.
