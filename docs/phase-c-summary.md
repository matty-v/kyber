# Phase C — User-Facing

Phase C of the Kyber implementation plan. Shipped 2026-04-11. Builds on Phase B (core controllers) and Phase A (scaffold).

## What shipped

| Task | What | Spec |
|------|------|------|
| C1 | API Module core routes — machine/agent/fleet/webhook/auth/middleware | `2026-04-10-api-module-design.md` |
| C2 | API Module WebSocket events + log streaming + exec proxy | same |
| C3 | PWA — React + Vite + TypeScript + Tailwind dashboard, embedded in the Go binary | `2026-04-10-pwa-design.md` |

## Commit trail on `main`

```
8223456 feat(pwa): React + Vite + Tailwind dashboard (C3)
6c78c18 feat(api): WebSocket events + log and exec proxy (C2)
cd28796 fix(api): C1 review — port conflict, constant-time secrets, health probes, 422 on attached agents
bf4f74d feat(api): public API core routes (C1)
8491d1f docs: Phase B completion summary
```

## API surface (complete after Phase C)

**Public routes** (API key auth via `Authorization: Bearer <key>` or `?token=<key>` for WebSocket):

```
Machines
  POST   /api/v1/machines
  GET    /api/v1/machines
  GET    /api/v1/machines/{name}
  POST   /api/v1/machines/{name}/start
  POST   /api/v1/machines/{name}/stop
  POST   /api/v1/machines/{name}/reboot
  DELETE /api/v1/machines/{name}
  GET    /api/v1/machines/{name}/logs      # chunked HTTP stream
  GET    /api/v1/machines/{name}/exec      # WebSocket

Agents
  POST   /api/v1/agents
  GET    /api/v1/agents
  GET    /api/v1/agents/{name}
  PATCH  /api/v1/agents/{name}
  POST   /api/v1/agents/{name}/start
  POST   /api/v1/agents/{name}/stop
  POST   /api/v1/agents/{name}/restart
  POST   /api/v1/agents/{name}/suspend
  POST   /api/v1/agents/{name}/set-model
  DELETE /api/v1/agents/{name}
  GET    /api/v1/agents/{name}/logs        # chunked HTTP stream
  GET    /api/v1/agents/{name}/exec        # WebSocket

Fleet
  GET    /api/v1/fleet
  GET    /api/v1/fleet/summary
  GET    /api/v1/events                    # WebSocket — CRD change stream
```

**Public (no auth):**

```
GET    /healthz
GET    /readyz
POST   /webhooks/telegram/{agent-name}     # secret header auth
GET    /                                   # PWA
GET    /assets/*, /manifest.json, ...      # PWA static assets
```

**Internal (cluster-only, port `:8082`):**

```
GET    /internal/agents/{name}/session-brief
```

## Ports (final layout)

| Port | Bound to | Purpose |
|------|----------|---------|
| `:8080` | `api.Server` | Public API + PWA |
| `:8081` | controller-runtime | Health probes (healthz / readyz) |
| `:8082` | `api.InternalServer` | Session brief endpoint for init containers |
| `:9090` | controller-runtime metrics | Prometheus metrics |

## PWA structure

8 pages, 7 components, 3 hooks, 3 libs — ~2,700 lines of TypeScript.

```
pwa/src/
├── main.tsx, App.tsx, index.css
├── lib/              # api.ts (typed fetch), websocket.ts, types.ts
├── components/       # Layout, StatusBadge, Card, Button, ConfirmDialog, LogViewer, ExecTerminal
├── pages/            # FleetOverview, MachineList, MachineDetail, AgentList, AgentDetail, CreateMachine, CreateAgent, Settings
└── hooks/            # useAPI (react-query wrapper), useWebSocket (shared event bus)
```

**Stack:** React 18, Vite 5, TypeScript 5 (strict), Tailwind 4 via `@tailwindcss/vite`, `@tanstack/react-query` v5, `react-router-dom` v6, `@xterm/xterm` for the exec shell, `vite-plugin-pwa` for the service worker.

