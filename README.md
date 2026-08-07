<p align="center"><img src="docs/assets/kyber-logo.svg" width="96" alt="Kyber logo"></p>

# Kyber

[![test](https://github.com/matty-v/kyber/actions/workflows/test.yml/badge.svg)](https://github.com/matty-v/kyber/actions/workflows/test.yml)
[![release](https://img.shields.io/github/v/release/matty-v/kyber)](https://github.com/matty-v/kyber/releases)
[![license](https://img.shields.io/github/license/matty-v/kyber)](LICENSE)

**Kyber runs long-lived AI coding agents as Kubernetes pods with whole-disk persistence.** An agent keeps its entire filesystem — installed packages, working repos, memory, credentials — across pod restarts, platform upgrades, and spot-VM preemption. Agents and machines are declared as two CRDs, a controller-runtime control plane reconciles them, and you operate the fleet from a PWA or straight from Telegram and Discord.

## Features

- **Persistent agents.** A three-tier overlay dispatcher (kernel overlayfs, fuse-overlayfs, bind-mount `$HOME`) gives each agent a durable whole-pod filesystem on a PersistentVolume. Agents survive recreation and spot preemption with full continuity.
- **Claude Code and Codex runtimes.** Claude Code boots non-interactively via Anthropic OAuth; Codex uses ChatGPT device login or an OpenAI API key.
- **Two-way chat channels.** Talk to any agent over Telegram or Discord through per-agent MCP sidecars, gated by per-agent user-ID allowlists.
- **Git-backed identity.** Each agent's memory, persona, and skills live in an identity repo, authenticated with short-lived GitHub-App-minted tokens.
- **Fleet PWA.** Create, wake, exec into, and observe agents; configure channels; multi-cluster aware.
- **Scheduled jobs.** Cron persists inside agents across restarts.
- **Runtime detection.** New Claude Code CLI versions and models are detected and adoptable without a Kyber release.
- **Runs cheap.** Spot/preemptible VMs, machine capacity management, per-agent context-window budgets.
- **Observable.** OpenTelemetry metrics, activity states, session transcripts and history.

## Try it locally

A deterministic, mock-backed Kyber you can bring up, drive over the API, and tear down. No cloud account, no credentials:

```bash
scripts/devenv/up.sh      # k3d + mock-provider Kyber, API on localhost:18080
scripts/devenv/down.sh    # tear down, no orphans
```

`up.sh` prints the entry-point contract: API base URL, health check, throwaway test credentials. [`scripts/devenv/README.md`](scripts/devenv/README.md) covers what's mocked and how to drive it.

## Install

| Path | For | Guide |
|---|---|---|
| **macOS / Linux, local** | Full stack on one machine via k3d (Docker Desktop on macOS): live agent pods with whole-disk persistence, mock cloud | [`scripts/devenv/full-local.md`](scripts/devenv/full-local.md) |
| **Windows, WSL2** | A Windows laptop as a standalone install; no cloud infra (native k3s + Tailscale Funnel) | [`docs/installation-wsl2.md`](docs/installation-wsl2.md) |
| **GCP** | Production multi-VM install (Terraform + GCE) | [`docs/installation.md`](docs/installation.md) |
| **Your own cluster** | You already run Kubernetes | Install the chart directly: [`deploy/helm/kyber`](deploy/helm/kyber) is the deployment contract, and every value is documented in [`values.yaml`](deploy/helm/kyber/values.yaml). The chart requires explicit image-tag pins. |

<details>
<summary><b>Installing with an AI assistant</b> (recommended for non-developers) — click to expand</summary>

Open your AI assistant of choice (Claude Code, Cursor, ChatGPT, etc.), give it the prompt below, and follow its instructions.

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

</details>

## Architecture

Three core components, plus per-agent sidecars:

- **Control Plane.** A modular monolith (single Go binary): REST API, agent and machine lifecycle controllers, telemetry, background workers. Built on controller-runtime.
- **Node Agent.** A thin DaemonSet binary, one per node, for machine-level concerns: node metrics and machine actions (reboot, stop) on instruction from the control plane.
- **Agent Runtime.** One pod per agent with a persistent whole-disk filesystem. Each agent pod also carries a **status sidecar** (activity and telemetry) and, per enabled channel, an **MCP channel sidecar** (`kyber-mcp-telegram`, `kyber-mcp-discord`) bridging chat to the agent session.

> **Pod requirements:** agent pods run `Privileged: true` (required for both fuse-overlayfs and bind mounts) and mount `/dev/fuse` from the host. See the [Security & threat model](#security--threat-model).

Two CRDs are the source of truth. Operators declare intent; controllers reconcile against reality:

- `Agent` (`kyber.io/v1`) — one AI agent instance: target machine, runtime, resources, scaling mode, identity, secrets, model
- `Machine` (`kyber.io/v1`) — one managed cloud VM: provider, machine type, disk, spot pricing, zone

Lifecycle flows through the k8s API. Async and real-time events (agent wake, machine health, task completion) flow through Redis.

Full detail: [`docs/architecture/overview.md`](docs/architecture/overview.md).

## Tech stack

| Component | Technology |
|---|---|
| Control plane | Go + controller-runtime (kubebuilder) |
| Node agent | Go (static binary) |
| PWA | React + Vite + TypeScript |
| Database | PostgreSQL (fleet metadata) |
| Events | Redis |
| Orchestration | Kubernetes (k3s / GKE) |
| Infrastructure | Terraform (GCP-first) |
| Deployment | Helm chart in-repo ([`deploy/helm/kyber`](deploy/helm/kyber)); optionally GitOps via ArgoCD |
| Telemetry | OpenTelemetry |
| Agent runtimes | Claude Code, Codex |
| Secrets | Kubernetes Secrets; GCP Secret Manager integration on GCP installs |

## Repository layout

```
kyber/
├── cmd/               # Entrypoints: control-plane, node-agent, status-sidecar,
│                      #   MCP channel sidecars, and supporting tools
├── pkg/               # Go packages: API + CRD types, controllers, adapters,
│                      #   runtimes, inbound dispatch, telemetry, …
├── apps/, packages/   # PWA: embedded app + published view components
├── images/            # Dockerfiles for the published container images
├── deploy/helm/kyber/ # Helm chart (+ generated CRD manifests)
├── infra/terraform/   # GCP infrastructure modules
├── scripts/devenv/    # One-command mock-backed dev environment
├── docs/              # Product docs, architecture, operator runbooks, ADRs
└── test/              # Contract, integration (envtest), and e2e (k3d) suites
```

The maintained, detailed map lives in [`AGENTS.md`](AGENTS.md).

## Runtimes & authentication

Claude Code agents authenticate via a PWA-driven PKCE OAuth flow: authorize once at agent creation, and every later pod boot refreshes credentials on its own, so agents come up already logged in. Codex agents use ChatGPT device login or an OpenAI API key. Details in [`docs/runtimes.md`](docs/runtimes.md) and the [OAuth design note](docs/design/2026-04-14-programmatic-oauth-design.md).

## CI & releases

CI builds and publishes all container images to GHCR (`ghcr.io/matty-v/kyber-*`) on every merge to `main` (`:latest` + `:<sha>`). Releases are semver tags: a `vX.Y.Z` tag rebuilds every image from source at that commit and publishes a [GitHub Release](https://github.com/matty-v/kyber/releases). The Helm chart is the deployment contract for both `helm install` and GitOps consumers. Operator-side detail: [`docs/upgrading.md`](docs/upgrading.md) and [`docs/operator/release-runbook.md`](docs/operator/release-runbook.md).

## Documentation

**Product & architecture:**

- [`docs/product/README.md`](docs/product/README.md) — product source of truth: what Kyber does, in operator-observable terms
- [`docs/architecture/overview.md`](docs/architecture/overview.md) — components, CRDs, control-plane module map, agent lifecycle, inbound dispatch
- [`docs/runtimes.md`](docs/runtimes.md) — runtime selection and authentication

**Operating Kyber:**

- [`docs/installation.md`](docs/installation.md) / [`docs/installation-wsl2.md`](docs/installation-wsl2.md) — install guides (see [Install](#install))
- [`docs/upgrading.md`](docs/upgrading.md) — chart and image upgrade flow
- [`docs/operator/release-runbook.md`](docs/operator/release-runbook.md) — cutting releases, promotion, rollback
- [`docs/operator/wedged-agent-recovery.md`](docs/operator/wedged-agent-recovery.md) — recovering a wedged agent via forced re-auth
- [`docs/api-keys.md`](docs/api-keys.md) — platform API key lifecycle and scopes

**Agent capabilities:**

- [`docs/agents-comms.md`](docs/agents-comms.md) — Telegram/Discord channel setup
- [`docs/agents-identity-repos.md`](docs/agents-identity-repos.md) — git-backed agent identity: memory, persona, skills
- [`docs/agents-scheduled-jobs.md`](docs/agents-scheduled-jobs.md) — cron persistence inside agents
- [`docs/runtime-detection.md`](docs/runtime-detection.md), [`docs/operator/adopting-cc-version.md`](docs/operator/adopting-cc-version.md), [`docs/operator/adopting-anthropic-model.md`](docs/operator/adopting-anthropic-model.md) — adopting new CLI versions and models without a release

Contributors: start with [`AGENTS.md`](AGENTS.md), then [`CODE_QUALITY.md`](CODE_QUALITY.md) and [`REVIEWING.md`](REVIEWING.md). Design notes and ADRs live in [`docs/`](docs/).

## Security & threat model

Kyber is designed to run agents with substantial power, and its deployment model reflects that:

- **Agent runtime pods run privileged.** Whole-disk persistence requires `Privileged: true` and mounting `/dev/fuse` from the host (see [Architecture](#architecture)). A compromised or misbehaving agent pod should be assumed to have significant reach on its node.
- **Agents accept prompts from connected chat channels.** Telegram and Discord bindings deliver external messages straight into agent sessions (after webhook-secret verification, dedup, and rate limiting). The primary access control is the per-agent **user-ID allowlist** — messages from anyone not on it are ignored — so treat allowlist membership as operator-level trust: anyone listed can influence what an agent does.

Run Kyber on a dedicated cluster whose workloads you trust the agents with — not on a shared production cluster. Treat the cluster as the blast radius of the agents it hosts.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## Project status

Kyber is v1.x and solo-maintained. The 1.0 release opened the project to the public after a long private development history; the [CHANGELOG](CHANGELOG.md) explains the versioning reset. It runs the maintainer's production fleets today. Expect the CRD schemas and REST API to keep evolving: breaking changes land in minor versions and are called out in the [CHANGELOG](CHANGELOG.md) and [Releases](https://github.com/matty-v/kyber/releases).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the dev environment, test gates, and PR conventions, and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for community expectations. For non-trivial changes, open an issue first.

## License

Kyber is licensed under the [Apache License 2.0](LICENSE).

Copyright 2026 Matt Voget.
