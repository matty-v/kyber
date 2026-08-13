# `scripts/devenv/` — one-command dev/test environment

A deterministic, mock-backed Kyber instance any agent (or human) can bring up,
drive over the API, and tear down — with **one command each way** and **no real
cloud, auth, or prod-network access**.

This is the shared primitive the per-agent empirical-capability work
([#402](https://github.com/matty-v/kyber/issues/402),
[#403](https://github.com/matty-v/kyber/issues/403), and the per-role skill
issues) builds on. It is the testing-env piece of the
[#397–399](https://github.com/matty-v/kyber/issues/399) source-of-truth trio.

> **Scope — Phase 1 (this PR, [#399](https://github.com/matty-v/kyber/issues/399)).**
> Bring-up / teardown scripts + the printed contract + this README. The
> headless-browser helper (Phase 2, #402) and pod-runnability — running this
> from inside an agent pod (Phase 3, #403) — are **deferred** and tracked
> separately. See [Running from an agent pod](#running-from-an-agent-pod-deferred--403).

## Quick start

```bash
# Bring it up (k3d + mock-provider Kyber, API port-forwarded to localhost:18080)
scripts/devenv/up.sh

# ... drive it (see "The contract" below) ...

# Tear it down (deletes the cluster, leaves no orphans)
scripts/devenv/down.sh
```

To exercise the managed Machine state machine without cloud credentials, use
the deterministic fake provider:

```bash
scripts/devenv/up.sh --compute-provider fake
scripts/devenv/smoke-fake-provider.sh   # Ready → Stopped → Ready → deleted
scripts/devenv/compute-scenario.sh list
scripts/devenv/down.sh
```

The default `mock` value is retained for compatibility and has the same
existing-node behavior as the explicitly named `static` provider. `fake`
creates an opaque in-memory instance, runs the normal provisioning/finalizer
path, and attaches the single local Machine to the real k3d node so workloads
remain schedulable.

To exercise the real GCE adapter without credentials or GCP, use the local
Compute Engine REST emulator:

```bash
scripts/devenv/up-full.sh --compute-provider gce-emulator
# Create a gce Machine in the PWA, then attach a synthetic node once provisioned:
scripts/devenv/compute-scenario.sh attach-node my-machine
scripts/devenv/compute-scenario.sh apply my-machine preempted
```

The scenario endpoint exists only when explicitly enabled, remains behind the
normal API key, and is disabled by production defaults. `fake` tests Kyber's
shared lifecycle; `gce-emulator` adds the real GCE request, operation polling,
native status, Spot, and error translation paths.
`attach-node` creates a tainted, unschedulable synthetic Node; it never labels
or risks the real k3d control-plane node. Agent pods are therefore tested in
`fake` mode, while `gce-emulator` focuses on compute lifecycle fidelity.
Only the emulator profile permits these explicitly labelled Nodes to remain
Ready without kubelet heartbeats; production defaults retain normal readiness.

`up.sh` prints the contract when the API is healthy. Re-running `up.sh` is
idempotent — it reuses an existing cluster and re-converges via `helm upgrade`.

### Full local stack — live agent pods

`up.sh` is **control-plane/API only** (mock compute, no agent workloads). To run
the **whole stack locally**, including live agent pods that actually schedule and
run Claude Code, use [`up-full.sh`](./up-full.sh) — see
[`full-local.md`](./full-local.md):

```bash
scripts/devenv/up-full.sh            # cold (builds all images)
scripts/devenv/up-full.sh --skip-build  # warm (reuse built :local images)
scripts/devenv/up-full.sh --compute-provider fake # managed lifecycle + live agents
```

To test App-minted credentials against real agent identity repos without
checking personal App details into the repository, use
[`setup-github-app.sh`](./setup-github-app.sh) and the ignored local bundle
documented in [`full-local.md`](./full-local.md#local-github-app-for-identity-repo-testing).

This is a developer-machine convenience and is **distinct from** the deferred
[Running from an agent pod](#running-from-an-agent-pod-deferred--403) work (#403),
which is about running this environment *inside* an agent pod — a separate,
threat-modeled infra concern that `up-full.sh` does not touch.

### Flags

| `up.sh` flag | Effect |
|---|---|
| `--skip-build` | Reuse an already-built/imported control-plane image (skips the slow `docker build` — the **warm fast path**). |
| `--recreate` | Delete an existing devenv cluster first, then build fresh. |
| `--api-port N` | Local port to forward the API to (default `18080`). |
| `--cluster-name NAME` | k3d cluster name (default `kyber-devenv`). |
| `--compute-provider mock\|static\|fake\|gce-emulator` | Select existing-node behavior, the neutral simulator, or the real GCE adapter against its local REST emulator. |

`down.sh` takes `--cluster-name` and is a clean no-op if the cluster is already gone.

## How it works (wrap, don't reimplement)

`up.sh`/`down.sh` orchestrate the **same steps** `test/e2e/setup_test.go` already
performs, calling `k3d` / `docker` / `helm` / `kubectl` directly so no Go
toolchain or knowledge of the `e2e` build tag is required:

```
up.sh:  k3d cluster create  →  docker build + k3d image import  →
        helm upgrade --install (mock profile)  →  kubectl port-forward  →
        poll /healthz  →  print the contract
down.sh: stop port-forward  →  k3d cluster delete
```

The mock-env Helm profile — [`test/e2e/values-test.yaml`](../../test/e2e/values-test.yaml)
— is **reused by reference, never forked**. A divergent copy that drifts from
the e2e profile is a slow-acting trap, so the scripts and the e2e harness share
one source of truth, and the printed credentials are read straight out of that
file.

## What is mocked

Everything that would need real credentials or network is replaced, so the
environment is fast, isolated, and prod-free:

| Dependency | In the dev env |
|---|---|
| Cloud compute | Default `mock`/`static` attaches to the k3d node; optional `fake` uses in-memory instances through the managed lifecycle. No GCP project or ADC. |
| PostgreSQL | Disabled — control-plane falls back to its in-memory store. |
| Redis | Disabled — in-memory token/buffer store. |
| Node agent DaemonSet | Disabled. |
| Telemetry (OTLP) | Disabled. |

The only network dependency is the local container image pull/build (`docker` +
`k3d`); there are **no external services** in the happy path.

## The contract

`up.sh` prints a stable, documented set of entry points that downstream agent
skills bind to. These are **throwaway, non-prod fixtures** — never reuse them
anywhere real.

| Field | Value |
|---|---|
| API base URL | `http://localhost:18080` (or `--api-port`) |
| Health endpoint | `http://localhost:18080/healthz` |
| API key | `test-api-key-e2e` (from `values-test.yaml`) |
| Webhook secret | `test-webhook-secret-e2e` (from `values-test.yaml`) |
| Namespace | `kyber-system` |
| Cluster (k3d) | `kyber-devenv` |
| PWA URL | `http://localhost:18080/` (or `--api-port`) — the real SPA, embedded in the control-plane binary (`pkg/api/embed.go`) and served at the root path. Drive it with the [browser harness](#driving-the-pwa-headless-browser). |

### How an agent skill invokes it

```bash
# 1. Bring the environment up (warm path once an image exists).
scripts/devenv/up.sh --skip-build

# 2. Drive the API with the contract creds.
curl -fsS -H "X-Api-Key: test-api-key-e2e" http://localhost:18080/healthz

# 3. ... exercise the behaviour under test ...

# 4. Always tear down, even on failure.
scripts/devenv/down.sh
```

### Driving the PWA (headless browser)

`scripts/devenv/browser/` is a self-contained Playwright project that drives the
**brought-up PWA** (the real control-plane + mock compute), distinct from
`apps/embedded-pwa/playwright.config.ts` (which targets an MSW-mocked vite dev
server on `:5174` and is left untouched). It targets the running bring-up via
`baseURL` derived from the contract above — no `webServer` is spawned.

One-time setup — fetch the Chromium binary (**the one network dependency**):

```bash
cd scripts/devenv/browser
npm install                 # installs @playwright/test (pinned ^1.59.1)
npm run install-browser     # = npx playwright install chromium (needs network)
```

Then, against a live bring-up:

```bash
scripts/devenv/up.sh                 # bring the instance up (root PWA at :18080)
cd scripts/devenv/browser && npm test  # runs the worked example smoke spec
```

`baseURL` resolution (env, both optional):

- `DEVENV_API_PORT` — port for the `http://localhost:<port>/` target (default
  `18080`; set automatically by `up.sh --api-port`).
- `DEVENV_PWA_URL` — explicit full-URL override for a non-localhost bring-up
  (e.g. a shared remote devenv). Takes precedence when set.

**The helper** (`scripts/devenv/browser/helper.ts`) exposes two usage classes
over a Playwright `page`:

| Class | For | API |
|---|---|---|
| **Read-only** | product observation (Yoda) | `goto`, `readText`, `shot` |
| **Read-write** | UI flows + verification (builders, Boba Fett QA) | `click`, `fill`, `submit`, `expectVisible`, `expectText` |

The read-only / read-write split is an **ergonomic, documented convention — not
an enforced sandbox** (a Playwright `page` can always do anything). The real
capability boundary is Phase 3 / #403 (running the browser from an agent pod).

**Failure modes** are reported up front by the project's `global-setup.ts`, not
as opaque stack traces:

- Chromium not installed → message pointing at `npm run install-browser`.
- No devenv instance up (nothing on the port-forward) → message pointing at
  `scripts/devenv/up.sh`.

> **`--skip-build` note:** the warm path reuses a previously-built image. The
> real-PWA serving guarantee comes from the multi-stage
> `images/control-plane/Dockerfile` (it builds the PWA into `pkg/api/pwa_dist`
> before `go build`), so a cold `up.sh` always serves the real SPA. If you
> `--skip-build` against a stale/placeholder image, the smoke spec's
> real-surface assertion fails loudly — that assertion is the standing guard.

## Requirements

Bring-up needs a local container runtime + Kubernetes tooling on `PATH`:
**`k3d`**, **`docker`**, **`helm`**, **`kubectl`**, **`curl`**. `up.sh` preflights
these and exits `3` naming anything missing. `--skip-build` drops the `docker`
requirement. Teardown needs only `k3d`.

Exit codes: `0` success · `2` usage error · `3` missing dependency.

## Running from an agent pod (deferred — #403)

The agents that consume this run as **pods** on kyber-falcon. Running this env
*from inside a pod* is **not covered by this PR** — it is Phase 3, tracked in
[#403](https://github.com/matty-v/kyber/issues/403), and is **blocked on an
infra decision + a threat-model pass**. Two reasons it is its own phase:

1. **Toolchain / privilege.** Running k3d in a pod means Docker-in-Docker
   (privileged), or pointing these scripts at a pre-provisioned cluster the pod
   reaches over the network. A headless browser (Phase 2) additionally needs
   browser system deps and a memory floor.
2. **Attack surface.** A pod that can run arbitrary containers (DinD/privileged)
   has a much larger blast radius than one that cannot. Per Obi-wan's security
   note this must be scoped tightly (prefer a dedicated cluster the pod *talks
   to* over DinD in every agent pod) and threat-modeled before it ships.

These are **agent-image / pod-provisioning concerns (Kyber/Dave infra)**, not
team-owned script changes. The team owns `scripts/devenv/`; the pod image deps,
memory floor, and cluster-access model are the explicit, documented infra ask in
#403 — not silently assumed here.

## Testing these scripts

The scripts have shell tests using the repo's fake-binary-on-`PATH` convention
(see `scripts/preflight-tag-not-published_test.sh`): fake `k3d`/`docker`/`helm`/
`kubectl`/`curl` record their argv, and the tests assert the orchestration
without needing a real cluster.

```bash
bash scripts/devenv/up_test.sh
bash scripts/devenv/down_test.sh
```

> A live end-to-end run (actually creating a k3d cluster and hitting a green
> `/healthz`) requires a container runtime, which the current agent pods do not
> have — that is exactly the Phase 3 (#403) gap. The orchestration is verified
> here against `test/e2e/setup_test.go` and the fake-binary harness.
