# Reviewing — reviewer gotchas & focus map

> **Update cadence: append-on-discovery.** This is a living map of where review
> attention pays off — fragile areas, known landmines, and past-incident traps.
> A reviewer appends an entry whenever a new landmine surfaces in review or QA.
> It grows organically; that's the point — the project compounds hard-won
> knowledge instead of relearning it. For forward-looking code standards, see
> [code-quality.md](code-quality.md).

**Location note:** this file lives at the **repo root** (not `.github/`) on
purpose — it sits beside its sibling [code-quality.md](code-quality.md) so the
working-knowledge docs are discovered together, and because it's edited often
(append-on-discovery) by reviewers rather than being GitHub machinery.

## How to use this during review

For each PR, scan the changed paths against the **fragile areas** below. When a
diff touches one, don't just read the diff — read it *against the trap*. Each
entry names the area and the specific failure mode to look harder at, not a
one-word label. This file is the *reviewer's* lens — "where to aim attention."

---

## Seeded gotchas

### 1. Sidecar image drift: `imageID` (digest) vs spec-string

- **Fragile area:** `pkg/controllers/agent/reconciler.go` — `isSidecarDrifted` /
  `isSidecarSpecMismatched`, and anything comparing container images for "is
  this pod on the right image?"
- **The trap:** kubelet reports a pod's **per-platform manifest digest** in
  `status.containerStatuses[].imageID`. The controller env
  (`KYBER_STATUS_SIDECAR_IMAGE`) is usually a **multi-arch image index** whose
  digest is the index/manifest-list digest. **These two digests are never equal
  for a multi-arch image** (index digest ≠ amd64 child digest), so any
  digest-vs-digest drift check false-positives forever — and with auto-roll on,
  rolls the whole fleet every stability window. Fixed in **kyber#371 Defect B**
  (origin kyber#299) by comparing the **spec image string**
  (`Spec.Containers[sidecar].Image`) on both ends, which derive from the same
  env. **Look harder at:** any new code that reaches for `imageID`,
  `containerStatuses`, or a registry digest to decide image equality — that's
  the foot-gun re-opening. Image equality here is a *string* comparison, not a
  digest comparison.

### 2. `helm --reuse-values` silently carries stale config

- **Fragile area:** any chart change to `deploy/helm/kyber/` that adds or
  changes a value's default; upgrade runbooks/scripts.
- **The trap:** `helm upgrade --reuse-values` re-applies the **previously-set**
  values and **does not pick up new chart defaults or newly-added keys**. A PR
  that introduces a new value or changes a default can look correct in the chart
  and still be inert on any release that was last upgraded with `--reuse-values`
  — the new behavior never reaches the cluster. **Look harder at:** chart PRs
  that rely on a new/changed default taking effect. Confirm the rollout path
  doesn't `--reuse-values` past it; the audit command is `helm get values
  <release>`.

### 3. `preview.enabled` chart values must never target `kyber-system`

> **Note (kyber#531):** the per-PR preview-cluster system — the
> `kyber-pr-<n>` ArgoCD Applications and the preview ApplicationSet in the
> deploy repo — is **retired**. Don't go hunting for that ApplicationSet; it no
> longer exists. The dev-test model is now a single shared **devenv-kyber**
> instance (deploy-to-dev → test-in-dev → dev-lock); see
> [`docs/operator/kyber-dev-verification.md`](../operator/kyber-dev-verification.md).
> The guard below is still a **live chart fence**, however — the
> `preview.enabled` value path remains in the chart and must stay fenced.

- **Fragile area:** the `preview.*` value path in `deploy/helm/kyber/`,
  specifically `templates/namespace.yaml`; any chart change that lets a
  `preview.enabled` render own a `Namespace` or other cluster-scoped/shared
  object.
- **The trap (the original incident):** on 2026-04-19 a preview rendered its
  stack into `kyber-system`; when ArgoCD later pruned that render it **cascaded
  into prod** — deleting the shared `kyber-system` namespace wiped prod. **PR
  #115** fenced this: `templates/namespace.yaml` now **`fail`s the render** if
  `preview.enabled=true` resolves the namespace to `kyber-system`. That fence is
  still in the chart today and is the real review point. **Look harder at:** any
  chart diff that removes or weakens the `namespace.yaml` `fail` guard, broadens
  what a `preview.enabled` install templates, or lets a preview render own a
  `Namespace`/cluster-scoped resource — regardless of which env (if any) ever
  installs with `preview.enabled=true`.

### 4. `fireAndForget` drops the ack (and the status update)

- **Fragile area:** node-agent **action dispatch** — the `fireAndForget` send
  path. (Distinct from the control-plane inbound dispatcher in `pkg/inbound/`,
  which has its own completion-ack contract.)
- **The trap:** a `fireAndForget` dispatch **skips both the result POST and
  `pushStatus`** — by design, for *terminal* actions (delete, log out) where no
  one is waiting on a follow-up. Use it on a **non-terminal** verb and the
  action's result/status update is silently dropped: the control plane never
  learns the outcome and the agent's status goes stale. **Look harder at:** any
  new caller of `fireAndForget`, or a verb being moved onto that path — confirm
  the action is genuinely terminal. Non-terminal work must go through the
  acking path.

### 5. GitGuardian "Generic High Entropy Secret" false positive on `AgentSecrets{AuthType: "oauth"}`

_Added 2026-06-01 — first observed kyber#361, recurred #368, #380._

- **Fragile area:** any test or production code containing `AgentSecrets{…}`,
  `AuthType:`, or the string `"oauth"` in a struct literal — particularly in
  `pkg/inbound/`, `pkg/api/`, and their test files.
- **The trap:** GitGuardian flags the pattern as a "Generic High Entropy Secret"
  on every PR that touches it. It is a **confirmed false positive** — `"oauth"`
  is an enum value for auth-type, not a credential. A reviewer who sees a
  GitGuardian failure and blocks on it without checking context wastes a full
  review cycle. **Look harder at:** the diff context around the alert line. If
  the flagged string is `AuthType: "oauth"` (or a similar typed literal), close
  the finding as false positive and continue. Only block if the flagged value
  is actually a token-shaped string (long hex, base64, etc.) that doesn't
  belong in code.

### 6. `pwa-views` source change without a version bump never publishes

_Added 2026-06-01 — caused review holds across kyber#336 / #337 / #341._

- **Fragile area:** `packages/pwa-views/src/` — any component, hook, or type
  change.
- **The trap:** the npm workspace link (`workspace:*` in `apps/embedded-pwa`)
  means **local development sees the change immediately**. The Holocron host
  (`matty-v/holocron`) consumes the *published* version from GHCR npm —
  anything in `packages/pwa-views/` that doesn't pair with a `package.json`
  version bump and a publish run is **invisible in production**. A PR that
  changes components but doesn't bump the version ships nothing to Holocron.
  **Look harder at:** any `packages/pwa-views/src/` diff without a matching
  `packages/pwa-views/package.json` version bump. If both are absent, the
  change is incomplete — hold until they're paired. See
  [CONTRIBUTING.md](../../CONTRIBUTING.md) for the publish checklist.

### 7. Reconciler step ordering: 5c and 5d share a concurrency budget, 5c runs first

_Added 2026-06-01 — observed during kyber#371 design review._

- **Fragile area:** `pkg/controllers/agent/reconciler.go` — the ordering of the
  `maybeAutoRollSidecarForDrift` (Layer 5c) and `convergeSidecarImage` (Layer
  5d) calls inside `Reconcile()`.
- **The trap:** 5c and 5d share `sidecarAutoRollDefaultMaxConcurrent=1` via
  `countAgentPodsBeingDeleted`. 5c runs first by design: if a drift-triggered
  auto-roll fires and deletes a pod, 5d's concurrency check sees one pod
  already deleting and defers. If a PR **moves 5d before 5c**, both can fire
  in the same reconcile cycle for the same agent (neither has observed the
  other's delete yet), effectively doubling the rollout rate and bypassing
  the cap entirely — the fleet gets two simultaneous deletes instead of one.
  The call ordering in `Reconcile()` is **load-bearing**. **Look harder at:**
  any reordering of the 5b–5d block, or a new reconcile step inserted between
  them that triggers a pod delete.

---

## Cross-cutting review reflexes

- **API ↔ PWA type parity is hand-verified.** There is no OpenAPI→TS codegen.
  On any API-shape change, read the Go shape, `test/contract/openapi.yaml`, and
  the hand-written PWA type side-by-side. See
  [code-quality.md](code-quality.md) § Wire-contract round-trip.
- **PWA source changes need a version bump** (`packages/pwa-views/`) or the
  change never publishes to Holocron — [CONTRIBUTING.md](../../CONTRIBUTING.md).
- **Subagent-written docs invent plausible claims.** If a PR's prose came from a
  subagent, grep the diff and cross-check every concrete claim (paths, function
  names, line refs) against current code before approving. (This very file's
  seed was grounded that way.)

### 8. Boot script `git config` on multi-valued keys (start-claude.sh)

_Added 2026-06-02 — surfaced reviewing #424 (kyber#418)._

- **Fragile area:** `images/claude-code/start-claude.sh` — any `git config --global` set on a key that git treats as multi-valued (e.g. `credential.*.helper`, `core.hooksPath` in some versions, `url.*.insteadOf`).
- **The trap:** git's `credential.helper` is a multi-valued key. A bare `git config --global <key> <value>` fails with "cannot overwrite multiple values with a single value" if the persisted `~/.gitconfig` on the PVC already holds more than one value for that key (e.g. left by an older boot revision that used `gh auth setup-git`, which writes an empty reset + the gh helper). Under `set -euo pipefail` (line 2 of the script), this non-zero exit **crashes boot** and the agent is stuck `Failed` with no auto-recovery because the bad config persists on the PVC. **Look harder at:** any new `git config --global` set added to boot for a potentially multi-valued key — it must use `--replace-all` (atomic collapse to one value) or `--unset-all … || true` + bare set. The inline comment at the current credential-helper block (line ~153) explains the requirement; enforce it on any new sibling set.

### 10. Centralized desired-phase kill switches in classifyEvent must come before the per-phase switch

_Added 2026-06-06 — surfaced reviewing #473 (kyber#468)._

- **Fragile area:** `pkg/controllers/agent/reconciler.go` — `classifyEvent`, and any future desired-phase kill switch (operator-forced re-auth #395, authoritative Stop #468, or any sibling).
- **The trap:** the `Failed` phase arm (`:640+`) returns `EventAutoRestartTriggered` before any later check in the per-phase switch runs. A desired-phase guard placed *inside* the `Failed` (or `MemoryExhausted`) arm would be pre-empted: the kill switch fires first, the guard is never reached, and a crash-looping agent keeps auto-restarting even with the desired phase set. **The fix and the invariant:** centralized desired-phase blocks (NeedsAuth at `:570`, Stop at `:598`) sit *ahead of* the per-phase switch, with an explicit allowlist. Any new operator-intent kill switch must follow the same pattern — centralized block, explicit allowlist, placed before the per-phase and pod-state switches — or it will silently fail on crash-loop phases. **Look harder at:** any new `if desired == <phase>` block added anywhere other than the top of `classifyEvent` (after the NeedsAuth/Stop blocks), and confirm the allowlist excludes transient/cleanup phases and has the right stable-fixed-point semantics.

### 9. Tier-1 windowed panel functions must be authoritative on empty results

_Added 2026-06-03 — surfaced reviewing #429 (kyber#428)._

- **Fragile area:** `pkg/api/routes_metrics.go` — any function that backs a Metrics panel via a Tier-1 Redis time-series read (e.g. `tokenUsageWindowedFromMetricsStore`, and any future sibling for `/metrics/state-changes` or a new panel).
- **The trap:** if the Tier-1 read function returns `(nil, false)` when its window yields zero rows, the caller falls through to Tier-2 (Prometheus) and **resurrects all-time or recent data** — the exact same bug this PR fixed. The correct contract for any authoritative Tier-1 source is: return `(result, true)` even when `result` is an empty slice, as long as the store is the authoritative source (MetricsStore configured + accumulator shows data has ever existed). Only return `(nil, false)` when *no token/panel data has ever been written* (empty accumulator), which allows a never-populated cluster to still fall back to Prometheus. `activityFromMetricsStore` uses `(nil,false)` on empty and was written before the authoritative-empty requirement was understood — do NOT copy that pattern for new windowed sources. **Look harder at:** any new Tier-1 read function returning `(nil, false)` — confirm the `false` case is strictly "no data has ever existed" and not "this window happens to be empty."

### 10. Per-agent infrastructure ensures belong in `createPod`, not just in birth-time actions

_Added 2026-06-06 — surfaced reviewing #475 (kyber#467)._

- **Fragile area:** `pkg/controllers/agent/reconciler.go` — any `ensure*` call that provisions a resource the pod spec references as a volume, init container, or env source.
- **The trap:** the agent reconciler has two categories of pod-creation paths. **Birth-time paths** (`ActionCreatePVAndPod`, `ActionCreatePV`) run explicit `ensure*` calls before the pod is created — for new agents this is fine. **Recreation paths** (`ActionWriteBriefAndCreatePod`, `ActionResetRetryAndCreatePod`) call `createPod` directly without any of those birth-time ensures — they assume the resource already exists. For any resource introduced *after* an agent is already running (e.g. the offsets PVC added in kyber#467), pre-existing agents have no such resource, and the first pod recreation after the controller rolls out references a non-existent volume → pod stuck Pending forever with no auto-recovery. The fix pattern: **place the ensure inside `createPod` itself** (idempotent Get-then-Create), so every pod-creation path is covered regardless of whether it's a birth or a recreation. **Look harder at:** any PR that adds a new volume, secret, or ConfigMap to the pod spec via `BuildPodSpec` / `AppendTranscriptTailer` / `AppendStatusSidecar` — confirm the corresponding ensure is in `createPod`, not only in the birth-time action cases.

### 11. classifyEvent runs two per-phase switches: an early `return` in the first pre-empts pod-death detection in the second

_Added 2026-06-09 — surfaced reviewing #527 (kyber#523)._

- **Fragile area:** `pkg/controllers/agent/reconciler.go` — `classifyEvent`. After the centralized kill-switch blocks (see #10) it runs **two** sequential switches on `agent.Status.Phase`: first the *desired-state* switch (`case Running:` handles preemption, desired Restarting, and now the #523 runtime-image-drift roll), then a *pod-state* switch (`case Running:` handles dead/terminated pods → `PodDied`/`OOMKilled`/`OAuthRefreshFailed`).
- **The trap:** any `return <event>` added to the **first** switch's `case Running` fires *before* the second switch's pod-death detection ever runs. The #523 drift block (`if isAgentRuntimeImageDrifted(...) return EventDesiredRestarting`) does exactly this — so a pod that is **both drifted and dead in the same reconcile** is rolled as `Restarting` instead of being classified as `PodDied`/`OOMKilled`/`OAuthRefreshFailed`. In practice it self-corrects within one cycle (the recreated pod is on the new image → no drift → death is caught on the next pass, including the `Starting`-phase OOM/OAuth guards), so it's low-severity — but it is a real ordering coupling. **Look harder at:** any new early `return` added to the first switch's `case Running` (or `case Starting`) — confirm it should win over a simultaneously-dead pod, or guard it (`pod.Status.Phase == PodRunning && !isAgentContainerTerminated(pod)`) so death detection keeps precedence. The two switches are not independent; the first short-circuits the second.

### 12. build.yml on `pull_request` events: `github.sha` is the synthetic merge commit, and push-event context vars are empty

_Added 2026-06-10 — surfaced reviewing #547 (kyber#544)._

- **Fragile area:** `.github/workflows/build.yml` — anything that tags, keys, or checks out code on `pull_request` events; any consumer that matches an image tag against a PR (deploy-to-dev #533, the stale-deploy guard kyber#534).
- **The trap:** on `pull_request` events `github.sha` is the **synthetic merge commit**, not the branch head — an image tagged or checked out with it will never match `pr.head.sha`, which is what deploy-to-dev and the stale-deploy guard key on (this exact confusion produced the wrong line-214 claim in the #533 skill text). Use `github.event.pull_request.head.sha` for both the checkout `ref:` and the tag. Second, push-only context fields are **empty** on `pull_request` runs: `github.event.head_commit.timestamp` (BUILD_DATE today) silently expands to "". **Look harder at:** any workflow change that adds a `pull_request` path to a formerly push-only job — audit every `github.sha` / `github.event.head_commit.*` / `github.ref` reference in the job for which event populates it.

### 13. start-claude.sh runs under `set -euo pipefail`: an unguarded command substitution kills boot, and a fallback on the next line is dead code

_Added 2026-06-10 — surfaced reviewing #546 (kyber#542); repro + fix tracked in kyber#548._

- **Fragile area:** `images/claude-code/start-claude.sh` — every plain `VAR=$(cmd)` assignment, especially git queries against the persisted identity-repo clone whose state is not script-controlled.
- **The trap:** under `set -e` a failing command substitution in a plain assignment **exits the shell**, and under `pipefail` a leading `git ... | sed ...` pipeline fails even though sed exits 0 — so a `${VAR:-default}` on the *next line* is unreachable for exactly the failure it was written for (the #546 `DEFAULT_BRANCH`/`origin/HEAD` case: boot dies on a clone with unset origin/HEAD or an empty identity repo, verified empirically). The existing code's idiom is the tell: every fallible boot step is `|| echo "[kyber] WARNING ..."`-guarded — a bare substitution next to those is the regression. **Look harder at:** any new `VAR=$(...)` in this script — require the guard inside the substitution (`VAR=$(cmd || true)`) or an explicit `|| fallback` on the same statement; a default-expansion on a later line proves nothing.

### 14. provider-rates refresh PRs: "regeneration-only" is provable byte-for-byte, and the chart refuses to render without image-tag stubs

_Added 2026-06-10 — surfaced reviewing #553._

- **Fragile area:** `deploy/helm/kyber/files/provider-rates.generated.{yaml,meta}` refresh PRs (the weekly refresh-bot or manual reruns of `cmd/fetch-provider-rates`).
- **The trap(s):** (1) Eyeballing the diff cannot prove the no-manual-cost-data rule held — but the generator is deterministic given the same inputs, so you don't have to: fetch the feed at the meta's `upstream_commit`, run `go run ./cmd/fetch-provider-rates -in <feed.json> -commit <sha> -now <fetched_at from meta>` and `diff` against the committed files. Byte-identical output = provably regeneration-only; any difference = a hand-edit. (2) A bare `helm template` to verify the ConfigMap renders the new model **fails before reaching the rates template** — `node-agent/daemonset.yaml` hard-refuses missing image tags (kyber#358/#457). Stub all five (`--set image.{controlPlane,nodeAgent,statusSidecar,runtimeBase,claudeCode}.tag=x`) or render just the one template with `-s templates/configmap-rates.yaml`. (3) The image-swap deploy cannot exercise helm-files-only PRs, and since kyber#554 (2026-06-10) the test-in-dev deployable filter excludes `deploy/helm/**` — they route straight to code review with no dev deploy; record test-in-dev N/A and rest the verdict on the regeneration proof + the `preflight-model-pricing.sh` gate + CI; runtime confirmation belongs to post-merge delivery.

### 15. Webhook/auth-contract changes must migrate `test/integration` too — it's Redis-gated and CI-only, so `go test ./pkg/api/...` won't catch the break

_Added 2026-06-14 — surfaced reviewing #567 (kyber#564)._

- **Fragile area:** any change to the auth contract of a route (API-key wall, secret validation, header requirements). Mirror tests can live in **two** places: unit tests in `pkg/api` (fake k8s, no Redis) **and** integration tests under `test/integration/` (real Redis via `sharedRDB`, gated behind the CI `integration` job).
- **The trap** (surfaced on the since-removed Telegram webhook routes): `test/integration`'s server builder and request helpers were written against the old auth behavior, so when kyber#564 flipped an empty secret to **fail closed**, the integration tests broke — while the PR's updated unit suite was green. A builder who runs only `go test ./pkg/api/...` locally sees all-green and ships a red `integration` check, because `test/integration` needs a Redis the local run doesn't have. **Look harder at:** any auth-contract change to a route — grep `test/integration` for the same route/helper and confirm both the unit AND the integration harness were migrated; as reviewer, never take a local-`pkg/api`-green claim as proof the suite passes — check the `integration` CI check (or run `go test ./test/integration/...` with Redis) before a `merge: yes`.

### 16. Regional scale-from-zero needs scheduler demand after Agents park

- **Fragile area:** `pkg/controllers/machine/capacity_request.go`, the Agent
  watch in `pkg/controllers/machine/controller.go`, and provider implementations
  of `CapacityNeedsSchedulerDemand`.
- **The trap:** GKE's configured autoscaling minimum does not proactively grow
  a pool from zero. It scales only for unschedulable Pod demand, while the Agent
  recovery contract intentionally deletes stale Agent pods in
  `WaitingForMachine`. Removing the Machine-owned capacity-request Pod, failing
  to enqueue the Machine when Agent intent changes, or marking a
  scheduler-driven provider as not needing demand wedges every new Agent on a
  zero-node pool indefinitely. Keep the request Pod credential-free and scoped
  to the provider's node selector; delete it as soon as capacity attaches or
  active demand disappears. The PWA must consume
  `compute.managed.capabilities.requiresSchedulerDemand`; inferring this from
  a location string diverges from provider configuration (for example, a GKE
  pool can have a regional location but only one configured node location).

### 17. Automatic pod rollouts must persist restart intent before deletion

- **Fragile area:** automatic convergence helpers in
  `pkg/controllers/agent/` and `countAgentPodsBeingDeleted`.
- **The trap:** deleting a Running Agent pod directly leaves its CR in
  `Running`. A pod watch can observe the missing pod first, derive `PodDied`,
  transition to `Failed`, increment `restartCount`, and impose crash backoff on
  a healthy agent. This occurred during status-sidecar and Telegram convergence
  on kyber-datawire.
- **The invariant:** only a currently `Running` Agent may persist
  `spec.desiredPhase=Restarting`; use optimistic locking and return. The next
  reconcile uses the normal `DesiredRestarting` transition, records
  `Restarting`, and owns deletion. Count persisted restart requests as rollout
  reservations. Arm a convergence canary only after that persistence succeeds,
  or conflicting operator intent creates a phantom canary that can never be
  verified. Never add an out-of-band `r.Delete` to a convergence helper.
