# Kyber documentation

New here? Start with the [Quickstart](quickstart.md). It takes any Kubernetes
cluster to a running fleet console with one live agent in about 15 minutes. Not
a developer? [Install with an AI assistant](install-with-an-ai-assistant.md).

## Install

| Guide | For |
|---|---|
| [quickstart.md](quickstart.md) | Any cluster (or a local k3d one-liner), the short path |
| [install-with-an-ai-assistant.md](install-with-an-ai-assistant.md) | Let an AI assistant drive the install for you |
| [installation.md](installation.md) | GCP: production multi-VM install, Terraform, HTTPS |
| [installation-wsl2.md](installation-wsl2.md) | A Windows laptop, no cloud account |
| [upgrading.md](upgrading.md) | Moving an existing install to a new version |

## Using Kyber

| Guide | Covers |
|---|---|
| [product/](product/README.md) | What Kyber does, in operator-observable terms |
| [agent-manual.md](agent-manual.md) | The manual each agent reads about the platform it lives on |
| [agents-comms.md](agents-comms.md) | Telegram and Discord channels for two-way chat |
| [agents-identity-repos.md](agents-identity-repos.md) | Git-backed agent memory, persona, and skills |
| [agents-scheduled-jobs.md](agents-scheduled-jobs.md) | Cron jobs that persist inside an agent |
| [runtimes.md](runtimes.md) · [runtime-detection.md](runtime-detection.md) | Claude Code and Codex: sign-in, models, adopting new versions |
| [clusters.md](clusters.md) · [api-keys.md](api-keys.md) · [metrics-tab.md](metrics-tab.md) | Multi-cluster, API keys, metrics |

## Operating Kyber

| Guide | Covers |
|---|---|
| [operator/release-runbook.md](operator/release-runbook.md) | Cutting releases, promotion, rollback |
| [operator/wedged-agent-recovery.md](operator/wedged-agent-recovery.md) | Recovering a wedged agent |
| [operator/telemetry.md](operator/telemetry.md) | Metrics and traces |
| [operator/](operator/) | The rest of the operator runbooks |

## Building Kyber

| Doc | Covers |
|---|---|
| [architecture/overview.md](architecture/overview.md) | Components, CRDs, lifecycle, inbound dispatch |
| [architecture/](architecture/README.md) | Per-subsystem architecture notes |
| [contributing/code-quality.md](contributing/code-quality.md) | Go and TypeScript conventions CI gates on |
| [contributing/reviewing.md](contributing/reviewing.md) | Reviewer gotchas and fragile-area map |
| [contributing/design-quality.md](contributing/design-quality.md) | Front-end design standard for the PWA |
| [adr/](adr/) | Architecture decision records, plus an inferred decision log |
| [design/](design/) · [specs/](specs/) | Dated design notes and specs |

The repo-level orientation file for contributors and agents is
[`AGENTS.md`](../AGENTS.md); [`CONTRIBUTING.md`](../CONTRIBUTING.md) covers the
dev environment, test gates, and PR conventions.
