# AWS EKS with EBS-first storage — implementation plan

**Status:** Draft for operator review; no implementation authorized
**Date:** 2026-08-25
**Issue:** [#103](https://github.com/matty-v/kyber/issues/103)
**Design:**
[AWS EKS with EBS-first storage](../design/2026-08-25-aws-eks-ebs-first-design.md)
**Evidence:**
[AWS EFS Phase 1 qualification](2026-08-25-aws-efs-phase1-qualification-plan.md)

## Outcome

Deliver a native EKS installation whose primary operator workflow matches GKE:
operators create, stop, start, and delete provider-neutral Machines using
reliable or cost-optimized availability classes. Agent durable state uses an
encrypted EBS volume that survives node interruption and is reattached by
volume ID to replacement capacity in the same Availability Zone.

Cost-optimized Machines fall back visibly to same-zone reliable capacity after
a reviewed threshold and can be manually returned to cost-optimized capacity.
Shared fallback work must not change existing GKE behavior. A parallel GKE
design spike defines a safe manual reliable-to-cost-optimized transition using
the same API and UX.

## Authorization boundary

This document authorizes planning only. Each implementation phase is a review
checkpoint. In particular:

- no AWS resource may be created until the Terraform plan, cost estimate,
  exact region/zones, run ID, expiry, and cleanup inventory are approved;
- no permanent AWS credential, access key, CI identity, or GitHub OIDC role is
  in scope for this exercise;
- live work uses Matt's short-lived interactive AWS session and is cleaned up
  in the same working session;
- no existing VPC, EKS cluster, EFS filesystem, IAM role, volume, snapshot, or
  other untagged resource may be adopted or mutated;
- CRD, RBAC, IAM, new dependency, and public API changes require focused review
  before their implementation slice begins;
- no production or `kyber-datawire` mutation is in scope.

## Locked decisions

- EKS Standard with EKS managed node groups, not Auto Mode or Karpenter v1.
- Encrypted gp3 EBS is the default Agent and platform StorageClass.
- One Agent retains the same PVC, PV, and EBS volume ID through node loss,
  fallback, suspend/resume, and manual return to cost-optimized capacity.
- The EKS cluster is regional/multi-AZ; each exact EBS volume and its Machine
  capacity remain zonal.
- Treat Regional GKE, Zonal GKE, and regional EKS with zonal EBS-backed
  Machines as three explicit installation topology profiles.
- Reliable Machine: one single-AZ On-Demand group, size zero or one.
- Cost-optimized Machine: one single-AZ Spot group plus one pre-created,
  size-zero On-Demand fallback group; never two desired writers.
- Fallback is explicit in status, events, notification, and cost language.
- `Retry cost-optimized capacity` is provider-neutral and rollback-safe.
- `spotFallbackAfter` is five minutes. The acceptance target is p95 Agent Ready
  within ten minutes after forced Spot loss, including fallback; do not claim
  p95 before at least 20 forced-loss samples and publication of the raw data.
- The live-test envelope is an isolated `us-east-1` installation across two
  AZs, with one explicit platform-data AZ and a hard $75 spend ceiling. A
  reviewed Terraform plan and hourly estimate are still required before apply.
- EBS backup scheduling is operator-owned and documentation-only. Kyber v1
  does not provision or control AWS Backup or Data Lifecycle Manager. The live
  recovery test may create an explicit test snapshot and must clean it up.
- EKS v1 accepts the documented single-AZ availability boundary for Postgres,
  Redis, and MinIO. Provider-native multi-AZ replacements are a separate
  follow-up.
- Cloudflare tunnel remains the default ingress path.
- EFS is excluded from v1.

## Decisions required before live apply

1. Exact two AZs, which one is the platform-data AZ, and network egress model.
2. Terraform plan, estimated hourly cost, and proof the run stays below $75.
3. Explicit resource-creation approval for the reviewed plan.

## Delivery rules

- Use small, reversible vertical slices. Shared provider/API work lands before
  the AWS-specific realization that consumes it.
- Keep wire and CRD changes additive; preserve deprecated `spot`, `zone`, and
  `interruptible` compatibility while they remain supported elsewhere.
- Provider-native resource names, ARNs, states, and purchasing terms do not
  enter the primary Machine API or PWA.
- Generated CRDs are changed only through `make generate`.
- Every `packages/pwa-views` source change includes its package version and
  changelog entry under repository policy.
- Every destructive provider action is cluster-scoped and ownership-tag
  gated. Missing ownership is a terminal refusal, not a warning.
- Machine lifecycle and Agent data lifecycle stay separate. Machine deletion
  never invokes PVC deletion.
- Each phase records commands, evidence, known gaps, and rollback notes in
  this plan before moving to the next phase.

## Workstream map

| Workstream | Primary surfaces | Result |
|---|---|---|
| Portable fallback contract | `pkg/adapters/compute.go`, `pkg/api/v1/machine_types.go`, Machine controller/API, generated CRD | Requested/effective class, fallback state, capabilities, retry intent |
| Operator UX | `packages/pwa-views` Machine pages/types/API hooks | Clear threshold/cost banner and generic retry action |
| GKE safety and transition spike | `compute_gke.go`, GKE tests, new design note | No regression; reviewed standard-to-Spot approach |
| EBS chart contract | Helm values/templates/tests and PVC config | Encrypted gp3, delayed binding, explicit durable consumers |
| EKS provider | new `compute_eks.go`, registry/config wiring, AWS SDK v2 | Ownership-gated managed node-group lifecycle and paired fallback |
| AWS interruption signal | `pkg/nodeagent`, `cmd/node-agent`, Helm | IMDSv2 interruption/rebalance notice through existing Kyber path |
| AWS infrastructure | new `infra/terraform/eks/` root | Isolated EKS/VPC/add-ons/Pod Identity/backup-ready profile |
| Installation and operations | EKS guide, IAM, backup/restore, uninstall docs | Reproducible install with visible parity differences |

## Phase 0 — baseline lock and schema review

### Deliverables

- Record current GKE, fake, static, and GCE provider contract tests that must
  remain green.
- Add missing characterization tests around:
  - GKE regional pool Online/Offline/Deleted;
  - GKE cost-optimized replacement and scheduler-demand behavior;
  - Machine/Agent phases during provider capacity loss;
  - Machine deletion versus Agent PVC ownership;
  - existing PWA reliable/cost-optimized rendering.
- Propose, review, and approve additive portable fields:
  - `Capabilities.ReliableFallbackMode` (`Unsupported`, `Manual`, `Automatic`);
  - `MachineStatus.EffectiveAvailabilityClass`;
  - `MachineStatus.FallbackReason` and `FallbackSince`;
  - `MachineStatus.CostOptimizedUnavailableSince`;
  - a durable, idempotent manual-rebalance request/observed token;
  - matching REST and PWA types.
- Decide whether the token is a spec field or a dedicated subresource action
  persisted by the API; do not use an unowned annotation as hidden state.
- Fix the exact fallback threshold/SLO decisions above.

### Tests

- `go test ./pkg/adapters/... ./pkg/controllers/machine/... ./pkg/api/...`
- current GKE Helm render tests and chart unit tests;
- `npm test`/Vitest for current Machine create/detail/recovery components;
- generated CRD diff review proving changes are additive.

### Exit gate

Matt approves the public status/action contract and exact fallback policy. The
baseline tests prove subsequent shared changes cannot silently alter GKE.

### Baseline evidence — 2026-08-25

Build approval was recorded in Telegram message 349. No AWS resources were
created during this checkpoint.

- Toolchains matched repository requirements with session-local Go 1.26.0 and
  Node 26.7.0. Downloaded artifacts were checksum-verified against the official
  Go and Node release manifests.
- `go test -p 2 ./pkg/adapters/... ./pkg/controllers/machine/... ./pkg/api/...`
  passed after installing the CI-pinned Kubernetes 1.31.0 envtest binaries.
  The first attempt's only controller failures were the absent envtest binary;
  adapters and API passed on that attempt, and the controller package then
  passed with `KUBEBUILDER_ASSETS` set.
- `npm test` in `packages/pwa-views` passed under Node 26: 81 files and 728
  tests. A concurrent first attempt produced timeout failures while the Go
  dependency graph was compiling on the constrained workspace disk; the
  authoritative sequential run was clean.
- Existing GKE coverage already pins provider capabilities, regional
  scheduler-demand behavior, total-size-one autoscaling across two zones,
  interrupted-capacity recovery without repeated resize, ownership-gated
  create/resize/delete, supported availability classes, and live-test safety
  guards.
- Existing Machine controller coverage pins provider capacity loss,
  preemption/replacement, capacity-request parking, lifecycle convergence, and
  node-derived capacity status. Phase 0 will add only gaps directly required
  by the additive fallback contract before changing production behavior.

The approved portable contract uses a dedicated API action whose idempotency
token is persisted in owned Machine spec/status fields. It does not use a
hidden annotation. Existing lifecycle phases remain unchanged, and providers
advertise fallback support through capabilities so GKE stays inert until its
separate implementation is approved.

## Phase 1 — provider-neutral fallback status and action

### Adapter and controller contract

- Extend `Capabilities` with fallback support without naming Spot or
  On-Demand.
- Extend `CapacityObservation` with effective availability class and portable
  fallback metadata.
- Pass requested availability class and one-shot retry intent through
  `DesiredMachine`; retain `Interruptible` compatibility until all callers are
  migrated.
- Keep core lifecycle phases unchanged:
  - interrupted replacement remains `Recovering`/`Replacing`;
  - a Ready fallback Machine is `Available`/Ready with requested
    `costOptimized` and effective `reliable`;
  - retry in progress returns to `Recovering` without inventing an AWS phase.
- Persist threshold timestamps so a control-plane restart cannot reset the
  wait or issue duplicate fallback.
- Require providers to prove single-active-capacity before reporting fallback
  Ready.

### API

- Add `POST /api/v1/machines/{name}/retry-cost-optimized` (final route name is
  reviewed with existing conventions).
- Admit only managed Machines whose requested class is `costOptimized`, whose
  effective class is `reliable`, and whose provider capability permits retry.
- Make duplicate request IDs idempotent.
- Return conflict when stop/delete/replacement is already in progress.
- Emit Kubernetes events for:
  - `CostOptimizedUnavailable`;
  - `ReliableFallbackStarted`;
  - `ReliableFallbackReady`;
  - `CostOptimizedRetryStarted`;
  - `CostOptimizedRetryReady`;
  - `CostOptimizedRetryRolledBack`.

### PWA

- Keep Machine creation unchanged: `Reliable` and `Cost optimized` only.
- Machine detail shows requested and effective classes together when they
  differ.
- Banner states the threshold, unavailability duration, reliable-rate cost,
  and whether the Agent is parked or Ready.
- Show `Retry cost-optimized capacity` only from capabilities and status.
- Confirmation explains brief interruption and exact-disk preservation.
- Poll/refetch until retry succeeds or rolls back; never expose AWS group names.

### Notification

- Route fallback/retry state changes through the existing operator event and
  channel reporting path rather than adding an AWS messenger.
- Deduplicate notifications by transition/request ID.

### Tests and exit gate

- table tests for every status/capability combination;
- envtest for timeout persistence, restart, duplicate reconcile, retry,
  rollback, stop/delete conflict, and Agent parking;
- API authorization/validation/idempotency tests;
- PWA visibility, wording, accessibility, loading, failure, and rollback tests;
- fake provider deterministic end-to-end fallback/retry test;
- all Phase 0 GKE characterization tests unchanged and green.

Gate: portable fallback works against fake capacity and is inert for GKE when
the GKE provider reports `Unsupported`.

### Build checkpoint — 2026-08-25

Completed and pushed:

- portable requested/effective availability, fallback timestamps/reason, and
  one-shot retry status through adapter, CRD, controller, and config API;
- authorized, validated, idempotent `retry-cost-optimized` API action;
- provider-neutral Machine detail fallback banner and guarded retry UX;
- deterministic fake-provider fallback, successful retry, and failed-retry
  rollback while retaining the same provider reference;
- deduplicated Kubernetes events for unavailability, fallback start/Ready,
  retry start/Ready, and retry rollback.

Verification: adapter tests, targeted API tests, Machine controller envtest,
PWA component tests, and TypeScript lint pass. GKE still advertises fallback
as `Unsupported`; no production GKE behavior has changed. A focused in-memory
controller/provider contract test now covers fallback, stable provider/storage
identity, retry success, and retry rollback without paying the envtest startup
cost. Phase 1 is complete; next is the Phase 2 GKE transition spike.

## Phase 2 — GKE transition design spike and non-regression

This phase designs and tests; it does not change production GKE behavior.

### Questions to resolve

- Can a managed GKE node pool safely change between Spot and standard, or is
  the provisioning model immutable?
- If replacement/paired pools are required, how are pool autoscaling, provider
  refs, ownership labels, quota, and one-active-node enforced?
- For regional GKE, can standard fallback use either regional-PD replica zone
  after Spot has been unavailable across both eligible zones?
- For zonal GKE, how does same-zone fallback retain the zonal Persistent Disk,
  and how are its whole-zone outage limits presented to the operator?
- How does each transition retain the same Persistent Disk and Agent Machine
  affinity?
- What is the rollback path when Spot capacity remains unavailable?
- Should GKE eventually support automatic fallback or manual transition only?

### Deliverable

Add a focused GKE transition design beside this plan with a recommendation,
API compatibility proof, resource/quota impact, and live-test proposal. Extend
fake GKE client tests for the chosen resource model, but keep the capability
`Unsupported` until a later separately approved implementation.

Update GKE installation documentation to distinguish the regional and zonal
profiles, their disk mobility, fallback scope, and zone-outage behavior. The
spike must evaluate the shared fallback contract for both profiles; regional
GKE's larger Spot capacity pool reduces risk but does not guarantee capacity.

### Exit gate

AWS work may continue when existing GKE behavior is demonstrably unchanged and
the shared contract does not block the reviewed future GKE path.

### Build checkpoint — 2026-08-26

The focused transition design is recorded in
[`2026-08-26-gke-cost-optimized-fallback-spike.md`](../design/2026-08-26-gke-cost-optimized-fallback-spike.md).
It confirms paired Spot/standard pools are required, regional PD mobility is
limited to its two replica zones, and zonal PD fallback remains same-zone.
The shared status/action contract supports this future path without provider-
specific UX. GKE continues to advertise fallback as `Unsupported`; no GKE
runtime behavior changed. Phase 2 is complete and AWS implementation may
proceed.

## Phase 3 — EBS Helm contract

### Values and templates

- Add `storage.awsEBS` beside `storage.gcePD`:
  - `enabled`, `storageClassName`, `type`, `encrypted`, optional `kmsKeyArn`;
  - `allowedZones`, `allowVolumeExpansion`, and supported filesystem type.
- Render `ebs.csi.aws.com`, encrypted gp3, `reclaimPolicy: Delete`,
  `volumeBindingMode: WaitForFirstConsumer`, expansion, and approved topology.
- Fail rendering for simultaneous GCE PD/AWS EBS enablement, missing zones,
  unencrypted production configuration, or mismatched Agent class.
- Require Agent roots, transcript offsets, Postgres, Redis, and MinIO to name
  the EBS class explicitly in the EKS preset.
- Add installation labels/annotations needed for independent volume inventory;
  do not put provider ownership on PVCs Kyber does not own.

### Tests

- golden renders for default gp3 and KMS-key encryption;
- negative renders for every fail-closed rule;
- PVC/PV ownership regression: Machine delete leaves Agent PVC untouched;
- Agent delete renders/executes the existing reclaim contract;
- expansion and zone topology assertions;
- existing GCE PD, WSL2, local-path, and default chart renders unchanged.

### Exit gate

Static manifests prove correct zonal delayed binding and no non-EKS chart
regression. No AWS resource is needed.

### Build checkpoint — 2026-08-26

Phase 3 is implemented: the chart renders an encrypted gp3 EBS CSI
StorageClass with `WaitForFirstConsumer`, `Delete`, expansion, optional KMS,
filesystem selection, and explicit allowed AZ topology. Enabling it
automatically selects the class for Agent PVCs. Rendering fails closed for
GCE/EBS dual enablement, missing zones, disabled encryption, unsupported disk
type/filesystem, or a mismatched explicit Agent class. Default and valid EBS
renders plus negative validation cases pass with verified Helm v4.0.0.

## Phase 4 — EKS adapter: read model and reliable lifecycle

### Dependency and configuration

- Add only required AWS SDK for Go v2 EKS/config/smithy modules after dependency
  review.
- Add `eks` registration and fail-closed configuration wiring in
  `cmd/control-plane/main.go` and Helm:
  - region, cluster, profiles, allowed zones, node role ARN;
  - subnet IDs by zone and launch-template identity/version;
  - fallback threshold and feature policy.
- Use the SDK default credential chain for EKS Pod Identity; accept no static
  key fields.

### Adapter

- Add `pkg/adapters/compute_eks.go` with a narrow fakeable client.
- Parse installer-curated profiles containing provider-neutral capacity and
  private compatible instance types, AMI/root disk settings, and classes.
- Validate same-shape instance lists, zone/subnet membership, encryption,
  required roles, and immutable profile inputs.
- Use opaque cluster-scoped `eks://` provider refs.
- Translate EKS states/errors into portable observations.
- Discover external groups read-only; reject platform, multi-node, untagged,
  cross-zone, or incompatible adoption.
- Implement reliable On-Demand create, size 1, size 0, repair observation, and
  ownership-gated delete before adding Spot.

### Tests and gate

- parser/provider-ref round trips and hostile refs;
- not-found, conflict, throttling, degraded, create/delete-in-progress, and
  partial-create tests;
- ownership refusal on every mutating path;
- control-plane restart/idempotent reconciliation;
- zero AWS SDK calls in non-EKS modes;
- shared provider contract suite plus full GKE suite.

Gate: reliable EKS behavior converges entirely against fake clients.

## Phase 5 — EKS paired Spot fallback

### Native realization

- Create Spot and On-Demand fallback groups in the same approved zone with
  identical Machine labels and compatible capacity.
- Pre-create fallback at minimum 0, maximum 1, desired 0.
- Spot group uses multiple same-shape instance types and EKS Capacity
  Rebalancing.
- Maintain an explicit active-group role in owned provider state/tags; infer
  nothing from names alone.
- On timeout:
  1. set Spot desired size to zero;
  2. wait for attached node count zero and authoritative node absence;
  3. wait for the Agent volume to be detached/available through Kubernetes
     attachment observation, not arbitrary sleep;
  4. set On-Demand desired size to one;
  5. report effective `reliable` only after a Ready node attaches.
- On manual retry, perform the reverse sequence. If Spot is not Ready inside a
  reviewed retry budget, scale it to zero and restore On-Demand.
- Offline scales both groups to zero. Delete scales both to zero, verifies no
  nodes, and deletes only owned groups. Neither path touches PVCs.

### Tests and gate

- fake-clock threshold and control-plane restart tests;
- Spot recovers before threshold (no fallback);
- Spot unavailable past threshold (single fallback);
- late Spot node race, dual-node race, stale volume attachment, API conflict,
  partial update, and rollback tests;
- exact same PV/volume identity asserted before and after fallback/retry;
- quota-exhausted fallback group creation fails Machine creation before Agent
  placement rather than discovering the problem during interruption;
- GKE suite unchanged.

Gate: deterministic tests prove bounded orchestration and no double writer.

## Phase 6 — AWS interruption source

- Refactor `pkg/nodeagent` preemption polling behind a source interface.
- Preserve the current GCE source byte-for-byte in behavior.
- Add EC2 IMDSv2 token acquisition and Spot interruption/rebalance metadata
  source with bounded timeouts and one-shot notification.
- Select source from provider/install configuration; never probe multiple
  clouds opportunistically.
- Keep forced termination correctness in Machine/provider observation; metadata
  notice is only a graceful-drain/session-brief enhancement.
- Add metadata fixtures for no notice, rebalance, interruption, token expiry,
  IMDS unavailable, malformed response, and duplicate notice.
- Verify Helm security context and hop limit allow only the privileged node
  agent to reach IMDS; ordinary Agent Pods receive no node-role credentials.

Gate: GCE node-agent tests remain unchanged and AWS fixtures add no network
dependency.

## Phase 7 — disposable EKS Terraform root

### Static implementation

Create `infra/terraform/eks/` as a separate root with:

- new isolated VPC, at least two AZ subnets, routing, and reviewed NAT/endpoints;
- EKS Standard cluster, access entries, control-plane logging;
- one single-AZ reliable platform managed node group;
- VPC CNI, CoreDNS, kube-proxy, EBS CSI, and Pod Identity add-ons;
- least-privilege roles/associations for control plane and EBS CSI;
- worker role, encrypted launch templates, IMDSv2, and cluster tags;
- optional AWS Backup/KMS resources only after policy approval;
- unique run/owner/issue/expiry tags and outputs needed by Helm;
- no static credentials, existing-resource IDs, load balancer, or EFS.

### Static checks

- formatting, validation, provider lockfile, and policy/static-analysis checks;
- exact resource-count and action summary from saved plan JSON;
- IAM action/resource review, including bounded `iam:PassRole`;
- quota preflight for VPCs, EIPs/NAT, EKS clusters, managed node groups, EC2,
  EBS, Pod Identity associations, and KMS grants;
- cost estimate with an explicit upper bound and eight-hour live-test expiry.

### Approval gate

Send Matt the rendered plan summary, IAM boundary, hourly/max cost, region/AZs,
public/private exposure, existing-resource inventory, and cleanup query. Do not
apply without a new explicit approval.

## Phase 8 — guarded live EKS acceptance

### Provision and install

- authenticate interactively and verify account/region without printing
  credentials;
- re-inventory and re-plan from empty state;
- apply only the saved approved plan;
- install pinned local-worktree Kyber images using the existing pre-PR process
  adapted to the disposable EKS cluster;
- verify Pod Identity, no static AWS secrets, EBS CSI health, StorageClass,
  platform PVC zones, and no public ingress beyond the approved path.

### Required scenarios

1. Reliable Machine create/stop/start/delete.
2. Cost-optimized Machine plus Agent; record Agent PVC/PV/EBS volume ID.
3. Pod restart and node replacement with exact-volume reattachment.
4. Proactive Spot rebalance path and hard node termination path.
5. Spot recovery before threshold: no reliable fallback.
6. Forced/mockable Spot-unavailable signal past threshold: explicit event,
   notification, reliable-rate banner, On-Demand Ready, exact same volume ID.
7. Manual retry: controlled interruption, Spot Ready, exact same volume ID.
8. Failed manual retry: rollback to reliable, same volume, no dual writer.
9. Agent deletion: PVC, PV, and EBS volume deleted; Machine groups retained.
10. Machine deletion while Agent data exists: operation refused or Agent data
    preserved according to the reviewed lifecycle rule.
11. PVC expansion and filesystem growth.
12. Snapshot restore into a second zone: new volume ID, measured RPO/RTO, clear
    distinction from same-disk recovery.
13. Existing GKE unit/chart suite and, if authorized, read-only live
    `kyber-datawire` observations after the shared changes.

The recovery-SLO run needs at least 20 controlled hard-loss/fallback samples to
make a p95 claim. Use a reviewed zero/short threshold test configuration to
exercise fallback deterministically rather than waiting for AWS to deny Spot
capacity naturally. If the approved time/cost budget cannot support the sample
count, report individual timings and do not claim p95.

### Evidence

- timestamps for every provider, node, Pod, VolumeAttachment, and Agent phase;
- requested/effective class and operator-visible event/notification captures;
- EBS volume IDs before/after each in-zone scenario;
- attach/detach errors and measured recovery distribution;
- actual spend and resource inventory;
- no secrets or unrelated account data in committed artifacts.

### Stop conditions

- any test creates a new Agent volume during in-zone recovery;
- two active Machine nodes can write the same Agent root;
- fallback or retry loses/duplicates operator input;
- On-Demand fallback cannot meet the reviewed recovery SLO;
- provider ownership checks can target unrelated groups;
- GKE behavior regresses;
- projected cost exceeds the approved ceiling.

## Phase 9 — cleanup, documentation, and handoff

- Always Terraform-destroy the exact live-test state after evidence capture,
  including on test failure.
- Independently query ownership/run tags plus explicit EKS, EC2, EBS, IAM,
  ELB, ENI, EIP, snapshot, backup, KMS, and CloudWatch resources.
- Treat terminated-resource tag API lag as stale only after native service APIs
  prove absence.
- Retain local state until independent cleanup passes, then remove generated
  plan/state artifacts from the worktree.
- Publish:
  - `docs/installation-eks.md`;
  - EKS IAM/Pod Identity/KMS reference;
  - EBS backup and cross-zone restore runbook;
  - Spot fallback/manual retry operations;
  - managed node-group quota/cost guide;
  - uninstall/orphan-volume audit procedure;
  - measured parity table against `kyber-datawire-regional`.
- Update this plan with commits, test evidence, live resource IDs while active,
  cleanup proof, measured cost, deviations, and remaining risks.

## PR/checkpoint sequence

1. Baseline characterization plus reviewed portable schema.
2. Portable status/action/PWA with fake-provider proof.
3. GKE transition spike and non-regression evidence.
4. EBS Helm contract.
5. EKS read-only/reliable adapter.
6. EKS paired fallback and retry.
7. AWS node-agent interruption source.
8. Static Terraform EKS root, IAM, and installer preflight.
9. Approved disposable live acceptance and cleanup evidence.
10. Installation/day-two documentation and final issue acceptance report.

Do not combine the shared contract, AWS adapter, Terraform apply, and live test
into one review unit.

## Definition of done

- The approved design conditions are implemented and covered by deterministic
  tests.
- Operators see fallback threshold, effective reliable capacity, cost impact,
  and manual retry without AWS terminology in the primary workflow.
- Every in-zone interruption/fallback/retry retains the exact Agent EBS volume
  ID and prevents a second writer.
- The live recovery SLO and failure rollback pass with recorded evidence.
- GKE behavior has no regression, and its manual transition path has a reviewed
  design.
- The installer sees the regional-EKS/zonal-EBS difference before apply.
- No long-lived AWS credential exists.
- The disposable live environment is independently proven absent.
- Matt approves measured behavior, cost, docs, and remaining zonal recovery
  tradeoff before issue #103 is considered complete.
