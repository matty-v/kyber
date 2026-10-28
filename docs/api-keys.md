# Kyber Platform API Keys

Use **Settings → API access** to inspect operator keys, their effective access,
management ownership, observed use and recent sensitive activity. On a compatible
release, **Manage existing keys in Kyber** explicitly transfers ownership of
existing keys into the managed registry. Upgrading alone does not perform setup.
See the [operator credential guide](operator/operator-credentials.md) for setup,
recovery, permissions and reporting coverage.

An **Administrator** key grants full operator access. A **Restricted** key grants
only supported actions on its allowed resources. These boundaries are always
enforced; naming a key “metrics” or leaving scopes empty does not grant access.
A shared key identifies its credential, not the individual person using it.

Provider API keys, Agent user secrets, channel tokens and per-pod credentials are
separate surfaces. Changing an operator key does not automatically rotate them.

## Ownership and storage

Before setup, installation configuration supplies the administrator key and
optional `callers`. By default these are in the `kyber-api-credentials` Secret;
`api.existingSecret` can select a different source. The control plane resolves
configuration and caller references at startup. Editing a source requires a
reload/restart and verification of every replica.

After explicit setup, the selected registry is the sole operator-key authority.
Existing accepted values, complete identities and grants are retained; missing
identity labels receive stable operation-scoped IDs. Source Secrets remain
intact, but editing them no longer changes accepted operator access. Referenced
client Secrets are not automatically synchronized when a managed key rotates:
save the replacement value and update the consuming client as part of rotation.

The installation-owned `kyber-operator-credentials` registry and
`kyber-operator-credential-authority` marker are separate from Helm's configured
source Secret. Do not delete or recreate them to return to configuration mode.
The current authority is read directly without a last-good authentication cache;
a read outage returns unavailable instead of restoring retired configuration keys.

## Create, edit, rotate and revoke

For a managed installation, use Settings to:

1. Create a named Administrator or Restricted key and choose supported grants
   and exact Agent/Machine targets. The server validates the policy.
2. Save the prepared secret value before activation. Existing values cannot be
   revealed later; losing a response does not make the value retrievable.
3. Edit grants or rotate an individual key. Rotation affects that credential's
   material and generation, not an unrelated administrator key.
4. Revoke a key with the affected credential identified. Revoked material is
   removed while identity/history metadata remains available.

Kyber protects the last active administrator that has no expiry. Keep a separate
backup administrator credential in a password manager. Named credentials and
optional expiry do not introduce human accounts, SSO or a per-browser session
inventory.

Management clients use the inventory version in `If-Match` and a canonical
UUID operation ID at `POST /api/v1/operator-credentials/mutations`. New material
must be canonical unpadded base64url representing 32 random bytes; the UI prepares
it securely. Imported installation values keep their original encoding.

A version conflict requires a fresh inventory review. An uncertain write requires
checking the **original operation**, not submitting a replacement key or repeating
a POST blindly. The operation and pending-operation APIs distinguish confirmed
success, failure and pending effects. Required audit storage must acknowledge
admission before a mutation starts.

The former `POST /api/v1/rotate-api-key` workflow is disabled when managed authority
is enabled, including before setup. Use API access rather than editing the
installation Secret as a substitute for a managed rotation. Older releases may
still offer their former configuration rotation endpoint.

## Browser sessions and live access

The embedded console exchanges a pasted key at `POST /api/v1/browser-session`
for an HttpOnly same-site cookie. It does not retain the raw key in readable
browser storage. Sessions are signed tokens, not individually tracked server-side
browser records. They last up to 30 days and renew on use while current access
permits renewal.

Managed sessions bind the current credential identity and generation. Revoking,
rotating or narrowing a managed key affects bearer requests, browser sessions and
already-open protected streams within the documented propagation bound of ten
seconds. Current grants apply even when the cookie has not expired. There is no
way to revoke one of two browsers sharing the same credential independently.

Compatible existing sessions survive setup within their original recorded
cutoff/expiry window. Setup does not extend their original lifetime; the review
reports callers that need to sign in again. Compatible state survives control-
plane replacement. Old browser-readable key storage is consumed once and removed.

