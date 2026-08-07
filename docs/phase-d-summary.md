# Phase D — Production Readiness

Phase D of the Kyber implementation plan. Shipped 2026-04-11. The final phase. After D4, the 17-task Kyber implementation plan is complete end-to-end.

## What shipped

| Task | What | Spec |
|------|------|------|
| D1 | Helm chart — full `deploy/helm/kyber/` with Chart.yaml, values.yaml, templates, Postgres + Redis sub-charts | `2026-04-10-infra-and-helm-design.md` |
| D2 | Telemetry — OTEL SDK init, 7 controller metrics, alert sinks | (integrated across specs) |
| D3 | E2E test harness — k3d-based integration tests + CI workflow | `2026-04-10-e2e-test-harness-design.md` |
| D4 | Integration + contract tests — Postgres/Redis roundtrip, OpenAPI contract validation | (plan section) |

## Commit trail on `main`

```
56a9b33 feat(tests): integration + API contract tests (D4)
0ea283c fix(control-plane): register healthz/readyz handlers (D3 iter)
f157cee fix(e2e): Helm namespace double-create (D3 iter)
c0285e4 fix(e2e): chdir to repo root in TestMain (D3 iter)
b11d87f fix(e2e): golang:1.25 Dockerfile + go-version (D3 iter)
3856e00 feat(e2e): k3d-based end-to-end test harness (D3)
c31b7a6 feat(telemetry): OTEL SDK + controller metrics + alert sinks (D2)
d7c9061 fix(ci): YAML parse error in build.yml (D1 followup)
3c11665 feat(helm): Helm chart for Kyber control plane + node agent (D1)
c2285e7 docs: session summary covering Phases A, B, C
```

## D1 — Helm Chart

**Files created:**
- `deploy/helm/kyber/Chart.yaml` — `apiVersion: v2`, version 0.1.0, Postgres + Redis as optional sub-chart dependencies
- `deploy/helm/kyber/values.yaml` — complete values surface: image refs, API secrets, k3s join token, Postgres/Redis, telemetry, ingress
- `deploy/helm/kyber/templates/_helpers.tpl` — fullname, labels, image, secretName helpers
- `deploy/helm/kyber/templates/control-plane/{serviceaccount,clusterrole,clusterrolebinding,configmap,secret,deployment,service,ingress}.yaml`
- `deploy/helm/kyber/templates/node-agent/{serviceaccount,daemonset}.yaml`
- `deploy/helm/kyber/templates/{namespace,NOTES}.{yaml,txt}`
- Committed sub-chart tarballs (`charts/postgresql-14.3.3.tgz`, `charts/redis-19.6.4.tgz`) for reproducibility

**Code changes (to satisfy Helm-configurable behavior):**
- `cmd/control-plane/main.go` — `KYBER_NAMESPACE` env var replaces the hardcoded `"kyber-system"`
- `pkg/controllers/machine/reconciler.go` — `NewMachineReconciler` constructor reads `KYBER_K3S_JOIN_TOKEN` and `KYBER_K3S_SERVER_URL` from env vars. Before D1, these were empty stubs in `buildMachineSpec` — one of the Phase B carry-forwards.

**CI changes:**
- `.github/workflows/build.yml` — added real docker build+push steps for four images (control-plane, node-agent, agent-base, claude-code), gated with `if: false` until `GHCR_PAT` repo secret is created. When the gate flips, images will publish automatically.

**Verification:** `helm lint` clean, `helm template` produces ~1500 lines of valid YAML, `kubectl apply --dry-run=client` passes.

## D2 — Telemetry Integration

**Package:** `pkg/telemetry/` — new Go package.

**OTEL SDK:**
- `pkg/telemetry/otel.go` — `Config` struct with `Enabled`, `Endpoint`, `ServiceName` fields. `Init(ctx, cfg)` returns a `Telemetry` handle with `MeterProvider`, `TracerProvider`, and `Shutdown` functions. Env var `OTEL_EXPORTER_OTLP_ENDPOINT` takes precedence over config. A `noopTelemetry()` returns empty providers when disabled, so call sites never need nil checks.
- Resource attributes use `resource.New(ctx, resource.WithAttributes(...))` rather than `resource.Merge(resource.Default(), ...)` to avoid schema URL mismatches between SDK v1.42 and semconv v1.26 — a subtle incompatibility the implementer self-caught.

