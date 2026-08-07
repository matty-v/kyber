# User-Defined Per-Agent Secrets — Design Spec

**Date:** 2026-04-18
**Issue:** [#75](https://github.com/matty-v/kyber/issues/75) — User-defined per-agent secrets (kv + file) via API/PWA
**Status:** Design. Not yet implemented.

## Context

Issue #75 asks for a sanctioned, operator-facing path to attach named secrets (kv or file) to a specific agent so the pod has them mounted on next boot — without cluster-level kubectl access. The scope, durability contract, API shape, naming grammar, and mount layout are all locked in the issue. This doc pins the architectural decisions that the issue deliberately leaves open, traces them to code, and sequences the work.

See issue #75 for motivation, user story, API surface, PWA requirements, key grammar, size limits, and acceptance criteria. This doc does not re-litigate any of that.

## Non-goals (from #75, restated)

- No multi-user / RBAC (single-tenant assumption).
- No auto-rotation / TTL.
- No external secret managers (Vault, SOPS, cloud KMS).
- No cluster-wide pool or cross-agent sharing.
- No hot-reload — pod always rolls on mutation.
- No separate audit store — control-plane INFO logs only.

## Architectural decisions

### 1. Mirror the `{name}-oauth` / `{name}-telegram` lifecycle

The controller already owns per-agent secrets with exactly the properties #75 requires: owner-ref'd to the Agent CR, survives pod recreation, GC'd on Agent deletion. `reconciler_test.go:1742-1790` already asserts that `{name}-oauth` and `{name}-telegram` are deleted when the Agent is deleted.

Both `{name}-user-secrets-kv` and `{name}-user-secrets-files` will follow the same pattern:

- Created eagerly (empty) during Agent reconciliation, before any pod is built.
- Owner reference on the Agent CR, not the pod.
- Extended cleanup test alongside the existing oauth/telegram lifecycle test — two more assertions, not a new test file.

No new secret flavor or lifecycle. This is load-bearing: it means the "accessible at all times" durability contract in #75 is automatically satisfied, because it's the same contract oauth/telegram already satisfy in production.

### 2. Pod-roll reuses `Spec.DesiredPhase = Running`

`routes_oauth.go:100-103` already has the pattern: after writing the secret, patch `agent.Spec.DesiredPhase = AgentPhaseRunning`. The state machine handles the restart; the existing brief-write path runs as a side effect, so agents get resume context on next boot. This is the "standard brief-write + restart path" referenced in #75.

`PUT /secrets/{key}` and `DELETE /secrets/{key}` will call the same code path. No new restart mechanism. No direct pod manipulation from the API layer.

### 3. Mount layout

Two mounts, both unconditional, each sourced from its own Secret (see Data model):

- **Env vars** — `envFrom.secretRef: {name}-user-secrets-kv` with `prefix: USER_`. Empty Secret → zero env vars added, which is fine.
- **File volume** — `Secret` volume mounted at `/user-secrets` from `{name}-user-secrets-files`. Each data key is a filename (`app_pem.bin`) so no `items:` projection is needed.

Because both Secrets always exist (decision 1), the pod builder can mount them unconditionally — no conditional-mount logic, no "exists vs doesn't" branching in `start-claude.sh`. This matches #75's "Accessible at all times" framing.

> **kv vs file — refresh semantics (kyber#514).** The two kinds refresh differently, and this distinction is load-bearing for short-lived rotated secrets (e.g. the externally-minted `FALCON_ISSUE_TOKEN`, fdc#10/#17):
> - **File-mode** is delivered live. The agent runs inside the `/merged` overlay chroot, and `entrypoint.sh` bind-mounts `/user-secrets` into it (alongside `/secrets`), so kubelet's atomic in-place updates to `{name}-user-secrets-files` are visible **without a pod roll**. A consumer should read the file **fresh per use** to pick up a rotation. (Before kyber#514 this bind was missing, so the agent saw only the empty boot-time overlay snapshot — file-mode user-secrets never reached the pod.)
> - **kv-mode** is **boot-time only**. `USER_*` env vars are projected via `envFrom` at pod start and do **not** refresh in place — a kv PUT to a live agent requires a **pod roll** to take effect. (The roll-on-mutation path must therefore actually recreate the pod for kv changes; see the `rollAgentForUserSecret` follow-up.)

### 4. Readback format

- **kv**: `Content-Type: text/plain; charset=utf-8`, body is the raw value.
- **file**: `Content-Type: application/octet-stream`, `Content-Disposition: attachment; filename={key.lower()}.bin`, body is raw bytes.

PWA fetches as blob either way and does click-to-copy or download in JS. Clean API, no guessing at content type.

### 5. Sequencing: two PRs

- **PR A (this work):** controller reconciliation + API + validation helper + tests. Operator can curl the API and see the secret mounted after a pod roll. Addresses acceptance criteria 1–7 of #75.
- **PR B (follow-up):** PWA Secrets tab. Mechanical once the API is stable. Addresses the PWA bullets of #75.

Rationale: the controller+API is the integration-heavy piece that benefits from shipping and getting used before the PWA locks in any shape. Landing them separately keeps review scope sane and lets us adjust the API based on real use before PWA lock-in.

## Components

### Controller — `pkg/controllers/agent`

- `reconciler.go` — add a reconciliation step that ensures both `{name}-user-secrets-kv` and `{name}-user-secrets-files` exist (empty on first create, left alone on subsequent reconciles) with the Agent CR as owner. Place this alongside whatever creates/ensures the existing `{name}-oauth` / `{name}-telegram` Secrets.
- `pod_builder.go` — unconditionally add:
  - `envFrom.secretRef` pointing at `{name}-user-secrets-kv` with `prefix: USER_`
  - Volume mount at `/user-secrets` from `{name}-user-secrets-files`
- `reconciler_test.go` — extend the existing secret-cleanup test to assert both Secrets are deleted when the Agent is deleted. Add a pod_builder test that asserts the envFrom + volume mount are always present.

### API — `pkg/api/routes_user_secrets.go` (new)

Four handlers, mirroring `routes_oauth.go` for style and the k8s client idioms:

- `PUT  /api/v1/agents/{name}/secrets/{key}` — upsert; kv (JSON body) or file (multipart)
- `GET  /api/v1/agents/{name}/secrets` — list keys with metadata (kind, size, sha256 prefix, created-at, updated-at)
- `GET  /api/v1/agents/{name}/secrets/{key}` — readback (see decision 4)
- `DELETE /api/v1/agents/{name}/secrets/{key}` — remove

All four: API-key auth via existing middleware. Mutations patch the Secret, then patch `Agent.Spec.DesiredPhase = Running`.

Route registration in `routing.go` next to the existing `/oauth` route.

### Validation helper — `pkg/usersecrets` (new small package)

Pure functions, no k8s dependency, fully unit-tested:

- `ValidateKey(key string) error` — grammar `[A-Z][A-Z0-9_]*`, length 1–64, rejects reserved prefixes `KYBER_` and `USER_`.
- `ValidateKVValue(value []byte) error` — size ≤ 64 KiB.
- `ValidateFileValue(value []byte) error` — size ≤ 64 KiB.
- `ValidateAggregate(entries map[string]int) error` — sum ≤ 256 KiB.

Keeping this in its own package means the route handlers stay thin and the rules are exercised directly in unit tests without spinning up an HTTP server or fake k8s.

### PWA — deferred to PR B

Listed here for completeness; implemented in the follow-up:

- New "Secrets" tab on `AgentDetail`.
- List: key, kind, size, sha256 prefix, timestamps.
- Add dialog: key name + value (textarea for kv / file picker for file).
- Delete button with confirm.
- Readback button: fetches as blob, shows modal with click-to-copy for kv or download link for file.

## Data model

Two Secrets per agent, same lifecycle (both owner-ref'd to the Agent CR, both eagerly created empty, both deleted on Agent deletion):

- `kyber-{agent}-user-secrets-kv` — data keys are the raw kv names (e.g. `FOO`), values are the raw values. Projected into the pod via `envFrom.secretRef` with `prefix: USER_`, so `FOO` becomes `$USER_FOO`.
- `kyber-{agent}-user-secrets-files` — data keys are the lowercased file names with `.bin` suffix (e.g. `app_pem.bin`), values are raw bytes. Projected via a `Secret` volume mount at `/user-secrets`, so `app_pem.bin` appears at `/user-secrets/app_pem.bin`.

The API layer routes PUT/GET/DELETE to the correct Secret based on the request's `kind`. Aggregate size limit (256 KiB) is computed across both Secrets together.

Metadata (created-at, updated-at, sha256 prefix per entry) stored in each Secret's `metadata.annotations` as a JSON blob. Annotations have a 256 KiB limit across all annotations; our per-entry metadata is ~100 bytes × 100 entries = 10 KiB worst case — well under the limit.

## Validation rules (summary)

- Key matches `^[A-Z][A-Z0-9_]{0,63}$`.
- Key does not start with `KYBER_` or `USER_`.
- Per-entry value ≤ 64 KiB.
- Aggregate across all entries ≤ 256 KiB (computed pre-write).
- Over-size / bad-grammar / reserved-prefix → 400 with specific error code.

## Test plan

Unit:

- `pkg/usersecrets` — exhaustive grammar + size tests.
- `routes_user_secrets_test.go` — happy paths for PUT (kv + file), GET list, GET readback, DELETE. 400s for bad grammar, reserved prefix, over-size, missing agent. Asserts `Spec.DesiredPhase = Running` gets patched on mutation.
- `pod_builder_test.go` — envFrom + volume mount always present.
- `reconciler_test.go` — Secret eagerly created; Secret deleted on Agent deletion (extend existing oauth/telegram cleanup test).

Integration (if there's a harness; see Open Questions):

- Create agent → PUT kv secret → pod rolls → `$USER_FOO` visible in pod.
- Create agent → PUT file secret → pod rolls → `/user-secrets/app_pem.bin` readable.
- Delete agent → Secret is gone.

## Acceptance mapping

Every checkbox in #75's Success Criteria:

- [x] kv via PWA → `$USER_FOO` — API covers this in PR A; PWA in PR B.
- [x] file via PWA → `/user-secrets/app_pem.bin` — same.
- [x] list never returns values — explicit in `GET /secrets` handler; test asserts.
- [x] secret survives pod restart / agent restart / preemption / wake — inherited from the oauth/telegram lifecycle; test extended.
- [x] secret destroyed on agent deletion — inherited; test extended.
- [x] #71 GH App PEM injection could be re-done through this API — verified by acceptance: PEM as file secret with key `GITHUB_APP_PEM`.
- [x] over-size / bad-grammar → 400 — `pkg/usersecrets` + handler tests.
- [x] reserved-prefix → 400 — `pkg/usersecrets` test.

## Open questions

1. **envFrom prefix projection for mixed-kind Secret.** ✅ **Resolved 2026-04-18:** going with option (a) — two Secrets per agent, `{name}-user-secrets-kv` (envFrom-projected) and `{name}-user-secrets-files` (volume-projected). Clean separation; the extra Secret per agent is cheap.
2. **Integration test harness.** ✅ **Resolved 2026-04-18:** unit tests only. Follow existing test patterns in the kyber repo (`reconciler_test.go`, `pod_builder_test.go`, `routes_*_test.go`). No new envtest/kind harness.
3. **Annotation metadata vs. separate ConfigMap.** Agreed — annotation JSON blob is fine for V1. Flagged for later if we ever want to list metadata without Secret-read RBAC.

## Implementation order (PR A)

1. `pkg/usersecrets` validation package + tests.
2. Controller reconciliation change — eagerly create both `{name}-user-secrets-kv` and `{name}-user-secrets-files` + cleanup on Agent deletion + reconciler_test extension.
3. Pod builder change — unconditional envFrom (from `-kv` Secret) + volume mount at `/user-secrets` (from `-files` Secret) + pod_builder_test.
4. `routes_user_secrets.go` + tests + routing registration.
5. Wire `DesiredPhase = Running` patch on every mutation.
6. Manual smoke: `curl` PUT / GET / DELETE against a local control plane pointing at k3s; verify env var and file appear in pod.
