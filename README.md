# Kyber — Self-Hosted Agent Fleet Platform

Kyber is a self-hosted platform for creating and managing AI agents on cost-optimized cloud infrastructure. It runs on Kubernetes (k3s for small installs, GKE for large), deployed via Terraform and Helm.

## Install

- **With an AI agent (recommended for non-developers).** Open your AI assistant of choice (Claude Code, Cursor, ChatGPT, etc.). Give it the prompt below and follow its instructions.
- **Manually.** See [`docs/installation-wsl2.md`](docs/installation-wsl2.md) (Windows + WSL2 standalone) or [`docs/installation.md`](docs/installation.md) (GCP production).

```
You're helping me install Kyber on my Windows laptop. Kyber is a self-hosted AI
agent fleet platform. Repo: https://github.com/matty-v/kyber.

Read `docs/installation-wsl2.md` end-to-end before starting. It's a numbered
runbook with explicit verify steps and recovery paths — your job is to drive
the install.

Operating principles:
1. I'm not a developer. Before each step, tell me in one plain-language
   sentence what you're about to do.
2. Don't proceed past a step until its verify command produces the expected
   output. If it fails, follow the recovery pointer in the doc — don't
   improvise.
3. Some steps need me (browser flows, PAT generation, OAuth approval). When
   you hit one, pause, tell me exactly what to do with the URL, and wait for
   me to come back with the value. Never invent a credential.
4. Don't paste secrets into our chat or commit them. They go into shell
   variables or files at paths the doc specifies.
5. Anything destructive (rm -rf, wsl --unregister, terraform destroy),
   confirm with me first.
6. If this is a resumed session and you've lost context, run each step's
   verify command starting at § 0 — your starting point is the first one
   that fails.

Start at § 0 (Bring up WSL2). Proceed sequentially.
```

## Architecture

The platform has three runtime components:

**Control Plane** — a modular monolith (single Go binary) that is the platform brain. It owns the REST API, agent lifecycle, machine lifecycle, telemetry, and background processing. Internal modules: API, Agent Controller, Machine Controller, Telemetry, Background Workers.

**Node Agent** — a thin DaemonSet binary (one pod per k8s node) responsible for machine-level concerns only: exporting node metrics to OpenTelemetry and executing machine-level actions (reboot, stop) on instruction from the control plane.

**Agent Runtime** — one pod per agent, managed by the Agent Controller via Agent CRDs. Each agent has a full persistent filesystem backed by a PersistentVolume. The entrypoint uses a three-tier dispatcher to achieve whole-disk persistence: (1) kernel overlayfs — fast path, works when the container root isn't already on overlayfs; (2) fuse-overlayfs — userspace overlay, works on top of any filesystem including k3s's containerd-overlay (the prod default); (3) bind-mount HOME — last resort fallback when `/dev/fuse` is unavailable, persists `$HOME` only. Modes (1) and (2) persist the entire pod filesystem including apt packages and system paths. Agents survive pod recreation, spot preemption, and restarts with full filesystem continuity.

> **Pod requirements:** agent pods run `Privileged: true` (required for both fuse-overlayfs and `mount(MS_BIND)`) and mount `/dev/fuse` from the host. See `docs/installation.md` § GCE CSI Driver for cluster prerequisites.

### CRDs

Two custom resource definitions are the source of truth for platform state:

- `Agent` (`kyber.io/v1`) — represents a single AI agent instance. Specifies the target machine, runtime type, compute resources, scaling mode, identity, secrets, and model.
- `Machine` (`kyber.io/v1`) — represents a cloud VM managed by the platform. Specifies the provider, machine type, disk size, spot pricing, and zone.

### Communication

- **k8s API:** All lifecycle operations flow through CRDs. Operators declare intent; controllers reconcile against reality.
- **Redis:** Async and real-time events (wake suspended agents, machine health events, task completion signals).

## Tech Stack

| Component | Technology |
|---|---|
| Control Plane | Go + controller-runtime (kubebuilder) |
| Node Agent | Go (static binary) |
| PWA | React + Vite + TypeScript |
| Database | PostgreSQL (fleet metadata) |
| Message Queue | Redis (async events) |
| Orchestration | Kubernetes (k3s / GKE) |
| Infrastructure | Terraform (GCP-first) |
| Deployment | Helm chart in-repo ([`deploy/helm/kyber`](deploy/helm/kyber)); optionally GitOps via ArgoCD + ArgoCD Image Updater |
| Telemetry | OpenTelemetry |
| Agent Runtimes (V1) | Claude Code, Codex (ChatGPT device login or OpenAI API key) |
| Secrets | GCP Secret Manager |

## Repository Structure

