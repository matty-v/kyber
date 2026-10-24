# Inspect operator credentials

As an installation administrator, open **Settings → API access → Operator
credentials**. The read-only inventory shows the installation administrator
and integration credentials currently accepted by the responding control
plane, plus additional entries in its configuration when that source can be
read. It never returns existing key values or fragments.

Each entry shows its access level, accepted permissions and allowed Agents.
Fleet metrics access is installation-wide, not restricted by an Agent list.
Empty integration permissions grant no additional operator operations; every
valid credential can still read basic API version information. Reserved task
administration permissions currently grant no additional access.

`Active` describes accepted runtime access. `Configured, not active` does not
mean the credential has ever worked: its configuration may be rejected or not
loaded yet. Removed configuration may still be active until the control plane
reloads it. A configured permission difference is shown separately from active
access. This comparison does not establish whether a pending key-value change
has loaded; externally replaced keys may require a restart. If the configuration
source cannot be read, a warning explicitly limits the inventory to runtime
access. Multi-replica consistency and UI-managed credential lifecycle are part
of the subsequent registry work, not guaranteed by this per-control-plane view.

If configured entries share the same key value, the inventory shows the
identity and permissions authentication actually selects, including full
administrator access when an integration contains the installation key.
Separate names do not create separate access for identical key values.
Duplicate caller names cannot start browser sessions; existing sessions with
ambiguous identities are rejected. Give integrations unique names and keys.

Creation times and general per-key usage summaries are not collected by this
inventory. Recent recorded access appears below it in Settings, with explicit
coverage limits. Unknown/unavailable usage does not imply a credential was never
used. The remaining creation/rotation/revocation and usage
work is tracked in MAT-134; the authorization/registry model is MAT-96.

The supported system identity is `installation-admin`, displayed as
**Installation administrator**. Existing administrator keys remain valid and
existing signed browser sessions resolve to the new identity. The installation
principal remains stable, preserving task and archive ownership. Old signed
administrator task-event cursors are recognized only after signature and
resource/identity checks; new cursors use the new credential ID. An already
open usage pagination snapshot may require a refresh after upgrade because
its owner was bound to the previous credential identifier. Stored usage data
is not changed or lost.

Restricted credentials receive 403 for the inventory, before configuration
reads. The UI clears loaded inventory when a refresh is denied; it does not
put this metadata in the shared query cache. Key rotation controls appear only
after administrator inventory access is verified. Older installations show an
explicit unsupported state until upgraded.


## Terminal permissions

Terminal access is explicit. `agents:exec` permits terminal modes and commands
only on exact `agentResources`; `machines:exec` uses a separate
`machineResources` list. An Agent grant does not grant a Machine terminal,
and a Machine grant does not grant the Agent terminal API. Lifecycle and task
permissions do not imply either. The installation administrator retains both.

Machine terminals control the host and may expose other Agents on that host.
Only give this permission to a trusted host administrator. Agent terminals may
read the Agent's own files and credentials; they are not a read-only log view.
The same check protects shell, direct commands, attach, history and device-auth
modes before Kubernetes lookup and WebSocket upgrade. Generic terminal-preview
routes remain administrator-only.

Configured terminal integrations require stable principal, tenant, credential
ID and generation plus exact namespace/name targets. Wildcards are rejected.
For example, add `scopes: ["machines:exec"]` and
`machineResources: ["kyber-system/worker-1"]` to an explicitly named integration
with those identity fields and a `keyFrom` reference. The inventory shows this
as host terminal access and lists the Machines separately from Agents.

Changes to externally configured credentials still require reload/restart as
described above. Bounded revocation of active streams and durable security
audit records remain part of MAT-96/MAT-97; current decision events are sanitized
control-plane logs, not a durable audit trail.

Terminal completion reports exit before closing the client connection. Client
disconnect cancels the exec context and closes an upstream connection waiting
for upgrade headers. The transport retains Kubernetes TLS verification,
authentication and configured proxy selection; exec redirects are rejected,
and failed upgrades do not copy upstream response bodies into terminal errors.


## Sensitive-data permissions

| Permission | Allowed action | Resource list |
| --- | --- | --- |
| `secrets:list` | List user-secret names, sizes and recorded timestamps | `agentResources` |
| `secrets:read` | Read user-secret values, including files | `agentResources` |
| `secrets:write` | Create, replace or delete user secrets; existing restart behavior applies | `agentResources` |
| `logs:read` | Read live and archived operational logs | `agentResources` |
| `transcripts:read` | Read archived conversation transcripts | `agentResources` |
| `machines:logs:read` | Read host/node-agent operational logs | `machineResources` |

