# CODE_QUALITY.md — Repo-local code standards

> **Update cadence: deliberate.** Changes to this file are proposed in a PR and
> reviewed/agreed before landing — it is a forward-looking standard, not a
> scratchpad. Contributors read it before coding; reviewers enforce it at
> review and cite the specific rule (objective, not taste). For reviewer
> *gotchas* and fragile-area hotspots, see [REVIEWING.md](REVIEWING.md). For
> pipeline/process rules, see [CONTRIBUTING.md](CONTRIBUTING.md) and
> [AGENTS.md](AGENTS.md) — this file is code-level.

These are the conventions already in force in this repo. Each rule is phrased
so a reviewer can point to it ("CODE_QUALITY.md § Go/Errors") rather than
appeal to taste, and each maps to code you can read today.

---

## Go

### Errors

- **Wrap with `%w` when adding context to a propagated error.** Use
  `fmt.Errorf("doing X: %w", err)` — never `%v` or `%s` for the underlying
  error, so callers can `errors.Is`/`errors.As` through the chain. Real
  examples: `pkg/tokenstore/redis.go` (`"marshaling snapshot: %w"`),
  `pkg/runtimedetect/npm.go` (`"building npm request: %w"`),
  `pkg/adapters/compute_gce.go` (`"GCE Insert(...): %w"`).
- **Context prefix is a lowercase verb phrase, no trailing punctuation.**
  `"fetching agent: %w"`, not `"Failed to fetch agent."`. Matches the wrapped
  examples above and the reconciler (`pkg/controllers/agent/reconciler.go`).
- **Use sentinel errors for conditions callers branch on.** Declare a
  package-level `var ErrFoo = errors.New("pkg: description")` and compare with
  `errors.Is`. Existing sentinels: `ErrInstanceNotFound`
  (`pkg/adapters/compute_gce.go`), `ErrQueueFull` (`pkg/inbound/queue.go`),
  `ErrBriefNotFound` (`pkg/briefstore/store.go`). Don't string-match error
  text.
- **Match k8s error kinds with the apimachinery helpers**, e.g.
  `errors.IsNotFound(err)` / `errors.IsAlreadyExists(err)` — see the
  `Reconcile` not-found short-circuit in
  `pkg/controllers/agent/reconciler.go`.

### Tests

- **Table tests, standard-library only.** This repo does **not** depend on
  testify — there is no `assert`/`require` import anywhere. Use `t.Run`,
  `t.Errorf`, `t.Fatalf`. Canonical shape to copy:
  `pkg/usersecrets/usersecrets_test.go` —

  ```go
  tests := []struct {
      name    string
      key     string
      wantErr error
  }{
      {"single uppercase letter", "A", nil},
      // ...
  }
  for _, tc := range tests {
      t.Run(tc.name, func(t *testing.T) {
          got := ValidateKey(tc.key)
          if !errors.Is(got, tc.wantErr) {
              t.Errorf("ValidateKey(%q) = %v, want %v", tc.key, got, tc.wantErr)
          }
      })
  }
  ```

  Don't introduce testify (or any new assertion lib) without a separate,
  agreed dependency change — see [AGENTS.md](AGENTS.md) § Do-not-touch zones
  ("Ask first").

- **What "done" means for a controller or API change:**
  - **Controller / reconciler changes require an integration test** under
    `test/integration/` (controller-runtime **envtest** against a real API
    server), not just unit coverage of a helper. The agent reconciler's own
    suite (`pkg/controllers/agent/`) runs envtest and is the bar for new
    reconcile behavior.
  - **API-shape changes must keep the OpenAPI contract green.**
    `test/contract/openapi_test.go` validates every API response against
    `test/contract/openapi.yaml` (via `kin-openapi`). If you change a
    request/response shape, update `openapi.yaml` in the same PR — and see the
    TS wire-contract rule below for the PWA half.
  - A change is not "done" because it compiles. It's done when `make test`
    is green (which is what CI runs — see Lint/format below).

### Naming

