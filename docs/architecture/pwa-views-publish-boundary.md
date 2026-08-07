# pwa-views publish boundary: kyber ↔ holocron

> **Auto-publish is now the standard path.** `release.yml` chains a `pwa-views/v*` tag push after every successful release build, which triggers `publish-pwa-views.yml` automatically. Manual dispatch via the Actions tab remains available as a fallback. The boundary contract below is unchanged — only the trigger is automated. See the [release runbook](../operator/release-runbook.md) for details and the `[skip-publish]` opt-out.

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
      package.json: "@matty-v/kyber-pwa-views": "0.4.0"
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
3. Update apps/embedded-pwa/package.json to match the new version string
   (keeps workspace resolution unambiguous; prevents npm ci fallback to registry)
4. Run npm install (updates package-lock.json)
5. Open PR → merge to main
6. After merge, push the version tag:
     git pull
     git tag pwa-views/vX.Y.Z
     git push origin pwa-views/vX.Y.Z
7. Confirm publish-pwa-views.yml completes successfully
8. In matty-v/holocron: update @matty-v/kyber-pwa-views dep → X.Y.Z and merge
```

> **Note on step 3:** If `apps/embedded-pwa` still references the old version string
> after you bump `packages/pwa-views`, npm workspaces satisfies the old string locally
> but `npm ci` inside the publish workflow — which runs against the full lockfile —
> can 401 against the registry for the old version. Always keep both version strings
> in sync.

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
- [ ] **Embedded-pwa sync:** `apps/embedded-pwa/package.json` version string updated to match
- [ ] **Lockfile:** `package-lock.json` regenerated (`npm install` at repo root)
- [ ] **No publish yet:** tag is pushed *after* the PR merges to main, not before
- [ ] **PR description:** includes post-merge instruction to push `pwa-views/vX.Y.Z` tag
- [ ] **Holocron tracked:** if the change should reach Holocron, a follow-up Holocron dep-update is noted (issue or PR)

If the change is internal to the monorepo only (no Holocron consumers need it), the
version bump is still required to keep the workspace and registry in sync for future
publishes — but the tag push and Holocron update can be deferred.

---

*— Obi-wan, 2026-05-25*