These permissions are independent. Value read does not imply listing or
mutation; mutation does not imply value read. Operational log permission does
not grant `source=transcript`, and transcript permission does not grant other
log sources. Unknown source aliases are not delegated to restricted callers.
The generic fleet logging/export routes remain administrator-only.

All grants require stable credential identity and exact resources, matching
the terminal configuration rules. Authorization runs before Kubernetes or
archive reads, regardless of the lifecycle rollout flag. Sensitive responses
use `Cache-Control: no-store`. Browser mutations retain same-origin CSRF
protection. Metadata-only secret lists omit value fingerprints because a digest
could verify guesses of low-entropy values; administrators and callers with
both list and value-read access retain the existing fingerprint view.

Terminal and disk-archive access can expose local credentials independently of
the user-secret API permissions. Host log access can expose activity from other
Agents on that host. Operational logs can contain sensitive application output;
a log grant is not a content-redaction boundary. Grant these only to trusted integrations. Decision logs
contain actor, action, exact Agent/Machine and outcome, never values or log
content. The required durable records below add a bounded first collection
slice; complete security history remains MAT-97.

## Required records for sensitive access

The operator terminal, `/agents/{name}/secrets` and Agent/Machine log or
transcript endpoints, including fleet log viewer/export aliases, require a durable authorization record before any backend
lookup, content read or terminal upgrade. The control plane uses its existing
PostgreSQL connection. Without that store, or when a required write fails,
these operations return `503 security_audit_unavailable` and do not start. The
same admission rule covers Agent/Machine lifecycle and deletion requests, model/
runtime/resource/disk changes, session maintenance, disk export/import/upload/
delete/cancel operations, download-link issuance and public archive downloads.
Installation-key rotation also requires a record before changing the Secret.
Sign-in, version information and the administrator credential inventory remain
available; an invalid permission still returns 403 without reading the backend.

Records contain credential/principal identifiers, action, bounded HTTP method,
exact Agent/Machine, request/operation IDs and timestamps. They do not include
credential values, user-secret names/values, request bodies, terminal arguments,
log text or transcripts. Denied unclassified routes record an installation-level
decision without copying unknown paths. Older configured callers without stable
IDs can have unknown attribution on denial; Kyber does not invent a person or
credential identity for them. Keys missing identification needed for an allowed
management action receive `403 credential_identity_required`; the operation does
not start and an administrator must update their configuration.

An allowed authorization and a completed operation are different records.
Failed lookups/validation are failed operations, not successful sensitive reads.
Terminal completion includes transport/command failure rather than treating a
WebSocket handshake as success. Client disconnects do not cancel the bounded
attempt to write completion history. A completion write can fail after content
or an external effect has already been delivered; its earlier admission remains
without a confirmed outcome. Treat that as incomplete history, not a rollback
or a reason to replay a mutation automatically. Failed/interrupted requests
can have partial effects or data delivery. A confirmed mutation
remains successful even when its response is lost; an unconfirmed delivery is
unknown rather than falsely reported as completed or rolled back.

This collection covers the operations above and restricted-route denials.
A successful asynchronous request records accepted intent, not completion of the
background Agent/controller/archive job. A verified but revoked download link
can identify its issuing credential for a denial; that reference never grants
access. Invalid/unverifiable links remain unattributed, and an unavailable
authority is not recorded as a permission denial.

The administrator can query recent history in Settings. Canonical credential
CRUD/adoption reconciliation and general authentication/usage summaries remain
MAT-97/MAT-134 work. The read-only inventory still does not claim general last
use or complete usage coverage. Bounded credential revocation is separate from
this audit collection and is not provided by an authorization record alone.

Retention runs in bounded batches each minute. By default it retains up to
30 days and 1,000,000 records per installation; either age or capacity can
shorten the window. Helm settings `api.securityAudit.retentionDays` (1–365)
and `api.securityAudit.maxRecords` (1,000–100,000,000) control these limits.
Invalid settings fail startup. The row cap is periodic, so a burst can exceed
it temporarily between sweeps. Size PostgreSQL for the selected policy and
its other data. Subsequent history/usage views must show actual retained
coverage, not promise a full time window when the capacity limit has trimmed it.

Current records label credential authority as `configuration`: IDs/generation
are those reported by the responding authenticator, not an independent
issuance or rotation ledger. In particular, the current shared installation
key does not track material generations. The canonical registry must own and
validate those versions before the later UI can claim per-key version history.

Fleet logging remains administrator-only. Its initial installation-level
admission covers target discovery; a second decision records the backend-matched
pod UID, container, component and workload before content reads/exports. Both
share one operation ID and completion; consumers must count operations rather
than treating each decision as a separate read. Unknown query selectors are not
copied into events. Fleet log responses also use no-store.
