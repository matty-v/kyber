# Prepare safe credential adoption and uncertain-write recovery

Status: candidate conversion and fencing are being implemented for MAT-134 and
MAT-97. They do not expose activation or credential mutation endpoints. Runtime
protocol remains zero. The complete administrator UI, audit/reconciliation and
activation preflight are still required.

## Candidate conversion

Build an independent private candidate from a mutex-protected snapshot of the
currently accepted configuration. Do not read root material and cookie signing
state from different snapshots. Candidate preparation writes nothing and cannot
change the live authenticator. Its serialization and diagnostics must reject or
redact private data, just like registry documents.

Keep the installation credential's stable identity and key value. Preserve each
accepted integration's existing principal, tenant, credential ID, generation,
provenance and effective grants. Imported issuance time stays unknown. Generate
only missing identities; a missing tenant receives its own ID rather than
borrowing another principal's installation tenant. Do not grant cross-principal
task access through identity repair.

Missing identity labels are derived with a versioned, purpose-separated UUID
namespace from the original admitted operation UUID and the ordered source
definition index. They are not secrets and do not depend on credential values
or their hashes. Reconstructing the same admission and unchanged source graph
therefore reconstructs the identical private candidate, including its original
browser compatibility cutoffs. A distinct operation assigns independent missing
identities. Existing identities are never rewritten. This does not authorize
replay: the worker must independently verify the frozen source graph, immutable
admission, current authority and original effect evidence before recovery.
Never rebase a pending operation onto changed/reordered sources or a new time.

Match actual authentication precedence: the last configured definition for a
value wins, and the installation value takes precedence over all copies. Collapse
shadowed definitions without combining permissions or dropping any distinct
accepted material. Select winners in linear time and enforce bounded capacity.
Distinct values with ambiguous credential IDs block adoption; never silently
rewrite stable identity or discard a working key. Unsupported grants/material
also block rather than activate a different policy.

Names are display metadata. Preserve usable names, disambiguate collisions and
replace invalid or unsupported `legacy` display names with plain imported-key
labels. Browser binding and task ownership must not depend on these labels.
Pre-identity browser sessions can bind to their unique current material; partial
old identity that cannot be preserved requires a truthful reconnect warning.

The candidate represents an explicit transfer to **Kyber-managed** authority,
not a frozen value falsely labelled externally managed. The future UI must show
what will be accepted and require confirmation that configuration will no longer
control those keys. Do not infer external ownership from a snapshot or pretend
Helm/GitOps edits will continue to rotate registry-owned material.

## Bounded browser compatibility

Preserve only a derived pre-adoption signer, with immutable issuance and expiry cutoffs
and per-credential eligibility generation. Candidate construction sets the cutoff
from the admitted operation time plus a one-minute cutover/clock allowance. The
private codec persists it; a signer with no cutoff fails closed. Do not invent a
fresh cutoff when loading old state.

The compatibility reader verifies signatures, then requires the fixed expiry
horizon, a bounded issued-at
and a maximum original browser-session lifetime (currently 30 days). It cannot
mint/renew old-format sessions indefinitely. New sessions use the individual
credential's V2 signer and remain valid after the compatibility horizon.

Rotating, expiring, revoking or removing administrator authority from the
installation signer ends scoped V1 trust. The old signer cannot keep signing
other credentials' browser sessions after its authority is retired. V2 sessions
remain independent. Existing signed installation cookies still resolve current
installation grants and cannot recover old administrator rights after narrowing.
The UI must explain that installation-key rotation can require older browsers to
reconnect; do not claim an active-session inventory.

## Fence unknown writes

After an uncertain compare-and-swap response, never replay issuance or reveal an
existing value. `SecretStore.Fence` changes a private metadata nonce at the
original expected resource version, preserving credential state and operation
references. Either the original update already committed, or the fence prevents
that older update from committing later. Resource versions are opaque: compare
for equality only, never numeric ordering.

On a conflict or lost fence acknowledgement, read the UID-pinned current
authority and operation outbox. A committed operation reference is evidence of
the original effect. A changed version without that reference fences the old
attempt; it is not permission to recreate the key automatically. Missing recovery
state is unavailable, never a bootstrap signal. Required audit admission and
current authorization belong to the caller; metadata fencing is not an API that
can bypass them.

The remaining mutation executor must persist admission and the original expected
version before effects, atomically commit the operation reference with credential
state, append final completion idempotently and reconcile admissions with no
outbox reference after crashes. Do not write an irreversible final failure while
an old update can still commit. Do not overwrite a final event or issue a second
key when an acknowledgement is lost.

## Remaining activation gates

Before offering adoption, complete current administrator authorization, required
durable admission, candidate/source freshness, consistent supported replicas and
old-executor quiescence. Serialize supported bootstrap credential rotation with
adoption/upgrade maintenance too; an in-flight rotation must not revive an old
value during handoff. External/uncoordinated source changes require an explicit
ownership boundary rather than a claim of cross-resource atomicity.

Complete uncertain-write reconciliation, recovery UI and guarded supported
upgrade/rollback behavior before advertising protocol one. Preserve identities
and values in real empty GCP dev testing, then exercise managed operations and
browser/stream revocation. Production activation remains a separate approved
operation. Do not silently adopt an existing unrelated Secret or remove the
marker/registry to restore access.

## Review evidence

Tests cover exact key/identity/grant/cookie preservation, final/root duplicate
precedence, no borrowed tenant identity, ambiguous-ID refusal, unavailable or
already-selected authority and private serialization/formatting. Cutoff storage
and transition tests reject an unbounded or extended compatibility window;
reader tests reject late/overlong old-format cookies while accepting current
per-key sessions. Installation rotation retires the old shared signer.

Fencing tests deliberately delay the original update until after the fence and
exercise lost acknowledgements in both directions. They verify that late writes
lose, committed operation references survive and missing state cannot bootstrap.
Real API resource-version verification is required before merging the slice.
