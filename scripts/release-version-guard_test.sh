#!/usr/bin/env bash
# scripts/release-version-guard_test.sh — kyber#591 regression guard.
#
# Asserts the kyber release flow keeps the Chart.yaml version bump INSIDE the
# tagged commit, so a canary image built at a release commit resolves a CLEAN
# `git describe` (the bare tag) instead of a stale pre-tag `vX.Y.Z-N-gSHA`
# string — the exact regression that left razer reporting `2.1.1-7-gd64fbbd`
# after the v2.2.0 release (kyber#591).
#
# Two halves:
#   1. Behavioral — build throwaway git fixtures and prove the `git describe`
#      mechanic the fix relies on: bump-in-tagged-commit → clean tag;
#      bump-after-tag → dirty `-N-g` string (the regression shape, locked in
#      as a negative so a reviewer can see exactly what re-decoupling produces).
#   2. Static — assert the workflows still encode the fix: prepare-release.yml
#      owns the validated bump-then-tag, release.yml retains the guarded
#      control-plane `:latest` refresh, and the retired chart-version-bump-pr
#      job is gone (its post-tag bump is what caused the bug).
#
# Run from repo root: bash scripts/release-version-guard_test.sh

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_YML="${REPO_ROOT}/.github/workflows/release.yml"
PREPARE_YML="${REPO_ROOT}/.github/workflows/prepare-release.yml"

PASS=0
FAIL=0
FAILED_TESTS=()

ok()  { PASS=$((PASS+1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL+1)); FAILED_TESTS+=("$1"); echo "FAIL: $1 — $2"; }

# Throwaway git repo with signing/identity pinned so commits never prompt.
new_repo() {
    local dir; dir=$(mktemp -d)
    git -C "$dir" init -q
    git -C "$dir" config user.email t@example.com
    git -C "$dir" config user.name test
    git -C "$dir" config commit.gpgsign false
    echo "$dir"
}

# A `git describe --tags` result is "dirty" (offset past the nearest tag) when
# it carries the `-<N>-g<sha>` suffix. This is the stale-describe shape kyber#591
# fixes; clean release labels must NOT match it.
DIRTY_RE='-[0-9]+-g[0-9a-f]+$'

# --- Behavioral 1: bump folded INTO the tagged commit → clean describe --------
d=$(new_repo)
(
  set -e
  cd "$d"
  echo "version: 2.2.0" > Chart.yaml
  git add Chart.yaml && git commit -q -m "chore(chart): bump to 2.2.0"
  git tag -a v2.2.0 -m "Release v2.2.0"   # tag the SAME commit that carries the bump
)
desc=$(git -C "$d" describe --tags)
if [ "$desc" = "v2.2.0" ]; then
    ok "bump-in-tagged-commit → clean describe (v2.2.0)"
else
    bad "bump-in-tagged-commit → clean describe" "got '$desc', want 'v2.2.0'"
fi
rm -rf "$d"

# --- Behavioral 2: bump AFTER the tag (the old flow) → dirty describe ----------
d=$(new_repo)
(
  set -e
  cd "$d"
  echo "seed" > f && git add f && git commit -q -m "seed"
  git tag -a v2.2.0 -m "Release v2.2.0"          # tag first ...
  echo "version: 2.2.0" > Chart.yaml             # ... then bump in a LATER commit
  git add Chart.yaml && git commit -q -m "chore(chart): post-tag bump"
)
desc=$(git -C "$d" describe --tags)
if [[ "$desc" =~ $DIRTY_RE ]]; then
    ok "bump-after-tag → dirty describe ($desc) [the regression shape]"
else
    bad "bump-after-tag → dirty describe" "got '$desc', expected a -N-gSHA suffix"
fi
rm -rf "$d"

# --- Static 3: prepare-release.yml exists, validates input, cuts the tag ------
if [ -f "$PREPARE_YML" ]; then
    ok "prepare-release.yml exists"

    if grep -Fq '[0-9]+\.[0-9]+\.[0-9]+' "$PREPARE_YML"; then
        ok "prepare-release.yml validates the version input (semver regex)"
    else
        bad "prepare-release.yml validates the version input" "semver regex not found"
    fi

    if grep -q 'git tag' "$PREPARE_YML" && grep -q 'git push' "$PREPARE_YML"; then
        ok "prepare-release.yml cuts the tag (git tag + push)"
    else
        bad "prepare-release.yml cuts the tag" "git tag and/or git push not found"
    fi

    if grep -q 'deploy/helm/kyber/Chart.yaml' "$PREPARE_YML"; then
        ok "prepare-release.yml bumps Chart.yaml"
    else
        bad "prepare-release.yml bumps Chart.yaml" "Chart.yaml path not referenced"
    fi

    # The install docs ride the same tagged commit as the chart bump, so the
    # published docs always name the latest release. These greps target the
    # DOCS assignment and the sed stamp lines SPECIFICALLY — a bare filename
    # grep also matches the git-add line, which made the original check
    # vacuous: deleting the seds kept it green (caught by review on kyber#94).
    if grep -Fq 'DOCS="README.md docs/quickstart.md docs/installation.md docs/installation-wsl2.md"' "$PREPARE_YML"; then
        ok "prepare-release.yml stamps all four install docs (DOCS list intact)"
    else
        bad "prepare-release.yml stamps all four install docs" "DOCS list line missing or trimmed"
    fi
    for shape in 's/--version [0-9]' 's/in place of `[0-9]' 's/tag: "v[0-9]' 's/`version: [0-9]'; do
        if grep -Fq "$shape" "$PREPARE_YML"; then
            ok "prepare-release.yml docs stamp keeps sed shape: ${shape}"
        else
            bad "prepare-release.yml docs stamp sed shape: ${shape}" "sed pattern missing"
        fi
    done
else
    bad "prepare-release.yml exists" "file not found at ${PREPARE_YML}"
    bad "prepare-release.yml validates the version input" "file missing"
    bad "prepare-release.yml cuts the tag" "file missing"
    bad "prepare-release.yml bumps Chart.yaml" "file missing"
fi

# --- Static 4: release.yml retired the post-tag chart-version-bump-pr job ------
if grep -Eq '^[[:space:]]*chart-version-bump-pr:' "$RELEASE_YML"; then
    bad "release.yml retired chart-version-bump-pr job" "the post-tag bump job is still present"
else
    ok "release.yml retired chart-version-bump-pr job"
fi

# --- Static 5: release.yml retains the guarded control-plane :latest refresh --
if grep -q 'kyber-control-plane:latest' "$RELEASE_YML"; then
    ok "release.yml refreshes the control-plane :latest tag"
else
    bad "release.yml refreshes the control-plane :latest tag" "no :latest refresh found"
fi

if grep -Eq 'ls-remote.*(main|HEAD)|rev-parse.*origin/main' "$RELEASE_YML"; then
    ok "release.yml guards the :latest refresh against origin/main HEAD"
else
    bad "release.yml guards the :latest refresh" "no origin/main HEAD guard found"
fi

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    printf 'Failed tests:\n'
    for t in "${FAILED_TESTS[@]}"; do printf '  - %s\n' "$t"; done
    exit 1
fi
