#!/usr/bin/env bash
# Tests for the docs/product/ source-of-truth set (kyber#397).
# Run from repo root: bash docs/product/product_docs_test.sh
#
# These assert the structural + boundary invariants the issue's acceptance
# criteria define — the ones that are mechanically checkable and would
# otherwise silently drift:
#   - the v1 page set exists,
#   - capability pages carry NO implementation detail (the WHAT/HOW boundary —
#     no file paths, function names, or controller internals),
#   - the README states scope (WHAT-not-HOW), ownership (Yoda), and cross-links
#     to the architecture (HOW) set,
#   - the two doc sets cross-link both ways, and the root README points here.
# (Content accuracy is a human/Yoda concern spot-checked against a running
# instance per the issue; that is out of scope for a grep test.)

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
PROD="${REPO_ROOT}/docs/product"

PASS=0
FAIL=0
FAILED=()

ok()   { PASS=$((PASS+1)); echo "PASS: $1"; }
bad()  { FAIL=$((FAIL+1)); FAILED+=("$1"); echo "FAIL: $1"; [ -n "${2:-}" ] && echo "      $2"; }

assert_file()    { [ -f "$1" ] && ok "exists: ${1#"${REPO_ROOT}"/}" || bad "missing file: ${1#"${REPO_ROOT}"/}"; }
assert_grep()    { grep -Eq "$2" "$1" && ok "$3" || bad "$3" "pattern not found in ${1#"${REPO_ROOT}"/}: $2"; }
# assert_no_impl <file>: the file must contain no implementation-detail markers.
assert_no_impl() {
    local f="$1" rel="${1#"${REPO_ROOT}"/}"
    # Markers that mean "this is HOW, not WHAT": Go file paths, package paths,
    # function decls, controller internals, go code fences, kubectl.
    local hits
    hits=$(grep -nEi '\.go([^a-z]|$)|pkg/|/controllers/|func [a-z]|reconciler|state_machine|```go|kubectl ' "$f" || true)
    if [ -z "$hits" ]; then
        ok "no implementation detail: ${rel}"
    else
        bad "implementation detail leaked into ${rel}" "$(echo "$hits" | head -3 | tr '\n' '|')"
    fi
}

# --- v1 page set exists (AC: docs/product/ with overview + agent-lifecycle + PWA) ---
assert_file "${PROD}/README.md"
assert_file "${PROD}/overview.md"
assert_file "${PROD}/_TEMPLATE.md"
assert_file "${PROD}/agent-lifecycle.md"
assert_file "${PROD}/pwa-holocron.md"

# --- WHAT/HOW boundary: capability pages carry no implementation detail ---
assert_no_impl "${PROD}/overview.md"
assert_no_impl "${PROD}/agent-lifecycle.md"
assert_no_impl "${PROD}/pwa-holocron.md"
assert_no_impl "${PROD}/_TEMPLATE.md"

# --- README states scope, ownership, and the boundary (AC) ---
assert_grep "${PROD}/README.md" 'WHAT'                      "README states scope is WHAT"
assert_grep "${PROD}/README.md" '[Hh][Oo][Ww]'             "README names the HOW boundary"
assert_grep "${PROD}/README.md" 'Yoda'                      "README names Yoda as maintainer"
assert_grep "${PROD}/README.md" 'source of truth'           "README declares itself the product source of truth"
assert_grep "${PROD}/README.md" 'release.?notes|ship'       "README states the update cadence"

# --- cross-links both ways + root README points here (AC: navigable boundary) ---
assert_grep "${PROD}/README.md"  'docs/architecture'        "product README links to the architecture (HOW) set"
assert_grep "${REPO_ROOT}/docs/architecture/overview.md" 'docs/product' "architecture overview back-links to the product (WHAT) set"
assert_grep "${REPO_ROOT}/README.md" 'docs/product'         "root README links to docs/product/"

# --- per-page template carries the agreed section set ---
for s in 'Concept' 'Observable behavior' 'States' 'Operator actions'; do
    assert_grep "${PROD}/_TEMPLATE.md" "$s" "template has section: ${s}"
done

# --- agent-lifecycle page names shipped phases (mirrors architecture vocabulary) ---
for ph in 'Running' 'Suspended' 'NeedsAuth' 'MemoryExhausted'; do
    assert_grep "${PROD}/agent-lifecycle.md" "$ph" "agent-lifecycle names the ${ph} phase"
done

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    printf 'Failed:\n'; for t in "${FAILED[@]}"; do printf '  - %s\n' "$t"; done
    exit 1
fi
