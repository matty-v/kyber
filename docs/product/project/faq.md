# FAQ

Common questions about running Kyber, answered plainly.

## What does it cost to run?

Kyber itself is free, open-source software under the Apache License 2.0. Claude Code agents sign in with your existing Claude subscription and Codex agents with your ChatGPT subscription, so you are not paying per token; API keys work too. Beyond that, you pay only for the machines you run it on, and Kyber supports cheaper interruptible spot VMs.

## What hardware or cluster do I need?

Any Kubernetes cluster: a Windows laptop, a Mac, a spare box, or any cloud. The one hard requirement is Kubernetes 1.33 or newer with containerd 2.0 or newer, because agent isolation runs on user namespaces; on an older cluster, agents refuse to start rather than run unisolated. Small installs run on k3s; larger ones run on GKE or GCE, where Kyber can provision the VMs for you. There are dedicated guides for [macOS, WSL2, and GCP](../getting-started/installation.md).

## Do I need a cloud account?

No. The [quickstart](../getting-started/quickstart.md) goes from an empty cluster, or a k3d one-liner, to a fleet console with one live Claude Code agent, with no cloud account required.

## Which agent runtimes are supported?

Claude Code and Codex. See [runtimes](../capabilities/runtimes.md) for how each signs in.

## Is it production-ready?

Kyber is v1.x, solo-maintained, and runs the maintainer's own fleets every day. The CRDs and REST API are still evolving: breaking changes land in minor versions and are called out in the [CHANGELOG](../../../CHANGELOG.md).

## Can the agents be trusted with the cluster?

Inside its own pod an agent is deliberately unrestricted: root, packages, services, all of it persistent. Toward the node, each agent pod runs in a Linux user namespace with no host devices and no service account token, so in-pod root maps to an unprivileged uid on the node. That boundary is a user namespace, not a separate kernel, so still give Kyber a dedicated cluster you are comfortable trusting the agents with, and treat that cluster as their blast radius. The [security model](security.md) covers this honestly.

## What happens when a machine is restarted or reclaimed?

The agent keeps its whole filesystem: installed packages, cloned repos, credentials, and memory all survive restarts, upgrades, and preemption. When a spot machine is reclaimed, Kyber parks the agent and brings it back once a replacement machine is ready. See [agents and persistence](../capabilities/agents-and-persistence.md).

## Do I need a terminal to manage the fleet?

No. You can manage agents from the web console, the API, or two-way Telegram and Discord chat on your phone. See [chat channels](../capabilities/chat-channels.md) and the [fleet console](../capabilities/fleet-console.md).

## How do I get updates?

New versions land on the [releases page](https://github.com/matty-v/kyber/releases), with changes described in the [CHANGELOG](../../../CHANGELOG.md). The fleet console also shows when a newer version is available and can install it; note that only the latest minor release receives security fixes.


