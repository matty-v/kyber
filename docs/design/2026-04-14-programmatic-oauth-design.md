# Programmatic OAuth for Agent Creation — Design Spec

**Date:** 2026-04-14
**Status:** Phases 1–3 shipped (2026-04-14). Phases 4–5 open — see plan doc.

## STATUS as of 2026-04-14

**Shipped (Phases 1–3):**
- PWA "Authorize with Claude" button → PKCE verifier/challenge generated client-side → Anthropic authorize URL opened in new tab
- User pastes OOB authorization code into PWA; submits `{oauthCode, pkceVerifier}` to Kyber API
- Kyber API exchanges code+verifier at Anthropic token endpoint → writes `<agent-name>-oauth` k8s secret with `access_token` + `refresh_token` keys
- `start-claude.sh` refresh-on-boot: reads refresh token from env, calls Anthropic token endpoint, writes `~/.claude/.credentials.json`, calls node-agent `/internal/agents/{name}/refresh-token` if token rotated
- New node-agent endpoint `POST /internal/agents/{name}/refresh-token` for rotation storage
- `MachineStatus.Allocatable` field plumbed from `Node.status.allocatable` (used by PWA capacity badge)
- Scopes actually granted: 5 of 6 — `org:create_api_key` was dropped (Anthropic does not grant it via the public client_id); remaining 5 are sufficient for Claude Code

**Not yet shipped (Phases 4–5):**
- Phase 4: `make oauth-iter` fast iteration harness, OAuth e2e tests against mock, smoke test playbook
- Phase 5: `NeedsAuth` agent phase, Re-authorize PWA action, legacy `oauthToken` field removal

## Context

Agent creation today requires one of:

- A `setup-token` OAuth token pasted into the PWA — but this token has only `user:inference` scope, which is insufficient for Claude Code's API calls (returns 401).
- A manual `/login` flow inside the agent's tmux after the pod boots — works, but requires a human to see the URL, visit it, paste the code back. Breaks the "create an agent in the PWA and walk away" UX.

**Goal:** PWA-driven OAuth during agent creation. The pod boots already authenticated with the full scope set Claude Code needs. No interactive /login inside tmux.

**Non-goals:**

- Shared/master refresh token across agents (explicitly rejected — per-agent isolation).
- Anthropic partner OAuth registration (we reuse the public Claude Code CLI client_id).
- OAuth for CLI-created agents (keep the `oauthToken` API field as a backdoor for scripting).
- Replacing the `api-key` auth path (unchanged).

## Constraints

- No partner relationship. We reuse Claude Code CLI's public client_id: `9d1c250a-e61b-44d9-88ed-5944d1962f5e`.
- Authorize endpoint: `https://claude.com/cai/oauth/authorize`. Token endpoint: `https://platform.claude.com/v1/oauth/token`.
- Claude Code reads credentials from a file at `~/.claude/.credentials.json` when no OS keyring is reachable. Verified empirically on 2026-04-14: with `DBUS_SESSION_BUS_ADDRESS` unset and only the file present, Claude Code authenticates to the real Anthropic API with full `user:sessions:claude_code` scope. The minimum required fields are `{accessToken, refreshToken, expiresAt, scopes}` — `subscriptionType` and `rateLimitTier` are optional. The `CLAUDE_CODE_OAUTH_TOKEN` env var only accepts setup-token scope and is not usable.
- Agents run on preemptible spot VMs — pods restart frequently. The creds path must survive preemption.
- Access tokens expire in ~24h. Refresh tokens are long-lived and rotate on each refresh.

## User flow

1. User fills the Create Agent form in the PWA (name, model, resources).
2. User clicks **Authorize with Claude**. PWA generates a PKCE verifier (32-byte URL-safe random) and S256 challenge client-side; opens the Anthropic authorize URL in a new tab.
3. User signs in and approves on their phone/laptop. Anthropic displays the authorization code (OOB flow).
4. User copies the code, returns to the PWA, pastes it into the **Authorization code** field.
5. User clicks **Create Agent**. PWA submits `{agent fields, oauthCode, pkceVerifier}` to Kyber.
6. Kyber exchanges `code + verifier` at the Anthropic token endpoint. On success it writes `<agent-name>-oauth` secret with `access_token` and `refresh_token` keys, then creates the Agent CR.
7. Pod starts. `start-claude.sh` refreshes the access token from the stored refresh token, writes `~/.claude/.credentials.json` with the fresh values, and starts `claude`. Claude Code reads full-scope credentials from the file.
8. Pod continues to run. Claude Code refreshes in-memory during the session.

## Preemption / restart path

The refresh token is the durable source of truth. It lives in the `<agent-name>-oauth` k8s secret (managed by Kyber, authoritative) and is materialized into `~/.claude/.credentials.json` on the agent's PVC for Claude Code to read.

