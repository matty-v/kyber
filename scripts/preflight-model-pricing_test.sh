#!/usr/bin/env bash
# Tests for scripts/preflight-model-pricing.sh — the CI guard (kyber#487 v2)
# that the pricing FEED is wired and fresh (not the old per-model hand-rate
# allowlist, which was removed with the pivot to a programmatic source).
# Run from repo root: bash scripts/preflight-model-pricing_test.sh
#
# Strategy: drive the checks with fixture files via env overrides
# (RATES_FILE / META_FILE / TEMPLATE_FILE / FRESHNESS_DAYS / PREFLIGHT_NOW_EPOCH)
# so the logic is exercised without helm or the real vendored dataset.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT_DIR}/preflight-model-pricing.sh"

PASS=0
FAIL=0
FAILED_TESTS=()

VALID_RATES='# generated header
claude-opus-4-8:
    input: 5
    output: 25
    cache_read: 0.5
    cache_creation: 6.25
gpt-4o:
    input: 2.5
    output: 10
    cache_read: 1.25
    cache_creation: 0'

# A wired template references the vendored file via .Files.Get.
WIRED_TEMPLATE='data:
  provider-rates.yaml: |
{{ .Files.Get "files/provider-rates.generated.yaml" | indent 4 }}'

# now = 2026-06-07T00:00:00Z epoch; fresh meta is dated the same day.
NOW_EPOCH=1780531200
FRESH_META='source: litellm
upstream_commit: 51769a8ede120b4bf9038bb06b77c812e81283db
fetched_at: 2026-06-07T00:00:00Z
model_count: 153'
STALE_META='source: litellm
upstream_commit: 51769a8ede120b4bf9038bb06b77c812e81283db
fetched_at: 2026-03-01T00:00:00Z
model_count: 153'

# run <name> <expected_code> <rates> <meta> <template>
run() {
    local name="$1" expected="$2" rates="$3" meta="$4" template="$5"
    local dir out rc
    dir=$(mktemp -d)
    printf '%s\n' "${rates}"    >"${dir}/rates.yaml"
    printf '%s\n' "${meta}"     >"${dir}/meta"
    printf '%s\n' "${template}" >"${dir}/template.yaml"
    out=$(RATES_FILE="${dir}/rates.yaml" META_FILE="${dir}/meta" \
          TEMPLATE_FILE="${dir}/template.yaml" FRESHNESS_DAYS=30 \
          PREFLIGHT_NOW_EPOCH="${NOW_EPOCH}" "${SCRIPT}" 2>&1)
    rc=$?
    if [ "$rc" -eq "$expected" ]; then
        PASS=$((PASS+1)); echo "PASS: ${name}"
    else
        FAIL=$((FAIL+1)); FAILED_TESTS+=("${name}")
        echo "FAIL: ${name} — expected exit ${expected}, got ${rc}"
        echo "      output: ${out}"
    fi
    rm -rf "${dir}"
}

# Case 1: valid dataset + fresh stamp + wired template → pass.
run "wired + fresh → pass" 0 "${VALID_RATES}" "${FRESH_META}" "${WIRED_TEMPLATE}"

# Case 2: dataset has 0 model entries → fail (parse/empty).
run "empty dataset → fail" 1 "# header only" "${FRESH_META}" "${WIRED_TEMPLATE}"

# Case 3: an implausible rate (unscaled per-token value) → fail (sanity bound).
run "implausible rate → fail" 1 \
'claude-x:
    input: 999999
    output: 25' "${FRESH_META}" "${WIRED_TEMPLATE}"

# Case 4: a non-positive rate → fail.
run "non-positive rate → fail" 1 \
'claude-x:
    input: 0
    output: 25' "${FRESH_META}" "${WIRED_TEMPLATE}"

# Case 4b: an implausible cache_creation rate (unscaled per-token value) → fail.
run "implausible cache_creation rate → fail" 1 \
'claude-x:
    input: 5
    output: 25
    cache_creation: 999999' "${FRESH_META}" "${WIRED_TEMPLATE}"

# Case 5: stale stamp (older than FRESHNESS_DAYS) → fail (forces refresh bot).
run "stale stamp → fail" 1 "${VALID_RATES}" "${STALE_META}" "${WIRED_TEMPLATE}"

# Case 6: stamp missing fetched_at → fail (wiring/stamp broken).
run "missing fetched_at → fail" 1 "${VALID_RATES}" \
'source: litellm
upstream_commit: abc' "${WIRED_TEMPLATE}"

# Case 7: template does not reference the vendored file → fail (not wired).
run "template not wired → fail" 1 "${VALID_RATES}" "${FRESH_META}" \
'data:
  provider-rates.yaml: |
    claude-opus-4-7:
      input: 15'

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    printf 'Failed tests:\n'
    for t in "${FAILED_TESTS[@]}"; do printf '  - %s\n' "$t"; done
    exit 1
fi
