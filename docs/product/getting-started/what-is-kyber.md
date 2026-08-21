# What is Kyber?

Kyber is a self-hosted platform for running and managing a fleet of long-lived
AI agents on a Kubernetes cluster. Each agent gets full autonomous access to its
own sandboxed pod, including disk, tools, repos, and memory, and keeps all of it
across restarts. You manage the fleet from a web console, the API, or from
Telegram and Discord on your phone. It is made to run with the Claude or ChatGPT
subscription you already pay for.

## The mental model

An agent in Kyber is not a chat session. It is long-lived infrastructure: a
worker with its own sandboxed pod, its own persistent filesystem, its own
identity, and its own model. Installed packages, cloned repos, credentials, and
memory all survive restarts, upgrades, and machine preemption.

You declare what you want, an agent of a certain shape on a certain kind of
machine, and Kyber works to make reality match that intent and keep it there.
Cron inside each agent survives restarts, and agents can send each other signed
messages, so one agent's output becomes another's next prompt.

## Agents and machines

Two concepts an operator creates and manages:

- **Agent**: a single AI agent instance. You choose its model, its compute
  size, its identity, and which machine it targets. Its filesystem is durable,
  so its work and memory persist across pod recreation, restarts, and machine
  interruption. An agent can also be given a private identity repo, its own
  GitHub repository of persona, long-term memory, and session state, so its
  identity is versioned and survives even a full teardown. See
  [Memory and identity](../capabilities/memory-and-identity.md).
- **Machine**: where agents run. A Machine can be a cloud VM that Kyber
  provisions and manages for you (on GCP, cheaper spot capacity included), or
  it can stand for a node that already exists in your cluster, the shape used
  by single-box installs and by managed Kubernetes services where your own node
  pool provides the capacity. Kyber handles bringing machines up and recovering
  agents when interruptible capacity is reclaimed.

## What ships in the box

- **A control plane** that manages agents and machines, plus in-cluster
  Postgres and Redis. Everything installs from a published Helm chart.
- **The fleet console**, a browser PWA served by the control plane: agent
  status, per-agent token usage and context budget, a live terminal into any
  running agent.
- **Two agent runtimes**: Claude Code and Codex. Claude Code agents sign in
  with your Claude subscription, Codex agents with your ChatGPT subscription.
  API keys work too. See [Runtimes](../capabilities/runtimes.md).
- **Telegram and Discord channels** for two-way chat with any agent, no
  terminal needed. See [Chat channels](../capabilities/chat-channels.md).

## Who it is for

Kyber is for operators who want to own the whole environment, not just the
agent: pick each agent's machine, disk, CPU, and memory, reboot it, stop it, or
open a shell into it. It is self-hosted, licensed Apache 2.0, at v1.x,
solo-maintained, and runs the maintainer's own fleets every day.

Ready to try it? Start with the [quickstart](./quickstart.md) or browse the
[install options](./installation.md).

## Learn more

- [The Kyber source repository](https://github.com/matty-v/kyber)
- [Architecture](../project/architecture.md)
