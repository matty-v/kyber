# MAT-76 — Claude authentication failure classification plan

Status: proposed; awaiting Matt's approval before implementation.
Issue: https://linear.app/matty-v/issue/MAT-76
Parent: MAT-7.
Baseline: `3e4afb9` (`origin/main`, Kyber v1.6.0).

## Outcome and boundaries

Make Claude startup failures tell the operator what action can actually help.
Only a missing or rejected credential may enter `NeedsAuth`. Provider/network
failures must use Kyber's bounded automatic crash-recovery path, while a
failure to persist rotated credentials must be identified separately from both.

This change will not add a lifecycle phase, CRD field, REST shape, PWA surface,
dependency, or authentication mode. It will preserve Claude API-key behavior,
Codex/Hermes behavior, the existing credential Secret format, and the current
three-attempt restart/backoff policy. MAT-77 owns stronger credential-rotation
durability and stale-bootstrap recovery; MAT-76 will expose that boundary
honestly without claiming atomic write-back.

## Verified baseline

- `images/claude-code/start-claude.sh` uses exit `2` for all of these distinct
  cases: a missing credential, any token-endpoint transport or HTTP failure,
  malformed/incomplete token responses, missing rotation endpoint wiring,
  rotation write-back failure, and an unrelated workspace-trust write failure.
- `pkg/runtimes/claudecode/contract.go` declares exit `2` as the one Claude
  authentication failure code.
- `pkg/controllers/agent/reconciler.go` therefore maps every current Claude
  exit `2` to `OAuthRefreshFailed`, and the state machine moves both `Starting`
  and `Running` agents to `NeedsAuth` without retry.
- The OAuth standard distinguishes `invalid_grant` (invalid, expired, revoked,
  or mismatched grant/refresh token) from `server_error` and
  `temporarily_unavailable`. Curl likewise reports transport/timeout and HTTP
  response failures separately. The startup script currently discards both
  distinctions through `curl -f`.
- Focused controller classification tests and the three relevant Claude
  integration tests pass on the baseline. They pin the conflated behavior that
  this issue will replace.

## Approved design requested

### 1. Runtime-owned failure vocabulary

Extend the private runtime descriptor with distinct, validated exit-code slots
for:

- confirmed authentication failure;
- authentication-provider/service failure; and
- credential synchronization failure.

The values must be non-negative and distinct when configured. Claude will keep
`2` for confirmed auth failure, use `44` for provider/service failure, and use
`45` for credential-sync failure. Existing Codex and Hermes auth codes remain
unchanged; integrations that do not declare the new categories inherit no new
classification.

Replace the controller's boolean auth-code check with one runtime-scoped
classification helper. The actual pod runtime label remains authoritative, with
the Agent spec used only for legacy pods. Inspect only the current agent
container termination, never a sidecar or `LastTerminationState`.

### 2. Claude startup classification

Use small shell helpers for the three explicit failure exits and reserve every
other nonzero exit for a generic runtime crash.

For the token endpoint:

- missing/incomplete stored credentials and an OAuth `invalid_grant` response
  exit as confirmed authentication failure;
- DNS/connect/TLS/timeout errors, HTTP 429/5xx, OAuth `server_error` or
  `temporarily_unavailable`, other non-`invalid_grant` HTTP errors, and
  malformed/incomplete success responses exit as provider/service failure;
- do not log the request body, refresh token, access token, authorization code,
  or raw response body.

For Kyber write-back:

- missing rotation endpoint wiring or a failed/non-2xx rotation push exits as
  credential-sync failure;
- the diagnostic states that the provider may already have rotated the token
  and that MAT-77 owns stronger recovery semantics;
- unrelated bootstrap failures, including workspace-trust state writes, use a
  generic non-reserved exit and can no longer enter `NeedsAuth` accidentally.

### 3. Lifecycle behavior and operator evidence

Add provider/service and credential-sync events for `Starting` and `Running`.
Both enter `Failed` through the existing bounded auto-restart action rather
than `NeedsAuth`:

- provider/service failure reports that Kyber will retry and that replacing
  credentials is not the indicated fix;
- credential-sync failure reports that persistence failed after refresh and
  reauthorization may become necessary only if the rotated credential cannot
  be recovered;
- retry-limit handling preserves the actionable failure message instead of
  clearing it when the Agent remains `Failed`.

No new phase is justified: `Failed` already represents bounded recoverable
runtime/service failures, and the existing 10s/30s/90s restart staircase
prevents a tight loop. `NeedsAuth` remains the stable human-action state only
for confirmed credential failure.

## Checkpoints

- [x] Load MAT-76, set it to In Progress, and create a clean branch from
  current `origin/main`.
- [x] Trace the Claude boot script, runtime descriptor, reconciler
  classification, state-machine transitions, status messaging, and existing
  tests/docs.
- [x] Run focused baseline controller and Claude startup tests.
- [ ] Obtain Matt's approval for the design above.
- [ ] Add failing shell, descriptor, controller/state-machine, and reconcile
  tests for the new taxonomy and bounded recovery behavior.
- [ ] Implement runtime-owned codes, Claude response classification, and
  controller translation without expanding the public API.
- [ ] Run focused tests, full Claude integration tests, controller envtest,
  `make build`, `make lint`, and `make test` with the required envtest assets.
- [ ] Update the harness contract/conformance matrix, lifecycle documentation,
  and this plan with exact evidence and remaining limits.
- [ ] Commit and push each verified checkpoint, open one consolidated PR, and
  record the PR/CI evidence on MAT-76.
- [ ] After merge, leave live staging validation to the combined MAT-7 closure
  matrix agreed for MAT-76 through MAT-79; do not cut a release from this issue.

## Test matrix

| Layer | Required cases |
|---|---|
| Claude startup | missing credential and `invalid_grant` -> auth; connection refusal/timeout, 429, 5xx, malformed or incomplete 2xx -> service; missing/failed write-back -> sync; unrelated exit `2` source removed |
| Secret safety | no access/refresh token or raw token response in output; existing private file modes retained |
| Descriptor | configured codes are unique; absent optional codes do not classify; runtime validation remains deterministic |
| Controller unit | current agent-container exit maps to the owning runtime's category; pod label beats changed spec; sidecars, old terminations, unknown runtimes, and other exit codes do not classify |
| State machine | service/sync failures from `Starting` and `Running` enter bounded `Failed` recovery; auth still enters `NeedsAuth`; retry limit preserves the actionable reason |
| Controller integration | terminal Claude pods exercise all three categories and result in the expected phase, restart count, event, and status message |
| Regression | Claude API-key/subscription startup suites; Codex/Hermes failure behavior; full repository build, vet, test, integration CI |

## Acceptance mapping

- Missing, invalid, expired, revoked, or human-action-required credentials are
  the only automatic paths to `NeedsAuth`.
- Provider outages and network errors remain visibly distinct and bounded by
  the existing restart budget.
- Credential write-back failures have their own code, event, diagnostic, and
  status message.
- No failure path logs secrets or silently changes billing/auth mode.
- Documentation records fixture evidence separately from the later combined
  staging certification.

## Current next action

Matt reviews this proposed design. On approval, begin with failing tests for the
three exit categories and state transitions, then implement the smallest code
changes that satisfy them.
