# Operator credentials and access visibility

Status: proposed implementation design, following Matt's 2026-10-23 product direction.
Related: MAT-94, MAT-95, MAT-96, MAT-97.

## Product outcome

An installation administrator can open Settings → API access and see every
operator credential the installation accepts, who or what it represents, and
its effective access. The installation administrator is a supported identity,
never a caller named `legacy`. One credential model covers administrator and
restricted integration access. Existing administrator access survives upgrade.

The single-operator experience remains simple: connect once, operate the
installation, and inspect credentials when needed. Integration credentials are
an explicit administrator choice. Their advertised restrictions must hold
across the entire public API before Kyber offers them as limited access.

## Current evidence

- `pkg/api/auth.go` authenticates an installation key and optional `callers`
  entries. The installation key receives the hard-coded name `legacy` and full
  access; named entries receive scopes. These use the same authenticator.
- Browser cookies refer back to a caller. They are not a separate operator
  account system. Task event cursors also bind credential identity and key.
- `handleAgentExec`, `handleMachineExec`, secret handlers, and log handlers lack
  action/resource checks. `handleRotateAPIKey` also lacks an administrator
  check. Adding an inventory must not expose that rotation action to a
  restricted caller or imply that existing scope declarations are sufficient.
- Lifecycle checks can be observe-only; task, archive, and usage checks have
  stricter semantics. A global feature-capability list describes installation
  features, not the authenticated caller's permissions.
- Settings currently offers connection and shared-key rotation, without an
  accepted-credential inventory or per-key usage view.
- On 2026-10-23, Datawire had no named operator callers; dev loaded two demo
  callers. No demo-caller use was observed in the examined logs. This is not an
  issuance history or proof of non-use.
- The read-only Falcon audit on 2026-10-23 confirmed the running control plane
  loads `lando-metrics` (empty declared scopes) and `jack-bridge` (task actions
  on `kyber-system/jack`). A read-only task metadata query found 26 tasks owned
  by `principal_jack_bridge`, created 2026-10-15 through 2026-10-18; latest
  creation was 2026-10-18 19:01:41 UTC. Recent request logs do not attribute
  general requests to credentials, so `lando-metrics` usage and later task
  reads/polling could not be established. No live keys were changed or used to
  exercise privileged endpoints. Empty scopes must not be shown as safe
  metrics-only access while ungated public operator routes exist.

## Credential model

Each operator credential has an immutable ID, a display name, a principal,
an explicit access level (`administrator` or `restricted`), an enabled state,
and its authentication material. Restricted records declare actions and exact
Agent/Machine resources. Names never determine access. Duplicate credential
IDs, names used for session lookup, or key material must be rejected rather
than giving cookies and bearer requests different identities.

The existing installation key becomes a system-provisioned credential named
`Installation administrator`. It appears in the same inventory as integration
credentials and passes through the same identity/permission resolver. Its
access is shown as `Administrator · All Agents and Machines`, not an empty
permissions list or an unexplained wildcard. An administrator role explicitly
permits management operations; it is not inferred from a name or old identity.

Browser sessions inherit a credential's current state and grants. An Agent's
internal pod identity, provider keys, GitHub credentials, and channel tokens
remain separate credential types; do not mix them into the operator-key list.

## Administrator UI

Settings → API access contains a credential inventory with:

- Name, access level, allowed actions, and allowed Agent/Machine resources.
- State: accepted, disabled/revoked, or configured but rejected, with a safe
  explanation. Configured records must not be presented as accepted merely
  because their Kubernetes Secret exists.
- Source: installation-provisioned or integration credential. Configuration
  ownership is explicit so a UI action cannot be silently undone by Helm or
  GitOps on the next deployment.
- Created/expiry information only when actually recorded. Imported records
  show `Creation time unknown`; Secret creation time is not issuance time.
- `Last observed use` when collected, otherwise `No recorded use`. This must
  never be labeled `Never used`. Requests sharing one credential cannot be
  attributed to individual humans or applications.

Values and key fragments are not returned by inventory endpoints. New managed
keys are displayed once at creation, with `Copy` and an explicit explanation
that they cannot be revealed later. Existing values cannot be revealed.
Rotation and revocation name the affected credential and explain which clients
will lose access. They must not unexpectedly rotate the installation key.
The last usable administrator cannot be revoked accidentally; preserve an
explicit installation recovery procedure.

Inventory, creation, changes, rotation, and revocation are administrator-only.
Restricted sessions cannot read other credential metadata. Use plain product
labels instead of exposing raw scope strings as the only explanation of access.

## Permission enforcement and UI behavior

Build a route matrix from `registerProtectedRoutes`, nested action dispatchers,
WebSocket routes, download-token routes, and A2A routes. Every public operator
route must be explicitly administrator-only or declare an allowed restricted
action/resource. Unclassified routes deny restricted callers. Preserve the
existing task ownership, tenant, and archive checks; a broad scope must not
erase those restrictions.

The initial route policy is deliberately bounded. Restricted human fleet
administration, SSO, and mutually untrusted tenants are not new product goals.

