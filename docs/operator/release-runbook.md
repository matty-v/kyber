# Kyber release runbook

The release entry point is `.github/workflows/prepare-release.yml`. It updates
chart and installation-document versions on a PR, waits for that PR to merge,
and tags the resulting main commit. The tag triggers `release.yml` to build and
publish release artifacts. Publishing a release does not install it on a cluster.

This procedure is grounded in those workflows, `publish-pwa-views.yml`,
`auto-publish-pwa-views.yml`, and the update implementation in `pkg/updates` and
`pkg/selfupgrade`. Dated notes about upstream cluster placement are not release
readiness evidence; verify the actual target and commit before claiming a smoke
check or rollout passed.

## Before cutting the tag

1. Fetch main and tags. Identify the latest **Kyber** release (`vX.Y.Z`), keeping
   the independently versioned `pwa-views/vX.Y.Z` tags out of the comparison.
2. Review all commits since that release against code, migration requirements,
   chart defaults, and documentation. Prepare operator-facing release notes and
   a version recommendation. Features normally justify a minor version; fixes
   alone normally justify a patch. Call out incompatible changes explicitly.
3. Merge the cleanup and release-note PRs through normal required checks. Check
   the candidate's CI and relevant dev acceptance evidence. A healthy cluster
   running an older release does not validate this candidate. See
   [dev verification](kyber-dev-verification.md) and the
   [harness conformance guide](../architecture/agent-harness-conformance.md).
4. Obtain the operator's approval of the concrete version and notes before
   dispatching the preparation workflow. It creates and auto-merges a real PR
   and publishes an immutable release tag; it has no dry-run input.

## Prepare the release

```bash
gh workflow run prepare-release.yml -R matty-v/kyber -f version=X.Y.Z
```

Use bare numeric semver without `v` or a suffix. The workflow:

- Mints a fresh GitHub App token scoped to the repository.
- Updates `deploy/helm/kyber/Chart.yaml` version and appVersion together.
- Stamps the known version patterns in `README.md`, the product quickstart,
  `docs/installation.md`, and `docs/installation-wsl2.md`.
- Opens `chore/prepare-release-vX.Y.Z`, enables normal squash auto-merge, and
  waits for required checks. If all stamped files already match, it skips the PR.
- Fetches main, checks the chart version at that HEAD, and pushes annotated tag
  `vX.Y.Z` there. Inspect any main commits that arrive during preparation: the
  workflow tags the current main HEAD, not a separately supplied candidate SHA.

Do not push a raw tag for a normal release. The preparation workflow ensures
that the chart version is present in the tagged commit. Its version check does
not make retries universally idempotent: an existing preparation branch or tag
can still require investigation.

## What release CI publishes

`release.yml` runs on `v*.*.*` tag pushes. Its manual recovery entry point uses
an existing tag as the ref:

```bash
gh workflow run release.yml -R matty-v/kyber --ref vX.Y.Z
```

Inspect a failed run before retrying. Prefer retrying failed jobs where possible;
starting the entire workflow again can hit the immutable-image preflight after
some images were already published.

The workflow builds nine images at the release SHA as linux/amd64 and
linux/arm64 manifest lists:

| Chart image key | GHCR image |
|---|---|
| `controlPlane` | `kyber-control-plane` |
| `nodeAgent` | `kyber-node-agent` |
| `statusSidecar` | `kyber-status-sidecar` |
| `discordSidecar` | `kyber-mcp-discord` |
| `telegramSidecar` | `kyber-mcp-telegram` |
| `slackSidecar` | `kyber-mcp-slack` |
| `agentBase` | `kyber-runtime-base` |
| `claudeCode` | `kyber-claude-code` |
| `codex` | `kyber-codex` |

The Claude Code and Codex images depend on runtime-base. Before normal image
builds, `preflight-check-tags` rejects any already-published target image tag.
The GitHub Release requires every image build and the reusable A2A conformance
workflow to pass. That conformance gate checks the pinned contract, official
TCK, and independent-client evidence; see
[the support matrix](../../conformance/a2a/1.0/SUPPORT.md).

After the GitHub Release:

- `publish-chart` checks chart version parity, stamps all nine release image
  tags into the packaged values, verifies Helm rendering, and publishes
  `oci://ghcr.io/matty-v/charts/kyber` at `X.Y.Z`. The source values keep their
  deliberate empty-image guards. An already-published chart version is skipped.
