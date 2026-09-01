# MAT-33 principal-scoped task authorization implementation plan

## Goal

Enforce stable tenant/principal ownership and exact task scopes at every public
durable-task boundary while preserving the internal agent-attempt trust model.

## Work

1. Extend API-key callers and browser sessions with stable principal, tenant,
   credential generation, exact task scopes, and immutable agent-resource
   allowlists. Preserve the legacy shared key as an explicit installation-admin
   migration principal.
2. Add immutable tenant, owner-principal, and agent-resource columns to durable
   tasks and scope idempotency keys and cursors to that authorization envelope.
3. Add repository-level authorized Get/List methods and owner-aware continuation
   and cancellation mutations. Return non-enumerating not-found results for all
   object-addressed authorization failures.
4. Apply exact create/read/list/continue/cancel/result scopes to every current
   public task route. Keep internal receipt/progress/result/interaction methods
   bound only to agent plus attempt.
5. Add adversarial API, repository, cursor, rotation/revocation, browser-session,
   result-download, and internal-boundary tests; update OpenAPI and operator docs.

## Security invariants

- Display names never establish task ownership.
- Tenant, principal, and agent-resource filtering happens before rows, results,
  messages, objects, or cursor material are returned.
- Rotation may change credential ID/generation without changing principal ID.
- Revoked or replaced credentials and their browser sessions fail on the next
  request on every replica using current configuration.
- No public principal is accepted by an internal mutation endpoint.
- Administrative override stays explicit and audited; normal routes never
  silently inherit it.
