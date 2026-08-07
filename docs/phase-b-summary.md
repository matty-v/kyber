# Phase B — Core Controllers

Phase B of the Kyber implementation plan (`2026-04-10-kyber-implementation-plan.md`). Shipped 2026-04-11.

## What shipped

| Task | What | Spec |
|------|------|------|
| B1 | Agent Controller — state machine, pod builder, reconciler, control-plane wiring | `2026-04-10-agent-controller-design.md` |
| B2 | Session continuity — brief builder, `BriefStore`, internal HTTP API | same |
| B3 | `RuntimeAdapter` interface + `ClaudeCodeAdapter` | same |
| B4 | Scale-to-zero wake handler — `MessageBuffer` (memory + Redis) | same |
| B5 | Machine Controller — state machine, `ComputeAdapter`, Mock + GCE | `2026-04-10-machine-controller-design.md` |
| B6 | Node Agent — metrics collector, action executor, DaemonSet | `2026-04-10-node-agent-design.md` |

## Commit trail on `main`

```
edc9f93 feat(node-agent): metrics loop + action executor + DaemonSet (B6)
3c74ad5 fix(machine-controller): replacement retry, stopping drain, error propagation (B5 review)
3750a34 feat(machine-controller): state machine + reconciler + ComputeAdapter (B5)
5af09ef feat(agent-controller): wake handler + MessageBuffer for scale-to-zero (B4)
b4daafb fix(agent-controller): rename TelegramToken to TelegramEnabled (B3 review)
ccf86ce feat(agent-controller): RuntimeAdapter interface + ClaudeCodeAdapter (B3)
a1cf903 fix(b2): code review — deterministic BuildBrief, HTTP length guard, MemoryStore copies
37a7213 fix(agent-controller): write brief on operator restart from Failed (B2 review)
a6d127e feat(agent-controller): session continuity — brief builder, store, internal API (B2)
bd9a937 fix(agent-controller): code review — persist startTime, clear Restarting, remove dead code
fab8416 fix(agent-controller): wire retry backoff, log empty registry, pin CI versions
abc1926 ci: wire setup-envtest into test workflow
2c28bc9 feat(agent-controller): state machine + pod management (B1)
```

## Package layout added

```
pkg/
├── api/
│   ├── internal.go           # internal HTTP API (session brief endpoint)
│   └── internal_test.go
├── briefstore/               # B2 — session brief storage
│   ├── store.go              # BriefStore interface + Brief types
│   ├── memory.go             # in-memory impl (tests + dev)
│   ├── postgres.go           # Postgres scaffold (compile-only)
│   └── *_test.go
├── messagebuffer/            # B4 — pending-message buffer for suspended agents
│   ├── buffer.go             # MessageBuffer interface
│   ├── memory.go             # in-memory impl
│   ├── redis.go              # go-redis/v9 impl (compile-only)
│   └── *_test.go
├── adapters/                 # B5 — compute adapters for Machine Controller
│   ├── compute.go            # ComputeAdapter interface + InstanceStatus
│   ├── compute_mock.go       # mock for tests + k3d
│   ├── compute_gce.go        # GCE impl via cloud.google.com/go/compute/apiv1
│   └── *_test.go
├── controllers/
│   ├── agent/                # B1-B4 — Agent Controller
│   │   ├── state_machine.go       # pure function, 22 transitions
│   │   ├── pod_builder.go
│   │   ├── adapter.go             # RuntimeAdapter interface
│   │   ├── adapter_claude_code.go # B3
│   │   ├── session_brief.go       # B2 — BuildBrief + briefInputForEvent
│   │   ├── reconciler.go          # Reconcile + executeAction + writeBrief
│   │   ├── controller.go          # SetupWithManager
│   │   └── *_test.go              # unit + envtest
│   └── machine/              # B5 — Machine Controller
│       ├── state_machine.go       # pure function, 22 transitions
│       ├── reconciler.go
│       ├── controller.go
│       └── *_test.go
└── nodeagent/                # B6 — Node Agent
    ├── metrics.go            # /proc parser
    ├── disk_linux.go         # syscall.Statfs (Linux-only build tag)
    ├── actions.go            # shutdown reboot/stop executor with dry-run
    ├── otel.go               # OTEL SDK init + 9 gauge metrics + push loop
    └── testdata/             # fake /proc fixtures
```

## Design invariants that held up

- **State machines are pure functions.** Both controllers use `NextPhase(current, event) → action` with zero k8s imports. Unit-tested in isolation. Reconcilers execute the returned actions. This separation caught several bugs during review (see below).
- **Adapters are interfaces with mock + real implementations.** Real impls are compile-only in CI. Tests use the mocks. `RuntimeAdapter` has one concrete (`ClaudeCodeAdapter`); `ComputeAdapter` has two (`MockComputeAdapter`, `GCEComputeAdapter`).
- **Brief writing is non-fatal.** All brief store failures are logged and skipped — the init container's `{}` fallback keeps agents functional if the control plane can't serve a brief.
- **Patch-base discipline.** `client.MergeFrom(obj.DeepCopy())` must be captured BEFORE any field mutations. Violating this silently drops the mutated field from the patch diff. B1 had the startTime bug because of this.
- **Status patches over status updates.** Every status change uses `r.Status().Patch(ctx, obj, patch)` for optimistic concurrency, not a full `Update`.
- **Finalizers are added before resource creation and removed after cleanup.** The two-step deletion flow (delete child resources → requeue → remove finalizer) prevents orphans.

