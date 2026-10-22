# Increase an Agent disk

Kyber can grow an Agent's existing `/persist` PVC when it uses a supported
CSI driver (`pd.csi.storage.gke.io` on GKE or `ebs.csi.aws.com` on EKS) and its
StorageClass has `allowVolumeExpansion: true`. Open the Agent detail page and
choose **Increase disk**, or inspect `GET /api/v1/agents/{name}/disk-options`
and submit `POST /api/v1/agents/{name}/increase-disk` with a whole-GiB
`sizeGiB` target. Kyber rejects a target at or below provisioned capacity.
Generic Agent PATCH cannot change the disk size.

The controller increases the original PVC request; the CSI driver expands
the bound PV and filesystem. Kyber checks the original PVC/PV identities and
shows requested, provisioned, and measured filesystem capacity separately.
The operation is complete only after the filesystem and Agent status report
the new size. A Running Agent receives one controlled restart so its status
sidecar uses the new limit. A Stopped Agent stays stopped; a short-lived pod
mounts only its existing PVC to complete and measure filesystem expansion.
The helper runs without Agent credentials and is removed after success.

An unsupported driver, unbound/missing PVC, mismatched volume identity, or
observable quota problem blocks the request with a reason. GKE or AWS cloud
capacity failures can still occur asynchronously after acceptance; inspect
the operation stage and PVC events. No replacement disk is created by this
workflow. Disk shrinking is unavailable: cloud Persistent Disks and EBS
volumes require a separate data migration to a smaller replacement volume.
Provisioned storage charges increase as soon as the provider grows the disk;
unused capacity is still billed.

If a larger PVC request fails before capacity is allocated, a later request
may lower the *unfulfilled target* while remaining above all capacity the PVC
and PV report as allocated. This changes a request, not the disk's physical
size. Kyber blocks the retry if Kubernetes already reports more capacity.

The Kyber operator needs Agent resource-edit access, not cloud credentials.
The control plane needs read access to PVs and StorageClasses and patch access
to PVCs; the CSI driver performs the provider operation.

The EKS/EBS path has controller and API fixture coverage; validate it on a
nonproduction EKS installation before relying on live expansion. Local-path
storage is explicitly unsupported because its reported `statfs` capacity can
describe a shared node filesystem rather than this Agent's allocation.
