# GKE cost-optimized fallback spike

Status: implemented as an explicit installer opt-in. Existing GKE installs
remain `Unsupported` until `compute.gke.reliableFallbackEnabled=true`.

## Decision

Use the same provider-neutral Machine status and retry UX designed for EKS,
but realize GKE fallback with two Kyber-owned node pools. A GKE node pool's
Spot setting cannot be enabled or disabled after creation, so in-place
conversion is not available. One pool is Spot and one is standard; only one
may have desired capacity for a Machine at a time.

This is an additive GKE feature. The adapter advertises automatic fallback
only when managed profiles exist and the installer enables it. With the flag
off, all regional and zonal behavior remains unchanged. When the flag is
enabled after an upgrade, any existing legacy single pool is detected by its
stable base name and continues through the old lifecycle; only new Machines
use paired pools.

## Topology behavior

### Regional GKE

- The Spot pool spans the two zones selected for the Machine's regional PD.
- The standard fallback pool uses the same two eligible zones.
- Kubernetes may reattach the same regional PD in either replica zone, subject
  to single-writer detach/attach ordering and available compute capacity.
- A regional control plane or a third cluster zone does not expand the disk's
  mobility beyond its two replica zones.

### Zonal GKE

- Both pools are fixed to the Persistent Disk's zone.
- Spot-to-standard fallback retains the same disk in that zone.
- A whole-zone outage parks the Machine until the zone recovers; fallback
  cannot move a zonal disk elsewhere.

## Transition contract

1. Keep the inactive pool at zero and reject adoption without Kyber ownership
   labels.
2. On fallback, remove capacity from the active Spot pool and wait until no
   node from it can mount the Agent PVC.
3. Scale the standard pool to one and report Ready only after its node is Ready
   and the existing PVC is attached.
4. Manual retry performs the reverse order. If Spot does not become Ready
   within the retry budget, scale it back to zero and restore standard.
5. Preserve a stable provider reference for the logical Machine while keeping
   both native pool names private to the adapter.

This matches the EKS operator experience: requested/effective availability,
a configurable automatic-fallback threshold (five minutes by default),
explicit cost visibility, and manual retry.
Provider and topology details remain installer documentation, not separate
Machine actions.

## Resource impact and risks

- Two node-pool objects per cost-optimized Machine; inactive desired capacity
  is zero, so there is no steady-state VM charge.
- Pool, instance-group, IP, and API quotas still apply to inactive pools.
- Regional PD doubles storage replication cost relative to zonal PD.
- Safe transition depends on authoritative node/PVC attachment observation;
  timeout or ambiguity must keep the Agent parked rather than risk two writers.

## Evidence and verification

Google documents that Spot can only be selected on a new node pool and cannot
be enabled or disabled on an existing pool:
<https://cloud.google.com/kubernetes-engine/docs/how-to/spot-vms>.

Google documents that regional PD replicates into two zones and uses topology
and node affinity to constrain attachment:
<https://cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/regional-pd>.

The current Kyber GKE adapter creates one pool whose immutable `Config.Spot`
comes from requested availability. Its capability remains unsupported, and
the existing GKE characterization tests must remain unchanged and green.

Fake-client coverage now verifies opt-in capability advertisement, legacy-pool
non-regression, one-active-pool ordering, zonal fallback, successful retry,
and bounded rollback. Existing regional placement and recovery tests remain
unchanged and green. Live qualification should run one
regional-PD and one zonal-PD Machine and assert the same volume handle across
both transitions.