## Review-caught bugs (what spec/code review actually found)

Every single B1-B6 task had at least one review-caught bug that required a fix commit. The pattern held — reviews are load-bearing.

**B1:**
- Retry backoff was dead code. `RetryBackoffDuration()` existed and was unit-tested, but the reconciler never called it. Failed agents would spin-restart at controller speed, not 10s/30s/90s.
- `status.startTime` was silently dropped. Mutated before patch-base capture; patch diff came out empty.
- `spec.desiredPhase = Restarting` was never cleared after honoring it. Agents with desiredPhase=Restarting would restart forever.
- `isPodCrashLooping` was dead code under `RestartPolicy: Never`. Pods with that policy never enter CrashLoopBackOff.
- `EventCRDDeleted` / `ActionRunFinalizer` were in the state machine table but unreachable — deletion is handled out-of-band in `handleDeletion`.

**B2:**
- `BuildBrief` called `time.Now()` internally despite being labeled "pure." Fixed by injecting timestamp via `BriefInput.Now`.
- `ActionResetRetryAndCreatePod` (Failed + operator override) skipped brief writing. Fixed by extracting a `writeBrief` helper used by both restart paths.
- `MemoryStore.Get` returned the stored pointer directly, so callers could mutate the stored value. Fixed with shallow copies on both Get and Put.
- `defer resp.Body.Close()` inside a `for` loop scopes to the function, not the iteration. Fixed with explicit close.

**B3:**
- `AgentSecrets.TelegramToken` was documented as "Secret Manager path" but the adapter only checked emptiness and derived the k8s secret name from `agent.Name`. Renamed to `TelegramEnabled bool` with accurate docs.

**B5:**
- **Replacement retry was dead code.** `ActionCreateReplacementInstance` only incremented `ReplacementCount` after a SUCCESSFUL create. Repeated failures would loop in Replacing forever because the retry limit was never reached. Fixed by incrementing via a separate status patch before attempting create.
- **Stopping phase skipped agent drain.** `ActionStopVM` fired immediately without checking agent count. Spec requires draining agents first. Fixed by checking `countAgentPods == 0` in `classifyStopping`.
- **`countAgentPods` swallowed errors.** Silent return of `(0, nil)` on field indexer failure caused false Running → Ready demotions. Fixed by returning errors and requeuing.

## Carry-forwards for Phase C/D

| Item | Where | Addressed by |
|------|-------|--------------|
| Empty `JoinToken` / `ServerURL` stubs in Machine Controller's `buildMachineSpec` | `pkg/controllers/machine/reconciler.go` | D1 (Helm chart) — wire from k8s Secret |
| Redis `MessageBuffer` never connected to real Redis | `cmd/control-plane/main.go` instantiates the in-memory impl | D1 (Helm chart) |
| Postgres `BriefStore` is compile-only | `pkg/briefstore/postgres.go` | D1 (Helm chart) |
| `ghcr.io/matty-v/kyber-node-agent:latest` image doesn't exist | `deploy/helm/kyber/templates/node-agent/daemonset.yaml` | D1 — image build + push |
| GCE `lookupInstanceByID` is O(n) across zones | `pkg/adapters/compute_gce.go` | Future optimization (not blocking) |
| Telegram token still visible in `/proc/$pid/cmdline` once Claude Code starts | `images/claude-code/start-claude.sh` | Upstream Claude Code CLI limitation |
| Agent Controller's `resolveNodeName` falls back to `spec.machine` if Machine CRD not found | `pkg/controllers/agent/reconciler.go` | Intentional — preserves k3d/test compatibility |

## Test pyramid at end of Phase B

```
Unit tests:           ~70  — state machines, pod builder, brief builder, adapters, metrics collector
Integration tests:    ~30  — envtest with real CRDs, reconciler lifecycle, finalizer flows
Contract compile:      2   — var _ ComputeAdapter = (*GCEComputeAdapter)(nil); same for Redis + Postgres
E2E (k3d):             0   — deferred to D3
Manual on-host:        1   — docker smoke test for overlay entrypoint (A3, still valid)
```

All 100+ tests pass in ~65 seconds on `make test`.

## Process notes

Phase B used the `superpowers:subagent-driven-development` skill. Per-task flow: implementer (sonnet) → spec reviewer (sonnet) → code-quality reviewer (sonnet via `superpowers:code-reviewer` agent) → fix dispatches for findings → verify CI green → next task. B5 used a consolidated spec+code review pass in one subagent to save time on the largest Phase B task.

Every task's fix commit landed green on CI. No force-pushes, no `--amend`, no `--no-verify`.
