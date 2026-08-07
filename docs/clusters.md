# Kyber Cluster Naming Convention

## Pattern

Logical cluster identifiers follow the `kyber-<env>` pattern. The `<env>`
segment identifies the deployment environment, not the underlying hardware.
The logical name is what shows up in the PWA, runbooks, dashboards, and
alerts, so pick it before the first deployment and keep it stable.

## Example clusters

| Logical name | Environment | Substrate | Helm release | ArgoCD Application |
|---|---|---|---|---|
| `kyber-laptop` | Canary / local dev | WSL2/k3s on a developer laptop | `kyber-laptop` | `kyber-laptop` |
| `kyber-falcon` | Release-track | k3s cluster; `environments/falcon/` in the deploy repo | `kyber-falcon` | `kyber-falcon` |
| `kyber-gcp` | Production | Cloud VM (e.g. a GCE e2-standard-4) running k3s | `kyber-gcp` | `kyber-gcp` |

Logical name, Helm release, and ArgoCD Application name all match. The
`helm.releaseName` field in each `environments/<env>/application.yaml`
pins the release name explicitly so future ArgoCD Application renames
don't drift.

In-cluster resources are prefixed accordingly (e.g.,
`kyber-gcp-control-plane`, `kyber-laptop-postgres`). The per-cluster
`values.yaml` and Application spec live in `environments/<env>/` in your
deploy repo (see below).

## VM and agent naming

Names for cloud VMs (e.g., `kyber-small-k3s-server`) and agent pods (e.g.,
`alice`, `r2-d2`) are free-form and chosen by the operator. There is no
enforced convention beyond Kubernetes' kebab-case requirement.

## Adding a new cluster

When a new cluster is spun up, add a row to your own version of the table
above before the first deployment so the logical name is settled before it
shows up in runbooks, dashboards, or alerts. Create `environments/<env>/`
in your deploy repo (the separate GitOps repo holding ArgoCD Applications
and per-environment values) with matching `application.yaml` (set
`metadata.name` and `helm.releaseName` to `kyber-<env>`) and `values.yaml`.
