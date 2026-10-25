# Preserve credential authority through supported upgrades

> Historical implementation plan. The integrated operator-key feature shipped in
> [PR #396](https://github.com/matty-v/kyber/pull/396). Staged or proposed status
> statements below describe development at the time, not current availability.
> See the [current operator guide](../operator/operator-credentials.md) for supported
> setup, management, recovery and reporting limits.

Status: implementing the supervised-upgrade integration for MAT-134, following
maintenance reservation PR #377. Runtime activation, recovery UI and the full
adoption preflight remain unavailable. Protocol zero is advertised explicitly.

## Existing behavior reviewed

`updates.Applier.Start` creates a Job using the current control-plane image.
`selfupgrade.Runner.Run` reads Helm status/values, rejects pinned image versions,
pulls the target chart, applies CRDs, upgrades, verifies rollout/image/health and
rolls back to the initial revision on failure. Before this change neither the Job nor the rollback
checked operator credential authority compatibility. The deployment
verifier intentionally does not mount administrator keys merely to read version
metadata. Preserve that separation.

The reservation primitive serializes cooperating processes. It cannot stop an
old Job that does not understand the protocol, arbitrary manual Helm commands,
a GitOps controller, or a privileged Kubernetes edit. Do not advertise a complete
rollback guarantee solely because a marker or reservation exists.

## Required operator behavior

Before adoption, normal supported upgrades continue working. After adoption,
Kyber must refuse an effective target or automatic rollback that could ignore
the selected authority. Explain the refusal before changing CRDs or the release.
Do not offer deletion of the marker/registry or fallback to configuration keys
as an upgrade recovery step.

Operators using the UI must see a maintenance block and its recovery state.
Installing a different version cannot restore a revoked key. A custom image or
unverified deployment path must be identified as unsupported for adoption rather
than assumed compatible from its version string.

## Upgrade execution contract

1. Acquire the live maintenance reservation before reading release or authority
   state. The current Job must be the sole executor; another Job may report the
   block but must not borrow its execution identity.
2. Resolve the fixed installation marker directly, with bounded reads. An
   unavailable or malformed marker is not an unadopted installation. After an
   observed adoption, a missing marker is an error.
3. Capture the current release revision and effective rollback workload. Pull
   and render the exact target with the actual values. Reject image overrides
   that bypass the supported release's authority contract. Chart metadata alone
   cannot establish the compatibility of an arbitrary replacement image.
4. For an adopted installation, verify the authority protocol for BOTH the
   effective target workload and initial rollback workload before applying any
   CRDs. Unknown, older or unsupported protocol means refuse. No arbitrary
   historical revision is substituted as an automatic recovery shortcut.
5. Check reservation ownership again before protected writes. Retain it through
   CRD application, Helm, rollout/health verification and any automatic rollback.
   Read actual rolled-out image state; a chart's requested version is insufficient.
6. Release only after a known terminal state and reconciliation of external
   effects. A failed/killed Helm process is not by itself proof that every API
   request it issued has stopped committing. Unknown state remains blocked.

Dry-run performs current authority/compatibility checks without reserving or
changing cluster state. It reports validation as of its reads; an actual attempt
must reserve and revalidate rather than trust a previous dry-run decision.

## Effective release compatibility

Published charts carry a literal authority protocol and control-plane release
image in chart metadata. The release/main packaging workflows stamp the image
from the chart's published defaults, independently of installation value
overrides. The Deployment pod template carries those assertions. The current
source chart and runtime explicitly declare protocol **zero**; a contract test
keeps those declarations aligned. Do not raise them until activation gates pass.

For selected authority, the supervisor accepts only the supported official chart
source, protocol one, its declared official versioned image, and the expected
installation Deployment/namespace. It rejects a differing effective image,
replaced command/unsupported flags, extra containers/init containers or mounts
which could replace the binary or installation namespace. This is a trusted
publisher artifact contract, not a signature or protection against a privileged
operator forging release history. Custom artifacts remain unverified.

The starting rollback manifest must be compatible and match the current live
Deployment image. Immediately before automatic rollback, re-read its recorded
revision and refuse a changed image, unsupported protocol or unavailable record.
Never echo rendered manifests, raw parse errors or render diagnostics: they may
contain credentials. Normal pre-adoption targets remain supported; both dry runs
and real attempts recheck current authority. Dry runs reserve nothing and change
no cluster state. Protocol-one operations reserve before reading release state.

Pre-activation protocol zero performs fail-closed authority reads but does not
create persistent maintenance reservations. A selected authority is unsupported
in that runtime and refuses. This preserves existing upgrade retry behavior
until recovery UI and full activation support are available. Tests exercise
the future protocol-one coordinator explicitly. Do not enable that protocol
merely because these supervisor mechanics are implemented.

The supervisor uses statically mapped Deployment and CRD clients; discovery must
not extend the guard's bounded read. Read-only/preflight failures attempt to release owned
reservations; unavailable ownership reads leave the block in place. Protocol-one successful verified operations release before logging completion.
Once a CRD/Helm write may have happened, failures retain maintenance even after an
attempted rollback. Do not infer that every in-flight write is settled merely
because Helm exited. Failed release is not reported as a successful operation.

## Adoption preflight contract

Adoption reserves the same boundary before examining the candidate and release.
It must verify all control-plane replicas and upgrade executors understand the
protocol, including terminating/old replicas and Jobs which could still run.
New reservation-aware Jobs must re-read authority after acquisition, so a queued
Job cannot use a pre-adoption decision. Existing unaware Jobs require explicit
quiescence before adoption is available.

Keep activation unavailable until the supported upgrade, rollback and recovery
paths are connected and verified. A known-compatible target declaration must be
tied to the runtime implementation, not added to the chart early as a promise.

## Deployment ownership and recovery

Kyber's supervised Helm path is the first supported adoption path. A GitOps
installation needs an equivalent explicit compatibility/preflight boundary;
merely running a check before a controller's later sync is not serialization.
Do not silently claim that arbitrary direct Helm rollback is protected. Document
how operators perform a checked version change, and when a deployment owner must
coordinate adoption first.

The admin recovery workflow must identify the execution and verify its old Job
and processes are no longer capable of writing. Reconcile Helm revision,
Deployment, marker, registry and pending audit operation before releasing the
inspected UID/resource version. If an adoption write has an uncertain outcome,
recover the admitted operation; do not declare it cancelled while its marker
could still commit later. New keys must never be reissued during recovery.

No unconditional unlock or age-based takeover. No recovery that restores old
credential material over current policy. Audit the administrator recovery action
separately from the original operation and retain uncertain original outcomes.

## Verification and review gates

- Refusal ordering: missing/malformed/unavailable authority; incompatible target;
  incompatible rollback; custom image; lost reservation. Assert no CRD or Helm
  mutation before refusal.
- Concurrency: adoption versus upgrade; queued Jobs; mixed/terminating old
  replicas; independent Jobs; cancelled requests; process death.
- Failure: CRD failure, upgrade failure, failed verification, rollback failure,
  lost acknowledgements and failed reservation release. No false success or
  configuration fallback; preserve recovery evidence.
- Real empty GCP dev installation: disposable test credentials only; rotate and
  revoke, perform a supported upgrade/restart, verify revoked bearer/cookie/link
  access stays rejected and current keys/owners remain intact. Exercise rejected
  old target before any release mutation. Clean up owned fixtures and retain
  truthful audit history.
- Review operator UI messages on desktop/mobile once the recovery surface exists.

Spec review findings: a time-limited lease cannot fence Helm; a chart version
cannot prove an overridden image's protocol; an automatic rollback needs the
same guard as the target; old queued Jobs are not covered by a new reservation;
a request failure does not establish cancellation of an adoption write. These
are implementation gates, not exceptions to be waved through during activation.
