# API authorization — caller scopes on lifecycle mutations (kyber#474)

> How Kyber decides *who* may drive an agent lifecycle verb, on top of the
> single-key authentication wall. Read this before changing `pkg/api/auth.go`,
> `pkg/api/authz.go`, or the `setAgentDesiredPhase` chokepoint.

## Why

Authentication answers *"is this a valid key?"*; it does **not** answer *"may
this caller do this?"*. Before #474, authorization was binary: any holder of the
single shared API key could drive every lifecycle verb — `start`, `stop`,
`restart`, `suspend`, and the #395 `force-needs-auth` — through the one setter
`setAgentDesiredPhase`. `suspend` and `force-needs-auth` are materially more
impactful than fail-safe `stop` (a leaked key could wedge the whole fleet into
`NeedsAuth`), yet were no better protected.

#474 adds a **caller-level** gate: a caller carries a scope set, and each verb
requires a scope. This is complementary to — not a replacement for — the
controller's `classifyEvent` effect-allowlist (`reconciler.go`), which bounds the
*effect* of a forged `desiredPhase`. This page is the *caller* gate; that is the
*effect* gate. Both layers stand.

## The flow

```
request ── authMiddleware ──> Authenticate(r) -> (*Caller{Name,Scopes}, err)
              │ 401 if err (unchanged)            │ stashed in request ctx
              ▼                                    ▼  (callerFrom)
   setAgentDesiredPhase(phase) ── authorizePhase(ctx, phase) ──┐
        start/stop/restart       → require lifecycle:write       │ deny → 403 + audit
        suspend/force-needs-auth → require lifecycle:admin  ─────┤ allow → audit + proceed
   handleReauthorize → Running   → require lifecycle:write       │
   (Telegram webhook wake → EXEMPT: own per-binding HMAC) ───────┘
```

