# Coordinate credential adoption and upgrades

Status: maintenance reservation foundation implemented for MAT-134. Supervised
upgrade integration is described in the [upgrade compatibility design](2026-10-24-credential-upgrade-compatibility.md).
Runtime registry activation remains unavailable until the remaining gates are
complete. These changes do not make old binaries safe.

## Problem and outcome

Adoption selects a persistent credential authority. An upgrade runs in a separate
Job and can outlive the control plane. Allowing those operations to overlap can
invalidate adoption's compatibility checks or let an older rollback restore an
authentication path that ignores revocations. They must share one installation
maintenance boundary before adoption can be offered.

## Reservation contract

Use a separate immutable Kubernetes Secret, outside Helm ownership and workload
garbage collection. For the live registry its name is
`kyber-operator-credentials-maintenance`. Probe registries derive their own
`<registry-name>-maintenance` name and never reserve installation maintenance.
The direct, statically mapped client retains the authority transport's shared
two-second deadline and safe error projection.

A reservation records protocol, adoption/upgrade kind, a fresh execution UUID and
start time. Acquisition uses Kubernetes create uniqueness, then an authoritative
read to reconcile a lost acknowledgement. Every acquisition generates a new UUID;
another process cannot reenter a reservation by reusing a logical operation ID.
No credential values, grants or configuration snapshots are stored in it.

The object has **no expiry**. A lease expiring cannot stop a Helm process or an
API request already in flight. Time is display metadata, never evidence that a
writer is dead. A crashed operation leaves a block for explicit recovery.

Check ownership before protected effects. Release requires a current ownership
read and a delete with both Kubernetes UID and resource-version preconditions.
A replacement between that read and delete is protected by the API server. Do
not release an uncertain operation in a generic deferred cleanup. Retain it until
external writes are reconciled; a failed read cannot use cached ownership.
Metadata changes invalidate the old reservation handle conservatively.

`AuthorityStore.Bind` now requires the same registry's live adoption reservation.
It checks before reading the candidate and again before creating the marker.
The caller must reserve **before** adoption preflight and retain ownership through
all adoption effects and reconciliation. The marker remains immutable and bound
to the registry UID. Reserving does not authorize the requesting operator; the
future administrator-only admission and adoption flow owns that decision.

## Activation gates still required

The primitive is necessary coordination, not complete upgrade protection:

- Upgrade entrypoints must acquire the same live reservation before inspecting
  authority/release state and retain it through upgrade, verification, rollback
  and uncertain effects. The supervised entrypoint now connects this boundary; see the linked design.
- Before adoption, prove that all control-plane replicas support the authority
  protocol and no old upgrade Job can execute later. Reservation-aware new Jobs
  must recheck current authority rather than trust a pre-adoption decision.
- Verify both the effective target image and rollback revision understand the
  selected authority. A chart version or annotation alone cannot prove that a
  custom image is compatible. Unsupported targets must fail before CRDs or Helm
  mutate the installation. Guard every automatic rollback path.
- Document supported manual/GitOps deployment paths and their compatibility
  checks. Arbitrary privileged Kubernetes edits or restoring old backups are
  outside Kyber's managed-operation guarantee; do not describe this reservation
  as a Kubernetes admission policy or protection against a cluster administrator.
- Complete adoption source ownership, stable identities, durable admission and
  uncertain-write reconciliation, plus administrator UI and recovery visibility.

No chart advertises authority compatibility and no registry activation endpoint
is enabled merely because this primitive exists.

## Recovery requirements

The future UI must show a blocked maintenance operation, its kind, execution ID,
start time and recovery guidance. Never suggest waiting for an automatic expiry.
A safe recovery must prove that the original Job/process can no longer write,
reconcile the release/marker/registry state, then release only the inspected UID
and resource version. Merely observing an old timestamp or a failed HTTP request
is insufficient. Do not offer an unconditional force-unlock button.

The recovery UI remains pending. The supervised upgrade entrypoint uses live
maintenance, while authority-binding probes reserve separate names. Recovery
of a test failure should inspect its exact probe, never delete the live marker,
registry or installation credential Secret to restore access.

## Review and validation

Review corrected an initial reentrant operation-ID design: independent executors
must not both obtain a handle for an existing reservation. Each acquisition now
has a fresh execution UUID. A lost acknowledgement is reconciled only within its
own acquisition call; failed acquisition never grants ownership.

Unit tests cover competing executors, lost acknowledgement, no age-based takeover,
replacement protection, wrong adoption boundaries/kinds, released reservations,
redaction and read outages/deadlines. The fake Kubernetes client ignores delete
preconditions, so it cannot establish the read/delete race guarantee. The opt-in
GCP dev probe replaces a reservation after the ownership read and verifies actual
UID and resource-version conflicts, immutable data, independent clients, cleanup,
unchanged installation credential metadata and zero Agents.
