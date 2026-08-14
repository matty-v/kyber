# Contributing to Kyber

Thanks for your interest in contributing! Kyber is a self-hosted platform that
runs long-lived AI coding agents as Kubernetes pods. This guide covers how to
get a working dev environment, run the test suites, and open a PR that passes
CI.

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).

## Before you start

- Read [`AGENTS.md`](AGENTS.md) — the canonical repository orientation file:
  architecture, conventions, gotchas, and do-not-touch zones. It links deeper
  docs in `docs/architecture/` (HOW) and `docs/product/` (WHAT).
- [`docs/contributing/code-quality.md`](docs/contributing/code-quality.md) documents the Go and TypeScript
  conventions CI actually gates on; [`docs/contributing/reviewing.md`](docs/contributing/reviewing.md) lists known
  fragile areas.
- For non-trivial changes, open an issue first. Ask before adding new
  dependencies (Go/npm/Helm), changing CRD schemas, or touching RBAC/CI
  permissions.

## Prerequisites

- **Go** (see `go.mod` for the required version)
- **Node.js + npm** (npm workspaces monorepo: `packages/*`, `apps/*`)
- **Docker**, **k3d**, **helm**, **kubectl**, **curl** — only needed for the
  local dev environment, e2e tests, and chart work
- `controller-gen` — installed automatically by `make generate`

## Building and testing

Go (from the repo root):

```bash
make build        # go build ./...
make lint         # go vet ./... (this IS the Go lint gate — golangci-lint is not wired)
make test         # go test ./... — required gate
make generate     # regenerate CRD manifests if you touched pkg/api/v1/ — commit the diff
```

`make test` includes envtest suites, which need control-plane test binaries:

```bash
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19
export KUBEBUILDER_ASSETS=$(setup-envtest use 1.31.0 -p path)
```

PWA / TypeScript (only if you touched `packages/` or `apps/`):

```bash
npm ci
npm run build --workspace=packages/pwa-views
npm run build --workspace=apps/embedded-pwa
npm run lint  --workspace=packages/pwa-views    # tsc --noEmit (there is no ESLint)
npm run lint  --workspace=apps/embedded-pwa
npm run test  --workspace=packages/pwa-views    # vitest run
npm run test  --workspace=apps/embedded-pwa
```

Heavier suites (CI runs these in `integration.yml` / `e2e.yml`):

```bash
go test -tags integration -timeout 15m ./test/integration/... ./test/contract/...
    # needs docker (postgres/redis services) — see test/integration/docker-compose.yml
go test -tags e2e -timeout 25m ./test/e2e/... -cluster-name kyber-e2e   # needs k3d + docker
```

Helm chart changes:

```bash
make helm-lint
make helm-template    # pins placeholder image tags for you
```

## Local dev environment

A deterministic local Kyber instance — no cloud credentials required:

```bash
scripts/devenv/up.sh      # k3d + mock-provider Kyber, API on localhost:18080
scripts/devenv/down.sh    # tear down, no orphans
```

`up.sh` is control-plane/API only by default. For the full stack with live
agent pods and managed lifecycle behavior, use
`scripts/devenv/up-full.sh --compute-provider fake`. To exercise the real GCE
adapter without GCP, use `--compute-provider gce-emulator`; its synthetic Nodes
cannot run agents. See
[`scripts/devenv/README.md`](scripts/devenv/README.md) for the entry-point
contract (URLs, test credentials), what is mocked, and flags like
`--skip-build`. For PWA work with hot reload, run `make pwa-dev` (dev server on
`:5173`, proxies `/api`).

## Branches, commits, and PRs

- `main` requires PRs; never bypass CI. Prefer one consolidated PR per logical
  change set (CI is expensive).
- Commit messages and PR titles follow
  [Conventional Commits](https://www.conventionalcommits.org/): `feat:`,
  `fix:`, `docs:`, `chore:`, `ci:`, with an optional scope — e.g.
  `fix(controllers): converge the Telegram sidecar onto running pods`.
- Fill in the PR template: summary, changes, how you tested, and the checklist.
- Update `CHANGELOG.md` for user-facing changes, and keep docs (including
  `AGENTS.md`) in sync with any change to architecture, layout, conventions,
  or verification commands.
- API request/response shape changes update three things in one PR: the Go
  handler, `test/contract/openapi.yaml`, and the hand-written PWA type.

## `packages/pwa-views`: version bump required

`packages/pwa-views` is a published package (`@matty-v/kyber-pwa-views` on
GitHub Packages) consumed by `apps/embedded-pwa` via the local workspace *and*
by external hosts via the published version. Two consequences:

1. **Any change to `packages/pwa-views` must bump the version** in
   `packages/pwa-views/package.json` (semver: patch for fixes, minor for
   features, major for breaking changes) and add an entry to
   `packages/pwa-views/CHANGELOG.md`. **CI enforces the bump on PRs**
   (`test.yml`).
2. Publishing happens only when a `pwa-views/vX.Y.Z` tag is pushed (triggers
   `publish-pwa-views.yml`). Changes that only affect `apps/embedded-pwa` (the
   in-binary PWA) don't need the publish step — it consumes the local
   workspace directly. External hosts keep consuming the stale published
   version until the tag is pushed and their dependency is bumped; in May 2026
   a UI tab went missing downstream (kyber#335) precisely because a PR changed
   `pwa-views` without a bump or tag.

Maintainer publish checklist:

```bash
git checkout main && git pull        # after the PR merges
git tag pwa-views/vX.Y.Z
git push origin pwa-views/vX.Y.Z     # triggers publish-pwa-views.yml
```

Then bump the dependency in any external host that pins the published package.

## Security issues

Please don't open public issues for vulnerabilities — see
[.github/SECURITY.md](.github/SECURITY.md).
