#!/usr/bin/env bash
# CI guard (kyber#487 v2): assert the pricing FEED is wired and fresh.
#
# Pricing is no longer hand-maintained — it is the build-time vendored
# projection of LiteLLM's public model-prices feed (cmd/fetch-provider-rates),
# rendered into the kyber-provider-rates ConfigMap by the chart. Any model the
# feed doesn't cover renders the fail-loud "unpriced"/"—" badge, never a fake
# $0.00, so per-model coverage is NOT a hard-fail anymore. What must hold is
# that the wiring is intact and the snapshot is current:
#
#   WIRED — the vendored dataset exists, parses, is non-empty, every rate
#           passes sanity bounds, and the chart actually renders it.
#   FRESH — the freshness stamp is present and newer than FRESHNESS_DAYS,
#           so a stale snapshot forces the weekly refresh-bot PR to land.
#
# Exit 0 = wired and fresh. Exit 1 = wiring broken / dataset absent or empty /
# implausible rate / stamp missing or stale.
#
# Env overrides (used by preflight-model-pricing_test.sh fixtures):
#   RATES_FILE, META_FILE, TEMPLATE_FILE, FRESHNESS_DAYS, PREFLIGHT_NOW_EPOCH

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RATES_FILE="${RATES_FILE:-${REPO_ROOT}/deploy/helm/kyber/files/provider-rates.generated.yaml}"
META_FILE="${META_FILE:-${REPO_ROOT}/deploy/helm/kyber/files/provider-rates.generated.meta}"
TEMPLATE_FILE="${TEMPLATE_FILE:-${REPO_ROOT}/deploy/helm/kyber/templates/configmap-rates.yaml}"
FRESHNESS_DAYS="${FRESHNESS_DAYS:-30}"
# USD-per-MTok ceiling; must match cmd/fetch-provider-rates bounds.
MAX_PER_MTOK=10000

fail() { echo "::error::preflight-model-pricing: $*" >&2; exit 1; }

# --- WIRED: dataset present, non-empty, parses, rates sane -------------------

[ -f "${RATES_FILE}" ] || fail "vendored dataset missing: ${RATES_FILE} (run cmd/fetch-provider-rates)"

# Model keys sit at column 0 (no leading space) and end with ':'; comments start
# with '#'. Sub-keys (input/output/cache_read) are indented, so excluded.
model_count=$(grep -cE '^[a-zA-Z0-9][a-zA-Z0-9._/:-]*:[[:space:]]*$' "${RATES_FILE}" || true)
[ "${model_count}" -gt 0 ] || fail "vendored dataset has 0 model entries: ${RATES_FILE}"

# Sanity bounds: input/output must be 0 < rate <= ceiling; cache_read >= 0.
# A value outside the bounds means a malformed/poisoned feed slipped the
# adapter — refuse it rather than ship a confidently-wrong price.
bad=$(awk -v ceil="${MAX_PER_MTOK}" '
    /^[[:space:]]+(input|output):[[:space:]]/ {
        v=$2; if (v+0 <= 0 || v+0 > ceil) { print $0; }
    }
    /^[[:space:]]+cache_read:[[:space:]]/ {
        v=$2; if (v+0 < 0 || v+0 > ceil) { print $0; }
    }
' "${RATES_FILE}")
[ -z "${bad}" ] || fail "implausible rate(s) in ${RATES_FILE} (outside 0..${MAX_PER_MTOK}/MTok):
${bad}"

# Wired: the chart template must actually render the vendored file.
grep -q 'files/provider-rates.generated.yaml' "${TEMPLATE_FILE}" \
    || fail "chart template ${TEMPLATE_FILE} does not render the vendored dataset — pricing is not wired"

# --- FRESH: stamp present and within the freshness window --------------------

[ -f "${META_FILE}" ] || fail "freshness stamp missing: ${META_FILE}"

fetched_at=$(grep -E '^fetched_at:' "${META_FILE}" | head -1 | sed -E 's/^fetched_at:[[:space:]]*//')
[ -n "${fetched_at}" ] || fail "freshness stamp ${META_FILE} has no fetched_at"

fetched_epoch=$(date -u -d "${fetched_at}" +%s 2>/dev/null) \
    || fail "freshness stamp fetched_at is not a parseable timestamp: ${fetched_at}"

now_epoch="${PREFLIGHT_NOW_EPOCH:-$(date -u +%s)}"
age_days=$(( (now_epoch - fetched_epoch) / 86400 ))
if [ "${age_days}" -gt "${FRESHNESS_DAYS}" ]; then
    fail "pricing snapshot is stale: ${age_days}d old (> ${FRESHNESS_DAYS}d). Land the refresh-bot PR (cmd/fetch-provider-rates) to refresh ${RATES_FILE}."
fi

echo "preflight-model-pricing: wired (${model_count} models, all rates within bounds, chart renders the vendored file) and fresh (${age_days}d old, threshold ${FRESHNESS_DAYS}d) — OK"
exit 0
