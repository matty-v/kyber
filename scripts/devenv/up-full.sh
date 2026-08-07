#!/usr/bin/env bash
#
# Bring up a COMPLETE local kyber on k3d — control-plane AND live agent pods that
# actually schedule and run Claude Code (the base scripts/devenv/up.sh is
# control-plane/API only; pod-runnability is deferred there, see its README).
#
# Self-contained on purpose: it does NOT call up.sh, because up.sh --skip-build
# skips importing the control-plane image, which leaves a fresh cluster with no
# runnable control-plane. This script imports every image itself, then installs
# the full-local profile in one pass.
#
# What it does:
#   - create the k3d cluster (reuse if present; --recreate to start fresh)
#   - build + import all images (build-local-images.sh)
#   - create the internal signing key so the in-pod status pipeline (:8082) works
#   - helm install with scripts/devenv/values-full-local.yaml layered on the base
#     test values (local-path storage, node-agent on, local images, CORS for the
#     vite dev servers)
#   - port-forward the API and wait for /healthz
#
# Usage:
#   scripts/devenv/up-full.sh [--skip-build] [--recreate] [--api-port N] [--cluster-name NAME]
#     --skip-build   reuse already-built :local images (skips the slow builds)
#
# If .kyber-local/github-app/ exists (created by setup-github-app.sh), this
# also installs the developer's GitHub App Secret and enables identity repos.
#
# Tear down with scripts/devenv/down.sh. Then create a live agent through the PWA
# wizard (OAuth) at http://localhost:<api-port>/ — see scripts/devenv/full-local.md.
#
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root
# shellcheck source=scripts/devenv/lib.sh
source scripts/devenv/lib.sh

OVERLAY="scripts/devenv/values-full-local.yaml"
GITHUB_APP_DIR="${DEVENV_GITHUB_APP_DIR:-.kyber-local/github-app}"
SKIP_BUILD=""
RECREATE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --skip-build)   SKIP_BUILD=1; shift ;;
    --recreate)     RECREATE=1; shift ;;
    --api-port)     API_PORT="$2"; shift 2 ;;
    --cluster-name) CLUSTER_NAME="$2"; shift 2 ;;
    *) die "unknown arg: $1" 2 ;;
  esac
done

preflight_deps "${SKIP_BUILD}"

# --- cluster ---------------------------------------------------------------
if [ -n "${RECREATE}" ] && cluster_exists; then
  log "recreating cluster ${CLUSTER_NAME}"
  k3d cluster delete "${CLUSTER_NAME}" >/dev/null 2>&1 || true
fi
if cluster_exists; then
  log "reusing cluster ${CLUSTER_NAME}"
else
  log "creating cluster ${CLUSTER_NAME}"
  k3d cluster create "${CLUSTER_NAME}" --no-lb --wait || die "k3d cluster create failed"
fi

export KUBECONFIG="$(k3d kubeconfig write "${CLUSTER_NAME}")"

# --- images ----------------------------------------------------------------
scripts/devenv/build-local-images.sh --cluster-name "${CLUSTER_NAME}" ${SKIP_BUILD:+--skip-build}

# --- namespace + internal signing key (before helm so the CP reads it at boot) -
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "${NAMESPACE}" create secret generic kyber-internal-signing-key \
  --from-literal=signing-key="$(openssl rand -hex 32)" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

GITHUB_APP_OWNER=""
if [ -d "${GITHUB_APP_DIR}" ]; then
  for file in app-id installation-id owner private-key.pem; do
    [ -s "${GITHUB_APP_DIR}/${file}" ] || die "incomplete GitHub App config: missing ${GITHUB_APP_DIR}/${file}"
  done
  GITHUB_APP_OWNER="$(tr -d '[:space:]' <"${GITHUB_APP_DIR}/owner")"
  [[ "$(tr -d '[:space:]' <"${GITHUB_APP_DIR}/app-id")" =~ ^[1-9][0-9]*$ ]] || die "invalid GitHub App ID"
  [[ "$(tr -d '[:space:]' <"${GITHUB_APP_DIR}/installation-id")" =~ ^[1-9][0-9]*$ ]] || die "invalid GitHub App installation ID"
  [[ "${GITHUB_APP_OWNER}" =~ ^[A-Za-z0-9][A-Za-z0-9-]{0,38}$ ]] || die "invalid GitHub App owner"
  log "configuring local GitHub App for identity repos owned by ${GITHUB_APP_OWNER}"
  kubectl -n "${NAMESPACE}" create secret generic kyber-github-app \
    --from-file=app-id="${GITHUB_APP_DIR}/app-id" \
    --from-file=installation-id="${GITHUB_APP_DIR}/installation-id" \
    --from-file=private-key.pem="${GITHUB_APP_DIR}/private-key.pem" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
fi

# --- install (full-local overlay on top of the base test values) -----------
log "installing kyber (full-local profile)"
HELM_ARGS=(
  upgrade --install "${HELM_RELEASE}" "${HELM_CHART}"
  -f "${VALUES_FILE}"
  -f "${OVERLAY}"
  --namespace "${NAMESPACE}" --create-namespace --set namespace.create=false
)
if [ -n "${GITHUB_APP_OWNER}" ]; then
  HELM_ARGS+=(--set-string "identityRepo.defaultOwner=${GITHUB_APP_OWNER}")
fi
HELM_ARGS+=(--wait --timeout 5m)
helm "${HELM_ARGS[@]}" || die "helm upgrade --install failed"

if [ -n "${GITHUB_APP_OWNER}" ]; then
  # The Secret is loaded only at control-plane startup and is intentionally not
  # chart-managed, so changing the local bundle would not otherwise roll pods.
  kubectl rollout restart -n "${NAMESPACE}" deployment/kyber-control-plane >/dev/null
  kubectl rollout status -n "${NAMESPACE}" deployment/kyber-control-plane --timeout=2m >/dev/null \
    || die "control-plane did not become ready after GitHub App configuration"
fi

# --- port-forward + health -------------------------------------------------
pkill -f "port-forward.*${API_PORT}:${SERVICE_PORT}" 2>/dev/null || true
sleep 1
nohup kubectl port-forward -n "${NAMESPACE}" "${SERVICE}" "${API_PORT}:${SERVICE_PORT}" \
  >"${PF_PIDFILE}.log" 2>&1 &
echo $! > "${PF_PIDFILE}"

log "waiting for API at http://localhost:${API_PORT}/healthz"
ok=""
for _ in $(seq 1 "${HEALTH_TIMEOUT}"); do
  if curl -fsS "http://localhost:${API_PORT}/healthz" >/dev/null 2>&1; then ok=1; break; fi
  sleep "${HEALTH_INTERVAL}"
done
[ -n "$ok" ] || die "API did not become healthy within ${HEALTH_TIMEOUT}s"

print_contract
cat <<EOF

=== devenv: full-local extras (live agent pods) ===
  node-agent:    $(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/component=node-agent --no-headers 2>/dev/null | awk '{print $3}' | head -1)
  GitHub App:    ${GITHUB_APP_OWNER:-not configured}
  Create a live agent via the PWA wizard (OAuth) at http://localhost:${API_PORT}/
  or the API — see scripts/devenv/full-local.md.
EOF