```
kyber/
├── cmd/
│   ├── control-plane/    # Control plane entrypoint
│   └── node-agent/       # Node agent entrypoint
├── pkg/
│   ├── api/              # REST API + CRD types (api/v1)
│   ├── controllers/
│   │   ├── agent/        # Agent Controller (CRD reconciliation)
│   │   └── machine/      # Machine Controller (GCE + k8s reconciliation)
│   ├── adapters/         # Cloud/infra adapters (GCE, Secret Manager)
│   ├── briefstore/       # Session-brief persistence (Postgres + memory)
│   ├── githubapp/        # GitHub App client (identity-repo scaffolding + GitHub API + short-lived per-agent identity-repo token minting, kyber#508 Stage 3/4)
│   ├── inbound/          # Inbound-prompt verifier, dedup, rate limit, queue
│   ├── messagebuffer/    # Telegram message buffer for suspended agents
│   ├── nodeagent/        # Node agent logic
│   ├── oauth/            # Anthropic OAuth (token exchange + refresh)
│   ├── telemetry/        # OpenTelemetry emission
│   ├── tokenreport/      # Per-agent context-budget snapshot model
│   ├── tokenstore/       # Token-budget storage (Redis + memory)
│   └── usersecrets/      # Per-agent user-managed secret integration
├── apps/embedded-pwa/    # React + Vite + TypeScript PWA (shared views in packages/pwa-views/)
├── images/
│   ├── agent-base/       # Base Dockerfile for agent containers
│   └── claude-code/      # Claude Code runtime image
│   └── codex/             # Codex runtime image
├── infra/terraform/      # Terraform modules
├── deploy/helm/kyber/    # Helm chart
│   └── crds/             # Generated CRD manifests
└── test/
    ├── contract/         # Cross-package contract tests
    ├── integration/      # Integration tests (envtest)
    ├── e2e/              # k3d-based end-to-end tests
    └── prod-e2e/         # Real-cluster smoke tests
```

## Agent Authentication

Agents authenticate to Anthropic via a PWA-driven PKCE OAuth flow. When creating an agent:

1. Click **Authorize with Claude** in the Create Agent form — the PWA opens Anthropic's authorize URL.
2. Approve the scope grant in your browser, copy the authorization code.
3. Paste the code back into the PWA and click **Create Agent**.

Kyber exchanges the code + PKCE verifier for `{access_token, refresh_token}`, stores them in a per-agent k8s secret. On every pod boot, `start-claude.sh` checks whether the stored access token is still valid (more than 5 minutes before expiry). If so, it skips the refresh and starts Claude Code immediately. If the token has expired or is close to expiry, it exchanges the stored refresh token for a new credential set, writes `~/.claude/.credentials.json`, and POSTs the new refresh token back to the control plane (blocking — pod exits if this fails). Agents boot fully authenticated — no interactive `/login` step.

See `docs/2026-04-14-programmatic-oauth-design.md` for full design detail and `docs/installation.md` § 9 for the step-by-step install flow.

## CI / Deployment

`.github/workflows/build.yml` uses path filters and per-image jobs to build and push container images to GHCR. Typical build times:

- PWA-only change: ~3 min
- Single Go image: ~4-5 min
- All images: ~10-12 min

Doc-only commits (`docs/**`, `*.md`) skip the build job entirely.

**CI does not deploy.** The Helm chart lives in this repo at [`deploy/helm/kyber`](deploy/helm/kyber), and a plain `helm install` following [`docs/installation.md`](docs/installation.md) is the supported route for deploying your own cluster. The maintainer's own clusters are additionally deployed via [ArgoCD](https://argo-cd.readthedocs.io/) driven by a separate (currently private) deploy repo that holds per-environment values and ArgoCD Application manifests — that repo is not required to run Kyber.

### How deployment works

Two release tracks per environment:

| Track | Chart source | Image tag | Used by |
|---|---|---|---|
| **latest** | `main` HEAD | `:latest` | dev/canary clusters |
| **release** | tagged commit (`vX.Y.Z`) | `:vX.Y.Z` (digest-pinned in the deploy repo) | production clusters |

```
Push to main (matty-v/kyber)
        │
        ▼
build.yml builds + pushes images to GHCR
  ghcr.io/matty-v/kyber-*:latest + :<sha>
        │
        ▼  (~2 min)
ArgoCD Image Updater on LATEST-track clusters
detects the new digest, writes override into the
Application's helm.parameters
        │
        ▼
ArgoCD syncs → pods roll. Canary clusters get the
new images automatically.


 ─── separately, cut a release (the operator approves via Telegram/Discord) ───

.github/workflows/prepare-release.yml — folds the
Chart.yaml bump into the commit, pushes tag vX.Y.Z
        │
        ▼
.github/workflows/release.yml — full rebuild of all 8
images from source at the tagged commit → pushed as
:vX.Y.Z, then a GitHub Release is created
        │
        ▼
post-release chain opens digest-pinned bump PRs on
the deploy repo for the production clusters
(per-environment values.yaml → :vX.Y.Z@sha256:...)
        │
        ▼
operator merges the bump PR; ArgoCD on RELEASE-track
clusters syncs the pinned digests on the next
sync (~5 min)
```