## Bootstrap configuration

At installation, generate a strong administrator key:

```bash
KYBER_API_KEY=$(openssl rand -hex 32)
```

Store it securely and provide it through the installation's Secret workflow.
Never commit values or paste them into shared logs, issue bodies or shell history.
See [installation](installation.md) and [WSL2 installation](installation-wsl2.md).

Before managed setup, callers can be configured with stable identity and targets:

```json
[{
  "name": "agent-automation",
  "principalId": "principal_agent_automation",
  "tenantId": "tenant_automation",
  "credentialId": "credential_agent_automation",
  "credentialGeneration": 1,
  "agentResources": ["kyber-system/researcher"],
  "keyFrom": {"secret": "agent-automation-key", "key": "api-key"},
  "scopes": ["lifecycle:write"]
}]
```

Use exactly one of inline `key` and `keyFrom`. References resolve in the control
plane's namespace only, are loaded at startup, and trim surrounding whitespace.
Malformed caller documents reject scoped configuration; an unresolved reference
rejects the affected caller. Managed setup refuses incomplete source resolution
rather than silently dropping an accepted credential.

Scope and resource checks remain mandatory. Lifecycle, terminal, secret/log,
task, capability and usage permissions are distinct; Agent and Machine terminal
targets use separate lists. Empty permissions grant no protected operator action,
although accepted keys can read basic version and their own current access
metadata. The old `api.authz.enforce` setting cannot disable these boundaries.
See the [permission guide](operator/operator-credentials.md).

## Lost access and compromise

If a browser is still signed in as an administrator, create and save a replacement
administrator key before retiring compromised access. If all known keys and
sessions are lost, recovery requires the installation owner's privileged
Kubernetes access to the **selected** credential store; there is no public API
that reveals existing key values or bypasses authentication. Handle recovered
material privately, then rotate it and store a backup through Settings.

Before managed setup, the installation owner can repair the configured source
Secret and restart all control planes. After setup, changing that source cannot
restore administrator access. Never delete authority/registry Secrets or restore
only old configuration to bypass managed state. Use a consistent protected backup
and a compatible release for installation recovery.

For suspected compromise:

- Revoke the affected integration or rotate the affected key promptly. Preserve
  an administrator recovery route while replacing an administrator credential.
- Review **Recent access** for attributable decisions/outcomes and **Observed key
  activity** for partial usage coverage. HTTP logs do not replace durable audit.
- Assess other credentials the compromised actor could read through its allowed
  terminals, user-secret or administration access. Rotate those separately where
  exposure is possible; operator-key rotation does not rotate provider/channel
  credentials automatically.

Full administrator access is a powerful trust boundary. It does not protect
against cluster/node compromise, an already-compromised privileged client, or
keys committed to an external repository. Scoped keys reduce access; they do not
identify individual humans sharing one value.

## Audit, usage and upgrade limits

Sensitive operations require a durable authorization record before content or
external effects. Required-store failure blocks those operations; ordinary
current authentication, sign-in and administrator inventory do not depend on
that database. Completion may remain unconfirmed after an effect, which is a
reason to inspect its operation, not replay it automatically.

History reports credential/principal, action, exact resource, request/operation
IDs, decision/outcome and time. It excludes key values, cookies, request bodies,
terminal arguments and transcript text. Usage has explicit windows, freshness,
coverage and gaps; no recorded use does not mean never used. Neither view invents
a human identity or promises a complete per-browser session list.

Supported upgrades and rollback verify compatible effective releases. Once
managed authority is selected, a pre-protocol image or custom image override is
not a supported way to downgrade. Restoring old configuration must not resurrect
a revoked credential. See [upgrading](upgrading.md) and the
[operator guide](operator/operator-credentials.md#credential-maintenance).

Automatic rotation policies, human SSO and external secret-manager synchronization
remain separate capabilities. Runtime/provider credentials remain documented in
[Agent identity repositories](agents-identity-repos.md).
