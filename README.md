<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/kyber-lockup-dark.svg">
  <img src="docs/assets/kyber-lockup-light.svg" alt="Kyber" height="56">
</picture>

[![test](https://github.com/matty-v/kyber/actions/workflows/test.yml/badge.svg)](https://github.com/matty-v/kyber/actions/workflows/test.yml)
[![release](https://img.shields.io/github/v/release/matty-v/kyber)](https://github.com/matty-v/kyber/releases)
[![license](https://img.shields.io/github/license/matty-v/kyber)](LICENSE)

**Kyber runs teams of AI coding agents on hardware you own.**

Each agent gets its own sandboxed computer — its own disk, tools, repos, and
memory — that keeps everything across restarts, upgrades, and crashes. Come back
tomorrow and your agent still has the repo it cloned, the packages it installed,
and everything it learned. You run the whole team from a web console, or from
Telegram and Discord on your phone.

![The Kyber fleet console: dashboard with agent status, per-agent context pressure, and a live terminal peek](docs/assets/pwa-dashboard.png)

## What you can do

**Run a team of long-lived agents, each in its own sandbox.**
An agent isn't a chat window that forgets — it's a persistent machine in a
Kubernetes cluster. It installs what it needs, keeps its own working copies of
your repos, and picks up where it left off after a restart or a hardware
failure.

**Run them wherever you want — a laptop, a spare box, or any cloud.**
Kyber installs the same way on a Windows laptop, a home server, or a managed
Kubernetes cluster at any cloud provider. On GCP it goes a step further and
creates the VMs for you, cheap preemptible ones included.

**Drive your agents from your phone.**
Connect Telegram or Discord and talk to any agent from wherever you are — hand
it work, read what it's doing, answer its questions, approve its next move. You
don't need a terminal to run your team.

**Own the environment, not just the agent.**
You choose the machine each agent lives on: the VM type, the disk, the CPU and
memory, what's installed on it. You can reboot it, stop it to save money, or
open a shell straight into the running agent from the console.

**Put agents on a schedule, and let them hand work to each other.**
Every agent has its own cron that survives restarts, so it can wake up and do
something on its own. Agents can also send signed messages to each other — one
agent's finished work becomes another agent's next prompt, and can wake a
sleeping agent to do it. This is what makes real multi-agent loops possible
instead of one-off prompts.

**Use the subscription you already pay for.**
Claude Code agents sign in with your Claude subscription and Codex agents with
your ChatGPT subscription, so a team of agents doesn't turn into a per-token
API bill. (API keys work too, if you'd rather.)

## Get started

| I want to… | Start here |
|---|---|
| Try Kyber on a cluster (or a local one-liner) — ~15 minutes to a live agent | [Quickstart](docs/quickstart.md) |
| Have an AI assistant do the install for me | [Install with an AI assistant](docs/install-with-an-ai-assistant.md) |
| Run it on my Windows laptop, no cloud account | [WSL2 install guide](docs/installation-wsl2.md) |
| Run it properly on GCP, with managed VMs and HTTPS | [GCP install guide](docs/installation.md) |

Installing is one `helm install` from a published chart — nothing to pin, no
registry credentials, no fork. Upgrades come from inside the console.

One thing to know before you start: agents are powerful on purpose, and their
pods run with elevated privileges so they can keep a whole persistent disk. Give
Kyber a cluster you're comfortable trusting the agents with, not a shared
production one — the [threat model](.github/SECURITY.md#deployment-threat-model)
explains why.

## Learn more

| | |
|---|---|
| **What Kyber does**, in plain operator terms | [docs/product/](docs/product/README.md) |
| **How it's built** — components, CRDs, agent lifecycle | [docs/architecture/overview.md](docs/architecture/overview.md) |
| **Agent capabilities** — [chat channels](docs/agents-comms.md), [git-backed memory & persona](docs/agents-identity-repos.md), [scheduled jobs](docs/agents-scheduled-jobs.md), [runtimes & sign-in](docs/runtimes.md) | [docs/](docs/) |
| **Operating Kyber** — [upgrading](docs/upgrading.md), [releases](docs/operator/release-runbook.md), [recovery](docs/operator/wedged-agent-recovery.md), [API keys](docs/api-keys.md) | [docs/operator/](docs/operator/) |
| **Contributing** — dev environment, test gates, PR conventions | [CONTRIBUTING.md](CONTRIBUTING.md) |
| **Security policy and threat model** — what to trust an agent with, and how to report a vulnerability | [.github/SECURITY.md](.github/SECURITY.md) |
| **What changed** | [CHANGELOG.md](CHANGELOG.md) · [Releases](https://github.com/matty-v/kyber/releases) |

## Status & license

Kyber is v1.x, solo-maintained, and runs the maintainer's own fleets every day.
The CRDs and REST API are still evolving; breaking changes land in minor
versions and are called out in the [CHANGELOG](CHANGELOG.md).

Licensed under the [Apache License 2.0](LICENSE). Copyright 2026 Matt Voget.
