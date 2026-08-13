#!/usr/bin/env bash
set -euo pipefail

API_PORT="${DEVENV_API_PORT:-18080}"
BASE_URL="${DEVENV_PWA_URL:-http://localhost:${API_PORT}}"
API_KEY="${DEVENV_API_KEY:-test-api-key-e2e}"
AUTH_HEADER="Authorization: Bearer ${API_KEY}"

usage() {
  echo "usage: $0 list | apply <machine> <scenario> | attach-node <machine>" >&2
  echo "scenarios: pending running stopped preempted failed fail-next-create fail-next-start fail-next-stop fail-next-delete fail-next-observe" >&2
  exit 2
}

case "${1:-}" in
  list)
    curl -fsS -H "${AUTH_HEADER}" "${BASE_URL}/api/v1/dev/compute/instances" | jq . ;;
  apply)
    [ "$#" -eq 3 ] || usage
    curl -fsS -X POST -H "${AUTH_HEADER}" -H 'Content-Type: application/json' \
      "${BASE_URL}/api/v1/dev/compute/scenarios" \
      -d "$(jq -nc --arg machine "$2" --arg scenario "$3" '{machine:$machine,scenario:$scenario}')" | jq .
    if [ "$3" = "preempted" ]; then
      # Set provider state before removing the Node; the deletion watch can
      # otherwise race ahead and be classified as an unexpected failure.
      kubectl delete nodes -l "kyber.io/machine=$2,kyber.io/simulated=true" --ignore-not-found >/dev/null
    fi ;;
  attach-node)
    [ "$#" -eq 2 ] || usage
    node="$2-simulated"
    kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Node
metadata:
  name: ${node}
  labels:
    kubernetes.io/hostname: ${node}
    kyber.io/machine: $2
    kyber.io/simulated: "true"
spec:
  unschedulable: true
  taints:
    - key: kyber.io/simulated
      value: "true"
      effect: NoSchedule
EOF
    kubectl patch node "${node}" --subresource=status --type=merge -p '{"status":{"capacity":{"cpu":"2","memory":"4Gi","ephemeral-storage":"20Gi","pods":"10"},"allocatable":{"cpu":"2","memory":"4Gi","ephemeral-storage":"20Gi","pods":"10"},"conditions":[{"type":"Ready","status":"True","reason":"ComputeSimulation","message":"Synthetic node for local compute lifecycle testing","lastHeartbeatTime":"2026-01-01T00:00:00Z","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}' >/dev/null
    echo "attached synthetic node ${node} to machine $2" ;;
  *) usage ;;
esac