- **`Authenticator.Authenticate`** (`pkg/api/auth.go`) now returns a `*Caller`
  (`{Name, Scopes}`) instead of just an error. `APIKeyAuthenticator` resolves the
  presented Bearer / `?token=` against a set of named scoped keys (constant-time,
  no early-exit so *which* key matched isn't a timing oracle) and the legacy key.
- **`authMiddleware`** stashes the resolved `*Caller` in the request context;
  `callerFrom(ctx)` reads it. The 401 path is unchanged.
- **`authorizePhase`** (`pkg/api/authz.go`) is the server-side guard at the
  `setAgentDesiredPhase` chokepoint (and the OAuth re-auth resume). Because every
  verb funnels through the setter, the check **cannot be bypassed** by hitting a
  route directly.

## Scope vocabulary

| Scope | Grants | Verbs |
|---|---|---|
| `lifecycle:write` | the fail-safe verbs | `start`, `stop`, `restart`, OAuth re-auth resume to `Running` |
| `lifecycle:admin` | the impactful verbs (⊃ `write`) | `suspend`, `force-needs-auth`, **agent/machine `DELETE`** (kyber#565) — **and** everything `write` grants |

**Privilege ordering (the #474 invariant):** scopes nest — `lifecycle:admin` ⊃
`lifecycle:write`. The impactful verbs require the strictly-higher scope, so they
can **never** be less-protected than fail-safe `stop`. A caller scoped only for
routine start/stop **cannot** suspend or wedge an agent into `NeedsAuth`.
`DELETE` is the single most impactful verb — irreversible identity destruction —
so it requires `lifecycle:admin`, the maximum (nothing is more impactful than
delete).

## Destructive DELETE — two interlocks (kyber#565)

`DELETE /api/v1/agents/{name}` and the machine DELETE path are guarded by **two
independent interlocks**, both enforced (in `deleteAgent` / `deleteMachine`)
before any k8s read or mutation:

1. **Confirmation (always-on safety).** The request must carry `?confirm=<name>`
   whose value equals the path name. A missing/mismatched value → **400**
   `confirmation_required`, nothing deleted. This is a *safety* interlock, not a
   security-scope one, so it is enforced **regardless of `KYBER_AUTHZ_ENFORCE`** —
   it stops the accidental `curl -XDELETE` and the fat-fingered script.
2. **Authorization (the #474 caller gate).** DELETE requires `lifecycle:admin`,
   funneled through the **same** machinery as the lifecycle verbs: `authorizePhase`
   and `deleteAgent`/`deleteMachine` both delegate to the shared
   `authorizeAction(w, r, name, action, required)` helper, so DELETE inherits the
   permissive/enforce rollout contract for free (permissive default audit-logs a
   would-deny and allows; enforce returns a non-leaky 403).

The two are orthogonal and both required: confirmation defeats *accident*, authz
defeats *unauthorized*. Only a request with the correct `?confirm` **and**
`lifecycle:admin` reaches the underlying delete, after which the finalizer reaps
all of the agent's state (see [agent-lifecycle.md](agent-lifecycle.md) §
finalizer cleanup). **Breaking change:** the `confirm` parameter is *required* —
pre-#565 callers that omit it now get 400. Internal callers (the PWA client,
e2e/contract tests) are migrated in the same change; the off-repo Holocron UI
must append `?confirm=<name>` to its delete calls.

The legacy shared `api-key` resolves to a **full-scope** caller (satisfies every
check), so single-key installs are unaffected. The vocabulary is designed to
extend later to non-lifecycle routes (e.g. `agents:write`) behind the same seam.

## Enforcement is opt-in (backward-compatible)

`KYBER_AUTHZ_ENFORCE` (chart value `api.authz.enforce`, default **false**)
governs behavior:

- **Off (default — permissive):** every decision is audit-logged, but an
  under-scoped caller is **allowed through** (a `would-deny` log line). Existing
  deployments are byte-for-byte unchanged. This is the observation window.
- **On (enforcing):** an authenticated-but-under-scoped caller gets **403** with
  a non-leaky body (`"insufficient scope for this action"` — it names neither the
  required scope nor the caller's granted scopes).

Independently, the legacy key is full-scope, so even with enforcement on a
single-key install is unaffected — the gap only closes once an operator issues
*scoped* (sub-full) keys. **Migration:** define scoped callers → watch the
permissive-mode audit log to confirm no legitimate caller would be denied → flip
`api.authz.enforce: true`.

## Scoped-key configuration

Scoped keys are an optional `callers` JSON document on the existing
`kyber-api-credentials` Secret (no new Secret/infra), surfaced to the process as
`KYBER_API_CALLERS`:

```json
[ {"name":"pwa","key":"<32-byte hex>","scopes":["lifecycle:write"]},
  {"name":"ops","key":"<32-byte hex>","scopes":["lifecycle:admin"]} ]
```

A malformed `callers` document is **fail-closed**: it is logged and rejected
(scoped keys are not loaded; the legacy key still works) rather than silently
granting. The audit log records the caller **name**, never key material.

## Key sourcing — inline `key` or `keyFrom` (kyber#557)

A caller's key value comes from exactly one of two places, validated at parse
(both set, or neither, is a parse error — the fail-closed contract above):

- **`key`** — the value inline in the callers doc, as originally shipped.
- **`keyFrom: {"secret","key"}`** — a reference to a Secret data key, resolved
  at startup. The doc then carries the *reference*, never the value.

**Two phases, authenticator untouched.** `ParseScopedCallers` stays pure (no
I/O) and enforces the mutual exclusion; `ResolveScopedCallers`
(`pkg/api/callers_resolve.go`) then fills in `keyFrom` values with a direct
client read **before the manager cache starts**, bounded by a 10s timeout
(the same pre-manager Secret-load pattern as the GitHub App config).
`NewAPIKeyAuthenticator` and everything downstream receive fully-resolved
callers and are byte-identical to the inline case.

**Resolution semantics:**

- **Startup-time only — restart-to-rotate.** Rotating a referenced Secret
  takes effect at the next control-plane restart. Accepted deliberately: the
  motivating stranding class (a duplicated consumer copy going stale across a
  cp roll) *is* a restart event. Secret-watch hot reload is a named follow-up.
- **Same-namespace pin (load-bearing).** References resolve only in the
  control plane's own namespace, enforced in code. The cp's ClusterRole covers
  Secrets cluster-wide, so this application-level restriction — not RBAC — is
  what stops the callers doc from acting as a cross-namespace
  Secret-to-API-key oracle. There is deliberately no `namespace` field.
- **Per-caller drop isolation.** An unresolvable reference (missing Secret or
  data key, empty value) rejects exactly that caller with a setup log naming
  the reference; other callers and the legacy key proceed. The startup line
  reports `count`/`rejected`. Never silently granted, never key material in
  any log.

**Trust consequence.** With `keyFrom`, read access to `kyber-api-credentials`
yields references instead of every caller's key value — a net exposure
reduction. The *write* boundary is unchanged and remains the trust boundary: a
callers-doc writer could always insert an arbitrary inline key, so the ability
to designate a same-namespace Secret data key as a credential is no
escalation.

**Version-floor / rollback caveat.** A pre-#557 binary fails to parse any
entry lacking inline `key`, which fail-closes the **whole** doc (all scoped
callers rejected, legacy key intact). Convert `keyFrom` entries back to inline
form **before** rolling the control plane back below the feature floor — the
operator procedure is in [`../api-keys.md`](../api-keys.md).

## Audit log

Both allow and deny decisions on a phase mutation are structured-logged with the
caller name, agent, requested phase, required scope, decision
(`allow`/`would-deny`/`deny`), and whether enforcement was on. This is the #474
audit AC and the observability that makes permissive-mode migration safe.

## Exempt path

The Telegram **webhook wake** (`routes_webhooks.go`) patches
`desiredPhase=Running` but is **exempt**: webhook routes bypass the Bearer wall
entirely and are gated by their own per-binding secret-header validation
(`X-Telegram-Bot-Api-Secret-Token`) — there is no API `Caller` in context to
authorize; the binding secret *is* the authorization.

## See also

- `pkg/controllers/agent/reconciler.go` `classifyEvent` — the effect-allowlist
  (defense-in-depth on the *effect*; this page is the *caller* gate).
- [`../api-keys.md`](../api-keys.md) — operator how-to for keys + scopes.
- [agent-lifecycle.md](agent-lifecycle.md) — the lifecycle verbs being gated.
- #143 (key rotation), #395 (force-needs-auth), #468 (authoritative Stop).
