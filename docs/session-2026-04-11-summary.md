# Session Summary — 2026-04-11

Single session that built the Kyber platform from an empty repo to a deployable control plane with a PWA dashboard. Phases A, B, and C complete. 17 of 17 tasks in the implementation plan shipped. Phase D (production packaging) is the remaining work.

## By the numbers

- **3 phases, 17 tasks** shipped
- **35 commits** on `main` of `matty-v/kyber`
- **~13,000 lines** of Go + TypeScript
- **100+ Go tests** passing (unit + envtest integration)
- **25+ review-caught bugs** fixed across the session, including several correctness blockers
- **0 force pushes, 0 amends, 0 `--no-verify`**
- **0 red CI runs** left on `main` — every commit ends green

## What was built (phase by phase)

### Phase A — Foundation (`docs/phase-b-summary.md` has the full B writeup; Phase A was built alongside it)

- **A1** Go monorepo scaffold (`pkg/api/v1` CRD types — Agent + Machine, Makefile, main stubs, controller-gen setup)
- **A2** CI — test workflow (lint + test on PRs), build workflow (main-only, GHCR push gated)
- **A3** Agent base image (Ubuntu 24.04 + overlayfs entrypoint, production-hardened from the spike) + Claude Code runtime image
- **A4** Terraform small profile — single GCE VM with k3s + Postgres + Redis via Helm, state-managed passwords

10 commits. Spec+code review caught a CRD version rendering bug (package name "types" leaked into the version field → renamed to `pkg/api/v1`), the retry backoff was dead code in the scaffold controller, and a subtle overlay mount idempotency bug that would have caused CrashLoopBackOff on container restart.

### Phase B — Core Controllers (`docs/phase-b-summary.md`)

- **B1** Agent Controller — pure-function state machine (22 transitions), pod builder, reconciler with finalizer + 10s/30s/90s retry backoff, envtest integration coverage
- **B2** Session continuity — `BriefStore` interface (memory + Postgres scaffold), internal HTTP API at `:8082`, reconciler writes briefs on all restart paths
- **B3** `RuntimeAdapter` interface + `ClaudeCodeAdapter` — Telegram token now flows via `SecretKeyRef` env var instead of a file on disk (Phase A carry-forward fixed)
- **B4** Scale-to-zero wake — `MessageBuffer` interface (memory + Redis), reconciler drains on Suspended→Running, overrides `shutdown_type` to `wake`
- **B5** Machine Controller — pure state machine (22 transitions), `ComputeAdapter` interface, Mock + GCE scaffold, reconciler with finalizer + preemption detection + same-zone replacement. Also wired `Agent.resolveNodeName` to read `Machine.Status.NodeName`.
- **B6** Node Agent — `/proc` metrics collector, OTEL push (9 gauges), action executor (reboot/stop), privileged DaemonSet manifest

15 commits. The highest-value review catches:

- **B1 patch-base timing bug**: `status.startTime` was silently dropped because the field was mutated BEFORE `client.MergeFrom(obj.DeepCopy())` captured the patch base. Watched for the same pattern in every subsequent reconciler.
- **B1 Restarting infinite loop**: `spec.desiredPhase = Restarting` was never cleared after a restart cycle, so agents would restart forever.
- **B1 retry backoff dead code**: `RetryBackoffDuration()` existed and was unit-tested, but the reconciler never called it. Failed agents would spin-restart at controller speed rather than 10s/30s/90s.
- **B5 replacement retry dead code**: `ActionCreateReplacementInstance` only incremented `ReplacementCount` AFTER a successful create. Repeated failures would loop forever in Replacing because the retry limit was never reached.
- **B5 Stopping phase skipped agent drain**: `ActionStopVM` fired immediately without checking agent count. Spec required draining agents first.
- **B2 non-deterministic "pure" function**: `BuildBrief` called `time.Now()` internally despite being labeled pure. Injected timestamp via `BriefInput.Now`.

### Phase C — User-Facing (`docs/phase-c-summary.md`)