**Auth:** API key stored in localStorage, injected as `Authorization: Bearer <key>` on REST calls and `?token=<key>` on WebSocket connections (browsers can't set headers on WS upgrades).

**Embedding:** The Vite build output (`pwa/dist/`) is copied to `pkg/api/pwa_dist/` via `make pwa-build`, then embedded into the Go binary via `//go:embed all:pwa_dist`. A committed `.gitkeep` placeholder at `pkg/api/pwa_dist/.gitkeep` lets `go build` succeed without running the PWA build first (useful for CI and Go-only development). `make pwa-build` overwrites the placeholder with real assets.

## Bundle size

- JS: 552 KB uncompressed, ~150 KB gzipped (xterm.js contributes ~300 KB)
- CSS: 26 KB

## Review-caught bugs in Phase C

**C1 — code review caught 3 blocking issues:**

- **Port collision `:8080`** — metrics server and public API both tried to bind `:8080`. Binary couldn't start in the full configuration. Fix: metrics moved to `:9090`.
- **Non-constant-time secret comparison** — `auth.go` and `routes_webhooks.go` used direct string equality on the API key and webhook secret. Fixed with `crypto/subtle.ConstantTimeCompare`.
- **`TestMiddleware_RecoverPanic` didn't test what it claimed** — the test was a duplicate of the missing-key auth test wearing a different name. Fix: rewrote the test to actually inject a panicking handler.

**C1 — spec compliance:**

- `/healthz` and `/readyz` were inside the auth middleware chain, returning 401 on unauthenticated probes. Fix: reorganized the handler so public routes (health, webhooks, PWA) route before the auth chain, and protected routes (`/api/v1/*`) run through the auth chain.
- `DELETE /api/v1/machines/{name}` didn't check for attached agents. Fix: list agents with `spec.machine == name` and return 422 if any attached.
- `/api/v1/backups` routes from the spec were not implemented — documented as deferred to a future task.
- `telegramToken` string in the spec became `telegramEnabled` boolean in the implementation (matches the CRD rename in B3 code review). Implementation is correct; the spec example is outdated.

**C2 — subtle bug self-caught by implementer:** the logging middleware's `responseWriter` wrapper didn't implement `http.Hijacker`, so the WebSocket upgrade failed silently inside the middleware chain. Added `Hijack()` passthrough.

## Carry-forwards from Phase C to Phase D

| Item | Where | Addressed by |
|------|-------|--------------|
| `make pwa-build` isn't wired into the default `make build` — CI builds Go and PWA in parallel jobs | `Makefile`, `.github/workflows/test.yml` | D1/D3 when the release pipeline wires the deployable binary |
| Bundle size (552 KB) not code-split | `pwa/vite.config.ts` | Future optimization |
| 6 `npm audit` vulnerabilities in transitive dev deps (vite-plugin-pwa / workbox) — no production exposure | `pwa/package.json` | Upstream dependency updates |
| PWA has no unit or component tests | `pwa/` | Nice-to-have, low priority |
| `/api/v1/backups` not implemented — spec defines it, plan didn't include it in C1 | spec reconciliation | Future task |
| Exec shell integration test (real pod, stdin/stdout round-trip) | `pkg/api/routes_exec.go` | D3 k3d e2e harness |

## State of Kyber at the end of Phase C

- Controllers compile and all tests pass (100+ tests)
- Public API is complete with 21 routes + 3 streaming endpoints + 1 WebSocket event stream
- PWA is complete with 8 pages, builds cleanly, embeds into the Go binary
- Auth works on both REST and WebSocket
- Webhook handler completes the wake flow (Telegram → buffer → patch desiredPhase → drain on next reconcile)
- Binary starts cleanly with no port conflicts

**Missing for production:** Helm chart (D1), OTEL telemetry wiring (D2), e2e test harness (D3), integration + API contract tests (D4). The Phase D tasks finish the production-readiness story — image builds + push, k8s manifests, Terraform → Helm → agents running in a real cluster.
