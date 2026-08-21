# GKE Machine pool operations

Use this runbook for Kyber Machines backed by dedicated GKE Standard node
pools. The platform pool is outside this lifecycle.

## Safety invariants

- A managed pool must be in the configured project, location, and cluster.
- The opaque provider reference is provider-owned and must not be edited or
  parsed by clients.
- Discovered pools remain `External`; discovery never grants Kyber deletion
  authority.
- Managed mutation requires Kyber ownership labels stamped at pool creation.
- A pool carrying the platform taint or platform identity is never a Machine
  candidate.
- One Machine maps to one pool and at most one normal Ready Node.

## Routine checks

```bash
kubectl get machines.kyber.io -n kyber-system -o wide
kubectl get nodes -L cloud.google.com/gke-nodepool,kyber.io/machine
gcloud container node-pools list \
  --project="$GCP_PROJECT" \
  --cluster="$GKE_CLUSTER" \
  --location="$GKE_LOCATION"
```

For a Machine, compare `status.providerRef`, the pool name, the selected Node,
and `status.availability`. `Recovering` during GKE repair, resize, or Spot
replacement is expected; an Agent should remain `WaitingForMachine` until a
Ready replacement Node appears.

## Lifecycle expectations

| Intent | External observation mode | Managed mode target |
|---|---|---|
| Online | observe existing pool | create or resize pool to one |
| Offline | rejected | drain Agents, resize pool to zero |
| Deleted | unregister only | delete only the owned pool |

The managed reconciler is implemented, but an installation must not enable it
for production Machines until its disposable-pool lifecycle test passes in the
target cluster.

## Disposable-pool acceptance test

Use a unique Machine/pool name; never use `platform` or `agents` for the first
test.

1. Create the Machine through Kyber using a curated profile.
2. Verify an owned node pool appears and reaches `RUNNING`.
3. Verify one Node becomes Ready and carries both the GKE pool label and the
   Kyber Machine label.
4. Create a disposable Agent and verify its PVC is network-backed.
5. Stop the Machine and verify Agents drain before the pool reaches zero.
6. Start it and verify a new Node becomes Ready and the Agent resumes with the
   same PVC data.
7. Delete the Machine and verify only its owned pool is removed.
8. Confirm the `platform` and `agents` pools are unchanged.

Record the Machine YAML, pool description, Node labels, PVC/PV topology, and
controller events as test evidence.

The guarded adapter test automates the pool-only subset (create, zero, restore,
delete) before testing through the installed control plane. Run the command in
the GKE installation guide, then verify cleanup explicitly:

```bash
gcloud container node-pools list \
  --project="$GCP_PROJECT" \
  --cluster="$GKE_CLUSTER" \
  --location="$GKE_LOCATION" \
  --format='table(name,status,config.machineType,initialNodeCount)'
```

Save the test date, disposable pool profile, elapsed time, and before/after
pool listing in the installation's private operations record. Do not put
project IDs, cluster names, operator identities, or other installation-specific
evidence in this reusable runbook. A passing pool lifecycle test proves the
provider adapter's pool and cleanup boundary; Agent scheduling, draining, and
persistent-volume checks remain required before production enablement.

## Recovery

- `Pending/ExternalWait`: verify the pool exists and the Machine name/reference
  identifies it.
- `Recovering`: inspect GKE node-pool status and operations; do not create a
  second pool manually with the same identity.
- A cost-optimized pool can remain `Recovering` while its managed instance
  group reports zonal capacity exhaustion. Kyber keeps the pool at size one,
  parks assigned Agents in `WaitingForMachine`, and resumes them when a Ready
  Node joins. Switching to reliable capacity requires replacing the pool; do
  not hand-create a competing Node in the same pool.
- `Failed/ProviderError`: inspect node-pool conditions and IAM audit logs.
- Node pool healthy but Machine has no Node: verify
  `cloud.google.com/gke-nodepool=<pool>` and Node readiness.
- Deletion blocked: confirm the reference is inside the configured cluster and
  ownership labels are intact. Never remove the finalizer until cloud state is
  independently understood.
