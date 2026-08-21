# Security Model

Kyber runs long-lived AI agents with substantial power by design. This page states plainly what the agent pods can do, which cluster to give them, and what the threat model does and does not cover.

## What an agent pod can do

Inside its own sandbox an agent is unrestricted by design: it can be root, install packages, edit any system file, run services, and keep all of it across pod replacement. Toward the node, the boundary is a Linux user namespace. Agent runtime pods are not privileged by default; in-pod root maps to an unprivileged uid on the node, there are no host devices, no hostPath volumes, and no Kubernetes service account token. The pod retains `CAP_SYS_ADMIN`, which it needs to assemble the persistent root filesystem the agent lives in, but inside the user namespace that capability carries no authority on the node. User namespaces need Kubernetes 1.33 and containerd 2.0 or newer; on an older cluster an agent refuses to start rather than run unisolated.

## Give Kyber a dedicated cluster

The sandbox boundary is a user namespace, not a separate kernel. Deploy Kyber on a cluster whose workloads and credentials you are willing to trust the agents with. Not a shared production cluster. Treat the cluster as the blast radius of the agents it hosts.

## Chat channels are an input surface

Agents accept prompts from connected Telegram and Discord channels. Anyone who can message a bound channel can influence what an agent does. Grant channel bindings deliberately.

## What the threat model covers

Bypasses of Kyber's own boundaries are always in scope for security reports: API authentication, webhook HMAC verification, secret handling, and sidecar forwarding. Reports that assume a hostile agent escaping an appropriately dedicated cluster, or abuse by a user who was deliberately granted a chat binding, may be treated as configuration guidance rather than vulnerabilities.

## Reporting and support

Only the latest minor release receives security fixes. Report vulnerabilities privately through [GitHub's private reporting](https://github.com/matty-v/kyber/security/advisories/new), not a public issue. Kyber has a solo maintainer: expect a reply within about a week, on a best-effort basis, with no formal SLA.

## Learn more

- [Security policy and deployment threat model](../../../.github/SECURITY.md): the full policy
- [Agent pod isolation design](../../design/agent-pod-isolation.md): the exact default security context and its remaining gap
- [Architecture](architecture.md): how the components fit together
