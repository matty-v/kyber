# Show credential maintenance to administrators

This slice adds read-only operator visibility for MAT-134. It does not enable
adoption, increase the runtime authority protocol, acquire a reservation or
release one. Recovery remains a separate operation with stronger evidence.

Settings → API access shows a maintenance block when the installation's exact
reservation exists. Administrators can expand its operation kind, execution ID,
start time and check time. An old start time never makes the block expire.
Unknown, malformed and unavailable reads remain unavailable rather than idle.
Protocol-zero supervised upgrades do not reserve maintenance; an idle reservation
therefore is not an inventory of all running upgrades. The reservation coordinates
adoption and upgrades; this read-only view must not claim to block every managed
key edit or rotation.

The store reads directly within its shared two-second bound, validates the
existing immutable reservation contract and returns whitelisted metadata. The
API is administrator-only, namespace-bound and no-store. It reauthenticates after
the read before returning protected metadata. Its opaque version identifies the
inspected object UID/resource version; it is not permission to release it. No
raw Secret data, transport diagnostics or private reservation handle is returned.

The UI polls only when the inventory advertises read capability, retains data
only within the current connection, and clears it after failed or denied reads.
No release or force-unlock control is offered. This establishes visibility for
the later recovery flow, which must prove the original writer has stopped and
reconcile external effects before releasing exactly the inspected reservation.

Review checks: confirmed missing object versus failed read; aged reservation;
malformed data; replacement/version changes; restricted and retired callers;
namespace mismatch; unavailable reporting; connection changes; desktop/mobile
layout and absence of mutation controls. Run the opt-in real API probe in the
empty GCP dev cluster before merge.
