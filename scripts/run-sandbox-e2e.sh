#!/usr/bin/env bash
set -euo pipefail

# run-sandbox-e2e.sh — run the agent-sandbox e2e suite against a real cluster.
#
# WHY THIS EXISTS (Matt, 2026-08-16): "your e2e tests should be runnable on a
# manual basis against the kyber dev environment."
#
# The sandbox suite (kyber#78/#79) is the only coverage that can catch a class
# of bug that unit tests structurally cannot: the pod spec can be perfectly
# correct and the agent still fails to boot, because what breaks is the
# interaction between the spec and a real node's LSM, kernel and runtime. That
# is exactly what v1.0.6 hit — containerd's default AppArmor profile denied
# mount(2), so the durable root could not be assembled and all 11 agents across
# two clusters fail-closed (kyber#82).
#
# Those tests EXISTED when #79 shipped. They never ran. They guard on
# KYBER_E2E_AGENT_A naming a live agent, nothing provisioned one, so every test
# skipped and the run still looked fine. This script is the missing half: it
# provisions the agents, points the suite at them, and refuses to let a skip
# masquerade as a pass.
#
# Usage:
#   scripts/run-sandbox-e2e.sh                      # against kyber-dev (default)
#   scripts/run-sandbox-e2e.sh --keep               # leave the throwaway agents up
#   KUBECONFIG=... scripts/run-sandbox-e2e.sh       # against any other cluster
#   scripts/run-sandbox-e2e.sh --agents a,b         # reuse existing agents, provision nothing
#
# Exit code is the suite's. A skipped sandbox test is a FAILURE here, by design.

KUBECONFIG_DEFAULT="/persist/kyber-dev/kubeconfig.yaml"
NAMESPACE="${KYBER_E2E_NAMESPACE:-kyber-system}"
MACHINE="${KYBER_E2E_MACHINE:-local}"
KEEP=0
REUSE=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --keep)   KEEP=1; shift ;;
        --agents) REUSE="$2"; shift 2 ;;
        -h|--help) sed -n '3,32p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 2 ;;
    esac
done

export KUBECONFIG="${KUBECONFIG:-$KUBECONFIG_DEFAULT}"
[[ -f "$KUBECONFIG" ]] || {
    echo "no kubeconfig at $KUBECONFIG" >&2
    echo "for kyber-dev, bring it up first: /persist/kyber-dev/kyber-dev.sh up" >&2
    exit 1
}

say() { echo "==> $*"; }

kubectl cluster-info >/dev/null 2>&1 || { echo "cannot reach the cluster via $KUBECONFIG" >&2; exit 1; }
say "cluster: $(kubectl config current-context 2>/dev/null || echo unknown)  ns: $NAMESPACE"

CREATED=()
cleanup() {
    if (( KEEP == 1 )) || (( ${#CREATED[@]} == 0 )); then return; fi
    say "cleaning up throwaway agents: ${CREATED[*]}"
    for a in "${CREATED[@]}"; do
        kubectl delete agent "$a" -n "$NAMESPACE" --wait=false >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

if [[ -n "$REUSE" ]]; then
    AGENT_A="${REUSE%%,*}"
    AGENT_B="${REUSE##*,}"
    say "reusing existing agents: $AGENT_A, $AGENT_B"
else
    # Two agents: the cross-agent isolation criteria (kyber#78 AC4) need a
    # second one to prove an agent cannot reach its neighbour.
    AGENT_A="e2e-sandbox-a"
    AGENT_B="e2e-sandbox-b"
    kubectl get machine "$MACHINE" -n "$NAMESPACE" >/dev/null 2>&1 || {
        echo "machine '$MACHINE' not found in $NAMESPACE — set KYBER_E2E_MACHINE" >&2
        exit 1
    }
    for a in "$AGENT_A" "$AGENT_B"; do
        if kubectl get agent "$a" -n "$NAMESPACE" >/dev/null 2>&1; then
            say "agent $a already exists — reusing"
            continue
        fi
        say "creating agent $a"
        kubectl apply -f - >/dev/null <<YAML
apiVersion: kyber.io/v1
kind: Agent
metadata:
  name: ${a}
  namespace: ${NAMESPACE}
spec:
  machine: ${MACHINE}
  runtime: claude-code
  resources:
    cpu: "1"
    memory: 2Gi
    disk: 10Gi
  # Required by the CRD. authType: oauth means "no key material handed to this
  # agent" — these are throwaway pods we only ever inspect, never drive, so
  # they must not carry a credential.
  secrets:
    authType: oauth
YAML
        CREATED+=("$a")
    done
fi

# The suite execs into agent-<name>; the pod has to actually be up. Waiting on
# the POD rather than the CR phase is deliberate — an agent can report a phase
# while its runtime container is dead, which is the failure mode this whole
# suite exists to detect.
for a in "$AGENT_A" "$AGENT_B"; do
    pod="agent-${a}"
    say "waiting for pod $pod"
    for i in $(seq 1 60); do
        ready=$(kubectl get pod "$pod" -n "$NAMESPACE" \
                  -o jsonpath='{.status.containerStatuses[?(@.name=="agent")].ready}' 2>/dev/null || true)
        [[ "$ready" == "true" ]] && break
        if (( i == 60 )); then
            echo "  pod $pod never became ready — last state:" >&2
            kubectl get pod "$pod" -n "$NAMESPACE" >&2 2>/dev/null || true
            kubectl logs "$pod" -n "$NAMESPACE" -c agent --tail=20 >&2 2>/dev/null || true
            echo >&2
            echo "  NOTE: if this says 'could not prepare the durable root', that IS the bug" >&2
            echo "  this suite exists to catch — see kyber#82." >&2
            exit 1
        fi
        sleep 5
    done
done

say "running the sandbox suite"
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# KYBER_E2E_SANDBOX_REQUIRED turns the suite's built-in skip into a hard
# failure. Without it, a misconfigured run reports success having tested
# nothing — which is precisely how #79 reached production.
# -skip-setup / -port-forward=false: reuse the cluster we were pointed at.
# Without them TestMain builds its OWN k3d cluster and the whole point of
# "run against the dev environment" is lost.
#
# KYBER_E2E_BASE_URL is what TestMain's readiness gate polls. The sandbox tests
# never call the API — they exec into pods — but the gate runs regardless, so
# it has to point somewhere real or the suite dies before running a single test.
BASE_URL="${KYBER_E2E_BASE_URL:-https://kyber-dev.voget.io}"
say "api readiness gate will poll: $BASE_URL"

KYBER_E2E_NAMESPACE="$NAMESPACE" \
KYBER_E2E_AGENT_A="$AGENT_A" \
KYBER_E2E_AGENT_B="$AGENT_B" \
KYBER_E2E_BASE_URL="$BASE_URL" \
KYBER_E2E_SANDBOX_REQUIRED=true \
    go test -tags e2e -v -timeout 15m -run 'TestSandbox' ./test/e2e/ \
      -skip-setup -skip-cleanup -port-forward=false
