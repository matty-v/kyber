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

Create the namespace and both runtime secrets before installing. The EKS
preset deliberately uses `namespace.create: false` and `api.existingSecret`,
so Helm will not manufacture credentials or rely on resource ordering:

```sh
kubectl create namespace kyber
kubectl -n kyber create secret generic kyber-api-key \
  --from-literal=api-key="$KYBER_API_KEY"
kubectl -n kyber create secret generic kyber-internal-signing-key \
  --from-literal=signing-key="$(openssl rand -hex 32)"
```

Keep the API key outside values files and shell history. For a production
installation, deliver both Secrets through the operator's normal secret
management path. The internal signing key is required for Agent-to-control-
plane calls; a missing key fails that internal API closed.

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
      - id: small
        displayName: Small
        cpu: "2"
        memory: 8Gi
        instanceTypes: [m7i.large, m7a.large, m6i.large]
        diskSizeGb: 100
        availabilityClasses: [reliable, costOptimized]
        launchTemplateId: lt-0123456789abcdef0
        launchTemplateVersion: "1"
      - id: medium
        displayName: Medium
        cpu: "4"
        memory: 16Gi
        instanceTypes: [m7i.xlarge, m7a.xlarge, m6i.xlarge]
        diskSizeGb: 100
        availabilityClasses: [reliable, costOptimized]
        launchTemplateId: lt-0123456789abcdef0
        launchTemplateVersion: "1"
      - id: large
        displayName: Large
        cpu: "8"
        memory: 32Gi
        instanceTypes: [m7i.2xlarge, m7a.2xlarge, m6i.2xlarge]
        diskSizeGb: 100
        availabilityClasses: [reliable, costOptimized]
        launchTemplateId: lt-0123456789abcdef0
        launchTemplateVersion: "1"
      - id: xlarge
        displayName: XLarge
        cpu: "16"
        memory: 64Gi
        instanceTypes: [m7i.4xlarge, m7a.4xlarge, m6i.4xlarge]
        diskSizeGb: 100
        availabilityClasses: [reliable, costOptimized]
        launchTemplateId: lt-0123456789abcdef0
        launchTemplateVersion: "1"

storage:
  agentStorageClass: kyber-ebs
  transcriptOffsets:
    storageClassName: kyber-ebs
  awsEBS:
    enabled: true
    storageClassName: kyber-ebs
    type: gp3
    encrypted: true
    allowedZones: [us-east-1a, us-east-1b]
```

`allowedZones` is the exact Zone catalog shown to the operator. Every listed
zone must have a matching worker subnet and must also appear in
`storage.awsEBS.allowedZones`; Kyber does not discover or add zones outside
this installer-approved set.

`profiles` is likewise an installer-curated catalog rather than a fixed Kyber
size list. The example provides four general-purpose sizes. Instance types
within one profile must have the same CPU and memory shape because EKS may
choose any of them for Spot capacity. Confirm every configured type is offered
in every allowed zone and fits the account's EC2 quotas before installation.

For EKS, the PWA disk selector is populated from the installer-owned profile
`diskSizeGb`. V1 requires every EKS profile to use the same size, so the
operator sees one approved value and the API can validate it. When a profile
uses `launchTemplateId`, its encrypted root-volume mapping is authoritative;
keep `diskSizeGb` aligned with that launch template. Multiple independently
selectable EKS disk sizes require a future mapping from each size to a matching
encrypted launch template.

The chart fails closed if AWS EBS is not encrypted gp3, has no approved AZ,
or conflicts with the selected Agent StorageClass. `WaitForFirstConsumer`
binds each new PVC in the selected Machine's zone. Kyber never deletes or
recreates a PVC during fallback, so the same volume is reattached to the
replacement node.

The supplied EKS values preset also pins the control plane, Postgres, and Redis
to the dedicated `kyber.io/role=platform` node and explicitly puts the two
stateful platform PVCs on `kyber-ebs`. This overrides the standalone chart's
k3s control-plane selector and avoids accidentally scheduling platform state on
a disposable Machine node.

Install with the preset plus a separate, release-pinned values file containing
the Terraform outputs and image tags:

```sh
helm upgrade --install kyber deploy/helm/kyber \
  --namespace kyber \
  -f deploy/helm/kyber/examples/values-eks.yaml \
  -f values-eks-live.yaml \
  --wait --timeout 15m
```

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
