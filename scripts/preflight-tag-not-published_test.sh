#!/usr/bin/env bash
# Tests for scripts/preflight-tag-not-published.sh — the release-time
# guard that errors if a kyber-* image tag is already published on GHCR.
# Run from repo root: bash scripts/preflight-tag-not-published_test.sh
#
# Strategy: drop a fake `gh` binary on PATH that returns canned fixtures
# based on its arguments, then exercise the script and assert exit codes.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT_DIR}/preflight-tag-not-published.sh"

PASS=0
FAIL=0
FAILED_TESTS=()

run() {
    local name="$1"; shift
    local expected_code="$1"; shift
    local mock_dir="$1"; shift
    # Remaining args are passed to the script under test.
    local out rc
    out=$(PATH="${mock_dir}:${PATH}" "${SCRIPT}" "$@" 2>&1)
    rc=$?
    if [ "$rc" -eq "$expected_code" ]; then
        PASS=$((PASS+1))
        echo "PASS: ${name}"
    else
        FAIL=$((FAIL+1))
        FAILED_TESTS+=("${name}")
        echo "FAIL: ${name} — expected exit ${expected_code}, got ${rc}"
        echo "      output: ${out}"
    fi
}

make_mock_gh() {
    # $1 = stdout fixture; $2 = exit code; returns path to mock dir.
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

# Case 1: tag is already published — script must exit non-zero.
# Fixture: jq filter on a real GHCR versions response returns one line.
mock1=$(make_mock_gh '{"id":1,"name":"sha256:abc","metadata":{"container":{"tags":["v1.3.3"]}}}' 0)
run "tag already published → fails fast" 1 "${mock1}" kyber-node-agent v1.3.3

# Case 2: tag is not published — script must exit 0.
mock2=$(make_mock_gh '' 0)
run "tag not published → passes" 0 "${mock2}" kyber-node-agent v1.3.4

# Case 3: package does not exist (first-ever push) — gh exits non-zero
# with a 404. Treat as "not published" — first release of a new image
# must not be blocked by the guard.
mock3=$(make_mock_gh 'gh: HTTP 404: Not Found' 1)
run "package not found (first push) → passes" 0 "${mock3}" kyber-new-image v0.1.0

# Case 4: missing args — usage error.
mock4=$(make_mock_gh '' 0)
run "missing tag arg → usage error" 2 "${mock4}" kyber-node-agent

# Cleanup mock dirs.
rm -rf "${mock1}" "${mock2}" "${mock3}" "${mock4}"

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    printf 'Failed tests:\n'
    for t in "${FAILED_TESTS[@]}"; do printf '  - %s\n' "$t"; done
    exit 1
fi
