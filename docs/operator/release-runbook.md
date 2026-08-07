# Kyber Release Runbook

This document describes the release process for Kyber, including which steps are automated by CI and which require the operator to act. The upstream project drives several steps through a private team of release-automation agents; where a step depends on that private setup it is marked operator-specific — a fork can perform the same step by hand.

---

## Overview

The full release flow is **merge-triggered, not tag-triggered**. A kyber PR merge produces these steps:

1. **The canary cluster is smoke-tested** *(operator-specific automation)*. The smoke-test agent polls the canary instance's `/api/v1/version` until it is running the merge SHA, then runs three smoke checks (`/api/v1/version`, `/api/v1/agents`, `/api/v1/metrics/summary`).
2. **On green-light: a release is proposed to the operator** (e.g. via a chat channel). The proposal includes the bump (computed from Conventional Commits) and the commits since the last tag.
3. **The operator approves with `release approve`.** The release-automation agent then:
   - **Dispatches `prepare-release.yml`** (`workflow_dispatch`, `version=X.Y.Z`) on `matty-v/kyber`. This workflow — **not** an ad-hoc `git tag` push — owns the tag-cut now (kyber#591): it folds the `Chart.yaml` `version`/`appVersion` bump **into the commit that gets tagged**, then pushes the annotated tag `vX.Y.Z` on that merged commit (see [Cutting a release](#cutting-a-release-manually) below).
   - Watches `release.yml` in CI
   - Watches Image Updater on falcon for the bump

`prepare-release.yml` performs the **pre-tag** chart bump, in order:

- **P1 — Bumps `deploy/helm/kyber/Chart.yaml`** `version` + `appVersion` to `X.Y.Z` (bare, leading `v` stripped, kept in lockstep) on a branch, opens a PR on `matty-v/kyber`, and auto-merges it with `gh pr merge --auto` (waits for `main`'s required `[test, pwa-build, changes, integration]`, ~15min — a `Chart.yaml`-only PR satisfies all four). Idempotent: if `main` is already at `X.Y.Z`, no PR is opened and the current HEAD is tagged. *(This relocates the retired post-tag `chart-version-bump-pr` job — folding the bump into the tagged commit is what gives the canary a clean `git describe`; kyber#591.)*
- **P2 — Pushes the annotated tag `vX.Y.Z`** on the merged commit, after verifying the chart version at that commit matches `X.Y.Z` (fail-loud otherwise).

When the tag push reaches `release.yml`, CI then:

4. **Builds all 8 images** in parallel (control-plane, node-agent, status-sidecar, mcp-discord, mcp-telegram, runtime-base, claude-code, codex) and pushes them to GHCR as `:vX.Y.Z`. (`claude-code` and `codex` build `FROM` runtime-base, so they wait on it.)
5. **Refreshes the control-plane `:latest` tag** to this release's image (kyber#591) so **the canary cluster** (which tracks `:latest` via ArgoCD Image Updater) reports the clean release label immediately instead of a stale pre-tag `git describe` string. Re-points `:latest` to the just-pushed `:vX.Y.Z` digest — no rebuild. **Guarded:** only moves `:latest` when the release commit is still `origin/main` HEAD, so a `main` merge racing the ~10-min build can never move the canary backward. Skipped for `-test` tags.
6. **Creates a GitHub Release** with an auto-generated changelog.
7. **Fires the release notification webhook** via the `release-notify` inbound binding (HMAC-signed) *(operator-specific — the upstream setup uses it to log the release, clear `pending-release.json`, and kick off release-notes drafting; safe to leave unconfigured)*.
8. **Chains pwa-views publish** — pushes a `pwa-views/vX.Y.Z` tag which triggers `publish-pwa-views.yml` to build and publish `@matty-v/kyber-pwa-views` to GitHub Packages. *(Automated — the build + Release must succeed first.)*
9. **Resolves all 8 GHCR manifest digests once** (`resolve-digests`), then **opens AND auto-merges a deploy-repo bump PR per production cluster** on `matty-v/kyber-deploy` — a `matrix: cluster: [falcon, gcp]`, both legs pinning the same digests. *(Automated — falcon and GCP both promote automatically once the release is cut. Each PR is opened for the audit trail, then squash-merged immediately.)* `fail-fast: false` keeps the legs atomic: a GCP failure does not strand falcon, or vice-versa. Each leg emits `cluster-promoted` / `cluster-promote-failed` to the release-notification inbound so per-cluster progress shows up in chat without opening CI *(operator-specific)*.

Steps 8 and 9 run in parallel after the GitHub Release is created. **The chart-version bump no longer runs here** — it now precedes the tag in `prepare-release.yml` (P1 above), which is what makes the canary's `git describe` resolve to the clean tag (kyber#591).

**Where the human gate actually is:** it's **the operator's `release approve`, before the tag is cut** — not a second promotion step afterwards. Once the tag exists, both production clusters take the release automatically.

> **Superseded:** GCP used to be manual-promote — an intentional second blast-radius checkpoint, where an operator copied the bump from falcon's PR into GCP's `values.yaml` by hand. In practice it just rotted: GCP sat on v1.0.0 while falcon tracked v1.7.x. kyber#449 folded GCP into the same matrix. To hold a cluster back now, pin its `environments/<cluster>/values.yaml` digests by hand and revert the bump PR — there is no hold workflow.

**Why both Image Updater AND the bump PR:** Image Updater (faster, write-back to Application spec) is the immediate fast path. The bump PR (slower, git audit trail) lands the same value with an auditable diff. They produce the same value in steady state; the PR is for "what version was running on falcon last Tuesday" archaeology.

---

## How the displayed version is derived

The version users see — the `chartVersion` field of `GET /api/v1/version`, surfaced in the app nav menu, Settings → Diagnostics, and Holocron — is **baked into the control-plane image at build time** (kyber#482), exactly like the `sha` and `buildDate` fields. It is injected via `-ldflags "-X main.Version=…"` and read once at boot by `resolveDisplayVersion` (`cmd/control-plane/version.go`):

- **Release images (`:vX.Y.Z`, run by falcon + gcp):** `release.yml`'s `meta` job sets `BUILD_VERSION` to the release tag with its leading `v` stripped — a bare `X.Y.Z`.
- **Canary image (`:latest`, run by the canary cluster):** `build.yml`'s `push-control-plane` job sets `BUILD_VERSION` from `git describe --tags` (leading `v` stripped). **At a release commit this is the clean tag** — e.g. `2.2.0` — because the chart bump is folded into the tagged commit (kyber#591), so a build there describes to the bare tag. **For genuine dev commits past the release** it is `2.2.0-3-gabc1234`: the canary runs *ahead* of releases, and that `-N-gsha` suffix is the canary's honest commit offset past the last tag, not a bug. The canary picks up the clean release label immediately because `release.yml` refreshes the `:latest` tag to the release image at tag time (see step 5 in [Overview](#overview)); before kyber#591 the canary could sit indefinitely on a stale pre-tag describe (`2.1.1-7-gd64fbbd` after v2.2.0) because no canary build had run since the tag.
- **Local / dev / preview builds (no ldflag):** `resolveDisplayVersion` falls back to the chart-rendered `/etc/kyber/chart-version` file (`readChartVersion`), or `""` if that isn't mounted (the PWA renders `—`).

**Convergence timing (expected, bounded).** Because the version rides inside the image alongside `sha`, the displayed version converges **exactly when the new image rolls out — one deploy/sync cycle**, the same instant `sha` changes. There is no separate clock and no second source: a cluster can never sit in a steady state where `sha` reflects release `vX.Y.Z` but the displayed version still reads `X.Y.(Z-1)`. The only transient window is the image rollout itself (image pull + pod restart, typically a couple of minutes); once the new pod is `Ready`, the new version is live. "Deployed but version not yet updated" is therefore bounded to that rollout window, not an indefinite trailing state.

This replaced the earlier chart-rendered derivation, which lagged by exactly one release: the image deploy (the ArgoCD sync trigger) always beat the ~15-min-gated chart-version bump merge to `main`, so each cluster re-rendered the version that was on `main` one release ago. The `Chart.yaml` `version`/`appVersion` advance is **retained for Helm/ArgoCD operator metadata only** (`helm list`, the ArgoCD UI) and is off the user-facing path — but since kyber#591 it happens **before** the tag (in `prepare-release.yml`, folded into the tagged commit), not in a separate post-tag PR. Cutting the tag on the bumped commit is also what makes the canary's `git describe` resolve to the clean tag.

**`git describe` requires tags at build time.** The `:latest` (canary) version depends on `git describe --tags` resolving in CI, which requires the `push-control-plane` checkout to fetch tags (`fetch-depth: 0`). If a future change reverts that to a shallow checkout, `git describe` returns empty and the build **fails loudly** (by design) rather than silently falling back to the lagging chart version — a guard against re-introducing kyber#482 on the canary.

---

## Cutting a release manually

The normal path is driven by the release automation (per Overview above). The **preferred manual path** — also the right one when that automation is down or absent — is the same workflow it dispatches — run `prepare-release.yml` yourself, so the chart bump still lands inside the tagged commit (kyber#591):

```bash
gh workflow run prepare-release.yml -R matty-v/kyber -f version=1.2.3
```

This bumps `Chart.yaml` to `1.2.3`, auto-merges the bump PR, and pushes `v1.2.3` on the merged commit (which then triggers `release.yml`).

**Raw tag push (last resort only — Actions outage, etc.).** If you must push the tag by hand, you are responsible for ensuring `Chart.yaml` `version`/`appVersion` is already at the release version **on the commit you tag** — otherwise the canary's `git describe` will resolve to a stale pre-tag string and `chartVersion` will be wrong on the canary (the exact kyber#591 bug). The CI guard `scripts/release-version-guard_test.sh` documents the invariant; it does not enforce it on a raw push.

```bash
# Only after confirming deploy/helm/kyber/Chart.yaml is at 1.2.3 on this commit:
git tag -a v1.2.3 -m "Release v1.2.3 — brief description"
git push origin v1.2.3
```

The annotated tag message is used for the `[skip-publish]` opt-out check (see below). For a normal release, the message is free-form.

---

## Tag immutability

**Published semver tags are immutable.** Once `release.yml` has built and pushed the 8 kyber-* images at `vX.Y.Z`, that tag must never be re-pushed under a different digest. If a release is broken, cut a new patch (`vX.Y.(Z+1)`) — never re-publish the same tag.

**Why:** kubelet's `IfNotPresent` cache keys on `(repo, tag)`. When a tag is deleted and re-pushed with a new digest, long-lived nodes that already pulled the original blob stay pinned to it forever — the cache entry never expires by itself, and ArgoCD's manifest-only sync check compares spec strings (which still match), so the drift is invisible. kyber#364 documents the failure mode: `kyber-falcon-node-agent` ran the `v1.2.0` digest under a `v1.3.3` spec for hours because the `v1.3.3` tag had been deleted and re-pushed at some point.

**Enforcement:** `release.yml` runs a `preflight-check-tags` job before any image build. For each of the 8 kyber-* images, it queries GHCR and fails the workflow if the target tag is already published. Re-push attempts get a fail-fast error with a pointer to this section. The guard is bypassed for `-test` tags so dev iteration can continue re-pushing.

**If you actually need to overwrite a release** (e.g., a leaked secret in the published artifact): yank both the GHCR tag and the GitHub Release manually, then cut the next patch. Don't try to re-occupy the same semver.

---

## Post-release manual checklist (operator-driven)

These steps require cluster credentials and/or are intentionally gated on human review. Do them after CI confirms steps 7 and 8 above have completed.

- [ ] **M1.** The falcon bump PR now **auto-merges** (falcon promotes automatically on release). No merge action needed; just confirm it landed: the `matty-v/kyber-deploy` PR opened by step 8 should already be merged, with each image `tag@sha256:...` matching the GitHub release page (matty-v/kyber Releases → `vX.Y.Z` → image manifests). To roll falcon back, revert the bump commit on `main`.
- [ ] **M2.** ArgoCD reconciles automatically once the PR merges; verify the kyber-falcon Application is `Synced + Healthy` (`argocd app get kyber-falcon` or check Holocron). NEVER `helm upgrade` directly — that would conflict with ArgoCD's reconciliation loop.
- [ ] **M3.** Verify deployment: `kubectl logs` on a control-plane pod should show startup messages consistent with the new version (e.g., Redis store enabled, feature flags applied).
- [ ] **M4.** (If you run a Holocron hub) confirm the cluster's Metrics tab panels populate within ~30 seconds.
- [ ] **M5.** (Optional) Promote to GCP: copy the image tag values from the merged falcon bump PR into `environments/gcp/values.yaml` on `matty-v/kyber-deploy` and open + merge a separate PR. Apply via `kubectl apply -f environments/gcp/application.yaml` on the gcp cluster if the Application annotations also need to be refreshed (see kyber-deploy README "footgun" section).

---

## Security-config rollout (internal-auth cutover — kyber#578)

A release that changes the internal-API auth posture (`internalAuth.graceMode`,
or first delivery of the `kyber-internal-signing-key` Secret) is **not** a normal
deploy — a missed key delivery on an enforce cutover fail-closes the whole
internal API (the v2.1.0 fleet outage). It is **grace-first + key-gated**:

- A new internal-auth rollout defaults to **grace** (`graceMode: true`); enforce
  is an **explicit, key-verified flip**, never an auto-flip.
- Before applying, run the deploy gate — it **aborts a keyless enforce cutover**:
  ```bash
  scripts/preflight-internal-auth-key.sh <namespace> <graceMode:true|false>
  # after apply, confirm the control plane enabled auth:
  scripts/preflight-internal-auth-key.sh <namespace> <graceMode:true|false> \
    --post-apply kyber-control-plane
  ```
- A keyless startup **pages** (`InternalAuthFailClosed` / `InternalAuthGraceNoKey`)
  — respond by delivering the Secret + restarting the control plane.

Full procedure + alert response:
[`internal-api-auth-rollout.md`](internal-api-auth-rollout.md). The per-cluster
key-presence verification is also a step in **the deploy-review checklist**
for any auth/signing-key rollout.

---

## Opt-out: `[skip-publish]`

If a tag should **not** trigger steps 4 or 5 (e.g., a docs-only patch that doesn't change images or the PWA bundle), include `[skip-publish]` anywhere in the annotated tag message:

```bash
git tag -a v1.2.4 -m "[skip-publish] docs: fix typo in README"
git push origin v1.2.4
```

The `publish-pwa-views-chain` and `deploy-bump-pr` jobs both check the tag message and skip entirely when `[skip-publish]` is present. *(The retired `chart-version-bump-pr` job honored this too; its replacement, `prepare-release.yml`, runs **before** the tag exists and is operator-dispatched, so `[skip-publish]` does not apply to it — bump `Chart.yaml` directly or skip running `prepare-release.yml` for a no-image patch.)*

---

## Test releases (B5)

To test the release chain without side effects, push a tag whose name contains `-test`:

```bash
git tag -a v0.0.0-test1 -m "Testing release chain"
git push origin v0.0.0-test1
```

When `is_test=true` (tag name contains `-test`):
- `publish-pwa-views-chain` logs what it *would* do but does not push the `pwa-views/v*` tag.
- `deploy-bump-pr` shows the `environments/falcon/values.yaml` diff but does not commit, push, or open a PR.
- The control-plane `:latest` refresh (step 5) is **skipped** — a test tag must never move the live canary tag.

This exercises the full dependency graph (all build jobs must pass, `release` job runs, chain jobs run) without publishing to npm, filing a PR on kyber-deploy, or moving the canary `:latest` tag. *(The chart bump is no longer part of `release.yml` — to dry-run it, run `prepare-release.yml` against a throwaway version on a scratch branch; it opens a real PR, so prefer reviewing its logic over executing it on `main`.)*

---

## Idempotency

If `@matty-v/kyber-pwa-views@<version>` is already published on GitHub Packages (e.g., from a manual `publish-pwa-views.yml` run), `publish-pwa-views-chain` detects this via `npm view` and skips the tag push. Re-running the release or re-tagging will not double-publish.

---

## Credentials / secrets surface

| Secret | Used by | Scope |
|---|---|---|
| `GHCR_PAT` | All 5 build jobs + `publish-pwa-views-chain` (npm view check) | `packages:write` on `matty-v/*` |
| `KYBER_APP_TOKEN` | `publish-pwa-views-chain` (tag push), `deploy-bump-pr` (gh pr create) | `contents:write` on `matty-v/kyber`; `contents:write` + `pull_requests:write` on `matty-v/kyber-deploy` |
| `KYBER_APP_ID` + `KYBER_APP_PRIVATE_KEY` | `deploy-bump-pr` (mints a `kyber-deploy` token), `prepare-release.yml` (mints a `kyber` token for the pre-tag chart bump + tag push) | The App's installation must grant `contents:write` + `pull_requests:write` on **`matty-v/kyber`** for the chart-version bump + tag push (kyber#457 delivery gate) — and on `matty-v/kyber-deploy` for the image bump |
| `LANDO_RELEASE_NOTIFY_URL` + `LANDO_RELEASE_HMAC` | `release` (release-notification webhook) | Webhook endpoint + HMAC key (optional — see step 7) |

`KYBER_APP_TOKEN` is the identity used for cross-workflow-triggering operations: pushing the `pwa-views/v*` tag (must trigger `publish-pwa-views.yml` — GITHUB_TOKEN-driven pushes do NOT trigger other workflows) and opening the kyber-deploy bump PR. Currently provisioned as a personal access token with `repo` scope (covers `contents:write` on both `matty-v/kyber` and `matty-v/kyber-deploy`, plus `pull_requests:write` on kyber-deploy). A dedicated GitHub App (`kyber-app`) with scoped install permissions would be a cleaner long-term identity; the PAT works today.

---

## Troubleshooting

**`publish-pwa-views-chain` fails with 403 on tag push:** `KYBER_APP_TOKEN` is missing or lacks `contents:write` on `matty-v/kyber`. Re-provision with a PAT that has `repo` scope (or App install with `contents:write`).

**`deploy-bump-pr` fails with 403 on `gh pr create`:** `KYBER_APP_TOKEN` is missing or lacks `contents:write` + `pull_requests:write` on `matty-v/kyber-deploy`. Same fix as above — PAT with `repo` scope covers both.

**`deploy-bump-pr` fails: branch already exists:** A previous run's branch was not deleted. Delete `chore/bump-kyber-<tag>` from kyber-deploy and re-run the job.

**`prepare-release.yml` fails on push/`gh pr create` (403/empty token):** the release GitHub App's installation on `matty-v/kyber` lacks `contents:write` + `pull_requests:write` (kyber#457 delivery gate). Grant the App those permissions on kyber, then re-run. The chart-bump PR uses `gh pr merge --auto` and waits for `main`'s required checks `[test, pwa-build, changes, integration]` — if `prepare-release.yml` times out waiting for the merge, check those checks rather than the workflow. To advance `chartVersion` manually meanwhile, bump `deploy/helm/kyber/Chart.yaml` `version`/`appVersion` to the release and merge a one-off PR (then push the tag on that commit).

**`prepare-release.yml` aborts with "Chart.yaml at <sha> is '<x>', expected '<ver>'":** the commit it was about to tag does not carry the matching chart version — usually a `main` merge raced the bump merge. Re-run `prepare-release.yml` for the same version; it is idempotent and will re-bump if needed before tagging.

**The canary reports a stale `vX.Y.Z-N-gSHA` after a release (the kyber#591 bug):** the `:latest` refresh in `release.yml` was skipped because the release commit was no longer `origin/main` HEAD (a `main` merge landed during the ~10-min build, so the guard correctly declined to move the canary backward). The canary will self-correct on the next `:latest` build from `build.yml`. To force it sooner, merge a no-op/real commit so a fresh `:latest` builds, or re-run the release at the new HEAD.

**pwa-views tag pushed but `publish-pwa-views.yml` did not trigger:** The `KYBER_APP_TOKEN` was replaced with `GITHUB_TOKEN` — pushes via `GITHUB_TOKEN` do not trigger downstream workflows. Ensure `KYBER_APP_TOKEN` is set to a PAT or App token.
