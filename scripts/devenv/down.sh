#!/usr/bin/env bash
# scripts/devenv/down.sh — one-command teardown of the dev/test environment
# brought up by up.sh (kyber#399, Phase 1).
#
# Idempotent and orphan-free: deletes the k3d cluster if present, reaps the
# background port-forward if its PID file exists, and is a clean no-op (exit 0,
# no error) when there is nothing to tear down.
#
# Usage:
#   scripts/devenv/down.sh [--cluster-name NAME] [-h|--help]
#
# Exit codes: 0 ok | 2 usage error | 3 missing dependency.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/devenv/lib.sh
source "${SCRIPT_DIR}/lib.sh"

# Print the leading comment block (the usage header) — every line after the
# shebang up to the first non-comment line.
usage() { sed -n '2,${/^[^#]/q;p;}' "${BASH_SOURCE[0]}" | sed 's/^#\{1,\} \{0,1\}//'; }

while [ "$#" -gt 0 ]; do
    case "$1" in
        --cluster-name) shift; [ "$#" -gt 0 ] || die "--cluster-name needs a value" 2; CLUSTER_NAME="$1" ;;
        -h|--help)      usage; exit 0 ;;
        *)              warn "unknown argument: $1"; usage; exit 2 ;;
    esac
    shift
done

# Reap the background port-forward first so deleting the cluster leaves no
# dangling kubectl process holding the local port.
if [ -f "${PF_PIDFILE}" ]; then
    pid="$(cat "${PF_PIDFILE}" 2>/dev/null || true)"
    if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
        log "stopping port-forward (pid ${pid})"
        kill "${pid}" 2>/dev/null || true
    fi
    rm -f "${PF_PIDFILE}"
fi

# Deleting the cluster needs only k3d; a missing k3d is a real error here.
command -v k3d >/dev/null 2>&1 || die "k3d not found — cannot tear down (see scripts/devenv/README.md)" "${DEP_MISSING_CODE}"

if cluster_exists; then
    log "deleting k3d cluster ${CLUSTER_NAME}"
    k3d cluster delete "${CLUSTER_NAME}" || die "k3d cluster delete failed"
    log "teardown complete — no orphaned cluster remains"
else
    log "no cluster ${CLUSTER_NAME} found — nothing to tear down"
fi
