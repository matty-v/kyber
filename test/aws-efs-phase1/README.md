# AWS EFS Phase 1 qualification harness

This directory implements the disposable EFS-versus-gp3 test described in
[`docs/specs/2026-08-25-aws-efs-phase1-qualification-plan.md`](../../docs/specs/2026-08-25-aws-efs-phase1-qualification-plan.md).

It creates only uniquely tagged resources in a new `us-east-1` VPC: one
no-ingress SSM-managed EC2 runner, one encrypted Regional EFS filesystem and
access point, and one encrypted gp3 control volume. It does not use any
existing VPC, EFS filesystem, or EKS cluster.

The remote script runs filesystem semantics, the exact `kyber-rootfs` entry
point from a pinned published runtime image, and a representative Git workload
against both mounts. Terraform must be applied and destroyed from the same
session-scoped working directory. After destroy, independently query resources
by the run tag and do not remove local state until that inventory is empty.

The harness is intentionally not wired into CI and contains no AWS credential
configuration. Authentication comes from the operator-approved interactive
AWS CLI session.
