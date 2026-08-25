# AWS EFS parity qualification — Phase 1 execution plan

**Status:** Phase 1 complete — EFS is not qualified without Kyber changes
**Date:** 2026-08-25
**Issue:** [#103](https://github.com/matty-v/kyber/issues/103)

## Outcome

Determine whether Amazon EFS can safely back Kyber's durable agent root while
preserving the installer and operator behavior of the live regional
`kyber-datawire` GKE installation. Compare EFS with gp3 EBS using Kyber's real
root preparation and representative agent filesystem operations. Do not build
an EKS cluster in this phase.

The result must be evidence, not a cloud-provider feature comparison. Phase 1
ends with one of three recommendations:

1. qualify EFS for an EKS lifecycle test;
2. qualify EFS only after named Kyber runtime changes; or
3. reject EFS and document the cross-AZ recovery gap of EBS-backed AWS
   installations.

## Verified baseline

The live `datawire-dev/kyber-datawire-regional` installation currently has:

- a regional GKE control plane in `us-central1`;
- one reliable platform pool and three Kyber-managed Spot Machine pools;
- each Machine pool eligible in `us-central1-a` and `us-central1-c`, with
  total minimum and maximum node count one;
- 16 attached regional `pd-balanced` PVC disks at the time of inventory,
  replicated across both eligible zones with real requested capacities from
  1 GiB through 20 GiB.

The AWS account `914373874199` (`datawire`) has no EKS cluster. Its existing
`redis-efs` filesystem is encrypted, backed up, and mounted in three
`us-east-2` Availability Zones. It is unrelated existing infrastructure and is
strictly out of scope: the qualification must not mount, alter, or reuse it.

## Known parity risks

- EFS does not support extended attributes. `kyber-rootfs` deliberately uses
  `tar --xattrs` so Linux file capabilities survive first boot and migration.
- EFS CSI treats requested PVC capacity as a Kubernetes placeholder rather
  than an enforced per-volume quota.
- EFS is NFS-backed and adds per-operation latency to metadata-heavy agent
  work such as Git checkouts, package installation, and dependency builds.
- Dynamic EFS provisioning deletes an access point by default but retains its
  backing directory. Kyber's current `reclaimPolicy: Delete` expectation is
  that deleting an Agent PVC removes the backing data.
- EFS provides shared `ReadWriteMany` storage underneath an agent lifecycle
  that assumes one active writer.

## Safety and cost boundaries

- Region: `us-east-1`.
- All created resources use a unique `kyber-efs-phase1-*` name and the tags
  `kyber.io/managed-by=phase1-qualification`, `kyber.io/issue=103`, and an
  expiry timestamp no more than eight hours after creation.
- Use a new isolated VPC, encrypted test EFS filesystem, encrypted gp3 volume,
  and one temporary Linux EC2 instance. Do not use a load balancer or EKS.
- One NAT gateway is the maximum. Prefer VPC endpoints or a tightly scoped
  temporary public-subnet path when the implementation review shows that it is
  safer and cheaper.
- Expected spend is below USD 5. Stop before creation if the rendered plan can
  exceed USD 10 under the eight-hour lifetime.
- Record the exact pre-existing resource inventory before apply. Cleanup must
  destroy only resources bearing both the unique run ID and expected ownership
  tags, then independently prove they are absent.
- Never print AWS credentials, authorization codes, EFS contents, or unrelated
  account inventory into CI logs.

## Delivery path

This is a session-scoped research and deployment exercise, not a permanent
Kyber deployment path. Matt explicitly approved using the current interactive
AWS login to provision and clean up the disposable qualification resources.
Do not add CI, a GitHub OIDC provider, an AWS IAM role, access keys, or another
long-lived authentication path for this exercise.

Terraform remains the only mutation interface so the intended resources,
ownership tags, cost boundary, and cleanup inventory are reviewable before
apply. The local state is a temporary test artifact and must be retained until
independent cleanup verification succeeds, then removed from the worktree.
This exception is limited to the approved research resources; it is not the
eventual installer or maintainer authentication design.

## Checkpoints

### Phase 1 recommendation and visible differences

Do not use EFS as a transparent replacement for the live GKE cluster's
regional Persistent Disks. The installer/operator differences are material:

- GKE regional PD presents a capacity-bounded block filesystem with Linux
  xattrs and one-writer attachment while replicating across the cluster's two
  eligible zones. EFS is a shared RWX NFS filesystem, does not preserve
  xattrs, and does not enforce the PVC's requested size.
- Normal first-start and Git operations become visibly slower on EFS. The
  measured 66x seed and 17x clone slowdowns would affect agent readiness,
  recovery time, upgrades, and interactive repository work.
- EFS access-point deletion does not by itself delete its backing directory.
  The AWS implementation needs explicit, ownership-safe data cleanup to match
  Kyber's current `reclaimPolicy: Delete` expectation.
- EFS must isolate every agent through a dedicated access point and IAM/CSI
  policy. Its shared filesystem otherwise increases cross-agent read/write
  blast radius compared with one disk per Agent.
- gp3 EBS is much closer to Kyber's filesystem semantics and performance, but
  it is zonal. Unlike GKE regional PD, a volume cannot simply follow a Spot
  replacement node into another Availability Zone. An EBS design therefore
  needs zone-pinned Machine scheduling plus a reviewed snapshot/replication
  and restore story; this is an operator-visible recovery difference.

EFS could only be reconsidered after a Kyber storage contract that rejects or
replaces xattr/file-capability dependence, reports capacity honestly, defines
backing-directory cleanup, enforces per-Agent isolation, and accepts or
mitigates the measured metadata latency. Those changes are outside Phase 1.

### Execution log

- 2026-08-25 19:04 UTC: reviewed a Terraform plan containing exactly 16
  creates and no changes or deletes, all under run ID `08251904`.
- The apply stopped when AWS returned `VpcLimitExceeded` for `us-east-2`.
  Before that failure Terraform created only the tagged test EFS filesystem,
  access point, IAM role, instance profile, and SSM policy attachment.
- Terraform destroyed all five partial resources. Independent AWS queries
  found no remaining resource with the run tag or test EFS name, and confirmed
  the temporary IAM role is absent. Existing `redis-efs` was untouched.
- Matt approved moving the isolated test to `us-east-1`, where the account had
  no VPCs at inventory time. Do not reuse an existing `us-east-2` VPC or
  request quota.
- 2026-08-25 19:08 UTC: reviewed a second empty-state plan containing exactly
  16 creates and no changes or deletes under run ID `08251908`, then applied
  it in the approved isolated `us-east-1` VPC.
- The first remote invocation exposed an EFS DNS propagation/runner dependency
  issue before the workload. The harness was changed to use the exact
  Terraform-created mount-target IP with TLS and the access point, avoiding a
  DNS or host-SDK dependency in the storage comparison.
- The repository's current runtime-base image at commit
  `fcd9ee2e0d40738ff3e1a44d600edf71d1402827` completed the definitive run.
  The previously selected v1.0.1 digest predates `kyber-rootfs` and was not
  used as evidence.
- gp3 supported user xattrs and reported its real 20 GiB capacity. EFS did not
  support user xattrs and reported an effectively unbounded shared capacity.
  Both supported hard links, symlinks, atomic rename, and the Git workload.
- `kyber-rootfs` first-boot seed of the same image took 6.33 seconds on gp3
  and 417.48 seconds on EFS (66.0 times slower). Second boot took 1.99 seconds
  on gp3 and 2.32 seconds on EFS. A shallow Kyber clone plus status took 0.96
  seconds on gp3 and 16.12 seconds on EFS (16.8 times slower).
- `kyber-rootfs` completed on both filesystems in this image, but EFS cannot
  preserve an xattr or file capability that is present now or added later.
  Passing this seed is therefore not proof of the durable-root contract.
- Terraform destroyed all 16 resources. State is empty; the EFS filesystem,
  IAM role, and VPC are absent; the EC2 instance is terminated; and both EBS
  volume IDs return `InvalidVolume.NotFound`. The tag API temporarily retained
  stale entries for the terminated instance and deleted volumes. Existing
  `redis-efs` remained untouched.
- Matt selected gp3 EBS as the AWS default. The next design is
  [AWS EKS with EBS-first storage](../design/2026-08-25-aws-eks-ebs-first-design.md),
  with explicit zonal recovery semantics. Do not proceed to EKS on EFS as if
  it were equivalent to GKE regional Persistent Disk.

### 0. Durable plan and authorization

- [x] Inventory live GKE topology read-only.
- [x] Inventory AWS EKS/EFS resources read-only.
- [x] Receive Phase 1 cost and resource approval.
- [x] Confirm session-scoped Terraform deployment with the current interactive
  AWS login; no CI or long-lived credential is in scope.

Exit criterion: the deployment and identity boundary is agreed.

### 1. Qualification harness

- [x] Add Terraform for the isolated VPC, security groups, encrypted EFS,
  encrypted gp3 EBS, and temporary EC2 runner.
- [x] Require the unique run ID, expiry, owner tags, and cost ceiling inputs.
- [x] Add a remote test entrypoint that runs the repository-pinned test suite
  without accepting arbitrary shell input.
- [x] Add cleanup that is ownership-gated and run it after success or failure.
- [ ] Add static tests for Terraform formatting/validation, forbidden existing
  resource IDs, tag requirements, expiry bounds, and cleanup targeting.

Exit criterion: local validation proves the harness cannot target existing
resources and always schedules bounded cleanup.

### 2. Filesystem contract tests

Run the same operations against EFS and a fresh ext4 filesystem on gp3:

- [x] first `kyber-rootfs prepare` seed from the current runtime image;
- [x] no-op second boot and a synthetic base-image upgrade;
- [x] extended attribute preservation probe;
- [ ] numeric ownership, modes, symlinks, hard links, atomic rename, fsync,
  advisory locks, and concurrent-writer guard behavior;
- [x] Git clone/status workload;
- [ ] keyring, tmux, cron, transcript, session-recall, and identity-repo writes;
- [x] requested-capacity and filesystem-usage reporting behavior;
- [ ] access-point and backing-directory deletion.

Capture elapsed time, p50/p95 operation latency where meaningful, transferred
and metered bytes, errors, and semantic differences. Do not use synthetic
throughput alone as the acceptance result.

### 3. Recommendation

- [x] Compare EFS results with gp3 and the documented regional-PD contract.
- [x] Name the decisive installer-visible and operator-visible differences.
- [x] Identify required Kyber changes without implementing them in this phase.
- [x] Independently verify all phase resources are absent.
- [x] Update this plan with evidence and the exact next action.

Exit criterion: Matt can approve or reject EFS before any EKS implementation
or cluster spending begins.

## Acceptance rules

EFS cannot proceed to the EKS phase if any of these remain unexplained:

- first boot or upgrade fails on unsupported filesystem operations;
- a required credential, package, runtime, or Git workflow loses correctness;
- PVC deletion leaves active data or an access point behind;
- Kyber reports an enforceable storage limit that EFS does not enforce;
- one agent can read or write another agent's root;
- a normal filesystem interruption can corrupt state or permanently wedge an
  agent;
- cleanup cannot prove the test resources are gone.

Performance thresholds will be set from the first gp3 control run rather than
invented in advance. Any EFS slowdown large enough to change normal interactive
agent work is an installer-visible tradeoff and requires review even if the
filesystem is functionally correct.