On every pod boot, `start-claude.sh` refreshes from Anthropic using the stored refresh token and writes a fresh `.credentials.json` with the rotated tokens. This keeps Kyber in the auth path only at boot, never during runtime — Claude Code refreshes in-memory during the session.

When the Anthropic refresh rotates the refresh token, `start-claude.sh` POSTs the new value to the node-agent, which updates the secret. Node-agent owns this path because it already has an authenticated channel to the control plane.

If refresh fails (token revoked or expired refresh grant), the pod surfaces `NeedsAuth` via node-agent → control plane. The PWA renders a **Re-authorize** action on that agent that runs the OAuth flow again and updates the existing secret.

## Components

### 1. PWA — `pwa/src/pages/CreateAgent.tsx`

Replace the current **OAuth Token** field with two UI elements:

- **Step 1 button** "Authorize with Claude" — generates the PKCE verifier (kept in component state only, never persisted), opens the authorize URL in a new tab.
- **Step 2 field** "Authorization code" — user pastes the OOB code.

Submit payload adds `secrets.oauthCode` and `secrets.pkceVerifier`; drops `secrets.oauthToken` from the UI path.

Authorize URL shape:

```
https://claude.com/cai/oauth/authorize
  ?response_type=code
  &client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e
  &redirect_uri=urn:ietf:wg:oauth:2.0:oob
  &scope=<URL-encoded space-separated scopes>
  &code_challenge=<base64url(sha256(verifier))>
  &code_challenge_method=S256
  &state=<random>
```

Scopes requested:
`org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload`.

> **Note (2026-04-14):** `org:create_api_key` is not granted by Anthropic via the public client_id — the token endpoint silently drops it. The remaining 5 scopes are sufficient for Claude Code. The authorize URL still requests all 6 for forward compatibility; ignore the missing scope in the token response.

### 2. Kyber API — `pkg/api/routes_agents.go`

Extend `agentSecretsRequest`:

```go
OAuthCode    string `json:"oauthCode,omitempty"`
PkceVerifier string `json:"pkceVerifier,omitempty"`
```

When both are present on create, call a new helper `exchangeAuthorizationCode(ctx, code, verifier)` that POSTs JSON to `https://platform.claude.com/v1/oauth/token` with `grant_type=authorization_code, code, client_id, redirect_uri=urn:ietf:wg:oauth:2.0:oob, code_verifier`. Parse the response `{access_token, refresh_token, expires_in}`.

Secret layout (same `<agent-name>-oauth` secret the code already creates, new key set):

- `access_token` — informational; `start-claude.sh` will refresh before use.
- `refresh_token` — authoritative.

The legacy `oauthToken` key path remains for scripted creates. If a request provides both the old and new fields, reject with 400.

On exchange failure, return 400 with a sanitized message ("Authorization code invalid or expired"). Never echo Anthropic's raw body to the client.

### 3. Node-agent — refresh-token rotation endpoint

New internal endpoint `POST /internal/agents/{name}/refresh-token` with body `{refresh_token}`. Authenticated by the same per-pod JWT mechanism existing internal endpoints use. Updates the `refresh_token` key in the agent's secret via the kube API.

### 4. `images/claude-code/start-claude.sh` — refresh-on-boot + credentials file

New block runs before launching Claude Code:

```bash
if [ -n "${CLAUDE_REFRESH_TOKEN:-}" ]; then
  resp=$(curl -fsS -X POST "$ANTHROPIC_TOKEN_URL" \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg rt "$CLAUDE_REFRESH_TOKEN" \
          '{grant_type:"refresh_token", client_id:"9d1c250a-e61b-44d9-88ed-5944d1962f5e", refresh_token:$rt}')")
  access=$(echo "$resp" | jq -r .access_token)
  new_refresh=$(echo "$resp" | jq -r '.refresh_token // empty')
  expires_in=$(echo "$resp" | jq -r '.expires_in // 3600')
  expires_at=$(( ($(date +%s) + expires_in) * 1000 ))

  # Write ~/.claude/.credentials.json — the file Claude Code reads when no
  # keyring is reachable. Verified 2026-04-14 to be sufficient with no
  # gnome-keyring running.
  mkdir -p "$HOME/.claude"
  cat > "$HOME/.claude/.credentials.json" <<EOF
{
  "claudeAiOauth": {
    "accessToken": "$access",
    "refreshToken": "${new_refresh:-$CLAUDE_REFRESH_TOKEN}",
    "expiresAt": $expires_at,
    "scopes": ["org:create_api_key","user:profile","user:inference","user:sessions:claude_code","user:mcp_servers","user:file_upload"]
  }
}
EOF
  chmod 600 "$HOME/.claude/.credentials.json"

  # Rotate the stored refresh token if Anthropic returned a new one.
  if [ -n "$new_refresh" ] && [ "$new_refresh" != "$CLAUDE_REFRESH_TOKEN" ]; then
    curl -fsS -X POST "${NODE_AGENT_URL}/internal/agents/${AGENT_NAME}/refresh-token" \
      -H "Authorization: Bearer $(cat /var/run/secrets/kyber/pod-token)" \
      -d "{\"refresh_token\":\"$new_refresh\"}"
  fi
fi
unset CLAUDE_CODE_OAUTH_TOKEN
```