- **Exported identifiers are PascalCase; unexported are camelCase.**
  `AgentReconciler`, `StatusSidecarImage` (exported) vs `isSidecarDrifted`,
  `extractSidecarSpecImage` (unexported helpers) in
  `pkg/controllers/agent/reconciler.go`.
- **Receivers are short and consistent per type** — single letter matching the
  type: `r` for reconcilers (`func (r *AgentReconciler)`), `s` for servers,
  `a` for adapters. Don't use `this`/`self` or spell the type out.
- **Interfaces are named for what they are, not `-er`-inflated.** `Runtime`,
  `Adapter`, `Probe` (`pkg/runtimes/runtime.go`). Keep interfaces small and
  defined at the consumer.
- **Env vars the control plane / node-agent read are `KYBER_*`-prefixed**
  (e.g. `KYBER_STATUS_SIDECAR_IMAGE`). See [AGENTS.md](AGENTS.md) § Conventions
  for the secret and Helm-helper naming rules.

---

## TypeScript / PWA

The PWA is an **npm-workspaces monorepo** (root package `kyber-monorepo`),
**not** a single `pwa/` directory:

- `packages/pwa-views/` — the shared React view library (published to
  GitHub Packages; consumed by the embedded app and the external Holocron host).
- `apps/embedded-pwa/` — the control-plane's bundled PWA app.

### Conventions

- **Function components, PascalCase file + symbol names.** Components live in
  `packages/pwa-views/src/components/` and pages in
  `packages/pwa-views/src/pages/` (e.g. `Layout`, `FleetOverview`). No class
  components.
- **Hooks use the `use*` prefix and live in
  `packages/pwa-views/src/hooks/`** — `useWebSocket`, `useAPI`,
  `useFleetHistory`, each `export function use…()`.
- **TypeScript `strict` mode is on** (`packages/pwa-views/tsconfig.json`,
  `apps/embedded-pwa/tsconfig.json`). Don't weaken it with `any` to dodge a
  type error — model the type. There is **no ESLint config**; the type-checker
  is the linter (see below), so a clean compile is the bar.

### Wire-contract round-trip (API ↔ PWA type parity)

The API is described by `test/contract/openapi.yaml` and the Go side is held to
it by `test/contract/openapi_test.go`. **PWA request/response types are
hand-written** (there is no OpenAPI→TS codegen today), so type parity is **not**
automatically enforced across the language boundary. The standard:

- **Any change to an API request/response shape must update three things in the
  same PR:** the Go handler, `test/contract/openapi.yaml` (keeps the Go
  contract test honest), and the corresponding hand-written PWA type.
- **Reviewers verify the round-trip by hand** until codegen exists: read the Go
  shape and the TS type side-by-side and confirm field names, optionality, and
  enums match. This is a named review hotspot — see
  [REVIEWING.md](REVIEWING.md).
- A PWA source change to `packages/pwa-views/` additionally requires a version
  bump per [CONTRIBUTING.md](CONTRIBUTING.md) (the Holocron publish guard).

---

## Lint / format — what is actually enforced

State only what CI gates on; don't cite aspirational tools.

| Surface | Command (what CI runs) | Notes |
|---|---|---|
| Go lint | `make lint` → `go vet ./...` | The Makefile target is a documented placeholder; **`golangci-lint` is not wired and there is no `.golangci.yml`.** |
| Go tests | `make test` → `go test ./...` | The required gate. Includes contract + envtest integration suites. |
| Go format | *(not gated)* | **`gofmt` is not checked in CI.** Run `gofmt -w` / `goimports` locally anyway — keep diffs clean — but a gofmt-only nit is not a merge blocker. |
| PWA lint | `npm run lint --workspace=packages/pwa-views` and `--workspace=apps/embedded-pwa` → `tsc --noEmit` | Type-check **is** the lint. No ESLint. |
| PWA tests | `npm run test --workspace=…` → `vitest run` | |
| PWA build | `tsup` (pwa-views) / `tsc -b && vite build` (embedded) | |

CI wiring lives in `.github/workflows/test.yml` — if this table and that
workflow disagree, the workflow wins and this file is the bug.
