# AWS EFS parity qualification — Phase 1 execution plan

**Status:** Approved, blocked on CI identity decision
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

- Region: `us-east-2`.
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

Kyber's platform rules prohibit ad-hoc manual deployment. The qualification
therefore requires a manually dispatched, environment-protected GitHub Actions
workflow using short-lived AWS OIDC credentials. It must not store long-lived
AWS keys in GitHub or the repository.

This adds `id-token: write` to one narrowly scoped workflow and requires a
dedicated AWS IAM OIDC role restricted to this repository, workflow
environment, region, resource-name prefix, and required ownership tags.
Repository guidance requires explicit approval before changing CI permissions,
so implementation stops at this checkpoint until that decision is approved.

## Checkpoints

### 0. Durable plan and authorization

- [x] Inventory live GKE topology read-only.
- [x] Inventory AWS EKS/EFS resources read-only.
- [x] Receive Phase 1 cost and resource approval.
- [ ] Receive explicit approval for the protected workflow's
  `id-token: write` permission and dedicated AWS OIDC role.

Exit criterion: the CI identity and environment approval boundary are agreed.

### 1. Qualification harness

- [ ] Add Terraform for the isolated VPC, security groups, encrypted EFS,
  encrypted gp3 EBS, and temporary EC2 runner.
- [ ] Require the unique run ID, expiry, owner tags, and cost ceiling inputs.
- [ ] Add a remote test entrypoint that runs the repository-pinned test suite
  without accepting arbitrary shell input.
- [ ] Add cleanup that is ownership-gated and runs after success, failure, or
  cancellation.
- [ ] Add static tests for Terraform formatting/validation, forbidden existing
  resource IDs, tag requirements, expiry bounds, and cleanup targeting.

Exit criterion: local validation proves the harness cannot target existing
resources and always schedules bounded cleanup.

### 2. Filesystem contract tests

Run the same operations against EFS and a fresh ext4 filesystem on gp3:

- [ ] first `kyber-rootfs prepare` seed from the current runtime image;
- [ ] no-op second boot and a synthetic base-image upgrade;
- [ ] extended attribute and file-capability preservation;
- [ ] numeric ownership, modes, symlinks, hard links, atomic rename, fsync,
  advisory locks, and concurrent-writer guard behavior;
- [ ] Git clone/status/checkout and representative Go and npm dependency
  workloads;
- [ ] keyring, tmux, cron, transcript, session-recall, and identity-repo writes;
- [ ] requested-capacity and filesystem-usage reporting behavior;
- [ ] access-point and backing-directory deletion.

Capture elapsed time, p50/p95 operation latency where meaningful, transferred
and metered bytes, errors, and semantic differences. Do not use synthetic
throughput alone as the acceptance result.

### 3. Recommendation

- [ ] Compare EFS results with gp3 and the documented regional-PD contract.
- [ ] Name every installer-visible and operator-visible difference.
- [ ] Identify required Kyber changes without implementing them in this phase.
- [ ] Independently verify all phase resources are absent.
- [ ] Update this plan with evidence and the exact next action.

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
