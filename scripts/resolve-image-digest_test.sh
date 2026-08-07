#!/usr/bin/env bash
# Tests for scripts/resolve-image-digest.sh — resolves the GHCR
# manifest digest for a given image:tag pair so deploy-bump-pr can
# write `tag@sha256:...` into kyber-deploy values.yaml.
# Run from repo root: bash scripts/resolve-image-digest_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT_DIR}/resolve-image-digest.sh"

PASS=0
FAIL=0
FAILED_TESTS=()

run() {
    local name="$1"; shift
    local expected_code="$1"; shift
    local expected_stdout="$1"; shift
    local mock_dir="$1"; shift
    local out rc
    out=$(PATH="${mock_dir}:${PATH}" "${SCRIPT}" "$@" 2>/dev/null)
    rc=$?
    if [ "$rc" -ne "$expected_code" ]; then
        FAIL=$((FAIL+1))
        FAILED_TESTS+=("${name}")
        echo "FAIL: ${name} — expected exit ${expected_code}, got ${rc} (output: ${out})"
        return
    fi
    if [ -n "$expected_stdout" ] && [ "$out" != "$expected_stdout" ]; then
        FAIL=$((FAIL+1))
        FAILED_TESTS+=("${name}")
        echo "FAIL: ${name} — expected stdout '${expected_stdout}', got '${out}'"
        return
    fi
    PASS=$((PASS+1))
    echo "PASS: ${name}"
}

make_mock_gh() {
    local body="$1" code="$2"
    local dir
    dir=$(mktemp -d)
    cat >"${dir}/gh" <<MOCK
#!/usr/bin/env bash
cat <<'BODY'
${body}
BODY
exit ${code}
MOCK
    chmod +x "${dir}/gh"
    echo "${dir}"
}

# Case 1: tag is present — print the version's name (the sha256 digest).
# The `--jq` filter on the real call returns just `.name` for the
# matching version, so the mock returns the digest directly.
mock1=$(make_mock_gh 'sha256:5089b6abc123' 0)
run "tag found → prints digest" 0 "sha256:5089b6abc123" "${mock1}" kyber-node-agent v1.3.3

# Case 2: tag not found — error (caller is the release pipeline, which
# just pushed the tag; if it isn't visible something is wrong).
mock2=$(make_mock_gh '' 0)
run "tag not found → error" 1 "" "${mock2}" kyber-node-agent v9.9.9

# Case 3: missing args.
mock3=$(make_mock_gh '' 0)
run "missing tag arg → usage error" 2 "" "${mock3}" kyber-node-agent

rm -rf "${mock1}" "${mock2}" "${mock3}"

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    printf 'Failed tests:\n'
    for t in "${FAILED_TESTS[@]}"; do printf '  - %s\n' "$t"; done
    exit 1
fi