- **C1** Public API core routes — 21 REST endpoints (Machines, Agents, Fleet, Webhooks), API key auth, middleware chain (recover → request-id → logging → auth), webhook wake flow
- **C2** WebSocket events + log/exec proxy — shared `EventBus` using controller-runtime cache informers, chunked HTTP log streaming via `client-go`, exec proxy via `k8s.io/client-go/tools/remotecommand`
- **C3** PWA — React 18 + Vite 5 + TypeScript strict + Tailwind 4, 8 pages, 7 components, 3 hooks, ~2,710 lines TS, embedded in the Go binary via `//go:embed all:pwa_dist`

8 commits. Phase C review catches included:

- **C1 port collision blocker**: metrics server and public API both defaulted to `:8080` — the binary couldn't start in its full configuration. CI didn't catch it because tests use `BuildHandler` directly, not `Start()`. Fix: metrics → `:9090`.
- **C1 timing attacks on secrets**: API key and webhook secret comparisons used `==`. Fixed with `crypto/subtle.ConstantTimeCompare`.
- **C1 `/healthz` behind auth**: probes returned 401 on unauthenticated requests. Fix: reorganized routing so public routes (health, webhooks, PWA) bypass the auth chain.
- **C1 `TestMiddleware_RecoverPanic` didn't test recovery**: the test was a duplicate of the missing-key auth test wearing a different name. Rewrote to actually inject a panicking handler.
- **C1 machine delete with attached agents**: returned 200 instead of 422. Fix: list agents, return 422 with the attached agent list.
- **C2 middleware `Hijack()` missing**: the logging middleware's `responseWriter` wrapper didn't implement `http.Hijacker`, so WebSocket upgrade silently failed through the middleware chain. Implementer self-caught.

## Architecture at the end of the session

```
┌─────────────────────────────────────────────────────┐
│                Control Plane Binary                 │
│                                                     │
│  ┌─────────────────┐  ┌───────────────────┐        │
│  │  Agent          │  │  Machine          │        │
│  │  Controller     │  │  Controller       │        │
│  │                 │  │                   │        │
│  │  - State        │  │  - State machine  │        │
│  │    machine      │  │  - ComputeAdapter │        │
│  │  - Pod builder  │  │    (Mock + GCE)   │        │
│  │  - RuntimeAdap. │  │  - Finalizer      │        │
│  │    (ClaudeCode) │  │  - Preemption     │        │
│  │  - Finalizer    │  │  - Same-zone      │        │
│  │  - Brief writer │  │    replacement    │        │
│  │  - Wake drain   │  │                   │        │
│  └─────────────────┘  └───────────────────┘        │
│                                                     │
│  ┌─────────────────────────────────────────────┐   │
│  │  Public API (:8080)                         │   │
│  │  - 21 REST routes (machines, agents, fleet) │   │
│  │  - WebSocket events (informer fan-out)      │   │
│  │  - Log streaming (chunked HTTP)             │   │
│  │  - Exec proxy (WebSocket + SPDY)            │   │
│  │  - Telegram webhook (wake flow)             │   │
│  │  - Health probes (auth-free)                │   │
│  │  - PWA at / (embedded React SPA)            │   │
│  └─────────────────────────────────────────────┘   │
│                                                     │
│  ┌─────────────────────────────────────────────┐   │
│  │  Internal API (:8082)                       │   │
│  │  - GET /internal/agents/{name}/session-brief│   │
│  └─────────────────────────────────────────────┘   │
│                                                     │
│  Metrics (:9090)  Health probes (:8081)             │
└─────────────────────────────────────────────────────┘

Agents                        Machines
├─ Creating → Starting        ├─ Provisioning → Ready
├─ → Running                  ├─ → Running
├─ → Stopping → Stopped       ├─ → Stopping → Stopped
├─ → Restarting               ├─ → Preempted → Replacing
├─ → Suspended (→ wake)       │       └─ (same-zone)
├─ → Failed (→ retry backoff) └─ → Failed
└─ → Deleted
```

## Ports (final)

