# Kyber Cluster Naming Convention

## Pattern

Logical cluster identifiers follow the `kyber-<env>` pattern. The `<env>`
segment identifies the deployment environment, not the underlying hardware.
The logical name is what shows up in the PWA, runbooks, dashboards, and
alerts, so pick it before the first deployment and keep it stable.

**The logical name is the Helm release name.** That is the whole convention —
`helm install kyber-gcp …` makes the logical name real, and the workloads are
named after it (`kyber-gcp-control-plane`, `kyber-gcp-postgres`). Getting it
right at install time matters because renaming a release later means
reinstalling.

A few shared ConfigMaps keep a fixed `kyber-` name at any release name
(`kyber-fleet-defaults`, `kyber-model-context-windows`,
`kyber-provider-rates`), so don't rely on the release prefix to select
everything the chart owns — use the chart's labels for that.

## Example clusters

| Logical name | Environment | Substrate | Helm release |
|---|---|---|---|
| `kyber-laptop` | Local / single box | WSL2 + k3s on a developer laptop | `kyber-laptop` |
| `kyber-gcp` | Production | Cloud VM (e.g. a GCE e2-standard-4) running k3s | `kyber-gcp` |

Pick names that describe the environment's *role*, not its hardware — a cluster
that moves from a laptop to a NUC should not need renaming.

## VM and agent naming

Names for cloud VMs (e.g., `kyber-small-k3s-server`) and agent pods (e.g.,
`alice`, `r2-d2`) are free-form and chosen by the operator. There is no enforced
convention beyond Kubernetes' kebab-case requirement.

## Adding a new cluster

Settle the logical name before the first deployment, so it is stable by the time
it appears in runbooks, dashboards, or alerts. Then keep that cluster's
`values.yaml` somewhere durable — the install guides put it at
`~/.config/kyber/values-<env>.yaml` — and pass it to every `helm install` and
`helm upgrade` for that cluster. The values file plus the release name is the
complete definition of a Kyber install.

## If you run GitOps

Nothing about Kyber requires a GitOps controller, and the install guides do not
use one. If you do put Kyber under ArgoCD or Flux, keep the Application name
equal to the Helm release name equal to the logical name, and pin the release
name explicitly (ArgoCD: `helm.releaseName`) so a future Application rename
doesn't silently rename every resource in the cluster.

Per-cluster values then live in your own deploy repo rather than in
`~/.config/kyber/`. Note that a self-upgrading cluster and a GitOps-managed
cluster are mutually exclusive — see
[upgrading.md § Two delivery models](./upgrading.md#two-delivery-models).
