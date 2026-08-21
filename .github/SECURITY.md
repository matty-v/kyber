# Security Policy

## Supported versions

Only the latest minor release (`vX.Y.*`) receives security fixes. If you are
running an older version, upgrade to the latest release before reporting an
issue you can no longer reproduce there.

## Reporting a vulnerability

Please do **not** open a public issue for security problems.

1. **Preferred:** use GitHub's private vulnerability reporting on
   [matty-v/kyber](https://github.com/matty-v/kyber/security/advisories/new)
   ("Report a vulnerability" under the repository's Security tab).
2. **Fallback:** email <matt.voget@gmail.com> with a description of the issue,
   reproduction steps, and the version/commit affected.

## Response expectations

Kyber is maintained by a solo maintainer. Reports are handled on a best-effort
basis: you should normally hear back within a week, but there is no formal SLA.
Please allow a reasonable window for a fix before public disclosure, and
coordinate disclosure timing in the report thread.

## Deployment threat model

Kyber runs long-lived AI coding agents as Kubernetes pods with substantial
power by design: agent runtime pods are unprivileged and run in a user
namespace (`hostUsers: false`, so in-pod root maps to an unprivileged uid on
the node), with `CAP_SYS_ADMIN` added *inside that namespace* to assemble the
chroot-based whole-disk persistence. No host devices, no hostPath volumes, no
service-account token. Agents also accept prompts from connected chat channels
(Telegram/Discord), so anyone who can message a bound channel can influence
what an agent does. Deploy Kyber on a dedicated cluster whose workloads and
credentials you are willing to trust the agents with — not on a shared
production cluster — and treat the cluster as the blast radius of the agents
it hosts. Reports that assume a hostile agent escaping an *appropriately
dedicated* cluster, or abuse by a user who was deliberately granted a chat
binding, may be treated as configuration guidance rather than vulnerabilities;
bypasses of Kyber's own boundaries (API authentication, webhook HMAC
verification, secret handling, sidecar forwarding) are always in scope. See
the [agent pod isolation design](../docs/design/agent-pod-isolation.md) for the
exact default security context and its remaining gap.