The Helm chart ([`deploy/helm/kyber/`](deploy/helm/kyber)) is the deployment contract, whether you install it directly with `helm install` or point a GitOps tool at it. Per-environment values, ArgoCD Application manifests, and bootstrap scripts for the maintainer's clusters live in a separate (currently private) deploy repo; running your own instance does not depend on it — follow [`docs/installation.md`](docs/installation.md) instead. See `docs/upgrading.md` for the day-to-day upgrade flow and `docs/operator/release-runbook.md` for the release (`vX.Y.Z`) promotion + rollback reference.

## Quickstart

> First-time install (Terraform + Helm chart + ArgoCD) is documented in [`docs/installation.md`](docs/installation.md); see `docs/upgrading.md` for the day-to-day upgrade flow. The commands below cover local build/test.

```bash
# Build
make build

# Run tests
make test

# Generate CRD manifests
make generate

# List managed images
make image-list
```

### Local dev/test environment (mock-backed, one command)

For a deterministic, mock-backed Kyber instance you can bring up, drive over the
API, and tear down — no real cloud or prod credentials:

```bash
scripts/devenv/up.sh      # k3d + mock-provider Kyber, API on localhost:18080
scripts/devenv/down.sh    # tear down, no orphans
```

See [`scripts/devenv/README.md`](scripts/devenv/README.md) for the entry-point
contract (URLs, test creds), what is mocked, and the agent-invocation pattern.

## Documentation

**Working-knowledge (repo root — read before contributing):**

- [CODE_QUALITY.md](CODE_QUALITY.md) — repo-local code standards: Go + TS/PWA conventions, what "done" testing means for a controller/API change, and the lint/format CI actually gates on
- [REVIEWING.md](REVIEWING.md) — reviewer gotchas / fragile-area focus map: known landmines and "look harder here" hotspots, seeded from real incident history

**Product & architecture — the WHAT and the HOW (a matched, cross-linked pair):**

- [docs/product/README.md](docs/product/README.md) — **product source of truth (the WHAT):** what Kyber does, the observable behaviors and concepts an operator sees and can do. No implementation detail.
- [docs/architecture/overview.md](docs/architecture/overview.md) — **architecture deep-dive (the HOW):** the three runtime components, CRDs, control-plane module map, agent lifecycle, and inbound dispatch path (entry point to `docs/architecture/`)
- [docs/runtimes.md](docs/runtimes.md) — Runtime selection and Codex ChatGPT-subscription authentication

**Design notes & runbooks:**

- [docs/installation.md](docs/installation.md) — GCP multi-VM install (Terraform + GCE + ArgoCD)
- [docs/installation-wsl2.md](docs/installation-wsl2.md) — WSL2 standalone install (native k3s + Tailscale Funnel, no cloud infra)
- [docs/upgrading.md](docs/upgrading.md) — chart and image upgrade flow
- [docs/operator/release-runbook.md](docs/operator/release-runbook.md) — semver (`vX.Y.Z`) release track: how to cut a release, when to promote, rollback recipe
- [docs/operator/wedged-agent-recovery.md](docs/operator/wedged-agent-recovery.md) — recover a wedged agent with the "Require re-auth" action (force `NeedsAuth` → re-authorize → Running)
- [docs/api-keys.md](docs/api-keys.md) — Kyber platform API key lifecycle (generation, rotation, revocation, scope + threat model)
- [docs/agents-identity-repos.md](docs/agents-identity-repos.md) — Git-backed agent identity (memory, persona, skills); identity-repo git auth via App-minted short-lived tokens, no PAT fallback (kyber#508 Stage 3/4); three creation modes
- [docs/agents-scheduled-jobs.md](docs/agents-scheduled-jobs.md) — OS-level cron persistence inside agents
- [docs/runtime-detection.md](docs/runtime-detection.md) — Detection of new Claude Code versions and models without a Kyber release (poller + `/api/v1/available`)
- [docs/operator/adopting-cc-version.md](docs/operator/adopting-cc-version.md) — How to pin a Claude Code CLI version per-agent or fleet-wide (PR-C)
- [docs/operator/adopting-anthropic-model.md](docs/operator/adopting-anthropic-model.md) — How to choose a Claude model, set its context window, and apply it per-agent or fleet-wide (PR-D)

See `docs/` for design notes and ADRs.

## Security & threat model

Kyber is designed to run agents with substantial power, and its deployment model reflects that:

- **Agent runtime pods run privileged.** Whole-disk persistence requires `Privileged: true` and mounting `/dev/fuse` from the host (see [Architecture](#architecture)). A compromised or misbehaving agent pod should be assumed to have significant reach on its node.
- **Agents accept prompts from connected chat channels.** Telegram and Discord bindings deliver external messages straight into agent sessions (after HMAC verification, dedup, and rate limiting). Anyone who can message a bound channel can influence what an agent does.

Run Kyber on a dedicated cluster whose workloads you trust the agents with — not on a shared production cluster. Treat the cluster as the blast radius of the agents it hosts.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## License

Kyber is licensed under the [Apache License 2.0](LICENSE).

Copyright 2026 Matt Voget.
