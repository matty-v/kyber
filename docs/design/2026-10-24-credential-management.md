# Managed operator key changes and recovery

> Historical implementation plan. The integrated operator-key feature shipped in
> [PR #396](https://github.com/matty-v/kyber/pull/396). Staged or proposed status
> statements below describe development at the time, not current availability.
> See the [current operator guide](../operator/operator-credentials.md) for supported
> setup, management, recovery and reporting limits.

Status: management API/client implementation for MAT-97 and MAT-134. Runtime
protocol remains zero and `cmd/control-plane` has not selected the registry.
Production authority adoption is not part of this change. Complete adoption,
maintenance recovery, administrator forms and usage visibility before activation.

## Operator behavior

An administrator can create a named Administrator or Restricted key, edit its
name/grants/expiry, rotate its value, or revoke it. Restricted grants use supported
permissions and exact installation-local targets; reserved/unknown permissions
and wildcard targets are rejected. Administrator grants cannot pretend to have
resource restrictions. Existing principal/tenant IDs and provenance never change,
so edits/rotation do not transfer task ownership. Revoked records remain tombstones.
The complete candidate validator protects the last durable administrator.

New values are prepared and saved by the client before activation. Neither change
responses, operation lookups nor recovery return a stored key. Request material
is private, bounded, redacted from diagnostics and never serialized outward.
Creation time is shown only when known; adoption does not invent issuance dates.

The registry inventory returns an opaque reviewed version bound to the registry
UID as well as its opaque resource version. Forms must also stay bound to their
connection context. Every change supplies
that exact value in `If-Match`; a conflict requires refresh and another review,
not a transparent retry with current permissions. The server reauthorizes the
current administrator after admission and pins the selected Secret UID/version
at the write. The typed client performs one POST and exposes operation lookup.

## Durable ordering and unknown outcomes

Before effects, admission and the original UID/version commit atomically in
PostgreSQL. Only the acknowledged first admission may start a new write. A repeated
POST never asserts its new body/material was accepted. A lost admission response
returns 503 with an explicit instruction to inspect the existing operation.

Credential state and the pending outbox reference commit in one Kubernetes CAS.
Confirmed effects retain their outcome if the client disconnects or self-rotates.
Completion uses the intent's stable event ID. A completion outage or uncertain
write stays pending; HTTP acceptance is not a claim that a key was created.
Ordinary request authentication has no PostgreSQL dependency.

`GET /api/v1/operator-credential-operations` is read-only. Explicit recovery uses
`POST /api/v1/operator-credential-operations/reconcile`, checks current admin
access and commits a separate recovery audit admission before effects. Its
result still describes the original key change. Resolving a failed key change
can therefore be a successful recovery operation in security history.

Recovery trusts a matching operation reference only in the pinned current
registry incarnation. A changed opaque version without that reference proves
that the original CAS cannot commit. At the original unchanged version, recovery
fences metadata, then rereads authority. Lost fence acknowledgements/conflicts
are reconciled; they never trigger issuance. Missing/replaced/unsupported state
or an unavailable completion leaves the operation pending. No final failure is
written while the older update can still commit. Merely polling must not cancel
an active request.

## Verification and remaining gates

Test preservation of identity/grants, separate material/policy counters, cosmetic
renames, last-admin protection, strict bounded input, required admission before
writes, stale/concurrent policy refusal, response privacy, no client retry and
canonical operation results. Recovery tests cover committed writes, uncommitted
attempts, lost fence responses and authority outage after fencing.

Use disposable registry/authority Secrets and an isolated PostgreSQL schema in
verified empty GCP dev for actual CRUD, durable audit and interrupted-write
recovery. Check original installation credential metadata, zero Agents and cleanup.
No production values or authority marker are changed.

The UI forms, saved-value/reconnect behavior, activation preflight, bootstrap
rotation serialization, maintenance recovery, outbox delivery/compaction and
usage collection remain subsequent integration. Do not advertise protocol one
or claim those end-to-end gates from these API tests alone.

### Admin forms (staged)

The registry inventory identifies the current key and supplies a reviewed version.
Permission choices come from the server's versioned options endpoint, including
whether each action requires Agent or Machine targets. New values are generated
with browser cryptographic randomness, held in the active form only, and saved
before activation. They never enter the shared query cache or browser storage.
Forms preserve their opening review version while inventory polling continues.

Create, edit, rotate and revoke call the SDK once. Interrupted requests expose
read-only status checks and explicit reconciliation, without issuing a fresh
mutation. The pending endpoint exposes a bounded live scan for recovery after a
form closes. A same-key browser rotation refreshes the signed session for the
same principal and tenant; bearer connections require updating their configured
value. Registry management hides the earlier configuration rotation control.
These controls remain capability-gated while runtime authority activation is staged.