**Controller metrics** (`pkg/telemetry/metrics.go`):

| Metric | Type | Labels |
|--------|------|--------|
| `kyber_agent_state_transitions_total` | Counter | `from`, `to` |
| `kyber_agent_reconcile_duration_seconds` | Histogram | `phase` |
| `kyber_agent_reconcile_errors_total` | Counter | `event` |
| `kyber_agent_restart_count` | Counter | `agent` |
| `kyber_machine_state_transitions_total` | Counter | `from`, `to` |
| `kyber_machine_reconcile_duration_seconds` | Histogram | `phase` |
| `kyber_machine_preemptions_total` | Counter | `machine`, `zone` |

Metrics are registered via `InitMetrics()` with `sync.Once` idempotency. The reconcilers (`pkg/controllers/agent/reconciler.go`, `pkg/controllers/machine/reconciler.go`) record latency via `defer`, state transitions on phase change, and error counts on non-nil error returns.

**Alert sinks** (`pkg/telemetry/alerts.go`):

- `AlertSink` interface with `Fire(ctx, Alert)` method
- `LogAlertSink` — logs alerts at warning level via `log.FromContext`
- `WebhookAlertSink` — POSTs JSON alerts to a configurable URL with a 5-second timeout, graceful failure on network errors
- `CompositeAlertSink` — fan-out to multiple sinks

Agent Controller and Machine Controller both call `r.AlertSink.Fire(...)` when entering `Failed` or `Preempted` phases.

**Helm values** (`deploy/helm/kyber/values.yaml`):

```yaml
telemetry:
  enabled: false
  otlpEndpoint: ""
  serviceName: kyber-control-plane
  alertWebhookUrl: ""
```

Wired into both the control-plane Deployment and the node-agent DaemonSet. The node-agent DaemonSet picks up the same endpoint via a `coalesce` of `telemetry.otlpEndpoint` and `nodeAgent.otelEndpoint`.

**Tests:** 14 new tests in `pkg/telemetry/` — init, metrics registration, all three alert sink types.

## D3 — E2E Test Harness

**Files created:**
- `test/e2e/e2e_test.go` — test functions for platform health, machine lifecycle, agent lifecycle, scale-to-zero wake, error handling, cleanup, fleet summary
- `test/e2e/helpers.go` — `APIClient`, `WaitForPhase`, `WaitForMachineDeleted`, `WaitForAgentDeleted`
- `test/e2e/setup_test.go` — `TestMain` that creates a k3d cluster, builds + imports images, installs Kyber via Helm, port-forwards `:8080`, tears down
- `test/e2e/values-test.yaml` — Helm overrides for the test install (mock compute, Postgres + Redis disabled, nodeAgent disabled, resource-minimal)
- `images/control-plane/Dockerfile` — new multi-stage Go 1.25 Dockerfile that the test harness builds locally (no published image yet)
- `.github/workflows/e2e.yml` — new workflow running on main pushes, installs k3d + helm + kubectl, runs `go test -tags e2e`

**Build tag:** `//go:build e2e` — so these tests do NOT run with the default `make test`. CI runs them via `go test -tags e2e ./test/e2e/...`.

**Iteration during D3:**
1. `golang:1.22` Dockerfile rejected `go.mod` requiring Go 1.25 — fixed to `golang:1.25-alpine`
2. `docker build` ran from the test directory, not the repo root — fixed via `runtime.Caller(0)` chdir in `TestMain`
3. Helm `--create-namespace` conflicted with the chart's `namespace.yaml` template — fixed with `--set namespace.create=false`
4. **Control-plane pod never became Ready** — root cause: `mgr.AddHealthzCheck` and `mgr.AddReadyzCheck` were never called. The probe endpoint at `:8081` returned 404. This was a **latent bug** from B1 that had been sitting in main for the entire B-C-D1-D2 period. The e2e harness caught it on first real run.

**Tests passing on CI:**

- `TestE2E_PlatformHealth` — healthz, readyz, auth required
- `TestE2E_MachineLifecycle` — create, get, list, delete, phase transitions
- `TestE2E_AgentLifecycle` — create, get, list, stop, delete
- `TestE2E_ScaleToZeroWake` — create suspended agent, trigger Telegram webhook, verify wake
- `TestE2E_ErrorHandling` — 422 on attached agents, 400 on missing fields, 404 on nonexistent resources
- `TestE2E_Cleanup` — delete all, verify empty
- `TestE2E_FleetSummary` — summary and alias endpoints

