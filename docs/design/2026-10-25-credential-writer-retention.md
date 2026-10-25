# Preserve supervised writer evidence

> Historical implementation plan. The integrated operator-key feature shipped in
> [PR #396](https://github.com/matty-v/kyber/pull/396). Staged or proposed status
> statements below describe development at the time, not current availability.
> See the [current operator guide](../operator/operator-credentials.md) for supported
> setup, management, recovery and reporting limits.

Follow-up to the staged MAT-134 recovery flow. Protocol remains zero.

A recoverable reservation must not outlive the Pod/Job evidence required to
verify its original writer. Protocol-one upgrade Jobs therefore start without
an automatic TTL, and bound acquisition rejects Jobs with an automatic TTL.
The adoption Job builder must use the same rule. A fresh metadata CAS binds a
Job to exactly one execution before reservation creation. A second acquisition
from that Job is rejected, even after release or manual TTL removal; this fences
cleanup against reuse of the same running writer. Unknown CAS acknowledgements
reconcile only the original nonce and never acquire from another execution. Protocol-zero Jobs retain the
existing seven-day cleanup behavior because they create no persistent lease.

After a confirmed release, the private handle may schedule seven-day Job cleanup
using the original Job UID and optimistic resource version. Never change a
replacement Job or schedule cleanup while any current reservation still uses
that writer. Missing, malformed or unavailable evidence remains retained.
Cleanup is housekeeping: an unconfirmed write may already have set the valid
cleanup TTL or may leave extra evidence retained. Neither outcome changes the
already confirmed release or invents a credential failure.
A lost release acknowledgement can leave evidence retained even after explicit
outcome reconciliation; there is no unsafe age-based fallback or blanket prune.

Review: retention changes no credential material or authority. The cleanup
permission is internal to an existing private handle, not a public force-delete
API. All mutations are bounded and UID/version checked. The supported normal
supervisor releases only after verified upgrade or verified rollback; recovery
releases only after termination and release reconciliation. Arbitrary privileged
Job/Pod deletion remains outside that guarantee. A future bounded cleanup scan
may reclaim confirmed orphan evidence; it is not an activation safety gate.

Checks: protocol-zero/one Job TTL behavior, refusal of expiring writer evidence,
retention while blocked, exact release cleanup, replaced Job and concurrent
metadata races, unavailable cleanup retaining evidence, unchanged authority
protocol and existing upgrade/recovery suites.

The protocol-one chart supplies a namespace Role for native Job creation and
metadata cleanup, plus ReplicaSet reads for release verification. There is no
Job delete grant and no cross-namespace credential-maintenance grant. Protocol
zero renders neither Role nor binding. The existing self-upgrade opt-in and
its dedicated supervisor account are unchanged. Chart/app metadata advances to
1.8.1 while the advertised authority protocol remains zero.
