# Kyber PWA — Operator Overview

The Kyber PWA (Fleet Command Console) is the browser-based control surface for a Kyber cluster. It is served by the control-plane binary and authenticated via an API key set on the Settings page.

## Orientation

The sidebar (desktop) and the mobile header show the cluster identifier — `<cluster-name> <version>` (e.g., `kyber-falcon v1.2.0`). Use it as a quick health-check signal: confirm you are on the right cluster and the right build before taking any action. If the identifier shows `version unavailable`, the control-plane API is unreachable. If a new version has been deployed since you opened the tab, a refresh icon appears next to the version — click it to reload.

The full version breakdown (SHA, build date, chart version, substrate) is on the **Settings** page under **Version**.

## Navigation

| Tab | Purpose |
|---|---|
| Fleet | Overview of all machines and agents in the cluster |
| Machines | CRUD for machine CRDs; terminal access |
| Agents | CRUD for agent CRDs; log streaming |
| Metrics | Token usage, cost, and rate metrics per agent |
| Settings | API key management, cluster version info |

## Package

The PWA UI is published as `@matty-v/kyber-pwa-views` on GitHub Packages and consumed by two hosts:

- **`apps/embedded-pwa`** — built into the `kyber` binary (served at the root URL of any kyber-falcon install)
- **`holocron`** — the multi-cluster hub (mounts the package as an npm dependency, one view per cluster)

See `docs/architecture/pwa-views-publish-boundary.md` for the version and publish boundary between the two consumers.
