#!/usr/bin/env bash
# Tests for scripts/devenv/up.sh — the one-command dev/test-environment
# bring-up wrapper (kyber#399, Phase 1).
# Run from repo root: bash scripts/devenv/up_test.sh
#
# Strategy (mirrors scripts/preflight-tag-not-published_test.sh): drop fake
# `k3d`/`docker`/`helm`/`kubectl`/`curl` binaries on PATH that append their
# argv to a shared command log, then exercise up.sh and assert (a) exit code
# and (b) the orchestration it drove (which tools it invoked, with which args).
# This verifies the bring-up logic deterministically without a real cluster or
# container runtime — the pod-runnability path (a live run) is Phase 3 (#403).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT_DIR}/up.sh"

PASS=0
FAIL=0
FAILED_TESTS=()

# make_mocks builds a temp dir of fake tools and returns its path. The fakes
# log every invocation to ${dir}/cmdlog. Behaviour is steered by env vars the
# caller exports before running up.sh:
#   FAKE_CLUSTER_EXISTS=1  -> `k3d cluster list <name>` exits 0 (cluster present)
make_mocks() {
    local dir
    dir=$(mktemp -d)

    cat >"${dir}/k3d" <<'MOCK'
#!/usr/bin/env bash
echo "k3d $*" >> "${CMDLOG}"
# `cluster list <name>` is the existence probe: exit 0 if the test says the
# cluster exists, non-zero otherwise.
if [ "${1:-}" = "cluster" ] && [ "${2:-}" = "list" ]; then
    [ -n "${FAKE_CLUSTER_EXISTS:-}" ] && exit 0 || exit 1
fi
exit 0
MOCK

    cat >"${dir}/docker" <<'MOCK'
#!/usr/bin/env bash
echo "docker $*" >> "${CMDLOG}"
exit 0
MOCK

    cat >"${dir}/helm" <<'MOCK'
#!/usr/bin/env bash
echo "helm $*" >> "${CMDLOG}"
exit 0
MOCK

    cat >"${dir}/kubectl" <<'MOCK'
#!/usr/bin/env bash
echo "kubectl $*" >> "${CMDLOG}"
# port-forward is started in the background by up.sh; just exit cleanly.
exit 0
MOCK

    cat >"${dir}/curl" <<'MOCK'
#!/usr/bin/env bash
echo "curl $*" >> "${CMDLOG}"
# The /healthz probe expects an HTTP status; report ready immediately.
echo "200"
exit 0
MOCK

    chmod +x "${dir}"/*
    echo "${dir}"
}

# run <name> <expected_code> <mock_dir> [args...]
# Exports a fresh CMDLOG per run; leaves it in $LAST_CMDLOG for assertions.
run() {
    local name="$1"; shift
    local expected_code="$1"; shift
    local mock_dir="$1"; shift
    local log="${mock_dir}/cmdlog"
    : > "${log}"
    LAST_OUT=$(cd "${REPO_ROOT}" && CMDLOG="${log}" \
        DEVENV_HEALTH_INTERVAL=0 DEVENV_HEALTH_TIMEOUT=2 \
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

# assert_logged <name> <grep-pattern> — the command log must contain a match.
assert_logged() {
    local name="$1" pat="$2"
    if grep -Eq "$pat" "${LAST_CMDLOG}"; then
        PASS=$((PASS+1)); echo "PASS: ${name}"
    else
        FAIL=$((FAIL+1)); FAILED_TESTS+=("${name}")
        echo "FAIL: ${name} — pattern not found in command log: ${pat}"
        echo "      log: $(tr '\n' '|' < "${LAST_CMDLOG}")"
    fi
}

# assert_not_logged <name> <grep-pattern> — the command log must NOT match.
assert_not_logged() {
    local name="$1" pat="$2"
    if grep -Eq "$pat" "${LAST_CMDLOG}"; then
        FAIL=$((FAIL+1)); FAILED_TESTS+=("${name}")
        echo "FAIL: ${name} — pattern unexpectedly present: ${pat}"
        echo "      log: $(tr '\n' '|' < "${LAST_CMDLOG}")"
    else
        PASS=$((PASS+1)); echo "PASS: ${name}"
    fi
}

# assert_out <name> <grep-pattern> — up.sh stdout/stderr must contain a match.
assert_out() {
    local name="$1" pat="$2"
    if printf '%s' "${LAST_OUT}" | grep -Eq "$pat"; then
        PASS=$((PASS+1)); echo "PASS: ${name}"
    else
        FAIL=$((FAIL+1)); FAILED_TESTS+=("${name}")
        echo "FAIL: ${name} — pattern not found in output: ${pat}"
        echo "      output: ${LAST_OUT}"
    fi
}

REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# ---------------------------------------------------------------------------
# Case 1 — happy path, fresh cluster: drives the full setup_test.go sequence.
# ---------------------------------------------------------------------------
m=$(make_mocks)
run "fresh bring-up succeeds" 0 "${m}"
assert_logged   "creates the k3d cluster"            'k3d cluster create kyber-devenv'
assert_logged   "builds the control-plane image"     'docker build .*images/control-plane/Dockerfile'
assert_logged   "imports the image into k3d"         'k3d image import .*kyber/control-plane:local'
assert_logged   "helm install references shared values-test.yaml" \
                'helm upgrade --install .*-f .*test/e2e/values-test.yaml'
assert_logged   "helm install targets kyber-system"  'helm upgrade --install .*--namespace kyber-system'
assert_logged   "starts the API port-forward"        'kubectl port-forward .*svc/kyber-control-plane'
assert_logged   "polls /healthz"                     'curl .*/healthz'
rm -rf "${m}"

# ---------------------------------------------------------------------------
# Case 2 — contract is printed (API base + creds sourced from values-test.yaml).
# ---------------------------------------------------------------------------
m=$(make_mocks)
run "bring-up prints the contract" 0 "${m}"
assert_out "contract shows API base URL"        'http://localhost:18080'
assert_out "contract shows healthz endpoint"    '/healthz'
assert_out "contract shows the test API key"    'test-api-key-e2e'
assert_out "contract shows the webhook secret"  'test-webhook-secret-e2e'
rm -rf "${m}"

# ---------------------------------------------------------------------------
# Case 3 — --skip-build skips docker build + image import (warm fast path).
# ---------------------------------------------------------------------------
m=$(make_mocks)
run "--skip-build succeeds" 0 "${m}" --skip-build
assert_not_logged "skip-build avoids docker build"  'docker build'
assert_not_logged "skip-build avoids image import"  'k3d image import'
assert_logged     "skip-build still installs"       'helm upgrade --install'
rm -rf "${m}"

# ---------------------------------------------------------------------------
# Case 4 — idempotent: an existing cluster is reused, not re-created.
# ---------------------------------------------------------------------------
m=$(make_mocks)
FAKE_CLUSTER_EXISTS=1 run "existing-cluster run succeeds" 0 "${m}"
assert_not_logged "does not re-create an existing cluster" 'k3d cluster create'
assert_logged     "still converges via helm upgrade"       'helm upgrade --install'
unset FAKE_CLUSTER_EXISTS
rm -rf "${m}"

# ---------------------------------------------------------------------------
# Case 5 — --api-port override flows to port-forward and the printed contract.
# ---------------------------------------------------------------------------
m=$(make_mocks)
run "custom api-port succeeds" 0 "${m}" --api-port 29090
assert_logged "port-forward uses the override"  'kubectl port-forward .*29090:8080'
assert_out    "contract reflects the override"  'http://localhost:29090'
rm -rf "${m}"

# ---------------------------------------------------------------------------
# Case 6 — unknown argument is a usage error (exit 2).
# ---------------------------------------------------------------------------
m=$(make_mocks)
run "unknown arg is a usage error" 2 "${m}" --bogus-flag
rm -rf "${m}"

# ---------------------------------------------------------------------------
# Case 7 — a missing dependency fails fast with a clear message (preflight).
# ---------------------------------------------------------------------------
m=$(make_mocks)
rm -f "${m}/helm"   # simulate helm not installed
run "missing dependency fails fast" 3 "${m}"
assert_out "names the missing dependency" 'helm'
rm -rf "${m}"

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    printf 'Failed tests:\n'
    for t in "${FAILED_TESTS[@]}"; do printf '  - %s\n' "$t"; done
    exit 1
fi
