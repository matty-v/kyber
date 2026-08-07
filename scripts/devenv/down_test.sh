#!/usr/bin/env bash
# Tests for scripts/devenv/down.sh — the one-command teardown (kyber#399,
# Phase 1). Run from repo root: bash scripts/devenv/down_test.sh
#
# Same fake-binary strategy as up_test.sh: a fake `k3d` logs its argv and
# reports cluster presence via FAKE_CLUSTER_EXISTS, so we can assert that
# teardown deletes the cluster when present and is a clean no-op (no orphans,
# no error) when it is already gone.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT_DIR}/down.sh"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

PASS=0
FAIL=0
FAILED_TESTS=()

make_mocks() {
    local dir
    dir=$(mktemp -d)
    cat >"${dir}/k3d" <<'MOCK'
#!/usr/bin/env bash
echo "k3d $*" >> "${CMDLOG}"
if [ "${1:-}" = "cluster" ] && [ "${2:-}" = "list" ]; then
    [ -n "${FAKE_CLUSTER_EXISTS:-}" ] && exit 0 || exit 1
fi
exit 0
MOCK
    cat >"${dir}/kubectl" <<'MOCK'
#!/usr/bin/env bash
echo "kubectl $*" >> "${CMDLOG}"
exit 0
MOCK
    chmod +x "${dir}"/*
    echo "${dir}"
}

run() {
    local name="$1"; shift
    local expected_code="$1"; shift
    local mock_dir="$1"; shift
    local log="${mock_dir}/cmdlog"
    : > "${log}"
    LAST_OUT=$(cd "${REPO_ROOT}" && CMDLOG="${log}" \
        PATH="${mock_dir}:${PATH}" "${SCRIPT}" "$@" 2>&1)
    local rc=$?
    LAST_CMDLOG="${log}"
    if [ "$rc" -eq "$expected_code" ]; then
        PASS=$((PASS+1)); echo "PASS: ${name}"
    else
        FAIL=$((FAIL+1)); FAILED_TESTS+=("${name}")
        echo "FAIL: ${name} — expected exit ${expected_code}, got ${rc}"
        echo "      output: ${LAST_OUT}"
    fi
}

assert_logged() {
    local name="$1" pat="$2"
    if grep -Eq "$pat" "${LAST_CMDLOG}"; then
        PASS=$((PASS+1)); echo "PASS: ${name}"
    else
        FAIL=$((FAIL+1)); FAILED_TESTS+=("${name}")
        echo "FAIL: ${name} — pattern not found: ${pat}"
        echo "      log: $(tr '\n' '|' < "${LAST_CMDLOG}")"
    fi
}

# Case 1 — cluster present: teardown deletes it.
m=$(make_mocks)
FAKE_CLUSTER_EXISTS=1 run "deletes an existing cluster" 0 "${m}"
assert_logged "k3d cluster delete called" 'k3d cluster delete kyber-devenv'
unset FAKE_CLUSTER_EXISTS
rm -rf "${m}"

# Case 2 — cluster absent: idempotent clean no-op (no orphans, exit 0).
m=$(make_mocks)
run "absent cluster is a clean no-op" 0 "${m}"
rm -rf "${m}"

# Case 3 — unknown argument is a usage error.
m=$(make_mocks)
run "unknown arg is a usage error" 2 "${m}" --bogus
rm -rf "${m}"

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    printf 'Failed tests:\n'
    for t in "${FAILED_TESTS[@]}"; do printf '  - %s\n' "$t"; done
    exit 1
fi
