# Regional GKE installations

**Status:** Implemented
**Date:** 2026-08-21

## Decision

A representative Google Cloud Kyber installation uses a regional GKE control
plane and regional Persistent Disks. The installer configures the cluster
resource location separately from the worker zones eligible for managed
Machine capacity.

For two or more eligible worker zones, each Kyber Machine remains one logical
unit of capacity. Its private GKE node pool uses autoscaling with total minimum
and maximum node counts of one and location policy `ANY`. GKE may therefore
choose the eligible zone with available Spot capacity without creating one
Node per zone or exposing provider resource kinds in Kyber's Machine API.

Offline intent disables autoscaling before resizing the pool to zero. Online
intent restores the total-size-one policy. Existing single-zone installations
with no configured node locations retain the legacy fixed-size behavior.

## Storage invariant

Regional Persistent Disk is attachable only in its replica zones. Every
`compute.gke.nodeLocations` entry must therefore appear in
`storage.gcePD.allowedZones`. When `replicationType=regional-pd`, the chart
requires exactly two allowed zones and renders them as StorageClass topology.
This is an installation consistency check; Kyber does not prevent an operator
from intentionally running a zonal cluster with zonal storage.

All durable consumers that must follow capacity across zones—agent roots,
transcript offsets, Postgres, and Redis—must explicitly use the regional
StorageClass in the installation values.

## Verification

- Unit tests prove regional pools start at zero and request exactly one total
  Node with `ANY` placement.
- Unit tests prove a healthy regional recovery observation does not submit a
  repeated resize operation.
- Helm rendering fails for missing or incompatible regional storage zones.
- A disposable live acceptance test must create one Spot Machine, bind a
  regional PVC, remove its active Node, and prove the replacement becomes Ready
  in an eligible zone with the same data before production use.
