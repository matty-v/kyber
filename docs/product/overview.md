# Kyber — Product Overview

> **Verification status:** grounded in current shipped behavior as described in
> the repo's operator-facing docs (`README.md`, `docs/installation.md`, the PWA
> overview) and the shipped product surfaces. Items not confirmable against a
> running instance are marked _Unverified_ on their capability page. Maintained
> by Yoda; see [README](README.md).

## What Kyber is

Kyber is a **self-hosted platform for running a fleet of long-lived AI agents on
cost-optimized cloud infrastructure**. An operator uses Kyber to create agents,
keep them alive and working across restarts and interruptions, send work to
them, and watch what they're doing — without babysitting the machines underneath.

The defining idea: each **agent** is a long-lived worker with its own persistent
filesystem, its own identity, and its own model. It survives the machine under
it being restarted or reclaimed. An operator declares *what they want* — an agent
of a certain shape, on a certain kind of machine — and Kyber works to make
reality match that intent and keep it there.

## What an operator works with

Two core concepts an operator creates and manages:

- **Agent** — a single AI agent instance. The operator chooses its model, its
  compute size, how it scales, its identity, and which machine it targets. The
  agent has a durable filesystem, so its work and memory persist across pod
  recreation, restarts, and machine interruption. An agent can also be given a
  private **identity repo** — its own GitHub repository of persona, long-term
  memory, and session state — so its identity is versioned and survives even a
  full teardown. Kyber manages that repo on the agent's behalf through a
  **Kyber Platform GitHub App** the operator configures once per install
  (see [`../installation.md` § 5b](../installation.md#5b-register-the-kyber-github-app)
  and [`../agents-identity-repos.md`](../agents-identity-repos.md)); an install
  without that App simply runs agents without a managed identity repo.
- **Machine** — a cloud VM that Kyber manages on the operator's behalf (its type,
  size, zone, and whether it uses cheaper interruptible/spot pricing). Agents run
  on machines; Kyber handles bringing machines up and recovering agents when an
  interruptible machine is reclaimed. A Machine can also stand for **a node that
  already exists in the cluster** instead of a VM Kyber provisions — the shape
  used by single-box installs, and by installs on a managed Kubernetes service
  where the operator's own node pool provides the capacity and Kyber creates no
  VMs. For that kind of Machine, an operator whose cluster has more than one
  node can say which node it belongs to by labelling that node
  `kyber.io/machine=<machine-name>`: Kyber keeps the Machine there and waits for
  that node to be ready rather than relocating its agents somewhere else. Left
  unlabelled, such a Machine claims the first available node, as it always has —
  fine on a single-node install, but on a multi-node cluster the label is what
  keeps a Machine from drifting between nodes underneath its agents.

## Capability areas

The product breaks down into these areas. Each has (or will have) its own page
describing the observable behavior in detail:

| Area | What an operator gets | Page |
|---|---|---|
| **Agent lifecycle** | Create, run, stop, suspend, restart, and delete agents; understand the state an agent is in and what to do about it | [`agent-lifecycle.md`](agent-lifecycle.md) |
| **Operator surfaces (PWA / Holocron)** | A browser console to see and control one cluster, and a multi-cluster hub across several | [`pwa-holocron.md`](pwa-holocron.md) |
| **Machine / capacity** | Manage the VMs agents run on, including cheaper interruptible capacity | _follow-up page_ |
| **Inbound prompts** | Let external senders deliver work to a specific agent over a gated, authenticated channel | _follow-up page_ |
| **Telegram channel** | Two-way mobile messaging with buttons, reactions, albums, files, and working indicators | [`telegram.md`](telegram.md) |
| **Authentication** | Authorize an agent to its model provider so it boots ready to work | _follow-up page_ |
| **Token / context budget** | See per-agent usage, cost, and how much of an agent's context budget is in use | _follow-up page_ |

## How an operator interacts with Kyber

- **The PWA (Fleet Command Console)** — the browser surface served by a Kyber
  cluster, where an operator creates and manages agents and machines, streams
  logs, and reads usage. See [`pwa-holocron.md`](pwa-holocron.md).
- **Holocron** — a multi-cluster hub that presents one view per cluster, for
  operators running more than one Kyber install.
- **Inbound prompts** — a gated channel external systems use to send work to a
  named agent (a follow-up page covers the observable behavior).

## Where to go next

- The states an agent moves through, and the operator action for each:
  [`agent-lifecycle.md`](agent-lifecycle.md)
- The operator surfaces in detail: [`pwa-holocron.md`](pwa-holocron.md)
- **How** any of this is built (the mechanism, not the behavior):
  [`../architecture/`](../architecture/overview.md)
