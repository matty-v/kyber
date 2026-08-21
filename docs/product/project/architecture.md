# Architecture

Kyber is a CRD-driven platform: you declare what you want, and controllers work to make the cluster match it and keep it there. This page is the short tour of the three runtime components, the two CRDs, the agent lifecycle, and how inbound messages reach an agent. The real depth lives in the [architecture docs](../../architecture/README.md).

## Three runtime components

Kyber ships as three pieces:

- **Control Plane.** A single Go binary. It serves the REST API, runs agent and machine lifecycle, collects telemetry, runs background workers, and dispatches inbound messages.
- **Node Agent.** A DaemonSet, one pod per Kubernetes node. It is intentionally thin: it ships node metrics and carries out machine actions like reboot and stop when the control plane asks.
- **Agent Runtime.** One pod per agent. At boot, an entrypoint prepares a durable root filesystem on the agent's own volume and chroots the runtime into it. That is how an agent keeps its packages, repos, credentials, and memory across pod recreation and restarts.

## Two CRDs as the source of truth

Platform state lives in two custom resources in the `kyber.io/v1` API group:

- **`Agent`**: one AI agent instance. Its runtime type, target machine, compute resources, scaling mode, identity, secrets, and model.
- **`Machine`**: a VM the platform manages, or a node you already have. Provider, machine type, disk size, spot pricing, zone.

Humans, the fleet console, and other agents all declare intent the same way: by writing the spec. Controllers reconcile reality toward it.

## The agent lifecycle

Each `Agent` moves through a state machine: created, starting, running, and the states around them. Two choices stand out. An out-of-memory kill gets its own state, so an undersized agent waits for a bigger memory limit instead of crash-looping. And suspension unifies spot preemption with idle parking: either way the agent sleeps, and a wake event, such as a Telegram message or a ready replacement machine, brings it back.

## Inbound message dispatch

External senders, including sibling agents and GitHub, POST a signed envelope to the control plane. It passes through a fixed gauntlet: HMAC signature verification, deduplication, binding match and filters, a per-binding rate limit, and a bounded per-agent queue. Only then is it delivered as a prompt into the agent's session. Every outcome, dispatched or dropped with a reason, is recorded on the `Agent` resource so you can see what happened.

## Learn more

- [Architecture overview](../../architecture/overview.md): the full component, CRD, lifecycle, and dispatch detail
- [Agent lifecycle](../../architecture/agent-lifecycle.md): every phase, event, and transition
- [Architecture doc set](../../architecture/README.md): subsystem deep-dives
- [Security model](security.md): what the agent pods' privileges mean for your cluster