| Surface | Restricted caller policy |
| --- | --- |
| Health, static PWA, public cluster-info, signed webhooks | Preserve their existing public or independently authenticated boundary. |
| Browser-session exchange | Authenticate the presented credential; the session never adds access. |
| Self identity/effective permissions | Authenticated caller's own safe projection only. |
| Credential inventory/creation/rotation/revocation, including the existing shared-key rotation endpoint | Administrator only. |
| Agent/Machine list, detail, creation, arbitrary PATCH, deletion, fleet, events, metrics, config/export, updates, GitHub repository access, fleet defaults, logging settings, inference credentials, development simulation | Administrator only initially; no implicit inheritance from task or lifecycle scope. |
| Agent lifecycle actions and bounded model/runtime/resource changes | Explicit existing lifecycle grant plus exact Agent resource; no observe-only authorization for restricted credentials. Authentication/credential-changing actions require administrator access initially. |
| Agent/Machine exec | Explicit exec action and exact Agent/Machine resource; includes direct commands, Shell, attach/history, device-auth terminal, and preview transport. |
| User secrets | Exact Agent plus separate metadata, read-value, or mutation action. |
| Agent/Machine operational logs and generic logging/export aliases | Explicit log-read action and exact resource; fleet-wide exports remain administrator only. |
| Agent transcripts, including log source aliases | Explicit transcript-read action and exact Agent, not merely log read. |
| Tasks, results, events, requests, A2A and capability discovery | Preserve the existing exact action, principal ownership, tenant, credential-generation and allowed-Agent checks. Audit every protocol alias against the same policy. |
| Usage events/aggregates and Agent token usage | Explicit usage-read and exact allowed Agents; reuse existing usage policy instead of opening an ungated alternate route. |
| Agent exports/imports, archive records/uploads/download tokens | Existing archive grant and resource checks; reauthorize on each asynchronous or token-mediated effect. |
| Jobs, comms, inbound bindings, skills/models/private Agent metadata and remaining Agent/Machine sub-actions | Administrator only until a specific restricted action is designed and tested. |

The implementation matrix must enumerate actual method/path/action cases under
each row, including negative unknown paths, so adding a dispatcher action cannot
silently add restricted access. Route coverage is a required security test.

MAT-94 covers terminal execution on exact Agent/Machine resources before
backend lookup or WebSocket upgrade. A visual `read-only` terminal is not an
authorization boundary: Terminal peek must either use a genuinely read-only
server transport with its own permission or require terminal-execution access.

MAT-95 distinguishes secret metadata, secret values, secret mutation,
operational logs, and transcripts. Check access before reading any backend.
Sensitive responses use `Cache-Control: no-store`. Secret mutation need not
grant value readback. Redact values and transcript contents from access events.

Expose effective caller permissions separately from installation capabilities.
The shared PWA views and embedded host use this projection to avoid fetching
protected content or opening terminals that the caller cannot access. Direct
links show an access-denied state. A 403 never triggers a login loop or appears
as an empty data set. Full administrators retain their normal workflow.

Revocation/narrowing is rechecked on requests and long-lived streams; affected
connections close and cached protected UI content is cleared. Define and test
the propagation bound for multiple control-plane replicas. Current startup
environment loading alone cannot implement immediate per-key revocation.

## Storage, rollout, and identity continuity

The credential registry is the single runtime authority. The installation
Secret and existing `callers` configuration are upgrade inputs, not two
permanent, independently mutable authorization systems. Before choosing the
registry writer, verify standalone and GitOps ownership and configuration
export/restore behavior. Unmanaged/external records must visibly report their
ownership and supported mutations rather than pretending revocation succeeded.

Migration must preserve existing secret values and task ownership. Rename the
live administrator caller and subject, and replace the live credential identity
containing `legacy` through an explicit compatibility mapping. Keep the stable
installation principal. Verify stored tasks/results, event cursors, usage
owners, signed browser cookies, and archive/download tokens across this change.
Old signed claims may be read only by a bounded migration adapter after full
signature and binding verification; new identities/logs/API responses must
never emit `legacy`. A scoped caller named `legacy` or `administrator` must
never gain administrator access through a migration alias.

Do not require operators to replace their administrator key merely to adopt
the model. Do not delete old audit history or rewrite unrelated Agent names
that happen to contain the word. Document any unavoidable one-time session or
cursor renewal before rollout. Rolling back must not resurrect revoked keys.

Usage evidence is metadata only: credential ID, action/resource, outcome, and
time. If persistence is unavailable, show the limitation. A last-used counter
is not a complete security audit trail; MAT-97 owns durable action auditing.

## Delivery sequence and review gates

1. Review this design against the route matrix, key/session code, rollout
   ownership, and identity-bound persistence. Record findings and resolve them
   before authentication behavior changes.
2. Add the canonical credential projection and administrator-only inventory,
   with the installation administrator row and complete access descriptions.
   Do not present restricted access as safe before step 3 is complete.
3. Enforce the full restricted-caller boundary, including MAT-94/95, key
   rotation/management, terminal previews, and every unclassified route.
4. Deliver named credential lifecycle, effective-permission UI, bounded
   revocation, identity migration, and operator documentation in reviewed PRs.
   Preserve a usable administrator throughout staged upgrades.
5. Test disposable credentials and Agents only on the verified GCP dev cluster.
   Capture desktop/mobile screenshots. Review each PR, fix findings, require
   green CI, and remove all test credentials and workloads afterward.

## Acceptance evidence

- Administrator inventory agrees with the authenticator's accepted records,
  including imported and rejected entries, without exposing secret material.
- Existing administrator bearer access and browser sessions retain intended
  access across upgrade; new runtime identities never use `legacy`.
- Restricted bearers and cookies deny shell, secret/log reads, key rotation,
  and unrelated routes without explicit grants, before backend access.
- Agent A grants fail against Agent B, including route aliases and streams.
- Permission changes and revocation affect bearer, cookies, SSE, and terminal
  connections within the documented bound across replicas and restarts.
- Creation/rotation values are returned only once; lists, errors, logs, telemetry,
  screenshots, and configuration exports do not expose them.
- UI tests cover allowed/denied/loading states, direct navigation, no forbidden
  background fetches, and small-screen access summaries in both PWA hosts.
- Migration/rollback fixtures preserve task/result ownership and prove that
  revoked credentials cannot be restored by stale configuration or rollback.
