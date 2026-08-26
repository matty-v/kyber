# EKS live acceptance and cleanup

This is the bounded issue-103 qualification run. It is destructive only inside
the unique Terraform state and `kyber.io/run-id` selected for the run. Do not
apply until the saved plan, IAM boundary, cost ceiling, account, region, AZs,
and API CIDR have been reviewed and explicitly approved.

## Before apply

1. Verify `aws sts get-caller-identity` and region `us-east-1`; never print
   credential material.
2. Record existing EKS clusters and tagged resources for the run ID. The run
   ID must be absent.
3. Use exactly two available AZs, an eight-hour RFC3339 expiry, and the
   operator's current bounded `/32` API CIDR.
4. Save both the binary plan and `terraform show -json` outside committed
   source. Review resource counts, replacements/deletes (must be zero), IAM,
   and estimated maximum cost.
5. Apply only that saved binary plan after explicit approval.

## Short acceptance

Capture timestamps and native IDs without secrets.

- Verify the EBS CSI add-on, Pod Identity associations, encrypted gp3
  StorageClass, and the single-AZ platform node.
- Reliable Machine: create, Ready, Offline, Online, Delete.
- Cost-optimized Machine and one disposable Agent: record PVC UID, PV name,
  EBS volume handle, Machine provider reference, native group names, and active
  Node UID.
- Force the deterministic unavailable condition: Spot desired zero, no Machine
  Node/VolumeAttachment, On-Demand desired one, Agent Ready. Assert the same
  PVC/PV/EBS volume and no overlap of active groups.
- Retry cost-optimized: assert the reverse ordering and the same volume.
- Force a failed retry: assert rollback to On-Demand, one active Node, the same
  volume, and the retry request acknowledged once.
- Delete the Agent and verify its PVC, PV, VolumeAttachment, and EBS volume are
  absent. Verify Machine deletion is refused while an Agent is attached.
- Run the unchanged GKE adapter/chart suite. Read-only live GKE observation is
  optional and must not mutate `kyber-datawire`.

One timing per transition is reported. This shortened run does not establish a
p95 recovery SLO.

## Mandatory cleanup

Register cleanup before the first apply. From the same state, run Terraform
destroy even if acceptance fails. Then independently verify absence using:

- EKS clusters, managed node groups, add-ons, and Pod Identity associations;
- EC2 instances, launch templates, VPC, subnets, routes, ENIs, public IPv4s,
  security groups, and EBS volumes/snapshots;
- IAM roles and inline/attached policies;
- load balancers, target groups, KMS grants/keys, AWS Backup resources, and
  CloudWatch log groups;
- Resource Groups Tagging API for the exact run ID.

Keep Terraform state until every native query is clean. Record any tag API lag
only after the owning service proves the resource absent. The test is not
complete while any run-owned billable resource remains.
