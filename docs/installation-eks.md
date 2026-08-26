# Install Kyber on EKS

Kyber on EKS uses one regional EKS control plane and EBS-backed Machine
capacity. Each Machine is assigned to one approved availability zone because
its Agent disk is an EBS volume in that zone. An EKS cluster may contain
Machines in several zones, but an individual Machine and its disk do not move
between zones.

## Operator-visible differences from GKE

| Operator concern | EKS | Regional GKE | Zonal GKE |
|---|---|---|---|
| Machine disk mobility | One EBS AZ | Either replica zone of a regional PD | One PD zone |
| Cost-optimized recovery | Spot to On-Demand after 5 minutes | Spot can move across configured zones; paired standard fallback is optional | Spot to standard after 5 minutes when paired fallback is enabled |
| Whole-zone outage | Machine waits for its AZ to recover | May recover in the disk's other replica zone | Machine waits for its zone to recover |
| Manual return to lower cost | `Retry cost-optimized capacity` | Same action when fallback is enabled | Same action when fallback is enabled |
| Native capacity per cost-optimized Machine | One Spot and one size-zero On-Demand managed node group | One Spot and one size-zero standard node pool | Same |

The primary Kyber workflow is deliberately the same: select `reliable` or
`costOptimized`, see the requested and effective class, and use **Retry
cost-optimized capacity** after fallback. AWS names and node-group operations
stay out of the Machine UI.

## Prerequisites and identity

- EKS with the EBS CSI add-on and worker subnets in every approved AZ.
- A fixed On-Demand platform node group. The v1 qualification layout places
  it in one AZ; the Kyber platform is unavailable during an outage of that AZ.
- The control-plane Pod uses EKS Pod Identity (or IRSA) and the AWS SDK default
  credential chain. Do not create static AWS access keys for Kyber.
- Its role needs only EKS managed-node-group read/create/update/delete access
  in the configured cluster. The managed node groups use a separate node role.
- EBS CSI permissions belong to the CSI driver identity, not the Kyber control
  plane. A customer-managed KMS key additionally needs the documented CSI
  grant permissions.

The disposable reference root is in [`infra/terraform/eks`](../infra/terraform/eks/README.md).
It creates tagged, expiring qualification infrastructure in two AZs while the
v1 platform node group remains single-AZ. Review a saved Terraform plan before
apply; destroy from the same state and independently audit tagged resources.
If the Helm release name or namespace is not `kyber`, set Terraform's
`kyber_control_plane_service_account` and `kyber_namespace` variables to the
rendered ServiceAccount identity before apply so the Pod Identity association
matches exactly.

## Helm configuration

Start from
[`deploy/helm/kyber/examples/values-eks.yaml`](../deploy/helm/kyber/examples/values-eks.yaml)
and replace every placeholder with Terraform output or an installer-reviewed
value. A representative provider section is:

```yaml
compute:
  provider: eks
  eks:
    region: us-east-1
    cluster: kyber-eks
    nodeRoleArn: arn:aws:iam::123456789012:role/kyber-eks-node
    allowedZones: [us-east-1a, us-east-1b]
    subnetsByZone:
      us-east-1a: subnet-aaaa
      us-east-1b: subnet-bbbb
    profiles:
      - id: standard
        displayName: Standard
        cpu: "2"
        memory: 8Gi
        instanceTypes: [m7i.large, m7a.large, m6i.large]
        diskSizeGb: 100
        availabilityClasses: [reliable, costOptimized]
        launchTemplateId: lt-0123456789abcdef0
        launchTemplateVersion: "1"

storage:
  agentStorageClass: kyber-ebs
  awsEBS:
    enabled: true
    storageClassName: kyber-ebs
    type: gp3
    encrypted: true
    allowedZones: [us-east-1a, us-east-1b]
```

The chart fails closed if AWS EBS is not encrypted gp3, has no approved AZ,
or conflicts with the selected Agent StorageClass. `WaitForFirstConsumer`
binds each new PVC in the selected Machine's zone. Kyber never deletes or
recreates a PVC during fallback, so the same volume is reattached to the
replacement node.

When a profile supplies `launchTemplateId`, EKS requires the worker root-disk
mapping to live in that launch template. Keep the profile's displayed
`diskSizeGb` aligned with Terraform's `machine_root_disk_size`; Kyber omits the
conflicting EKS `diskSize` request while retaining the profile's multiple
same-shape `instanceTypes` for Spot capacity diversity.

## Day-two behavior

For a cost-optimized Machine, Kyber pre-creates a Spot group at size one and
an On-Demand group at size zero in the same AZ. After Spot capacity has no
attached Node for five minutes, Kyber scales Spot to zero, confirms no Machine
Node remains, and only then scales On-Demand to one. The UI makes the higher
effective class and fallback reason visible.

**Retry cost-optimized capacity** reverses that order. If Spot still cannot
attach within five minutes, Kyber removes it and restores On-Demand. The
logical Machine/provider reference and Agent PVC remain unchanged throughout.
Offline and Delete also wait for authoritative Node absence; ambiguous state
parks the Machine instead of risking two writers.

An AZ outage is not a cross-zone failover. The Machine stays unavailable while
its EBS AZ is down and reconciles normally when the AZ and EKS node-group APIs
recover. Other Machines assigned to healthy AZs can continue running.

## Backups and cleanup

EBS snapshots are operator-owned AWS policy, not a Kyber lifecycle feature.
Document and apply an AWS Backup or Data Lifecycle Manager policy appropriate
to the installation. Restoring a snapshot into another AZ creates a new EBS
volume and is a disaster-recovery procedure, not automatic Machine fallback.

Before uninstalling, record retained data requirements, delete Machines
through Kyber, and verify PVC/PV and EBS volume disposition. After a disposable
test, query EKS node groups, EC2/EBS volumes and snapshots, ENIs, load
balancers, IAM resources, KMS grants, and CloudWatch logs by the test run tag.