Total e2e workflow runtime: ~5 minutes.

**Deferred:**
- Phase 4 (PVC persistence / overlay exec) — requires a real runtime image and real k8s nodes
- Phase 6 (GCE preemption) — requires real GCE credentials

## D4 — Integration + API Contract Tests

**Files created:**
- `test/integration/docker-compose.yml` — Postgres 16 on `5433`, Redis 7 on `6380` (non-default ports to avoid clashing with local services)
- `test/integration/helpers.go` — `TestMain` lifecycle, db/redis wait-for-ready, request builders
- `test/integration/briefstore_postgres_test.go` — 7 roundtrip tests against a real Postgres, validating the `BriefStore` interface
- `test/integration/messagebuffer_redis_test.go` — 6 roundtrip tests against a real Redis, including TTL verification
- `test/integration/api_test.go` — 4 API tests that send webhooks → real Redis → simulate control-plane restart with fresh client → verify messages persist
- `test/contract/openapi.yaml` — hand-written OpenAPI 3.0.3 schema covering fleet, machines, agents
- `test/contract/openapi_test.go` — 16 contract tests via `github.com/getkin/kin-openapi` validating that every tested route's request and response shape matches the schema
- `.github/workflows/integration.yml` — CI workflow running on main, uses GitHub Actions `services:` block for Postgres + Redis (no docker-compose needed in CI)

**Code changes:**
- `pkg/briefstore/postgres.go` — added `Migrate(ctx)` method that creates the `session_briefs` table if it doesn't exist. Called by `TestMain` at integration test startup.

**Build tag:** `//go:build integration` — not run by default `make test`.

**Tests passing on CI:** 31 total (15 integration + 16 contract). Local run against docker-compose also passes.

**Design note:** validation-error contract tests use a response-only validation helper. Intentionally malformed requests fail OpenAPI request validation by design, so the interesting property to verify is that the 400 response body shape matches the schema's `ValidationError` response.

## End-of-plan status

**17 of 17 tasks complete.** Every task in `docs/plans/2026-04-10-kyber-implementation-plan.md` is shipped on `main`.

**Test pyramid at end of Phase D:**

| Layer | Count | Run via |
|-------|-------|---------|
| Unit | ~100 | `make test` (default) |
| Envtest integration | ~25 | `make test` (default) |
| Contract | 16 | `go test -tags integration ./test/contract/...` |
| Integration (real DB/Redis) | 15 | `go test -tags integration ./test/integration/...` |
| E2E (k3d install) | 7 test funcs / 30+ sub-tests | `go test -tags e2e ./test/e2e/...` |

**CI workflows:**
- `test` — unit + envtest, runs on PRs and main
- `build` — build + image scaffold (push gated on `GHCR_PAT`), runs on main
- `e2e` — k3d + helm install + end-to-end tests, runs on main
- `integration` — Postgres + Redis + contract, runs on main

All four workflows are green on the final commit (`56a9b33`).

**The one latent bug the session caught via e2e testing:** `mgr.AddHealthzCheck` and `mgr.AddReadyzCheck` were never called in `cmd/control-plane/main.go`. The control plane advertised `:8081/healthz` and `:8081/readyz` via `HealthProbeBindAddress`, but because no handlers were registered, both endpoints returned 404. This had been in `main` since B1 and the existing unit + envtest coverage didn't catch it — unit tests use `BuildHandler` directly, envtest mocks the manager. The e2e harness was the first test that exercised the real binary in a real pod, and the readiness probe failure prevented Kyber from ever becoming Ready. Fix: add the calls.

This is exactly the argument for having an e2e layer. Unit and integration tests can only verify what they exercise. E2E tests exercise the deployed binary the same way production does.

## Carry-forwards for the future (not blocking)

- `GHCR_PAT` repo secret needs to be created; once created, flip `if: false` in `.github/workflows/build.yml` and the four images will publish
- `/api/v1/backups` — spec defines them, plan doesn't include them, nothing implemented. Reconcile the spec or add a task.
- Real GCE adapter has no runtime test (all GCE paths are compile-only)
- PWA has no unit tests
- Bundle size (552 KB uncompressed) not code-split
- Phase 4 e2e tests (PVC persistence / overlay exec) are stubbed out; will need real images + real k8s nodes
- Performance tests and load tests are out of scope for V1
