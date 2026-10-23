# Operator credential store foundation

Status: implementing the persistence foundation for MAT-134. Authentication,
adoption and management activation remain separate reviewed gates in the
[unified credential design PR](https://github.com/matty-v/kyber/pull/356).

The operator outcome is one Settings experience for accepted keys, their grants,
creation, individual rotation and revocation. This slice gives that work a
durable store without activating a second authentication path.

## Persistence contract

Use one Kyber-owned Kubernetes Secret in the installation namespace. The public
factory creates a direct client with a static Secret mapping, preventing cached
readers or discovery from bypassing the two-second bound. There is no last-good-state
fallback on read failure. Ordinary authentication must not acquire a PostgreSQL
dependency from this store; required security auditing has its own contract.

The document contains a schema version, namespace, credential records and a
bounded operation outbox. Unknown schemas, malformed documents and unexpected
Secret ownership fail closed. Existing unrelated Secrets are never adopted or
overwritten implicitly. Resource versions are opaque compare-and-swap tokens.
The backing Secret has no garbage-collection owner reference, and both its
registry marker and Kyber management label must remain present.
Conflicts return for review/retry, without replaying a mutation automatically.

Credential records preserve immutable credential, principal and tenant IDs.
Existing identity kind, issuer and subject are preserved as immutable provenance.
Role is explicit, separate from display name. Material generation is separate
from permission revision. A grant/name edit cannot change task ownership.
Imported creation time stays unknown. References to external material are
installation-local metadata; later adoption/resolution owns their authority.
The storage layer validates bounded data and state transitions, while the API's
single permission compiler must validate supported grants before activation.

Authentication material is private to the record implementation, returned only
through an explicitly named accessor that copies bytes. Public record/document
JSON serialization fails, and diagnostic formatting redacts contents. A private
storage codec writes material only into Secret data. API responses must use
explicit safe projections, including hiding source bindings and outbox internals.

## Mutation invariants

- IDs and recorded creation time remain immutable after insertion.
- Revoked records remain as tombstones and cannot be removed or reenabled.
- Rotation advances material generation; grant/source/state changes advance
  permission revision. Cosmetic name edits do not advance either counter.
- A durable, non-expiring administrator with registry-owned material remains.
  Two administrators revoking each other from the same version cannot both win.
- Credential changes carry a new pending operation reference. Pending operations
  cannot disappear or have actor/action metadata rewritten. Delivered references
  may be pruned only after the audit drainer has acknowledged them.
- Document and entry-count limits stop writes before capacity is exceeded;
  tombstones and pending audit references are never dropped to make room.

Outbox records contain typed operation/actor/request/target/time metadata and
event IDs, never key values or arbitrary payloads. The future mutation handlers
must persist required audit admission before effects and commit the operation
reference with credential state. The drainer appends completion idempotently.
An uncertain write is reconciled by operation identity, never by replaying key
issuance or revealing an existing value.

## Review and verification

Review the codec for material/binding leaks, ownership checks for unintended
Secret replacement, and compare-and-swap transitions for stale writes and
tombstone loss. Verify cancellation and unavailable-authority errors without
exposing Kubernetes response details.

Tests cover codec round trips and diagnostic redaction, two independent store
readers, stale writes, administrator protection, immutable identities and
generation/revision rules, operation retention, capacity and unsupported schema.
Use disposable, inert Secrets in the verified GCP dev namespace for real API
resource-version checks. Remove only those fixtures; never write production
credentials or activate the registry during foundation verification.

Before runtime activation, additionally complete configuration ownership/adoption,
stable-identity compatibility, target-image/automatic-rollback compatibility,
current bearer/cookie resolution, bounded stream revocation, audit reconciliation
and the administrator management UI. Persistence tests alone do not establish
those guarantees.

## Foundation review decisions

The public factory builds its own direct client and static Secret mapper instead
of accepting a possibly cached reader. API configuration is copied, preserving
stricter timeouts. Namespace/name and ownership are checked on every load.

Public transport methods reject record/document serialization; all diagnostic
format verbs redact data. Wrapped cancellation errors preserve only safe context
sentinels, rather than propagating response text. The private storage codec
rejects duplicate/case-aliased keys, unknown fields and trailing data, while
preserving binary material and full-width generations.

The store retains immutable issuer/subject/kind and imported creation-time
uncertainty. Actor/target references must correspond to retained records. New
operation references require an active administrator record at the previous
generation; this consistency check does not replace request authentication.

Before the mutation/audit mapper is connected, extend the audit model's current
configuration-only authority qualifier and strict credential-target identifier
validation to support registry authority and existing opaque credential IDs.
Do not enable handlers that cannot write a valid, attributable audit admission.


## Durable authority selection primitive

`AuthorityStore` adds an inert adoption boundary; it is not connected to HTTP
handlers or authentication yet. `Bind` creates a separate immutable, Kyber-owned
Secret with protocol version 1 and the candidate registry's name and Kubernetes
UID. It checks the reviewed candidate's resource version before creating the
marker. It never updates or implicitly replaces a marker. Both Secrets have no
workload owner reference and must remain outside Helm-rendered resources.

`Load` uses direct reads for the marker and current registry with one shared
two-second deadline. Only an authoritative missing marker before this resolver
has observed adoption returns `ErrUninitialized`. An unavailable API never means
bootstrap. After adoption, missing, corrupt or recreated registry state cannot
fall back to configuration keys. UID binding survives a process restart and
rejects a replacement registry even if its name and document match. A process
that has observed a marker also rejects its later disappearance. An uncertain
marker-create response suppresses bootstrap in that process; reconcile with a
read rather than automatically repeating the write.

This is a persistence primitive, **not the complete rollback guarantee**.
Activation must remain unavailable until source ownership, audit admission,
permission/cookie/stream compilation and the upgrade/rollback compatibility
floor are implemented and verified. Existing old binaries do not understand the
marker. Supported downgrade paths must reject those binaries after adoption;
merely adding this marker does not make them safe. Privileged external deletion
of the marker on a fresh process or restoration of old backups is also outside
this primitive's guarantee. Recovery must preserve the marker and registry
identity, rather than instructing an operator to delete them to restore access.

The opt-in GCP dev test uses random probe names for both Secrets. It verifies
real API immutability, independent replica reads and missing/recreated registry
rejection, then checks UID/ownership before deleting only its own fixtures.
Live installation credential Secret UID/resourceVersion and zero-Agent state
are checked before and after the probe.
