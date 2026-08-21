# A personal agent fleet

Long-lived assistants that keep their tools, repos, credentials, and memory, running on hardware you control.

A chat session forgets you when the tab closes. An agent on Kyber does not. It lives in its own pod with its own disk, so the packages it installed last month, the repos it cloned, the credentials you granted it, and the notes it wrote to itself are all still there tomorrow.

That changes what you can delegate. Instead of re-explaining your setup every time, you build up a small staff of assistants that already know it.

## What a personal fleet looks like

A typical fleet starts with two or three agents, each with a job:

- A **projects agent** that knows your repos and your conventions. You send it work from Telegram, it opens pull requests, and it remembers the feedback you gave it last week.
- An **ops agent** that holds the credentials for your home infrastructure and can investigate when something breaks, even when you are away.
- A **research agent** that watches topics you care about and writes up what changed.

Each agent's persona, memory, and skills live in a [git-backed identity repo](../capabilities/memory-and-identity.md), so an agent survives not just restarts but full pod rebuilds. You can version, review, and roll back who your agent is.

## Why a cluster and not a laptop

Agents that only run while your terminal is open are not really staff. Kyber runs them on any Kubernetes cluster: a spare box, a Windows laptop that stays plugged in, or cloud VMs it provisions for you. The fleet stays up when your laptop does not, and [you talk to it from your phone](../capabilities/chat-channels.md).

Because agents sign in with the [Claude or ChatGPT subscription you already pay for](../capabilities/runtimes.md), an always-on fleet does not mean a per-token bill.

## Start here

- [Quickstart](../getting-started/quickstart.md): one live agent in about fifteen minutes.
- [Agents and persistence](../capabilities/agents-and-persistence.md): what actually survives a restart.
