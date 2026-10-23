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

Creation times and usage history are not collected by this first inventory
release. The UI reports them as unknown/unavailable rather than implying a
credential was never used. The remaining creation/rotation/revocation and usage
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
