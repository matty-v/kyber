# Install Kyber on GKE

This guide covers the opinionated GKE Standard layout used by Kyber's managed
Machine work. It supports observation-only attachment and an ownership-gated
managed lifecycle. Treat managed mode as pre-release until the disposable-pool
acceptance test in the maintenance runbook has passed for each target cluster.
The adapter-level lifecycle test passed against
`datawire-dev/us-central1-a/kyber-datawire` on 2026-08-20; installation
acceptance still includes the Agent and PVC checks in that runbook.

## Target layout

- One small, fixed-size `platform` node pool runs the Kyber control plane and
  backing services.
- The platform pool has the taint
  `kyber.io/platform=true:NoSchedule`; Kyber never offers or mutates it as
  Machine capacity.
- Each Kyber Machine maps to one dedicated GKE Standard node pool.
- Machine pools use one zone, autoscaling disabled, and zero or one desired
  node. Spot is selected through the neutral `costOptimized` availability
  class.
- Agent state uses a network-backed StorageClass such as `pd-balanced`; do not
  rely on node-local storage when pools may resize to zero or replace nodes.

## Google Cloud identity

Run the control plane with a Google service account through Workload Identity.
Observation-only installs need permission to read the configured cluster and
node pools. Managed mode additionally needs narrowly scoped node-pool create,
resize, and delete permissions. Do not grant project-wide Owner or Editor.

The provider is restricted in configuration to one project, location, and
cluster. Kyber rejects opaque references outside that boundary.

## Helm configuration

```yaml
compute:
  provider: gke
  gke:
    project: datawire-dev
    location: us-central1-a
    cluster: kyber-datawire
    profiles:
      - id: standard
        displayName: Standard
        description: Good default for one or two typical agents
        cpu: "4"
        memory: 16Gi
        availabilityClasses: [reliable, costOptimized]
        recommended: true
        machineType: e2-standard-4
        diskSizeGb: 200
        diskType: pd-balanced
        imageType: UBUNTU_CONTAINERD
```

The rendered control plane receives `KYBER_GKE_PROJECT`,
`KYBER_GKE_LOCATION`, and `KYBER_GKE_CLUSTER`. Missing values fail Helm
rendering when `compute.provider=gke`. Profile mappings are serialized into
`KYBER_GKE_PROFILES`; the operator API exposes only the neutral ID, display
metadata, capacity, and availability classes. GKE machine type, disk, and image
choices remain installer-owned.

## Observation-only migration

1. Confirm the pool is healthy:

   ```bash
   gcloud container node-pools describe agents \
     --project=datawire-dev \
     --cluster=kyber-datawire \
     --zone=us-central1-a
   ```

2. Confirm its Nodes carry `cloud.google.com/gke-nodepool=agents`.
3. Create or migrate a Machine named `agents` with `provider: gke` and
   `managementMode: External`.
4. Verify `status.providerRef` becomes
   `gke://datawire-dev/us-central1-a/kyber-datawire/nodePools/agents` and
   availability becomes `Available`.
5. Verify the Machine resolves exactly one Ready Node before assigning Agents.

Observation-only deletion unregisters the Machine and never deletes the pool;
Offline is rejected. A Machine created with `managementMode: Managed` uses its
curated profile to create a size-one pool, resizes it to zero for Offline, and
deletes it only when both Kyber ownership labels match.

## Verification

The repository includes a read-only live adapter check:

```bash
KYBER_TEST_GKE_PROJECT=datawire-dev \
KYBER_TEST_GKE_LOCATION=us-central1-a \
KYBER_TEST_GKE_CLUSTER=kyber-datawire \
KYBER_TEST_GKE_NODE_POOL=agents \
go test ./pkg/adapters -run TestGKELiveObservation -v
```

After the read-only check passes, run the guarded managed-capacity check with a
unique, disposable pool name:

```bash
KYBER_TEST_GKE_MANAGED=true \
KYBER_TEST_GKE_PROJECT=datawire-dev \
KYBER_TEST_GKE_LOCATION=us-central1-a \
KYBER_TEST_GKE_CLUSTER=kyber-datawire \
KYBER_TEST_GKE_MANAGED_POOL=kyber-test-$(date +%m%d%H%M) \
go test ./pkg/adapters -run TestGKELiveManagedLifecycle -v -timeout 20m
```

The test refuses names without the `kyber-test-` prefix, refuses the protected
`platform` and `agents` names, requires the pool to be absent at startup, and
registers ownership-checked cleanup before creating it. It verifies create at
size one, resize to zero, restore to one, and deletion. Independently list node
pools afterward and confirm the disposable pool is absent and protected pools
are unchanged.

See [GKE Machine pool operations](operator/gke-machine-pools.md) for ownership
rules, lifecycle checks, and recovery procedures.
