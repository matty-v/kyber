# Durable admission and effect checkpoints for credential adoption

Extend the existing credential operation journal with a distinct adoption kind;
do not put maintenance identities in registry UID fields. Admission pins the
original actor, operation UUID/time, completion event, maintenance UID/version,
root source name and the complete sorted source name/UID/version graph. No key,
key hash, serialized candidate or private signer belongs in this metadata.
The root document's pinned version also pins caller ordering/reference keys.

A first acknowledged admission owns the attempt. An identical retry reads the
original immutable admission, never obtains a new effect permit. Old unresolved
adoptions remain discoverable separately from key mutation and maintenance
recovery work. Reuse the existing JSONB row and retention semantics.

Effects advance through candidate-create admitted, registry UID observed,
authority-selection admitted, and authority UID observed. Each transition is
serialized with finalization under the existing database lock. Original UIDs
cannot change; identical checkpoints are idempotent. Missing objects after an
admitted create are not proof of failure: a delayed request could still commit.
Only a checkpoint-free operation can finalize failure. Success requires the
selected checkpoint and exact expected checkpoint comparison. Generic key or
maintenance finalizers must not finalize adoption rows.

The journal proves metadata ordering, not Kubernetes outcomes. The native worker
must verify ownership/freshness/maintenance, perform only acknowledged admitted
effects, reconcile fixed-name creates and UID-pinned authority, and provide the
observed checkpoint to finalization. No automatic retry, activation, UI endpoint
or protocol-one claim is introduced by this storage contract alone.
