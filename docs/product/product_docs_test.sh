#!/usr/bin/env bash
# Tests for the docs/product/ source-of-truth set.
# Run from repo root: bash docs/product/product_docs_test.sh
#
# docs/product/ is published twice from one source: rendered in-repo by GitHub
# and mirrored to kyber.voget.io by the kyber-site build (driven by
# manifest.json). These assert the invariants that would otherwise drift
# silently:
#   - manifest.json and the section dirs agree in BOTH directions (a page not
#     in the manifest does not publish; a manifest entry without a file breaks
#     the site build),
#   - every published page is site-convertible: single H1 on line 1, no YAML
#     frontmatter,
#   - pages are location-neutral: no hardcoded kyber.voget.io links,
#   - plain-voice rule: no em dashes anywhere in the tree,
#   - the WHAT/HOW boundary holds on concept pages (capabilities/, use-cases/):
#     no file paths, function names, or controller internals,
#   - the README states the charter and the doc sets cross-link both ways.
# (Content accuracy is a human concern; out of scope for a grep test.)

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
PROD="${REPO_ROOT}/docs/product"

PASS=0
FAIL=0
FAILED=()

ok()   { PASS=$((PASS+1)); echo "PASS: $1"; }
bad()  { FAIL=$((FAIL+1)); FAILED+=("$1"); echo "FAIL: $1"; [ -n "${2:-}" ] && echo "      $2"; }

assert_file() { [ -f "$1" ] && ok "exists: ${1#"${REPO_ROOT}"/}" || bad "missing file: ${1#"${REPO_ROOT}"/}"; }
assert_grep() { grep -Eq "$2" "$1" && ok "$3" || bad "$3" "pattern not found in ${1#"${REPO_ROOT}"/}: $2"; }

# --- manifest.json parses, and manifest <-> filesystem agree both ways ---
MANIFEST="${PROD}/manifest.json"
assert_file "${MANIFEST}"
MANIFEST_PAGES=$(python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
for s in m["sections"]:
    for p in s["pages"]:
        f = p if isinstance(p, str) else p["file"]
        print(s["dir"] + "/" + f)
' "${MANIFEST}" 2>/dev/null)
if [ -n "${MANIFEST_PAGES}" ]; then
    ok "manifest.json parses and lists pages"
else
    bad "manifest.json parses and lists pages" "python3 could not read sections/pages"
fi

while IFS= read -r rel; do
    assert_file "${PROD}/${rel}"
done <<< "${MANIFEST_PAGES}"

# Every .md anywhere under docs/product/ except the charter README must be in
# the manifest — including pages in a section dir the manifest doesn't know
# yet, and stray top-level pages.
while IFS= read -r f; do
    rel="${f#"${PROD}"/}"
    [ "${rel}" = "README.md" ] && continue
    if echo "${MANIFEST_PAGES}" | grep -qxF "${rel}"; then
        ok "in manifest: ${rel}"
    else
        bad "page on disk but not in manifest (will NOT publish): ${rel}"
    fi
done < <(find "${PROD}" -name '*.md' | sort)

# --- every published page: single H1 on line 1, no YAML frontmatter ---
while IFS= read -r rel; do
    f="${PROD}/${rel}"
    [ -f "$f" ] || continue
    first=$(head -1 "$f")
    case "$first" in
        '# '*) ok "H1 on line 1: ${rel}" ;;
        '---')  bad "YAML frontmatter in ${rel} (site derives frontmatter from the H1)" ;;
        *)      bad "no H1 on line 1 of ${rel}" "line 1: ${first}" ;;
    esac
done <<< "${MANIFEST_PAGES}"

# --- every relative link in a published page resolves to a real file ---
# Cross-boundary links (out of docs/product/) publish as github.com URLs via
# the site's rewrite; that only works if the target actually exists. Catch a
# broken or moved target here, at PR time, on either side of the boundary.
BROKEN_LINKS=$(python3 -c '
import os, re, sys
root, prod = sys.argv[1], sys.argv[2]
pages = [l for l in sys.argv[3].splitlines() if l]
link_re = re.compile(r"!?\[[^\]]*\]\(([^)\s]+)\)")
for rel in pages:
    page = os.path.join(prod, rel)
    if not os.path.isfile(page):
        continue
    for target in link_re.findall(open(page).read()):
        if re.match(r"^(https?:|mailto:|#|/)", target):
            continue
        resolved = os.path.normpath(os.path.join(os.path.dirname(page), target.split("#")[0]))
        if not os.path.exists(resolved):
            print(f"{rel}: {target}")
' "${REPO_ROOT}" "${PROD}" "${MANIFEST_PAGES}")
[ -z "${BROKEN_LINKS}" ] && ok "all relative links in published pages resolve" \
    || bad "broken relative link(s) in published pages" "$(echo "${BROKEN_LINKS}" | head -5 | tr '\n' '|')"

# --- location-neutral + plain voice, whole tree (charter README exempt from
# --- the site-link rule: it documents the mirror) ---
hits=$(grep -rn 'kyber\.voget\.io' "${PROD}" --include='*.md' | grep -v 'product/README.md' || true)
[ -z "$hits" ] && ok "no hardcoded site links in published pages" \
    || bad "hardcoded kyber.voget.io link (pages must be location-neutral)" "$(echo "$hits" | head -3 | tr '\n' '|')"
hits=$(grep -rn $'—' "${PROD}" --include='*.md' || true)
[ -z "$hits" ] && ok "no em dashes anywhere in docs/product/" \
    || bad "em dash found (plain-voice rule)" "$(echo "$hits" | head -3 | tr '\n' '|')"

# --- WHAT/HOW boundary on concept pages: no implementation-detail markers ---
while IFS= read -r f; do
    rel="${f#"${REPO_ROOT}"/}"
    hits=$(grep -nEi '\.go([^a-z]|$)|pkg/|/controllers/|func [a-z]|reconciler|state_machine|```go' "$f" || true)
    [ -z "$hits" ] && ok "no implementation detail: ${rel}" \
        || bad "implementation detail leaked into ${rel}" "$(echo "$hits" | head -3 | tr '\n' '|')"
done < <(find "${PROD}/capabilities" "${PROD}/use-cases" -name '*.md' 2>/dev/null | sort)

# --- README states the charter; the two doc sets cross-link both ways ---
assert_grep "${PROD}/README.md" 'WHAT'            "README states scope is WHAT"
assert_grep "${PROD}/README.md" 'WHAT, not HOW|WHAT/HOW' "README names the WHAT/HOW boundary"
assert_grep "${PROD}/README.md" 'source of truth' "README declares itself the product source of truth"
assert_grep "${PROD}/README.md" 'manifest\.json'  "README documents the publication manifest"
assert_grep "${PROD}/README.md" 'docs/architecture|\.\./architecture' "product README links to the architecture (HOW) set"
assert_grep "${REPO_ROOT}/docs/architecture/overview.md" 'docs/product|\.\./product' "architecture overview back-links to the product (WHAT) set"
assert_grep "${REPO_ROOT}/README.md" 'docs/product' "root README links to docs/product/"

# --- lifecycle vocabulary survives on the persistence page ---
for ph in 'Running' 'NeedsAuth'; do
    assert_grep "${PROD}/capabilities/agents-and-persistence.md" "$ph" "agents-and-persistence names the ${ph} phase"
done

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    printf 'Failed:\n'; for t in "${FAILED[@]}"; do printf '  - %s\n' "$t"; done
    exit 1
fi
