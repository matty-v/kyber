# Durable launch and effect checkpoints for credential adoption

The existing credential operation journal admits the complete adoption operation
BEFORE creating its native Job. The immutable admission pins the original actor,
operation/time, completion event, root source name, complete sorted source
name/UID/version graph, deployment name/UID, published control-plane image and
release name. ExpectedVersion is the root source version, not a future lease.
RegistryUID and MaintenanceUID stay empty because neither object exists yet.
No key, key hash, private signer or candidate is stored in this metadata.

A first acknowledged admission owns initial Job creation. An identical replay
returns the original operation and cannot create another initial job. Pending
launches remain discoverable even if the response is lost or the Job has not
started. Reuse the existing JSONB row and retention semantics; no new table.

After the worker holds maintenance, its first checkpoint records the actual
maintenance UID and original reservation version. These remain unchanged across
all later checkpoints and replacements. Then advance through candidate-create
admitted, registry UID observed, authority-selection admitted and authority UID
observed. Each transition is serialized with finalization under the database
lock. UIDs cannot change, steps cannot be skipped and identical retries are
idempotent. The native worker must retain the original reservation version when
fencing a stopped writer, so a late original checkpoint matches the same intent.

Before candidate creation is admitted, failure may finalize against the exact
nil/reserved checkpoint. That terminal row fences any late worker from admitting
credential effects, including a Job whose create response was lost. After a
candidate create is admitted, absence cannot prove failure: a delayed Kubernetes
create could still commit. Only successful reconciliation can finalize success,
with the exact selected checkpoint. Generic key and maintenance finalizers must
not consume adoption rows. Unknown operations do not expire based on age.

The native API must require current administrator access before admission and
before returning protected outcomes. Replacement jobs require a separately
recorded current-administrator recovery authorization and proven old-writer
shutdown. Their payload is built from the original journal, not resubmitted
source definitions or a new time. The journal establishes ordering, not Kubernetes
outcomes: the worker still verifies ownership, frozen sources/replicas, exact
maintenance and UID-pinned authority, and reconciles fixed-name creates.

This storage contract does not activate authority, launch a worker or add an
ownership control by itself. Runtime protocol stays zero until the complete
native flow passes acceptance.
