# AWS EKS with EBS-first storage

**Status:** Proposed for operator review
**Date:** 2026-08-25
**Issue:** [#103](https://github.com/matty-v/kyber/issues/103)
**Evidence:** [AWS EFS Phase 1 qualification](../specs/2026-08-25-aws-efs-phase1-qualification-plan.md)

## Decision summary

The first native AWS installation should use Amazon EKS Standard, EKS managed
node groups, and encrypted gp3 EBS volumes. One Kyber-managed Machine maps to
one size-zero-or-one, single-Availability-Zone managed node group. This is the
closest AWS realization of the existing GKE adapter while preserving Kyber's
one-Machine/one-node isolation and its Linux filesystem contract.

The EKS control plane and VPC span at least two Availability Zones, but each
EBS-backed workload remains bound to the Availability Zone in which its volume
was created. Spot replacement is automatic only inside that zone. A zone
outage requires restore into another zone; it is not equivalent to the live
GKE installation's regional Persistent Disks.

EFS is not the default or an install-time toggle in the first implementation.
It remains a separate future design because it lacks xattrs and capacity
enforcement and was materially slower in live qualification.

## Goals

- Install the Kyber platform on a real EKS cluster without static AWS keys.
- Register an `eks` compute provider beside `gke`, `gce`, and local providers.
- Fulfill Online, Offline, Deleted, reliable, and Spot Machine intent through
  EKS managed node groups.
- Preserve the current Kyber API and primary operator workflow.
- Use gp3 EBS for agent roots and all other durable single-writer consumers.
- Make every loss of GKE regional-PD behavior explicit before installation.
- Provide tested backup and cross-zone restore procedures before production.

## Non-goals for the first implementation

- EKS Auto Mode, Karpenter, Fargate, bare EC2 workers, or self-managed groups.
- Multi-node Kyber Machines or sharing one managed node group between Machines.
- Transparent cross-zone failover of an attached EBS volume.
- EFS support, multi-writer agent roots, or storage selected per Agent.
- A permanent CI identity or long-lived AWS credential.
- AWS Load Balancer Controller unless an installation chooses AWS-native
  ingress; the existing Cloudflare tunnel path remains supported.

## Baseline and parity boundary

The representative `kyber-datawire-regional` GKE installation has a regional
control plane, reliable platform capacity, and size-one Spot Machine pools
eligible in two zones. Agent roots and other durable consumers use regional
`pd-balanced` volumes replicated across those zones. A replacement node may
therefore land in either eligible zone and reattach the same data.

AWS EBS preserves the required block-filesystem behavior and performed like
the gp3 control in Phase 1, but an EBS volume belongs to one Availability Zone.
The design deliberately trades regional mobility for filesystem correctness.

## AWS realization

### Cluster and network

The new `infra/terraform/eks/` root provisions one explicit installation
profile rather than adding AWS conditionals to the existing GCP root:

- an EKS Standard cluster using a currently supported Kubernetes version;
- a VPC with cluster subnets in at least two Availability Zones;
- private worker subnets by default, with either bounded NAT egress or reviewed
  VPC endpoints for ECR, S3, STS, EKS, EC2, logs, and SSM;
- one On-Demand platform managed node group, initially fixed at one node in a
  designated `platformAvailabilityZone`;
- EKS add-ons for VPC CNI, CoreDNS, kube-proxy, EBS CSI, and Pod Identity;
- cluster access entries for the installer and operator roles;
- CloudWatch control-plane logs with a configurable retention period;
- ownership, installation, and expiry tags on every supported resource.

The managed EKS control plane remains available through an Availability Zone
failure, but Kyber's single-replica stateful platform services do not. Postgres,
Redis, MinIO, transcript offsets, and Agent roots are all subject to the same
EBS zonal recovery contract unless they are replaced by provider-native
multi-AZ services in a later design.

### Machine capacity

The `eks` provider maps one managed Machine to one EKS managed node group:

- `Online` ensures the node group exists with minimum 0, maximum 1, and desired
  1.
- `Offline` sets desired size 0 but retains the group and its immutable launch
  configuration.
- `Deleted` removes only a node group carrying the expected Kyber ownership
  tags and labels.
- reliable profiles use `ON_DEMAND`; cost-optimized profiles use `SPOT`.
- each node group contains multiple same-shape compatible instance types where
  AWS permits it, improving Spot capacity without changing the advertised CPU
  and memory promise.
- each node group uses exactly one installer-approved Availability Zone.
- nodes receive `kyber.io/managed-by=kyber` and
  `kyber.io/machine=<machine-name>` labels plus matching AWS tags.
- Agent Pods continue to select the provider-owned Machine label; EKS-native
  node group names and ARNs remain private adapter details.

A single-AZ group is intentional. A multi-AZ Auto Scaling group may replace a
node in a zone that cannot attach an existing Agent volume. AWS's Cluster
Autoscaler guidance likewise treats EBS-backed stateful capacity as separate,
otherwise-identical node groups per Availability Zone.

Managed node groups are the first backing resource because EKS owns node
bootstrap, health repair, updates, drains, and Spot Capacity Rebalancing. The
adapter must not mutate the backing Auto Scaling group directly.

### Location contract

`eks` exposes installer-approved Availability Zones as Machine locations, not
only the AWS region. A managed Machine's zone becomes immutable once created.
The UI and API continue to call this field `location`, but the EKS installer
and Machine detail view explain that it is an Availability Zone and controls
storage recovery.

If the operator omits a zone during Machine creation, Kyber selects one from
the configured list using a stable least-Machine-count rule. This balances new
Machines but does not move an existing Machine or volume after creation.

### Storage

The Helm chart gains `storage.awsEBS`, parallel to `storage.gcePD`:

```yaml
storage:
  agentStorageClass: kyber-ebs
  transcriptOffsets:
    storageClassName: kyber-ebs
  awsEBS:
    enabled: true
    storageClassName: kyber-ebs
    type: gp3
    encrypted: true
    kmsKeyArn: ""
    allowedZones: [us-east-1a, us-east-1b]
```

The rendered StorageClass uses `ebs.csi.aws.com`, `gp3`, encryption,
`reclaimPolicy: Delete`, `allowVolumeExpansion: true`, and
`volumeBindingMode: WaitForFirstConsumer`. `allowedTopologies` contains only
installer-approved zones. Explicit Agent node affinity makes the first
consumer bind storage in its Machine's zone.

The chart fails rendering when:

- both GCE PD and AWS EBS storage are enabled;
- `storage.agentStorageClass` does not name the enabled AWS class;
- an EKS Machine zone is outside `storage.awsEBS.allowedZones`;
- regional/multi-zone behavior is claimed for EBS; or
- a production preset leaves encryption disabled.

Postgres, Redis, MinIO, transcript offsets, and Agent roots explicitly select
`kyber-ebs`; relying on whichever default StorageClass happens to exist is not
accepted for the EKS preset.

### Identity and permissions

Interactive AWS CLI credentials are bootstrap-only. Terraform creates no
access keys.

Runtime AWS access uses EKS Pod Identity:

- the EBS CSI controller service account receives the documented EBS CSI
  permissions through its own role and association;
- the Kyber control-plane service account receives a separate least-privilege
  role for EKS node-group read/reconcile operations and `iam:PassRole` limited
  to the dedicated worker-node role;
- Agent runtime service accounts receive no AWS role by default;
- node instance roles contain only EKS worker, CNI, registry pull, and narrowly
  justified node telemetry permissions;
- IMDSv2 is required and ordinary Pods are prevented from using the node role.

Every mutation checks both the configured cluster identity and Kyber ownership
tags. Provider references use an opaque `eks://` form and never contain
credentials.

### Spot interruption

EKS managed node groups enable Capacity Rebalancing and perform best-effort
node drain for Spot interruption and rebalance notices. That provider behavior
does not by itself preserve Kyber's user-visible preemption brief.

The node agent therefore gains an AWS interruption source behind its existing
provider-neutral notification path. It reads EC2 IMDSv2 Spot interruption and
rebalance metadata, emits the existing preemption notice once, and lets EKS
own replacement and drain. GCE metadata polling remains unchanged and only
the selected source runs.

Correctness cannot depend on receiving the warning: forced termination must
still transition the Agent to waiting/recovery based on lost node capacity.
The notice improves graceful state capture but is not a guarantee.

### Backup and cross-zone recovery

EBS snapshots are the cross-zone recovery primitive, not transparent
replication. The production preset must require an AWS Backup plan or an
equivalent documented snapshot policy targeting Kyber-owned volumes.

Before production approval, the operator chooses and reviews:

- snapshot frequency and resulting recovery-point objective;
- retention and cost;
- KMS key policy and cross-account recovery requirements;
- whether restore is an operator runbook or a later automated Kyber action.

The first release supplies an operator-run restore path:

1. stop the affected Agent or platform workload;
2. select a recovery point and target Availability Zone;
3. restore a new encrypted EBS volume in that zone;
4. create or patch the Kubernetes PV with explicit ownership metadata;
5. move/recreate the Machine capacity in the same zone;
6. start the workload and verify the durable-root manifest, identity repo, and
   latest transcript state;
7. retain the old volume until acceptance, then delete it explicitly.

This is cold recovery with data loss bounded by the snapshot interval. The
installer must display that statement before applying an EKS production plan.

## Adapter contract and code shape

Add `pkg/adapters/compute_eks.go` and focused tests. Configuration remains
provider-private:

- `eks-region`
- `eks-cluster`
- `eks-profiles`
- `eks-availability-zones`
- `eks-node-role-arn`
- `eks-subnet-ids-by-zone`
- `eks-launch-template-id` and version when required

An `EKSProfile` advertises the same provider-neutral fields as `GKEProfile`
while privately containing compatible instance types, root volume settings,
AMI type, and capacity classes. Parsing rejects mixed-size instance lists,
unknown zones, unencrypted launch templates, incomplete profiles, and duplicate
IDs.

The adapter uses the AWS SDK for Go v2 default credential chain. Its client
interface is narrow and fakeable, covering EKS Describe/Create/Update/Delete
node-group calls. Reconcile translates EKS states to the existing neutral
capacity states and treats AWS throttling/conflict responses as requeueable.
It never falls back to `static`, `mock`, or `gke` when initialization fails.

Discovery returns existing node groups as external candidates. Adoption is a
separate explicit action and cannot adopt the platform node group, untagged
groups, multi-node groups, or groups outside the allowed zones and subnets.

## Installer workflow

`docs/installation-eks.md` is a sibling to the GKE guide, not a conditional
appendix. The expected operator flow is:

1. authenticate the AWS CLI with the organization's normal short-lived
   identity and confirm account/region;
2. select `eks-standard-ebs` and review VPC, zones, platform zone, IAM roles,
   KMS key, backup policy, estimated hourly cost, and zonal recovery warning;
3. run read-only quota and permission preflight;
4. review and apply Terraform;
5. install/sync the Helm chart with pinned images and explicit `kyber-ebs`
   storage for every durable consumer;
6. verify Pod Identity, EBS dynamic provisioning, one reliable Machine, one
   Spot Machine, suspend/resume, and deletion cleanup;
7. run a Spot replacement test in-zone and a snapshot restore drill into a
   second zone before production sign-off.

Uninstall is two-stage: remove Kyber workloads and verify CSI-created volumes
are deleted or intentionally retained, then destroy cluster infrastructure.
Terraform must refuse to hide unmanaged volumes or snapshots during teardown.

## Installer/operator differences requiring review

| Concern | Live GKE behavior | Proposed AWS behavior | Visible consequence |
|---|---|---|---|
| Agent storage | Regional PD replicated across two zones | gp3 EBS in one zone | Zone outage requires snapshot restore and Machine relocation |
| Spot placement | One pool may choose either eligible zone | Each Machine is pinned to one zone | Less Spot capacity diversity for an existing Machine |
| Spot replacement | Same disk follows replacement in either replica zone | Replacement must be available in the volume's zone | Recovery can wait on zonal Spot capacity; operator may switch Machine to reliable capacity in-zone |
| Filesystem | Block filesystem with xattrs and enforced size | Block filesystem with xattrs and enforced size | Closest semantic parity; Phase 1 rootfs and Git tests passed on gp3 |
| Control-plane state | Regional PD for stateful chart dependencies | Zonal EBS for stateful chart dependencies | EKS API is multi-AZ but a platform-zone outage still interrupts Kyber services |
| Workload identity | GKE Workload Identity | EKS Pod Identity plus agent add-on | Different bootstrap and IAM audit surface; no static keys in either case |
| Machine backing | Size-one GKE node pool | Size-one EKS managed node group | Similar UI; EKS create/delete may take longer and consumes node-group quotas |
| Preemption | Kyber polls GCE metadata | EKS drains plus Kyber polls EC2 metadata | Warning remains best effort and may provide less than two minutes |
| Ingress | Cloudflare tunnel in the representative install | Same by default | AWS Load Balancer Controller is optional, not a hidden dependency |
| Backup | Regional replication is immediate availability, not backup | Scheduled EBS snapshots/AWS Backup | Explicit RPO, retention cost, and restore drill are required |

## Implementation phases and gates

### A. Static design and chart contract

- land this reviewed design and a detailed execution plan;
- add EBS StorageClass values/template/schema and Helm tests;
- add EKS values wiring and fail-closed configuration validation;
- publish IAM policy documents and installer preflight requirements.

Gate: rendered manifests prove every durable PVC uses encrypted gp3 with
`WaitForFirstConsumer`, and all zone/identity mismatches fail before apply.

### B. EKS adapter

- implement profile parsing, registry wiring, Pod Identity credential use,
  provider refs, observation, and ownership checks;
- implement managed node-group create, desired-size update, suspend, repair,
  deletion, and external discovery;
- add deterministic unit tests for throttling, conflicts, partial creation,
  Spot replacement, wrong-zone capacity, and refusal to mutate unowned groups.

Gate: no AWS test account mutation is needed to prove controller convergence
and destructive ownership guards.

### C. Disposable live cluster

- provision an isolated, tagged EKS cluster in the approved AWS account;
- install Kyber with On-Demand platform capacity and gp3 storage;
- create reliable and Spot Machines and run an Agent end-to-end;
- prove suspend/resume preserves data and Agent deletion removes its EBS
  volume;
- force Spot replacement and prove recovery on a new node in the same zone;
- restore a snapshot into a second zone and measure RPO/RTO;
- destroy everything and independently inventory leftovers.

Gate: Matt reviews measured create/recovery/delete times, spend, interruption
behavior, and the restore drill before any production deployment work.

### D. Documentation and production readiness

- publish `installation-eks.md`, IAM/KMS/backup reference, quota table,
  day-two node-group operations, restore runbook, and uninstall procedure;
- add dashboards/alerts for EBS attach failures, node-group degradation, Spot
  interruption, snapshot age, restore failures, and orphan volumes;
- record supported versions and upgrade ordering for EKS and managed add-ons.

Gate: a new installer can reproduce the cluster using only documented inputs
and sees every AWS-specific behavior before approving the plan.

## Acceptance criteria

- The Kyber UI/API remains provider-neutral and exposes no raw node-group ARN.
- No long-lived AWS credential is created or stored by Kyber.
- Every managed mutation is cluster-scoped and ownership-tag gated.
- Agent root seed, second boot, Git, credentials, tmux, cron, transcripts, and
  identity-repo writes survive pod restart and in-zone Spot replacement.
- Requested PVC capacity is enforced and expansion is tested.
- Agent deletion removes its PVC, PV, and EBS volume without deleting a
  snapshot retained by policy.
- Platform and Agent recovery behavior is measured for node loss, Spot
  interruption, EBS attach conflict, zonal capacity shortage, and zone outage.
- The snapshot restore drill succeeds in a different Availability Zone.
- Cleanup independently proves the disposable VPC, EKS cluster, node groups,
  roles, volumes, snapshots not intentionally retained, and load balancers are
  absent.

## Open decisions for Matt

1. **Backup objective and cost:** choose snapshot frequency and retention. A
   recommendation should be costed from observed live Agent volume churn before
   production approval.
2. **Platform-zone recovery:** accept cold snapshot restore for Postgres,
   Redis, and MinIO in v1, or separately scope provider-native multi-AZ
   replacements before production.
3. **Machine creation latency/quota:** accept one managed node group per
   Machine for GKE-like semantics, or authorize a later Karpenter evaluation
   after the first live timings.
4. **AWS-native ingress:** keep Cloudflare tunnel parity, or add the AWS Load
   Balancer Controller and its IAM/subnet requirements as a separate option.

## Primary AWS references

- [EKS managed node groups](https://docs.aws.amazon.com/eks/latest/userguide/managed-node-groups.html)
- [EKS Cluster Autoscaler guidance for EBS volumes](https://docs.aws.amazon.com/eks/latest/best-practices/cas.html#ebs-volumes)
- [EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html)
- [EKS EBS CSI driver](https://docs.aws.amazon.com/eks/latest/userguide/ebs-csi.html)
- [EKS subnet and Availability Zone guidance](https://docs.aws.amazon.com/eks/latest/best-practices/subnets.html)
- [Amazon EBS snapshots](https://docs.aws.amazon.com/ebs/latest/userguide/ebs-snapshots.html)