| Port | Purpose |
|------|---------|
| `:8080` | Public API + PWA (auth required for `/api/v1/*`, public for `/healthz`, `/webhooks`, `/`) |
| `:8081` | controller-runtime health probes |
| `:8082` | Internal session-brief endpoint (cluster-only) |
| `:9090` | Prometheus metrics |

## Process notes for future phases

**Subagent-driven development worked.** 35 commits, 25+ review-caught bugs, every single task had at least one review-caught bug that required a fix commit. The two-stage review (spec compliance → code quality) caught different classes of issues. Spec review catches scope creep and undocumented deviations. Code quality review catches correctness, security, and design issues. Skipping either misses real bugs.

**Consolidated review worked for the largest tasks.** B5 and C1 used a single combined reviewer pass. Still caught all the blocking issues. Saves ~50% of review time. Not recommended for tasks that change CRDs or interface surfaces — split reviews are better there.

**Patch-base timing bugs were the most common class.** Any time a status field was mutated BEFORE `client.MergeFrom(obj.DeepCopy())`, the field was silently dropped from the patch. This happened twice (B1 StartTime, and almost-again in B5 ReplacementCount — caught by review).

**Dead-code-with-tests was the second most common class.** Unit-tested helpers that were never wired into the reconciler path. B1 retry backoff, B5 replacement retry, C1 dead `patchMachineDesiredPhase`. The unit tests give false confidence — they pass but prove nothing about whether the code is reachable.

## Carry-forwards for Phase D

See `docs/phase-a-summary.md` (doesn't exist — Phase A items are in `docs/phase-b-summary.md`), `docs/phase-b-summary.md`, and `docs/phase-c-summary.md` for the full list. High-priority items:

- **Helm chart** (D1) — the real wiring for the config that's currently hardcoded or stubbed: namespace, image refs, Postgres/Redis connection strings, API key secret, JoinToken/ServerURL for machine provisioning, GHCR image publishing
- **OTEL telemetry** (D2) — wire the control plane (not just the node agent) into the OTEL pipeline with controller metrics + alerts
- **k3d e2e test harness** (D3) — the first real end-to-end test that provisions a machine and runs an agent pod
- **Integration + contract tests** (D4) — docker-compose Postgres + Redis, API contract tests against OpenAPI

Beyond the explicit plan:
- Image publishing pipeline (`kyber/control-plane`, `kyber/node-agent`, `kyber/agent-base`, `kyber/claude-code`) currently has no real push step. The CI `build.yml` has a gated placeholder (`if: false`). D1 needs to fix this.
- `/api/v1/backups` routes from the API spec were never implemented. The plan didn't include them. Needs a C1-followup task or documentation that they're descoped.

## Commit trail

```
a12c09f docs: Phase C completion summary
8223456 feat(pwa): React + Vite + Tailwind dashboard (C3)
6c78c18 feat(api): WebSocket events + log and exec proxy (C2)
cd28796 fix(api): C1 review — port conflict, constant-time secrets, health probes, 422 on attached agents
bf4f74d feat(api): public API core routes (C1)
8491d1f docs: Phase B completion summary
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
abc1926 ci(test): install envtest binaries before running tests
2c28bc9 feat(agent-controller): state machine + pod management (B1)
f14e3f4 fix(infra): code review — password values via tempfile, idempotent installs, hide join token
55e967c fix(infra): spec review — drop extra outputs, clarify post-apply retrieval, document IAM scope
9704367 feat(infra): Terraform small profile (single VM with k3s, Postgres, Redis)
6a980f0 fix(images): code review fixes — mount idempotency, quoted exec args, set -euo pipefail
701d608 fix(claude-code): drop CLAUDE_AUTH_TYPE scope creep, match spec auth block
f529f62 feat(images): agent base image with overlay FS and Claude Code runtime
17e4b75 ci: add test and build workflows
b0b5a76 fix(types): address code review — typed enums, metav1.Duration, spec required
79e917f fix(types): move pkg/types to pkg/api/v1 so CRD version renders as v1
9cd30e9 feat: scaffold Kyber monorepo with CRD types
```