Env vars `CLAUDE_REFRESH_TOKEN` and `AGENT_NAME` come from the `<agent-name>-oauth` secret mounted as env. `ANTHROPIC_TOKEN_URL` defaults to Anthropic's prod endpoint; tests override it.

### 5. Agent CR + reconciler — `NeedsAuth` phase

Add `NeedsAuth` to `AgentPhase` enum. Node-agent detects refresh failure (either `start-claude.sh` exits non-zero, or Claude Code logs a `/login` prompt post-boot) and patches the Agent status.

PWA's agent list renders a **Re-authorize** button on NeedsAuth. Clicking runs the same PKCE flow as Create Agent, then submits to a new endpoint `POST /v1/agents/{name}/oauth` with `{oauthCode, pkceVerifier}`. The handler does the same code exchange and PATCHes the existing `<agent-name>-oauth` secret rather than creating a new agent. Reconciler transitions the agent out of NeedsAuth once the pod boots clean.

## Error handling

| Failure | Detection | User-visible result |
|---------|-----------|---------------------|
| Invalid or expired authorization code | Token endpoint returns 4xx | PWA shows "Authorization code invalid or expired — click Authorize again"; agent not created |
| PKCE mismatch | Token endpoint returns 4xx | Same as above; verifier discarded |
| Network error during exchange | Timeout / 5xx | PWA retry button; agent not created; code may still be valid for retry |
| Refresh fails on boot | `start-claude.sh` exit code; node-agent watches | Agent → NeedsAuth; PWA surfaces Re-authorize |
| Credentials file write fails | `start-claude.sh` logs; pod fails health check | Agent phase stuck; alert via existing mechanisms; operator debugs |

Secrets discipline: never log the code, verifier, or refresh token. Access tokens are allowed in debug logs (short-lived, already in keychain).

## Testability

OAuth codes are single-use and expire in minutes. A naive iteration loop — change code, redeploy, manually authorize in a browser, paste code, observe — burns one real OAuth grant per cycle and takes ~60 seconds each. This is unacceptable for anything more than initial spike work.

Design for three progressively broader test rings, all of which can run without a human in the loop except the outermost:

### Ring 1 — Anthropic mock server

A tiny in-process HTTP server (`pkg/oauth/mockserver`) that speaks Anthropic's OAuth surface:

- `POST /v1/oauth/token` with `grant_type=authorization_code`:
  - Validates `code_verifier` against a stored `code_challenge` (real SHA-256 check, not a stub).
  - Returns canned `{access_token, refresh_token, expires_in}` with deterministic but unique values per call.
- `POST /v1/oauth/token` with `grant_type=refresh_token`:
  - Validates the refresh token against a rotating store.
  - Returns a new access token and (occasionally) a rotated refresh token, so tests cover the rotation code path.
- Failure modes injected via query params: `?mode=expired_code` returns 400 with Anthropic's shape; `?mode=invalid_verifier`, `?mode=server_error` available for error-path tests.

Kyber control-plane reads the token endpoint URL from an env var `ANTHROPIC_TOKEN_URL` (default `https://platform.claude.com/v1/oauth/token`). Tests spin up the mock server and point Kyber at it. Prod uses the default.

The same env-var pattern applies to `start-claude.sh` — refresh-on-boot respects `ANTHROPIC_TOKEN_URL`. This keeps the pod-side path testable end-to-end against the mock.

### Ring 2 — Fast iteration harness

`make oauth-iter` runs:

1. Start mock Anthropic server on a local port.
2. Launch control plane + a single-pod kind cluster pointed at the mock.
3. Simulate the PWA submit: POST `/v1/agents` with a pre-generated `{oauthCode, pkceVerifier}` pair the mock will accept.
4. Wait for pod to reach Running, check `kubectl exec` into the pod for evidence Claude Code saw the keychain entry.
5. Tear down.

End-to-end cycle target: under 60 seconds, no human, no Anthropic. Iterate on `start-claude.sh` or the exchange handler, re-run `make oauth-iter`, done.

### Ring 3 — Production smoke test

A single manual playbook in `docs/testing/oauth-smoke.md`:

1. Open PWA at `https://kyber.your-tailnet.ts.net/`.
2. Create a test agent `oauth-smoke-N` (N increments so the PVC is fresh).
3. Click Authorize, approve on phone.
4. Paste code, submit.
5. Observe agent reaches Running within 90 seconds with no /login prompt in tmux.
6. Wait 25 hours (or force-expire the access token via `kubectl delete secret <name>-oauth-access-only` trick if we build one) and confirm the session keeps working via in-memory refresh.
7. Kill the pod, confirm it boots clean from the stored refresh token.

This is the only path that exercises the real Anthropic OAuth endpoint; run it before merging any phase. Everything else below the smoke test runs in CI against the mock.

## Automated test suite

- **Unit:**
  - PKCE verifier/challenge round-trip.
  - `exchangeAuthorizationCode` against the mock (happy + each failure mode).
  - `start-claude.sh` refresh path exercised via a shell unit harness (`bats` or similar) with the mock.

- **Integration (Go):**
  - API route accepts `{oauthCode, pkceVerifier}` → creates secret with both keys.
  - Rejects when either is missing or when both old and new OAuth fields are present.
  - NeedsAuth PATCH endpoint updates existing secret without recreating the agent.
  - Refresh token rotation endpoint (`POST /internal/agents/{name}/refresh-token`) updates the secret.

- **E2E (kind + mock, CI):**
  `test/oauth-e2e/oauth_e2e_test.go`. Full flow in a kind cluster against the Anthropic mock:
  1. Spin up mock; deploy Kyber with `ANTHROPIC_TOKEN_URL` pointed at it.
  2. `POST /v1/agents` with a valid PKCE pair the mock accepts.
  3. Wait for pod Running; verify secret has both keys.
  4. Kill the pod, observe refresh-on-boot picks up the (mock-rotated) refresh token and writes it back.
  5. Inject `?mode=expired_refresh` on the mock, kill the pod, verify Agent transitions to NeedsAuth.
  6. Simulate Re-authorize: `POST /v1/agents/{name}/oauth` with a fresh code, verify NeedsAuth clears.

- **Regression:** `api-key` path unchanged; legacy `oauthToken` scripted path still works until Phase 5.

## Rollout

Five phases, each its own PR:

1. **API + exchange helper.** Add code/verifier fields to request; implement `exchangeAuthorizationCode`; writes `<agent-name>-oauth` secret with `access_token` + `refresh_token` keys. No UI change.
2. **start-claude.sh refresh-on-boot + credentials file.** Write `~/.claude/.credentials.json` from refreshed tokens on every boot. Validate on a test agent with manually-injected refresh token.
3. **PWA UX.** Replace OAuth Token field with Authorize button + code paste. Submit new fields.
4. **End-to-end verification on prod.** Create a fresh agent through the PWA, confirm boot-authenticated behavior.
5. **Remove legacy path.** Drop `oauthToken` from `agentSecretsRequest`. Keep only the new flow.

## Security

- Refresh token is the credential of record. `<agent-name>-oauth` secret RBAC is unchanged (only kyber-system service accounts read it).
- PKCE verifier lives only in PWA component memory and in the request body. Not persisted, not logged.
- Authorize URL opens in a new tab; the PWA never sees the user's Anthropic session.
- Revocation: delete the `<agent-name>-oauth` secret and re-authorize. Account-level compromise is handled by the user rotating at `claude.com/account`.

## Open questions

1. **OOB redirect URI support.** Does Anthropic's authorize endpoint accept `redirect_uri=urn:ietf:wg:oauth:2.0:oob`? If not, we host a simple page at `https://kyber.your-tailnet.ts.net/oauth/redirect` that echoes the code back for the user to copy. Spike in Phase 1 before going wide.

**Resolved** (2026-04-14 empirical tests):

- *Claude Code keychain schema.* No longer relevant — Claude Code reads `~/.claude/.credentials.json` when no keyring is reachable. See "Constraints" above.
- *Scope grant.* Confirmed `user:sessions:claude_code` is granted on tokens minted via the CLI's client_id — checked against a live credentials file from this machine. `org:create_api_key` is NOT granted; see note in § Components § 1.
- *Code-display UX.* The literal `redirect_uri=urn:ietf:wg:oauth:2.0:oob` is **rejected** by Anthropic's CLI OAuth client ("Redirect URI urn:ietf:wg:oauth:2.0:oob is not supported by client"). However, Anthropic exposes an equivalent manual-mode endpoint at `https://platform.claude.com/oauth/code/callback` (named `MANUAL_REDIRECT_URL` in the Claude Code binary) that displays the authorization code on an Anthropic-hosted page. We use that as the redirect_uri. Same UX, no Kyber-hosted redirect page needed. The code is displayed in `<code>#<state>` format and the PWA splits on `#` before submitting.
