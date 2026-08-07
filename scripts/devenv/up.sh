#!/usr/bin/env bash
# scripts/devenv/up.sh — one-command bring-up of a local, mock-backed Kyber
# instance for agents to run and observe (kyber#399, Phase 1).
#
# Wraps the orchestration test/e2e/setup_test.go already performs, reusing the
# mock-env Helm profile test/e2e/values-test.yaml (by reference — not forked)
# and the chart's default compute.provider=mock (MockComputeAdapter). No real
# cloud, auth, or prod-network access is required or used.
#
# Usage:
#   scripts/devenv/up.sh [--skip-build] [--recreate] [--api-port N]
#                        [--cluster-name NAME] [-h|--help]
#
# Flags:
#   --skip-build        reuse an already-built/imported control-plane image
#                       (the warm fast path; skips the slow `docker build`).
#   --recreate          delete an existing devenv cluster first, then build fresh.
#   --api-port N        local port to port-forward the API to (default 18080).
#   --cluster-name NAME k3d cluster name (default kyber-devenv).
#
# Exit codes: 0 ok | 2 usage error | 3 missing dependency.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/devenv/lib.sh
source "${SCRIPT_DIR}/lib.sh"

SKIP_BUILD=""
RECREATE=""

# Print the leading comment block (the usage header) — every line after the
# shebang up to the first non-comment line. Robust to edits above set -uo.
usage() { sed -n '2,${/^[^#]/q;p;}' "${BASH_SOURCE[0]}" | sed 's/^#\{1,\} \{0,1\}//'; }

while [ "$#" -gt 0 ]; do
    case "$1" in
        --skip-build)   SKIP_BUILD=1 ;;
        --recreate)     RECREATE=1 ;;
        --api-port)     shift; [ "$#" -gt 0 ] || die "--api-port needs a value" 2; API_PORT="$1" ;;
        --cluster-name) shift; [ "$#" -gt 0 ] || die "--cluster-name needs a value" 2; CLUSTER_NAME="$1" ;;
        -h|--help)      usage; exit 0 ;;
        *)              warn "unknown argument: $1"; usage; exit 2 ;;
    esac
    shift
done

[[ "${API_PORT}" =~ ^[0-9]+$ ]] || die "--api-port must be numeric, got: ${API_PORT}" 2

preflight_deps "${SKIP_BUILD}"
cd "${REPO_ROOT}" || die "cannot cd to repo root ${REPO_ROOT}"

# --- Cluster: create fresh, or reuse an existing one (idempotent) ----------
if cluster_exists; then
    if [ -n "${RECREATE}" ]; then
        log "recreating cluster ${CLUSTER_NAME} (--recreate)"
        k3d cluster delete "${CLUSTER_NAME}" || die "k3d cluster delete failed"
        k3d cluster create "${CLUSTER_NAME}" --no-lb --wait || die "k3d cluster create failed"
    else
        log "reusing existing cluster ${CLUSTER_NAME} (idempotent re-run; pass --recreate for a clean slate)"
    fi
else
    log "creating k3d cluster ${CLUSTER_NAME}"
    k3d cluster create "${CLUSTER_NAME}" --no-lb --wait || die "k3d cluster create failed"
fi

# --- Build + import the control-plane image (skippable warm path) ----------
if [ -n "${SKIP_BUILD}" ]; then
    log "skipping image build/import (--skip-build)"
else
    log "building control-plane image ${CONTROL_PLANE_IMAGE}"
    docker build -t "${CONTROL_PLANE_IMAGE}" -f images/control-plane/Dockerfile . \
        || die "docker build failed"
    log "importing image into cluster ${CLUSTER_NAME}"
    k3d image import -c "${CLUSTER_NAME}" "${CONTROL_PLANE_IMAGE}" \
        || die "k3d image import failed"
fi

# --- Install Kyber via Helm with the mock profile (idempotent upgrade) -----
log "installing Kyber via Helm (mock profile: ${VALUES_FILE})"
helm upgrade --install "${HELM_RELEASE}" "${HELM_CHART}" \
    -f "${VALUES_FILE}" \
    --namespace "${NAMESPACE}" \
    --create-namespace \
    --set namespace.create=false \
    --wait \
    --timeout 3m \
    || die "helm upgrade --install failed"

# --- Port-forward the API in the background --------------------------------
log "starting API port-forward localhost:${API_PORT} -> ${SERVICE}:${SERVICE_PORT}"
kubectl port-forward -n "${NAMESPACE}" "${SERVICE}" "${API_PORT}:${SERVICE_PORT}" \
    >/dev/null 2>&1 &
echo "$!" > "${PF_PIDFILE}"

# --- Wait for /healthz -----------------------------------------------------
health_url="http://localhost:${API_PORT}/healthz"
log "waiting for API at ${health_url}"
deadline=$(( SECONDS + HEALTH_TIMEOUT ))
ready=""
while [ "${SECONDS}" -lt "${deadline}" ]; do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${health_url}" 2>/dev/null || true)"
    if [ "${code}" = "200" ]; then ready=1; break; fi
    [ "${HEALTH_INTERVAL}" -gt 0 ] && sleep "${HEALTH_INTERVAL}"
    [ "${HEALTH_INTERVAL}" -eq 0 ] && break  # fast-poll mode for tests
done

if [ -z "${ready}" ]; then
    warn "API did not become ready within ${HEALTH_TIMEOUT}s at ${health_url}"
    warn "leaving the environment up for inspection; tear down with scripts/devenv/down.sh"
    exit 1
fi

print_contract
