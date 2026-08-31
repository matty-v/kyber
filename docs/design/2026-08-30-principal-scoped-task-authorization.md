# Principal-scoped task authorization

**Status:** Proposed
**Date:** 2026-08-30
**Tracker:** [MAT-23](https://linear.app/matty-v/issue/MAT-23/designplatform-principal-scoped-task-authorization)
**Depends on:** [MAT-19](2026-08-30-durable-agent-tasks.md), [MAT-20](2026-08-30-task-progress-typed-results.md), [MAT-21](2026-08-30-cooperative-task-cancellation.md), and [MAT-22](2026-08-30-resumable-multi-turn-tasks.md)
**Origin:** [MAT-6](https://linear.app/matty-v/issue/MAT-6/spikeplatform-what-formal-a2a-protocol-support-would-require-for-kyber), [A2A gap study](2026-08-30-a2a-protocol-support.md), and gap-study PR #183

## 1. Decision

Give every authenticated caller a stable principal and tenant identity. A task
is owned by the principal that creates it, within that tenant. Every public
task operation must satisfy all of these conditions:

1. the principal has the specific action scope;
2. the principal belongs to the task tenant;
3. the principal owns the task; and
4. the principal is permitted to use the task's agent resource.

Version one is owner-only by Matt's decision on 2026-08-30. Principals in the
same tenant do not automatically see or mutate one another's tasks. Sharing,
groups, service delegation, and task transfer are deferred until a concrete
Kyber use case justifies an ACL model.

The only bypass is an explicit, separately scoped, audited administrative
override. Authentication remains pluggable: API keys ship first, while future
OAuth/OIDC credentials map into the same principal envelope. Claude Code and
Codex remain execution engines; they do not decide caller authorization.

## 2. Why this belongs in Kyber

Harnesses can authenticate their own tools and services, but they do not own
Kyber's durable task IDs, result objects, event streams, continuation routes,
or cancellation requests. Once Kyber exposes those resources through one API,
it must prevent one caller from guessing or listing another caller's work.

This is therefore a platform envelope, not a replacement for harness-native
authorization. Kyber authenticates the external principal, applies resource
policy, and binds the accepted task to an agent attempt. The selected harness
continues to enforce its sandbox, tool approvals, and downstream credentials.

## 3. Current state and gap

`pkg/api/auth.go` currently authenticates a `Caller` with a display `Name` and
a set of coarse scopes. The name is for audit output, not a stable security
identifier. Configured callers and the legacy shared API key can receive
`lifecycle:write`, `lifecycle:admin`, `requests:write`, and `requests:read`.
There is no tenant, task owner, credential generation, or agent-resource
constraint. Browser sessions hold an in-process snapshot of that caller.

Consequently, a caller with request-read or request-write authority can act on
any request addressable by the route. The durable task, result, interaction,
file, and event surfaces from MAT-19 through MAT-22 would amplify that gap.
Filtering after repository reads would also leak existence through list
pagination, timing, error shape, downloads, and event replay.

Retain the existing `Authenticator` boundary so API keys can later be joined
by OIDC or workload identity without changing task policy. Replace the caller
security model behind that boundary rather than teaching every route about a
credential type.

## 4. Principal envelope

Successful authentication produces an immutable request-scoped envelope:

```go
type Principal struct {
    PrincipalID         string
    TenantID            string
    Kind                string // user, service, installation-admin, platform
    Issuer              string
    Subject             string
    DisplayName         string // audit/display only
    CredentialID        string
    CredentialGeneration uint64
    Scopes              ScopeSet
    AgentResources      ResourceSet
}
```

`principal_id` and `tenant_id` are opaque, stable IDs. Renaming or rotating an
API key preserves both. `display_name`, email, Telegram username, and other
mutable labels never participate in authorization. Issuer and subject support
future federated identities; the API never trusts caller-supplied identity
headers unless an authenticated proxy integration explicitly establishes
them.

Configured API-key callers must declare stable principal and tenant IDs plus
allowed scopes and agent resources. Credential material is independently
versioned and revocable. Multiple credentials may resolve to one principal.
Deleting a credential removes that credential's access but does not delete or
reassign the principal's tasks.

## 5. Scope and resource policy

Use narrow public scopes:

| Scope | Permitted operation |
| --- | --- |
| `tasks:create` | Create a task on an allowed agent |
| `tasks:read` | Get owned task metadata and progress |
| `tasks:list` | List owned tasks |
| `tasks:continue` | Read/respond to owned interactions |
| `tasks:cancel` | Request cancellation of an owned task |
| `task-results:read` | Read owned result metadata and content |
| `task-events:read` | Subscribe to or replay owned task events |
| `tasks:admin` | Explicit same-tenant owner override |
| `tasks:platform-admin` | Explicit cross-tenant support override |

Scopes do not imply one another. A deployment may define named bundles, but
the resolved envelope contains the expanded scopes and policy checks the exact
action. Existing `requests:read` and `requests:write` map temporarily to a
documented compatibility bundle during migration and emit deprecation audit
events.

Every principal also has an exact agent allowlist or equivalent normalized
resource rules. A task operation checks the task's agent against that policy,
including reads and downloads. This prevents a credential intended for one
agent from becoming a discovery credential for the installation. Agent rename
and deletion behavior must preserve an immutable agent resource ID.

## 6. Authorization matrix

| Surface | Scope | Resource checks |
| --- | --- | --- |
| Create task | `tasks:create` | tenant and allowed agent |
| Get/list task or progress | `tasks:read` / `tasks:list` | tenant, owner, allowed agent |
| Read/respond to interaction | `tasks:continue` | tenant, owner, allowed agent |
| Cancel task | `tasks:cancel` | tenant, owner, allowed agent |
| Result metadata/content/file | `task-results:read` | tenant, owner, allowed agent |
| Subscribe/replay events | `task-events:read` | tenant, owner, allowed agent |
| Delete/retention operation | administrative scope | explicit override and reason |

The policy is evaluated on every route, including range requests, redirects,
signed-download creation, authorization-flow completion, and reconnect after
an event-stream cursor. A successful check on a parent response is never used
as lasting authority for a later child request.

Internal receipt, progress, result, interaction-request, pause, and completion
updates use a different boundary. The status sidecar authenticates with pod
identity; the control plane verifies the assigned immutable agent ID, task ID,
and current MAT-19 delivery-attempt token. A pod cannot acquire external owner
authority and an external principal cannot call internal mutation routes.

## 7. Persistence and repository boundary

Add non-null ownership to `agent_tasks` after migration:

```text
tenant_id            text not null
owner_principal_id   text not null
agent_resource_id    text not null
```

Task creation writes ownership in the same transaction as the task and
idempotency record. Idempotency uniqueness includes tenant and principal, so
two principals may reuse the same key without observing each other. Result,
message, interaction, event, and attempt rows inherit authority through their
task foreign key rather than copying mutable policy fields.

Public repository methods accept an `AuthorizationContext` and include tenant,
owner, and agent-resource predicates in SQL. Avoid exporting an unfiltered
`GetTask(id)` to API code. Separate internal methods require an internal agent
attempt context; administrative methods require a typed override context.

List filters run in SQL before ordering, limit, and cursor generation. A cursor
is opaque and bound to the principal ID, tenant ID, normalized filters, sort,
and policy version. Reusing it under a different identity or filter is invalid.

## 8. Non-enumeration and object delivery

For object-addressed public routes, an existing but unauthorized task is
indistinguishable from an absent task: return the same `404`, body, headers,
and practical timing class. Missing scope may return `403` only when no object
identifier is involved and doing so does not confirm resource existence.

Authorize before looking up or disclosing result-object metadata. Prefer an
authenticated streaming endpoint. If object storage requires a signed URL,
mint it only after a fresh authorization check, constrain it to the exact
object and safe response headers, and keep its lifetime very short. Kyber must
not expose durable bearer URLs that outlive revocation. Range requests repeat
the same authorization checks.

Authenticated responses use private/no-store cache policy as appropriate.
Any shared gateway cache key includes the authorization boundary; endpoints
must not accidentally cache one owner's response for another principal.

## 9. Administrative override

Normal ownership failure never silently falls back to administrator access.
The caller must invoke an explicit override path or provide an override flag
with:

- `tasks:admin` for the same tenant or `tasks:platform-admin` across tenants;
- a bounded human-readable reason;
- a ticket or correlation reference; and
- the exact intended action and resource.

Kyber writes a tamper-evident audit record containing actor principal,
credential, target owner/tenant/task, action, reason, correlation ID, outcome,
request ID, and timestamp. Operator reads, downloads, cancellation, deletion,
and ownership repair are distinct events. UI and CLI output visibly indicate
override mode.

## 10. Rotation, revocation, and sessions

Key rotation creates a new credential generation for the same principal. The
new credential retains access to owned tasks; the old generation stops
authenticating after its overlap window or immediate revocation.

Browser sessions cannot remain process-local caller snapshots. Store or sign
only a session reference with principal ID, credential/session generation,
expiry, and anti-CSRF state, then validate current revocation state on every
request across replicas. A credential or principal suspension invalidates all
affected sessions promptly.

If all credentials for a principal are removed, retained tasks stay owned by
that principal. They become inaccessible until the same principal is
re-credentialed or an administrator uses the explicit repair/override path.
They are never reassigned by matching a display name.

## 11. Channel and connector callers

A Telegram, Slack, webhook, or service integration may eventually resolve a
channel identity to the same principal envelope. The adapter must authenticate
both the channel delivery and the mapping; user labels inside message content
are untrusted. The resulting principal receives no more scopes or agent access
than its mapping grants.

When an agent uses a connector while executing a task, the connector's own
credential and downstream scopes remain separate from task ownership. Task
ownership does not authorize a caller to retrieve connector secrets, and an
agent's connector access does not let its pod impersonate the task owner.

## 12. Migration and rollout

1. Add nullable ownership columns, principal configuration, audit events, and
   policy metrics while existing authorization still decides requests.
2. Create an explicit installation tenant and installation-admin principal for
   the legacy shared key. Backfill all existing tasks to that owner and record
   counts/digests. Do not create a permanent wildcard owner.
3. Require principal/tenant IDs on new scoped caller configuration. Translate
   legacy scopes only through the temporary compatibility bundle.
4. Run dual authorization in audit mode. Alert on missing ownership, policy
   disagreements, unfiltered queries, and unknown agent resource IDs.
5. Move reads/list/results/events to enforced owner filtering, then mutations,
   then creation. Keep rollback able to disable new policy without dropping
   ownership data.
6. Remove legacy scope translation and the shared-key compatibility path after
   installations migrate.

No enforcement phase begins until all task rows have valid ownership, all
public routes are in the authorization matrix, and audit-mode disagreement is
under an agreed threshold.

## 13. Security verification

Required tests include:

- guessed task, result, interaction, and event IDs return non-enumerating 404;
- two owners in one tenant cannot list, read, continue, cancel, or download one
  another's tasks;
- cross-tenant and disallowed-agent requests fail even with action scope;
- every individual scope is required and scopes do not imply hidden grants;
- owner filters apply before pagination and cursors cannot cross principals;
- result range requests and signed-link creation enforce fresh ownership;
- revoked keys, old key generations, and stale browser sessions stop working;
- rotated credentials for the same principal retain owned-task access;
- admin override requires the right boundary, reason, and complete audit data;
- channel identity cannot be forged through message content or headers; and
- stale pods/attempts cannot mutate tasks through the internal route.

Fuzz route identifiers, pagination cursors, tenant/principal encodings, and
policy combinations. Add a route-inventory test so new task-derived endpoints
must declare their required action and authorization strategy.

## 14. Observability and operations

Metrics distinguish authentication failure, missing scope, owner mismatch,
tenant mismatch, agent-policy mismatch, stale credential, and override usage,
but public error responses preserve non-enumeration. Logs use opaque IDs and
never include API keys, session material, task content, or result content.

Audit retention must outlive normal task retention. Dashboards track legacy
scope use, tasks missing owners, cross-owner denials, admin overrides, policy
latency, revocation propagation, and dual-read disagreement. Alerts cover
unexpected override volume and any successful access whose policy context is
missing.

## 15. A2A projection

An A2A adapter can map an authenticated A2A caller into this principal
envelope and map A2A operations to the narrow scopes above. Task IDs, artifacts,
messages, cancellation, and event subscriptions remain authorized by Kyber's
native task policy. A2A auth-scheme discovery and OAuth/OIDC flows are separate
gap work; this design supplies the stable enforcement target they will use.

## 16. Out of scope

- implementation of OAuth/OIDC, A2A authentication schemes, or discovery;
- task sharing, tenant-wide readers, groups, delegation, and ownership transfer;
- harness-native sandbox, tool approval, and connector authorization;
- user provisioning, billing tenancy, or a general policy language; and
- making task IDs secret or treating unguessability as authorization.

## 17. Estimate

Estimated implementation is **3–5 engineer-weeks**: principal/config and
migration (1 week), repository and route enforcement (1–2 weeks), session and
revocation behavior (0.5–1 week), and adversarial testing/rollout telemetry
(0.5–1 week). OAuth/OIDC and sharing are not included.

## 18. Acceptance criteria

- Every public task-derived operation has an exact scope and owner/resource
  check at the repository boundary.
- Every task is transactionally assigned an immutable tenant and owner.
- Same-tenant principals cannot access one another's tasks in v1.
- Unauthorized object access is non-enumerating and list/cursor behavior cannot
  leak hidden rows.
- Credential rotation preserves principal ownership while revocation propagates
  across API replicas and browser sessions.
- Internal agent mutations require the assigned agent and current attempt.
- Administrative override is explicit, separately scoped, and fully audited.
- Legacy data and credentials have a measurable, reversible migration path.

