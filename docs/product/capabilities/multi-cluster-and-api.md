# Multi-cluster and the API

Kyber scales from one box to several clusters, and everything the console does is backed by a REST API you can call yourself. That means the same platform covers a laptop experiment, a production install, and scripts that automate both.

## Several clusters, one place

Each Kyber install is its own cluster with its own [console](fleet-console.md). For operators running more than one, Holocron is a multi-cluster hub: it mounts the same console once per cluster, so you move between installs without juggling URLs or separate logins. Every screen shows a cluster identifier with the cluster's name and version, so you always know which cluster and which build you are about to act on.

Clusters follow a simple naming convention: the logical name is the Helm release name, in the `kyber-<env>` pattern, such as `kyber-laptop` or `kyber-gcp`. Pick a name that describes the environment's role, not its hardware, and keep it stable. The values file plus the release name is the complete definition of a Kyber install, so standing up a new environment is a values file and an install away. See the [quickstart](../getting-started/quickstart.md) for the first one.

## The API and its keys

All operator-facing requests go through the control plane's REST API under `/api/v1/*`, authorized by the Kyber API key: a single 256-bit credential generated at install time. The console itself is a client of this API, exchanging the key once for a session cookie rather than keeping it in browser storage.

The key can be rotated programmatically with no downtime: one authenticated call swaps the key, and the old one stops working on the next request. A manual rotation path exists for compromise recovery.

For agent lifecycle actions, you can also issue scoped keys. A `lifecycle:write` key can start, stop, and restart agents; the more impactful force-re-auth verb needs `lifecycle:admin`, which includes everything `write` grants. Enforcement is opt-in per cluster: under-scoped callers are audit-logged but not blocked until you turn it on, so you can define scoped callers, watch the audit log, and then enable enforcement.

Agents authenticate to the platform separately, with per-pod credentials that only let an agent act on itself; no agent can drive another agent through the API.

One rule to know when creating agents over the API rather than the console: an agent that signs in with a Claude subscription must be created with the complete authorization exchange, meaning the authorization code and both PKCE values, in the create call itself. Skip them and the agent is created without stored credentials, and the re-authorize action cannot backfill them later; the only fix is to delete the agent and recreate it with the full flow. An invalid or already-used code fails the create immediately with a clear error, so a create that succeeds always leaves working credentials.

## Learn more

- [Cluster naming convention](../../clusters.md): the pattern, examples, and the GitOps note.
- [Kyber platform API keys](../../api-keys.md): generation, rotation, scoped callers, and the threat model.
- [What agents can and cannot do](../../agent-manual.md): the platform's contract with the agents themselves.
