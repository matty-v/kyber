#!/usr/bin/env bash
# scripts/pwa-views-should-tag_test.sh — regression guard for the pwa-views
# tag/publish decision (PR #97).
#
# Two halves, mirroring release-version-guard_test.sh:
#   1. Behavioral — pin the decision table of pwa-views-should-tag.sh,
#      including the two modes that produced real incidents: a version
#      stranded by commit-to-commit change detection (0.27.2) and the
#      sort -V prerelease inversion that would strand a final release
#      behind its own rc.
#   2. Static — assert both workflows that push pwa-views/v* tags actually
#      route their decision through the shared script, so the semantics
#      cannot silently fork into per-workflow variants again.
#
# Run from repo root: bash scripts/pwa-views-should-tag_test.sh

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${REPO_ROOT}/scripts/pwa-views-should-tag.sh"
AUTO_YML="${REPO_ROOT}/.github/workflows/auto-publish-pwa-views.yml"
RELEASE_YML="${REPO_ROOT}/.github/workflows/release.yml"

PASS=0
FAIL=0
FAILED_TESTS=()

ok()  { PASS=$((PASS+1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL+1)); FAILED_TESTS+=("$1"); echo "FAIL: $1 — $2"; }

# expect <want:tag|skip> <NEW> <PUBLISHED> <label>
expect() {
  local want="$1" new="$2" published="$3" label="$4"
  local out
  out="$(bash "$SCRIPT" "$new" "$published")"
  if [ "${out%%[: ]*}" = "$want" ]; then
    ok "$label"
  else
    bad "$label" "want '$want', got '$out' (NEW=$new PUBLISHED=$published)"
  fi
}

# --- 1. Behavioral -----------------------------------------------------------

expect tag  0.27.4 0.27.1 "plain newer version tags"
expect tag  0.27.2 0.27.1 "a stranded intermediate version still tags (the 0.27.2 incident shape)"
expect skip 0.27.1 0.27.1 "same version skips"
expect skip 0.27.0 0.27.1 "a reverted (older) bump skips — never republish old as latest"
expect tag  0.27.4 ""     "first-ever publish tags"
expect tag  0.27.10 0.27.9 "numeric compare, not lexicographic (10 > 9)"
expect tag  1.0.0  0.99.99 "major bump beats larger minor/patch fields"
expect skip 0.28.0-rc.1 0.27.4 "a prerelease NEW never auto-tags"
expect tag  0.28.0 0.28.0-rc.1 "a final supersedes its own published rc (sort -V would invert this)"
expect skip 0.27.4 0.28.0-rc.1 "an older final does not beat a newer published rc"
expect skip garbage 0.27.1 "non-semver NEW skips"
expect skip 0.27.4 garbage "unparseable PUBLISHED skips rather than guessing"

if bash "$SCRIPT" 0.1.0 >/dev/null 2>&1; then
  bad "usage error exits non-zero" "accepted a single argument"
else
  ok "usage error exits non-zero"
fi

# --- 2. Static ---------------------------------------------------------------

if grep -q 'pwa-views-should-tag.sh' "$AUTO_YML"; then
  ok "auto-publish-pwa-views.yml routes its decision through the shared script"
else
  bad "auto-publish-pwa-views.yml routes its decision through the shared script" "no reference to pwa-views-should-tag.sh"
fi

if grep -q 'pwa-views-should-tag.sh' "$RELEASE_YML"; then
  ok "release.yml pwa-views chain routes its decision through the shared script"
else
  bad "release.yml pwa-views chain routes its decision through the shared script" "no reference to pwa-views-should-tag.sh"
fi

if grep -Eq 'sort -V' "$AUTO_YML"; then
  bad "auto-publish-pwa-views.yml contains no sort -V version compare" "sort -V found — prerelease ordering inverts semver"
else
  ok "auto-publish-pwa-views.yml contains no sort -V version compare"
fi

# --- Summary -----------------------------------------------------------------

echo
echo "${PASS} passed, ${FAIL} failed"
if [ "$FAIL" -gt 0 ]; then
  printf 'failed: %s\n' "${FAILED_TESTS[@]}"
  exit 1
fi
exit 0
