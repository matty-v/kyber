# Multi-cluster and the API

Kyber scales from one box to several clusters, and everything the console does is backed by a REST API you can call yourself. That means the same platform covers a laptop experiment, a production install, and scripts that automate both.

## Several clusters, one place

Each Kyber install is its own cluster with its own [console](fleet-console.md). For operators running more than one, Holocron is a multi-cluster hub: it mounts the same console once per cluster, so you move between installs without juggling URLs or separate logins. Every screen shows a cluster identifier with the cluster's name and version, so you always know which cluster and which build you are about to act on.

Clusters follow a simple naming convention: the logical name is the Helm release name, in the `kyber-<env>` pattern, such as `kyber-laptop` or `kyber-gcp`. Pick a name that describes the environment's role, not its hardware, and keep it stable. The values file and release name define the configured services. Durable Agent data and managed credentials are installation state and need their own protected backup; copying a values file does not restore them. See the [quickstart](../getting-started/quickstart.md) for the first one.

## The API and its keys

Operator-facing requests use the control plane's REST API under `/api/v1/*`.
An installation administrator starts with a strong key and can inspect access in
**Settings → API access**. On a compatible release, explicit setup retains
existing keys and moves their future management into Kyber; upgrading alone does
not transfer ownership.

Administrators can create named keys, choose enforced actions and exact targets,
edit grants, rotate an individual key and revoke it. Existing values cannot be
revealed later. Kyber protects the last non-expiring administrator and applies
managed access changes to bearer requests, browser sessions and protected live
connections within ten seconds. The console exchanges a key for an HttpOnly
session cookie rather than storing the raw value in readable browser storage.

Restricted access is always enforced. Lifecycle, terminals, secrets/logs, tasks,
capabilities and usage have separate permissions; a narrow key cannot gain full
access through the historical lifecycle observe-only switch. Durable tasks also
check tenant/principal ownership and the allowed Agent resource.

The UI shows effective access, management source, observed usage and recent
sensitive activity with explicit coverage limits. Shared key use identifies the
credential, not an individual human. Unknown outcomes remain pending until
verified; status reads do not replay a key change.

Agents authenticate to the platform separately, with per-pod credentials that only let an agent act on itself; no agent can drive another agent through the API.

When creating agents through the API, supply the selected runtime’s required authentication inputs. Claude subscription creation uses the complete PKCE exchange; Codex subscription creation starts the in-pod device flow. The console guides those flows, and the runtime descriptor advertises supported auth modes. Do not assume that a credential format accepted by one runtime works for another.

## Learn more

- [Cluster naming convention](../../clusters.md): the pattern, examples, and the GitOps note.
- [Kyber platform API keys](../../api-keys.md): generation, rotation, scoped callers, and the threat model.
- [What agents can and cannot do](../../agent-manual.md): the platform's contract with the agents themselves.
