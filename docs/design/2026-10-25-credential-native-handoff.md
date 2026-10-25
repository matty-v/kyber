# Native explicit credential handoff and recovery

Implementation integration plan for MAT-134/MAT-97. Keep runtime protocol zero
until the complete handoff, recovery, management and release gates pass dev.

## Operator flow

Settings API access shows the accepted keys and their current source. A current
administrator can inspect the handoff proposal, then explicitly confirm that
Kyber will own future key changes. Existing accepted values and stable identities
are retained. Show only concrete reconnect warnings and ownership consequences;
never protocol, fencing or Kubernetes jargon in the confirmation dialog.

Persist authorization before creating a non-retrying supervised Job. Use one
operation UUID as audit target and deterministic Job identity. The immutable
journal stores source versions and deployment context before launch. Job
environment carries operation/control-plane names and Downward-API identity,
never operator values, private candidates or database passwords. Workers load
the original inputs from that journal instead of resubmitted definitions. Unknown launch responses remain
pending and are inspected explicitly, without issuing another operation.

The worker loads database connection configuration privately from the supported
control-plane profile and referenced Secret. It validates the original launch
audit, its own Pod/Job identity and published image, reserves maintenance, and
verifies source versions, supported replicas and old-executor quiescence.
The original immutable launch admission already exists. Record the actual
reservation UID/original version before admitting any credential effects; keep
that version stable across replacement writers and late original acknowledgements.

## Effects and completion

Reconstruct the same private candidate from the original operation and unchanged
sources. Admit each effect checkpoint before its create. An existing candidate
must match the original operation and candidate privately; never adopt an
unrelated Secret. After selection, verify the UID-pinned CURRENT authority and
original operation evidence rather than overwriting later legitimate state.
Seal the selected marker UID in registry metadata for restart-safe fail-closed
behavior. Finish required audit, acknowledge the adoption outbox and release maintenance
before declaring the workflow complete. Keep named-key mutation unavailable
while the adoption outbox is still pending. Authentication must remain independent
of database availability and must never fall back after authority selection.

## Interrupted worker

Recovery is explicit and requires a current administrator. Inspect exact writer
Pod/Job shutdown evidence; age, missing objects and node loss are not proof.
Launch a fresh non-retrying recovery Job for the SAME admitted operation. Fence
the old reservation with an exact UID/resource-version metadata CAS, recording
the replacement writer in a validated annotation because maintenance Secret data
is immutable. The original private handle must fail after this CAS. Concurrent
replacement attempts have one winner; an unknown CAS response reconciles only
that attempt's nonce, never adopts a different winner.

The active writer projection must follow the replacement receipt so another
recovery cannot displace a running replacement by inspecting the original dead
writer. Keep original Job evidence until confirmed release; cleanup uncertainty
retains evidence rather than changing the credential outcome. Missing source or
writer proof stays pending; there is no force-unlock or rollback to configuration.

## Remaining acceptance

Wire the current authority, live access watcher, named-key management and audit
outbox only as the complete protocol. Supported bootstrap rotation must require
the explicit handoff rather than race it. Verify publisher-asserted effective
images and supported rollback artifacts. Test native execution in the verified
empty GCP dev cluster, including restart/lost responses, source or replica change,
authority outages, current bearer/cookie/stream access and stable task ownership.
Capture desktop/mobile UI, clean disposable test data and update operator docs.
Production credential activation remains a separate approved operation.

## Integration checkpoint

The API records the complete immutable intent before launching the supervised
native worker. The worker rereads the original journal record and verified
release/source graph, uses a private reservation, preserves original identity
and cookie deadlines, seals the selected authority, and confirms audit delivery
before releasing maintenance. A lost response remains an operation to inspect,
never a reason to issue another key or repeat a launch automatically.

Settings exposes an explicit ownership confirmation, read-only status checks,
and a Continue setup action only after a fresh server observation establishes
that recovery is eligible. Changed connections and access denials clear all
protected setup state. UI capability metadata alone never authorizes a write.

Activation remains protocol zero pending the complete reviewed backend, contract,
UI, native dev-cluster, release/rollback and screenshot acceptance checks.
Production installations are not adopted by this integration checkpoint.
