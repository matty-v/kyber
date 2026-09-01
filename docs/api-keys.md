# Kyber Platform API Keys

The **Kyber API key** is the single credential that authorizes all operator-facing requests against the control-plane API (`/api/v1/*`). One key per Kyber install. Anyone with the key can do anything the API exposes — create agents, edit specs, restart pods, rotate secrets, the lot.

This doc is the operator reference for managing it: where it lives, how to rotate, how to handle compromise, and what it does (and doesn't) protect.

> Adjacent credentials Kyber agents use — Anthropic OAuth tokens, GitHub App installation tokens, per-agent Telegram bot tokens, operator-uploaded user secrets — rotate independently and are **not affected** by anything in this doc. See [`docs/agents-identity-repos.md`](agents-identity-repos.md) for those.

## TL;DR

- **Generate**: `openssl rand -hex 32` at install time, stored in the `kyber-api-credentials` Kubernetes Secret under key `api-key`.
- **Rotate (in-PWA, no downtime)**: `POST /api/v1/rotate-api-key` swaps the key in-memory and updates the Secret. Old key 401s on the next request.
- **Rotate (manual, e.g. compromise)**: `kubectl edit secret kyber-api-credentials` + restart the control-plane pod.
- **Browser session**: the embedded PWA exchanges the pasted key for an opaque,
  HttpOnly, same-site session cookie; it does not retain the key in readable storage.
- **Single-tenant**: there is no per-key audit trail or RBAC. The key is a shared secret between operator and control-plane.

## Where the key lives

### In the cluster

The control-plane reads the key from the Kubernetes Secret `kyber-api-credentials` (default name; configurable via the chart's `api.existingSecret` value). The Secret has one data key:

| Data key | Value |
|---|---|
| `api-key` | The 64-character hex string (32 bytes of entropy) |

The control-plane pod's spec mounts the Secret value into the `KYBER_API_KEY` env var via `secretKeyRef`, and the process reads it at startup. After startup, runtime mutation is driven by the `APIKeyAuthenticator` (`pkg/api/auth.go`) — that's how `/api/v1/rotate-api-key` swaps the key without a pod restart.

### In the PWA

The embedded PWA exchanges the pasted key once at
`POST /api/v1/browser-session`, then uses an HttpOnly session cookie. The raw
key is not retained in browser-readable storage.

The cookie is a **signed token, not a server-side session record** — it carries
the caller and an expiry, signed with a key derived from the live API key
(`pkg/api/browser_session_token.go`). Two consequences worth knowing as an
operator:

- Sessions **survive a control-plane restart**. Upgrades and pod recycles no
  longer sign browsers out.
- Sessions last 30 days and **renew on use**: any request made with more than
  half the lifetime spent gets a fresh cookie, so a PWA opened at least monthly
  never asks for the key again.

Rotating the API key changes the derived signing key, so it still invalidates
every outstanding session immediately — the browser that performed the rotation
is re-issued a cookie and stays signed in. That is the only way to sign other
browsers out; there is no per-browser revocation.

Legacy `localStorage['kyber_api_key']` values are consumed once and removed.

### What the key grants

Single-tenant install — **everything**:

- Create / edit / delete agents and machines
- Restart pods, stop and start agents, set-resources
- Read agent activity (heartbeat, last-activity)
- Manage user secrets, inbound bindings, scheduled jobs
- Rotate the API key itself
- Read fleet summary, telemetry, and OS-level pod logs

The legacy single key grants **everything** (full scope). For agent **lifecycle**
verbs specifically, you can now issue narrower keys — see below.

### Caller scopes on lifecycle verbs (kyber#474)

You can issue **scoped API keys** that are limited to certain agent lifecycle
actions, instead of handing out the full-scope key. Scoped keys live in an
optional `callers` JSON document on the same `kyber-api-credentials` Secret:

```json
[ {"name":"pwa","key":"<32-byte hex>","scopes":["lifecycle:write"]},
  {"name":"ops","key":"<32-byte hex>","scopes":["lifecycle:admin"]} ]
```

- `lifecycle:write` — `start` / `stop` / `restart` (and OAuth re-auth resume).
- `lifecycle:admin` — the impactful verb `force-needs-auth`, **and**
  everything `write` grants (scopes nest: `admin` ⊃ `write`). So a `write`-only
  key cannot force-needs-auth an agent.

The legacy `api-key` keeps working as a **full-scope** caller, so this is
backward-compatible. Enforcement is **off by default** (`api.authz.enforce:
false`): under-scoped callers are audit-logged but not blocked. Set
`api.authz.enforce: true` (a 🚦-gated per-cluster operator step) to return **403**
to an under-scoped caller. Roll out by defining scoped callers, watching the
permissive-mode audit log, then enabling enforcement.

Other (non-lifecycle) routes still accept any valid key — the scope model is
designed to extend to them later. See
[`architecture/api-authorization.md`](architecture/api-authorization.md) for the
full model and [Out of scope](#out-of-scope) for the broader "RBAC" follow-up.

### Sourcing a caller's key from a Secret (`keyFrom`, kyber#557)

Instead of an inline `key`, a callers entry may reference a Secret data key:

```json
[ {"name":"some-caller","keyFrom":{"secret":"some-caller-api-key","key":"api-key"},"scopes":["lifecycle:write"]} ]
```

The control plane resolves the reference **once, at process start**, and the
caller then authenticates exactly as if the key were inline (same scopes, same
Bearer presentation). Use this when a consumer also mounts its own key Secret:
the value then lives in **exactly one place** — the referenced Secret — instead
of being duplicated into the shared `callers` doc, where rotation strands the
consumer's copy and any reader of the doc sees every caller's key.

Rules:

- **Exactly one of `key` / `keyFrom`** per entry. Both, or neither, is a parse
  error and — per the existing fail-closed contract — rejects **all** scoped
  callers for that boot (the legacy key still works).
- **Same namespace only.** The reference resolves in the control plane's own
  namespace. There is no `namespace` field, deliberately: the restriction is
  the application-level boundary that keeps the callers doc from designating
  Secrets elsewhere in the cluster.
- **An unresolvable reference rejects only that caller** (missing Secret,
  missing data key, or empty value) with a setup log naming the reference.
  Other callers and the legacy key are unaffected. The startup line
  `loaded scoped API callers` reports `count` (accepted) and `rejected`.
- The resolved value is trimmed of surrounding whitespace (a `stringData`
  value with a trailing newline matches what a file-mounting consumer reads).

**Rotation procedure (restart-to-rotate, stated plainly):** update the
referenced Secret's value, then **restart the control plane** — resolution
happens only at process start; there is no hot reload. The old key stops
authenticating at that restart, the new value starts. Consumers mounting the
same Secret pick the new value up via the kubelet refresh (~60s) with no
action of their own.

**Rollback ordering (read before downgrading the control plane):** a control
plane older than the `keyFrom` feature fails to parse **any** entry that has
no inline `key` — and a callers-doc parse failure rejects **every** scoped
caller, not just the `keyFrom` one (legacy key unaffected). So, to roll the
control plane back below the `keyFrom` version floor:

1. **First** convert every `keyFrom` entry in the callers doc back to inline
   `key` form (copy the value out of the referenced Secret).
2. **Then** roll back the control-plane image and restart.

Rolling back first takes all scoped callers dark until the doc is reverted.

## Generation (install time)

The install path is documented in [`docs/installation.md` § 1](installation.md#1-generate-secrets) and [`docs/installation-wsl2.md`](installation-wsl2.md). The relevant command:

```bash
KYBER_API_KEY=$(openssl rand -hex 32)
```

Why those flags:
- `rand` — pulls from the OS CSPRNG (kernel `/dev/urandom` on Linux).
- `-hex` — encode as ASCII hex so the key fits cleanly in a Kubernetes Secret value and is easy to copy/paste.
- `32` — 32 bytes = 256 bits of entropy. The hex encoding doubles the printed length to 64 characters. 256 bits is well past the bar for a bearer token; lower values are tempting but offer no operational benefit.

Then create the Secret:

```bash
kubectl -n kyber-system create secret generic kyber-api-credentials \
  --from-literal=api-key="$KYBER_API_KEY"
```

> Never commit the key to git. The install scripts write it to `prod-secrets.env`, which is gitignored. If you need to share the key with a teammate, use an out-of-band channel (1Password, Bitwarden, signal). Don't paste into Slack / email / chat backups.

## Rotation

### Programmatic rotation (no downtime)

From any authenticated client (PWA, `curl`):

```bash
curl -X POST -H "Authorization: Bearer $KYBER_API_KEY" \
  https://<your-kyber-host>/api/v1/rotate-api-key
```

Response (the new key is returned **once** in plaintext — this is the only opportunity to copy it):

```json
{
  "apiKey": "<new 64-char hex string>"
}
```

What happens server-side, in order:

1. Generate 32 bytes of CSPRNG entropy, hex-encode.
2. Patch the `kyber-api-credentials` Secret to set `data.api-key` to the new value (base64-encoded by k8s).
3. Mutate the running `APIKeyAuthenticator`'s in-memory key atomically.
4. Return the new plaintext key in the response body.

After the response lands, **the old key 401s on the next request**. The PWA's caching means an operator's other browser tabs will hit a 401 and prompt for the new key on next request.

The pod is **not** restarted. The Secret update is purely the persistence layer — the next pod restart will read the new key from the Secret via the existing `KYBER_API_KEY` env var, so the rotation survives pod recycle.

### Manual rotation (compromise / install repair)

When the programmatic path is unavailable (e.g. you've lost the current key, or you need to verify the key in the Secret matches what the running pod has):

```bash
# 1. Generate a new key
NEW_KEY=$(openssl rand -hex 32)

# 2. Patch the Secret directly
kubectl -n kyber-system patch secret kyber-api-credentials \
  --type=merge -p "{\"data\":{\"api-key\":\"$(echo -n "$NEW_KEY" | base64 -w0)\"}}"

# 3. Restart the control-plane pod so the env-var-derived key updates
kubectl -n kyber-system rollout restart deployment kyber-laptop-control-plane
# (substitute your release name; check `helm list -n kyber-system` if unsure)
```

After step 3 the control-plane pod restarts and reads the new key from the Secret on startup. **All in-flight authenticated sessions get 401s** until clients update to the new key.

> Manual rotation has a brief window of unavailability (the pod restart). Programmatic rotation does not. Use programmatic when possible.

## Revocation (compromise)

If you suspect the key has leaked:

1. **Rotate immediately** — programmatic if you still have the key, manual otherwise. The compromised key is dead the moment step 2 of either path completes.
2. **Audit recent access**:

```bash
# Recent API requests (look for unexpected source IPs, unusual paths,
# burst patterns)
kubectl -n kyber-system logs deployment/kyber-laptop-control-plane --since=24h \
  | grep -E "INFO http request"
```

The control-plane logs every API request with method, path, status, and request ID — but does **not** log the source IP today (single-tenant assumption). If you need richer audit, enable cloudflared or your ingress's request log.

3. **Rotate adjacent credentials if the same threat actor could have reached them**:
   - **Inbound binding secrets** — the per-binding HMAC secrets for signed inbound webhooks rotate through the binding rotate endpoint (each agent's Webhooks tab, or the API); rotate any binding whose secret lived in the same compromised environment.
   - **GitHub App private key** (separate Secret, see [`docs/agents-identity-repos.md`](agents-identity-repos.md)) — only if the actor could reach the cluster, not just the API key.
   - **Anthropic OAuth tokens, agent Telegram bot tokens, user-uploaded secrets** — separately scoped per agent, not affected by a platform-API-key compromise.

## Scope + threat model

### Single tenant

The Kyber control-plane assumes one operator (or one trusted team) per install. The API key is a **shared secret** between operator and control-plane. There is no per-user, per-role, or per-action scoping.

### Trust boundary

The API key is the only thing standing between someone with network access to the control-plane and full control of the platform. Network access today comes from:

- The cluster's pod network (anything on the cluster can reach the control-plane Service via cluster-internal DNS — but the protected port is gated by Bearer auth)
- Whatever ingress you've put in front (Cloudflare tunnel, ingress-nginx, plain LoadBalancer)

The key does NOT defend against:

- Compromise of the cluster itself (root on a node = root on every Secret)
- Compromise of the GitHub repo where install scripts wrote the key (if you committed the key — don't do that)
- Compromise of the operator's browser (PWA cache)

### What's NOT in the audit log

- Source IP of API requests (single-tenant assumption; add ingress logs if needed)
- Per-key identity (only one key, no need)
- Mutation diffs (status patches in particular are noisy; mutation logs are at INFO level but not structured for forensics)

## Out of scope

These are deliberate "not yet" choices, not bugs:

- **Per-user API keys / RBAC** — would require an identity layer (OAuth, OIDC, magic links). Tracked as a future epic if/when multi-tenancy lands.
- **Key expiration / auto-rotation policies** — programmatic rotation exists; automated policy is a separate concern.
- **Integration with external secret managers** (Vault, SOPS, sealed-secrets) — current install path uses a plain Kubernetes Secret. Operators who want sealed-secrets can substitute their own Secret name via the chart's `api.existingSecret` value.
- **Per-PR preview environment key delivery** — original [#124](https://github.com/matty-v/kyber/issues/124) bundled this; the per-PR preview system was retired ([kyber#168](https://github.com/matty-v/kyber/issues/168) + kyber-deploy#16) so there's no delivery story to document.

## Related

- [`docs/installation.md`](installation.md) — the install path that generates the initial key
- [`docs/installation-wsl2.md`](installation-wsl2.md) — WSL2 standalone install path
- [`docs/agents-identity-repos.md`](agents-identity-repos.md) — separate auth surface for agents (the agent's identity repo is managed exclusively by the Kyber Platform GitHub App — reads and writes, no PAT fallback; the generic PAT user-secret is used only for the agent's other repos)
- `pkg/api/auth.go` — the authenticator
- `pkg/api/routes_rotate_api_key.go` — the programmatic rotation endpoint
