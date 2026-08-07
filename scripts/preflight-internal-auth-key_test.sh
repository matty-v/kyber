#!/usr/bin/env bash
# Tests for scripts/preflight-internal-auth-key.sh — the kyber#578 Layer-2
# internal-auth key-presence deploy gate. Run from repo root:
#   bash scripts/preflight-internal-auth-key_test.sh
#
# Strategy: drop a fake `kubectl` on PATH whose behavior is driven by env vars
# (secret present? logs contain the marker?), then exercise the script and assert
# exit codes + key output.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT_DIR}/preflight-internal-auth-key.sh"

PASS=0
FAIL=0
FAILED_TESTS=()

# make_mock_kubectl SECRET_EXISTS(0|1) LOGS_TEXT -> echoes mock dir.
# The fake kubectl:
#   `get secret <name>` → exit 0 if MOCK_SECRET_EXISTS=1, else exit 1 (NotFound).
#   `logs deploy/<dep>` → prints $MOCK_LOGS, exit 0 (or exit 1 if MOCK_LOGS_FAIL=1).
make_mock_kubectl() {
    local secret_exists="$1" logs_text="$2" logs_fail="${3:-0}"
    local dir
    dir=$(mktemp -d)
    cat >"${dir}/kubectl" <<MOCK
#!/usr/bin/env bash
args="\$*"
case "\$args" in
    *"get secret"*)
        exit ${secret_exists}_INVERT
        ;;
    *"logs "*)
        if [ "${logs_fail}" = "1" ]; then
            echo "Error from server (NotFound): deployments.apps not found" >&2
            exit 1
        fi
        cat <<'LOGS'
${logs_text}
LOGS
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
MOCK
    # secret_exists=1 should mean `get secret` exits 0 (present); map it.
    if [ "$secret_exists" = "1" ]; then
        sed -i 's/exit 1_INVERT/exit 0/' "${dir}/kubectl"
    else
        sed -i 's/exit 1_INVERT/exit 1/' "${dir}/kubectl"
    fi
    chmod +x "${dir}/kubectl"
    echo "${dir}"
}

run() {
    local name="$1"; shift
    local expected_code="$1"; shift
    local mock_dir="$1"; shift
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

# --- Pre-apply gate ----------------------------------------------------------

# Enforce + key absent → ABORT (the v2.1.0 outage shape). THE headline case.
mock_no_secret=$(make_mock_kubectl 0 "")
run "enforce + no key → ABORT (exit 1)" 1 "${mock_no_secret}" kyber-system false

# Enforce + key present → OK.
mock_secret=$(make_mock_kubectl 1 "")
run "enforce + key present (graceMode=false) → OK" 0 "${mock_secret}" kyber-system false

# Grace + key absent → WARN but proceed (fail-closed + alert, not a hard abort).
run "grace + no key → WARN, proceed (exit 0)" 0 "${mock_no_secret}" kyber-system true

# Grace + key present → OK.
run "grace + key present → OK (exit 0)" 0 "${mock_secret}" kyber-system true

# --- Post-apply check --------------------------------------------------------

# Logs contain the auth-enabled marker → healthy.
mock_logs_ok=$(make_mock_kubectl 1 "starting control plane
internal API per-agent auth enabled
ready")
run "post-apply: auth-enabled logged → healthy (exit 0)" 0 "${mock_logs_ok}" \
    kyber-system false --post-apply kyber-control-plane

# Logs do NOT contain the marker (fail-closed) → abort.
mock_logs_failclosed=$(make_mock_kubectl 1 "starting control plane
KYBER_INTERNAL_SIGNING_KEY is empty — internal API (:8082) FAILING CLOSED
ready")
run "post-apply: auth-enabled NOT logged (fail-closed) → ABORT (exit 1)" 1 "${mock_logs_failclosed}" \
    kyber-system false --post-apply kyber-control-plane

# kubectl logs error → abort (can't confirm healthy).
mock_logs_err=$(make_mock_kubectl 1 "" 1)
run "post-apply: kubectl logs error → ABORT (exit 1)" 1 "${mock_logs_err}" \
    kyber-system false --post-apply kyber-control-plane

# --- Usage -------------------------------------------------------------------
run "missing args → usage error (exit 2)" 2 "${mock_secret}" kyber-system
run "bad graceMode → usage error (exit 2)" 2 "${mock_secret}" kyber-system maybe

# --- Summary -----------------------------------------------------------------
echo ""
echo "preflight-internal-auth-key tests: ${PASS} passed, ${FAIL} failed"
if [ "$FAIL" -gt 0 ]; then
    printf '  failed: %s\n' "${FAILED_TESTS[@]}"
    exit 1
fi
exit 0
