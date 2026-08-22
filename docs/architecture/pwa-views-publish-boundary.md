# pwa-views publish boundary: kyber ↔ holocron

> **Auto-publish is now the standard path — twice over.** `release.yml` chains a `pwa-views/v*` tag push after every successful release build, and outside a release, `auto-publish-pwa-views.yml` detects a `packages/pwa-views/package.json` version bump merged to `main` and pushes the tag itself. Either tag push triggers `publish-pwa-views.yml` automatically; both paths are idempotent (skip if the version is already on the registry), so there is no double-publish. Manual dispatch via the Actions tab remains available as a fallback. The boundary contract below is unchanged — only the trigger is automated. See the [release runbook](../operator/release-runbook.md) for details and the `[skip-publish]` opt-out.

`@matty-v/kyber-pwa-views` is a published npm package on GitHub Packages. This document
explains how the package is consumed, where the version boundary lives, and what contributors
must do when they change `packages/pwa-views`.

---

## Two consumers, two resolution paths

```
matty-v/kyber (monorepo)
├── packages/pwa-views/          ← the package source
│     version: 0.4.0
└── apps/embedded-pwa/           ← consumer A
      package.json: "@matty-v/kyber-pwa-views": "*"
      resolved: via npm workspaces (local symlink — always current)

GitHub Packages
  @matty-v/kyber-pwa-views@0.4.0  ← published artifact

matty-v/holocron (separate repo)
└── package.json: "@matty-v/kyber-pwa-views": "0.4.0"   ← consumer B
      resolved: GitHub Packages registry (pinned version)
```

**`apps/embedded-pwa`** lives inside the kyber monorepo. npm workspaces resolves
`@matty-v/kyber-pwa-views` to the local `packages/pwa-views/` directory. Every
monorepo change is immediately visible — no publish step needed to pick up local
development.

**Holocron** lives in a separate repository and has no access to the kyber workspace.
It installs the package from GitHub Packages and sees exactly what was published at
the pinned version. Changes to `packages/pwa-views` are invisible to Holocron until:
1. A new version is published to GitHub Packages, AND
2. Holocron's `package.json` dependency is updated to reference that version.

---

## Why the version boundary matters

The gap between workspace resolution and published resolution has caused at least one
incident. In May 2026 the Metrics tab was added to `pwa-views` in PR #329. Because
no version bump or publish tag followed, `apps/embedded-pwa` showed the Metrics tab
(workspace-local) while Holocron did not (still on the 0.3.0 published artifact from
before the tab existed). The fix required two additional PRs (#336, #337) and a manual
tag re-push.

**The rule:** if a change in `packages/pwa-views` should reach Holocron, it must be
published. Workspace-local visibility is not the same as published visibility.

---

## Publish process

The `publish-pwa-views.yml` workflow triggers on tags matching `pwa-views/v*`. It
verifies the tag version matches `packages/pwa-views/package.json`, builds, tests,
and publishes to GitHub Packages using `secrets.GHCR_PAT`.

Steps to publish a new version:

```
1. Bump version in packages/pwa-views/package.json
2. Add CHANGELOG entry in packages/pwa-views/CHANGELOG.md
3. Run npm install (updates package-lock.json)
4. Open PR → merge to main
5. auto-publish-pwa-views.yml detects the bump on main and pushes the
   pwa-views/vX.Y.Z tag automatically (no manual tag push)
6. Confirm publish-pwa-views.yml completes successfully
7. In matty-v/holocron: update @matty-v/kyber-pwa-views dep → X.Y.Z and merge
```

> **No embedded-pwa version sync is needed.** `apps/embedded-pwa` pins
> `@matty-v/kyber-pwa-views` as `"*"`, so npm workspaces always resolves the
> local package regardless of its version — the old "keep both version strings
> in sync" step is retired.

---

## Semver guidance

| Change type | Bump |
|---|---|
| New exported component, hook, or type (additive) | minor |
| Bug fix in existing component with no API change | patch |
| Removed or renamed export, changed prop type | major |
| Internal refactor, style-only, no API change | patch |

Holocron consumers pin to an exact version (`"0.4.0"`, not `"^0.4.0"`). Breaking changes
(major bumps) require a coordinated update with the Holocron maintainer.

---

## PR checklist for contributors touching `packages/pwa-views`

When opening a PR that modifies anything under `packages/pwa-views/`:

- [ ] **Version bump:** `packages/pwa-views/package.json` version incremented (patch / minor / major per table above)
- [ ] **CHANGELOG:** entry added to `packages/pwa-views/CHANGELOG.md` under the new version
- [ ] **Lockfile:** `package-lock.json` regenerated (`npm install` at repo root)
- [ ] **No manual tag:** do not push a `pwa-views/v*` tag yourself — `auto-publish-pwa-views.yml` pushes it after the merge to main
- [ ] **Holocron tracked:** if the change should reach Holocron, a follow-up Holocron dep-update is noted (issue or PR)

If the change is internal to the monorepo only (no Holocron consumers need it), the
version bump is still required — CI guards it, and the auto-publish keeps the
registry in sync — but the Holocron update can be deferred.

---

*— Obi-wan, 2026-05-25*
