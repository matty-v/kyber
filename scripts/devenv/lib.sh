#!/usr/bin/env bash
# scripts/devenv/lib.sh — shared helpers for the dev/test-environment scripts
# (kyber#399, Phase 1). Sourced by up.sh and down.sh; not executed directly.
#
# Design: these scripts wrap the exact orchestration test/e2e/setup_test.go
# already performs (k3d create -> build/import -> helm install with the mock
# profile -> port-forward -> /healthz wait), so that any agent can bring the
# environment up with one command and no Go toolchain. The mock-env Helm
# profile (test/e2e/values-test.yaml) is REUSED BY REFERENCE, never forked —
# a divergent copy that drifts from the e2e profile is a slow-acting trap.

# --- Config (override via flags or the listed env vars) --------------------
# A distinct cluster name from the e2e harness's "kyber-e2e" so a devenv
# bring-up never collides with an in-progress `go test` e2e run.
CLUSTER_NAME="${DEVENV_CLUSTER:-kyber-devenv}"
HELM_RELEASE="${DEVENV_HELM_RELEASE:-kyber}"
HELM_CHART="${DEVENV_HELM_CHART:-deploy/helm/kyber}"
VALUES_FILE="${DEVENV_VALUES_FILE:-test/e2e/values-test.yaml}"
NAMESPACE="${DEVENV_NAMESPACE:-kyber-system}"
API_PORT="${DEVENV_API_PORT:-18080}"
CONTROL_PLANE_IMAGE="kyber/control-plane:local"
SERVICE="svc/kyber-control-plane"
# The control-plane Service listens on 8080 (api.service.port in the chart).
SERVICE_PORT=8080

# Health-poll tuning (tests set INTERVAL=0 / a short TIMEOUT to run fast).
HEALTH_INTERVAL="${DEVENV_HEALTH_INTERVAL:-2}"
HEALTH_TIMEOUT="${DEVENV_HEALTH_TIMEOUT:-90}"

# Where the background port-forward PID is recorded, so down.sh can reap it.
PF_PIDFILE="${TMPDIR:-/tmp}/kyber-devenv-portforward.pid"

# --- Logging ---------------------------------------------------------------
log()  { printf '=== devenv: %s ===\n' "$*"; }
warn() { printf 'devenv: %s\n' "$*" >&2; }
die()  { printf 'devenv: error: %s\n' "$1" >&2; exit "${2:-1}"; }

# --- Repo root -------------------------------------------------------------
# Scripts live in scripts/devenv/; the repo root is two levels up. Relative
# paths (images/, deploy/helm/, test/e2e/) resolve against it, exactly like
# setup_test.go's cdToRepoRoot().
repo_root() {
    cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd
}
REPO_ROOT="$(repo_root)"

# --- Dependency preflight --------------------------------------------------
# Exit code 3 == missing dependency (distinct from usage error 2), so callers
# and CI can tell "you didn't install the toolchain" apart from "you passed a
# bad flag". skip_build drops docker from the required set.
DEP_MISSING_CODE=3
preflight_deps() {
    local skip_build="${1:-}"
    local need=(k3d helm kubectl curl)
    [ -n "$skip_build" ] || need=(k3d docker helm kubectl curl)
    local missing=()
    local t
    for t in "${need[@]}"; do
        command -v "$t" >/dev/null 2>&1 || missing+=("$t")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        warn "missing required dependencies: ${missing[*]}"
        warn "this environment needs a container runtime + k8s tooling (k3d, docker, helm, kubectl, curl)."
        warn "running from an agent pod? that path is gated on infra provisioning — see scripts/devenv/README.md (kyber#403)."
        exit "${DEP_MISSING_CODE}"
    fi
}

# --- Cluster existence probe ----------------------------------------------
# `k3d cluster list <name>` exits 0 when the cluster exists, non-zero otherwise.
cluster_exists() {
    k3d cluster list "${CLUSTER_NAME}" >/dev/null 2>&1
}

# --- Read mock-env creds straight from the (unforked) values file ----------
# Sourcing the creds from test/e2e/values-test.yaml keeps the printed contract
# in lockstep with the actual install — no second copy to drift.
values_field() {
    # values_field <yaml-key> — first "<key>: <value>" match, quotes stripped.
    local key="$1" file="${REPO_ROOT}/${VALUES_FILE}"
    [ -f "$file" ] || return 1
    grep -E "^[[:space:]]*${key}:" "$file" | head -1 \
        | sed -E "s/.*${key}:[[:space:]]*\"?([^\"#]+)\"?.*/\1/" \
        | sed -E 's/[[:space:]]+$//'
}

# --- The contract: stable entry points downstream agent skills bind to ----
print_contract() {
    local api_key webhook_secret
    api_key="$(values_field apiKey)"
    webhook_secret="$(values_field webhookSecret)"
    cat <<EOF

=== devenv: ready — Kyber dev/test environment contract ===
  Cluster (k3d):     ${CLUSTER_NAME}
  Namespace:         ${NAMESPACE}
  Compute provider:  mock (MockComputeAdapter — no real GCE/cloud calls)
  API base URL:      http://localhost:${API_PORT}
  Health endpoint:   http://localhost:${API_PORT}/healthz
  API key:           ${api_key:-<unset>}
  Webhook secret:    ${webhook_secret:-<unset>}
  PWA URL:           http://localhost:${API_PORT}/   (real SPA, embedded in the
                     control-plane binary and served at the root path; drive it
                     with the headless-browser harness in scripts/devenv/browser)

  These are throwaway, non-prod fixtures from ${VALUES_FILE}. Never reuse them
  anywhere real. Tear down with: scripts/devenv/down.sh
EOF
}