- `publish-pwa-views-chain` can push `pwa-views/v<package-version>` using a fresh
  App token. The version comes from `packages/pwa-views/package.json`, **not**
  the Kyber release number. It only publishes a version newer than the registry
  latest. Missing App credentials or `[skip-publish]` in the tag message skips
  this optional chain. Normal main-branch package bumps already trigger
  `auto-publish-pwa-views.yml`, so this step often has nothing to publish.

The control-plane `:latest` refresh is a separate image-build-dependent job.
It only moves the tag when the release commit still equals main HEAD and is
not a test tag. Its success does not prove the GitHub Release, A2A gate, chart,
or any cluster deployment succeeded.

The GitHub Release body is initially generated from commit subjects since the
previous stable Kyber tag, excluding package and prerelease tags. Replace that
commit list with the reviewed operator-facing notes after publication.

There are no `resolve-digests`, `deploy-bump-pr`, post-tag chart-bump, or
release-notification webhook jobs in the current workflow.

## Verify publication and installation separately

1. Confirm the preparation tag SHA, chart version/appVersion, and approved
   changes agree. Check all required release jobs, including `publish-chart`.
2. Confirm all nine image tags resolve for both architectures and the published
   chart's image defaults name the release. A GitHub Release page alone is
   insufficient evidence of chart publication.
3. Apply the reviewed notes to the GitHub Release using a file, for example:
   `gh release edit vX.Y.Z -R matty-v/kyber --notes-file <reviewed-notes.md>`.
4. Check the independent pwa-views publish outcome when relevant; downstream
   hosts need their package dependency updated before they consume the new UI.
5. Report artifacts as published. Report a cluster as upgraded only after its
   operator applies the release and its version/health checks pass.

Self-updating Helm installations check for updates but apply only when requested
through Settings → Updates or `POST /api/v1/updates/apply`. The supervised Job
preflights, applies CRDs, performs the Helm upgrade, and verifies the rollout.
Pinned image overrides prevent this path from safely selecting a coherent image
set. See [upgrading](../upgrading.md) for guards, recovery, and rollback limits.
For a GitOps-managed installation, change its declared release through that
installation's GitOps process; do not run a competing Helm upgrade.

## Immutability and test limitations

Published semver tags and charts are immutable. Repair a broken release with a
new patch version; never delete and reuse the same semver with different bytes.
Kubelet image caching and existing Helm artifacts make such replacements unsafe.

`[skip-publish]` only skips the pwa-views chain. It does **not** skip image
builds, the GitHub Release, chart publication, or the guarded `:latest` refresh.
It is not a docs-only or side-effect-free mode.

Tags containing `-test` are **not a working end-to-end dry run**. They skip
`preflight-check-tags`; image jobs require that job without an `always()`
override, so the build/release/chart dependency chain is skipped too. Use local
workflow/Helm checks and approved dev acceptance instead. Do not dispatch
`prepare-release.yml` with a throwaway version on main to test it: it creates
real branches, PRs, and tags.

## Credentials and recovery

| Credential | Current use |
|---|---|
| `GHCR_PAT` | image builds, chart registry login, npm registry checks |
| `KYBER_APP_ID` + `KYBER_APP_PRIVATE_KEY` | fresh installation tokens for preparation PR/tag writes and optional pwa-views tag writes |
| `GITHUB_TOKEN` | repository-local GitHub Release creation with workflow permissions |

The App must be installed on Kyber with contents and pull-request write access.
A static `KYBER_APP_TOKEN` is not used by the current release chain. Never print
or embed secret values in notes or troubleshooting output.

- Preparation PR stalled: inspect its actual required checks and merge state.
  Do not bypass branch protection.
- Existing branch/tag: inspect the earlier preparation run before retrying.
  Preserve a published tag and use a new version for changed artifacts.
- Image preflight fails: determine which artifacts already exist and whether
  this is partial publication; do not disable the immutability guard.
- Chart publication fails: inspect version parity, all image pins, rendering,
  and registry access. Images may already be published even if the chart failed.
- PWA tag exists but package did not publish: inspect the tag workflow, then use
  `auto-publish-pwa-views.yml`'s documented recovery dispatch on main.
- A cluster still reports the old version: check whether an update was applied
  there, then inspect the upgrade Job and rollout. Publication alone does not
  start an update.

A release changing internal signing-key or auth enforcement configuration must
also follow [the internal-auth rollout procedure](internal-api-auth-rollout.md).
