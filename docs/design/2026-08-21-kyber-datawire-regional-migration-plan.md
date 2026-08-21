# kyber-datawire regional GKE migration plan

**Status:** Approved for Phases A and B
**Date:** 2026-08-21
**Source:** zonal GKE Standard cluster `kyber-datawire` in `us-central1-a`
**Target:** regional GKE Standard cluster `kyber-datawire-regional` in `us-central1`

## Outcome

Build a parallel, representative Google Cloud Kyber installation. Recreate its
Machines and Agents from clean state, prove regional Spot recovery, and retain
the old cluster until the operator separately approves public cutover and
destruction.

The source Agent roots, transcript offsets, Postgres data, Redis data, and PVC
contents are disposable. Preserve only configuration and selected credentials
needed to reconstruct the installation. Never copy decoded secrets into git or
ordinary logs.

## Target topology

- Regional GKE Standard control plane in `us-central1`.
- Reliable platform pool for Kyber, Postgres, Redis, and the tunnel connector.
- Managed Machine node pools eligible in `us-central1-a` and `us-central1-c`.
- Each logical Machine uses GKE total-count autoscaling with minimum and maximum
  one and location policy `ANY`, preserving Kyber's one-Machine/one-Node
  invariant while allowing GKE to choose either eligible zone.
- A `pd-balanced` regional-PD StorageClass replicated in `us-central1-a` and
  `us-central1-c`, with `WaitForFirstConsumer` and matching allowed topology.
- Agent roots, transcript offsets, Postgres, and Redis use that regional class.
- Spot remains non-guaranteed and never silently falls back to standard
  capacity.

## Phase A — build and prove the target substrate

1. Record immutable source inventory: cluster description, Helm values and
   manifest, CRDs, sanitized Agent/Machine specs, selected ConfigMaps, Secret
   names, PVC/PV mappings, disk identifiers, and tunnel configuration.
2. Release the regional-capable Kyber changes normally; do not deploy an
   unreleased `main` image to the representative cluster.
3. Create the new regional cluster and reliable platform pool alongside the
   source, after confirming network ranges, quotas, Workload Identity, PD CSI,
   and enforcing NetworkPolicy.
4. Install Kyber with a temporary private access path. Do not move the public
   hostname.
5. Transfer only selected credentials directly and securely.
6. Verify API/PWA health, browser auth, NetworkPolicy, provider permissions,
   model discovery, storage provisioning, and logs.
7. Create disposable reliable and Spot Machines. Force loss of the active Spot
   node and prove a replacement becomes Ready in an eligible zone.

**Gate A:** a disposable Agent must retain a sentinel on its regional PVC
across node replacement before reconstruction proceeds.

## Phase B — reconstruct the installation

1. Install fresh Postgres and Redis on new regional PDs.
2. Recreate Machine intent through the target API/PWA so Kyber owns every node
   pool from birth.
3. Recreate Agents through the normal wizard and re-establish runtime and comms
   authentication. Treat roots and transcript offsets as new.
4. Keep the source serving and avoid meaningful concurrent work on both.

**Gate B:** at least one real Agent is authenticated, uses a regional PVC, and
can complete normal work before public cutover is considered.

## Phase C — acceptance and public cutover

Run full Agent, Machine, PWA, API, WebSocket, auth, networking, metrics, logs,
and forced cross-zone Spot-recovery checks. Moving the Cloudflare route is a
separate action requiring explicit operator approval after the results and
rollback procedure are presented.

## Phase D — retirement

Keep the source as the immediate hostname rollback path until the operator
confirms normal work on the target. Then present an exact inventory of the old
cluster, node pools, disks, addresses, tunnel connector, and other owned
resources. Deletion is destructive and requires a separate explicit approval.

## Approval boundaries

Approval to begin authorizes implementation plus Phases A and B, including the
new cluster and its associated cost. It does not authorize a GitHub PR or
release workflow without the normal repository confirmation, the Cloudflare
cutover, or deletion of any source resource.
