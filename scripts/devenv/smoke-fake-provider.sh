#!/usr/bin/env bash
# Exercise the managed Machine lifecycle against a running local fake-provider
# stack. Bring it up first with:
#   scripts/devenv/up.sh --compute-provider fake
set -euo pipefail

API_PORT="${DEVENV_API_PORT:-18080}"
BASE_URL="${DEVENV_PWA_URL:-http://localhost:${API_PORT}}"
API_KEY="${DEVENV_API_KEY:-test-api-key-e2e}"
MACHINE="${DEVENV_FAKE_MACHINE:-fake-lifecycle-smoke}"
AUTH_HEADER="Authorization: Bearer ${API_KEY}"

request() {
    curl -fsS -H "${AUTH_HEADER}" -H 'Content-Type: application/json' "$@"
}

phase() {
    request "${BASE_URL}/api/v1/machines/${MACHINE}" | jq -r '.phase'
}

wait_phase() {
    local want="$1" deadline=$((SECONDS + 90)) current=""
    while [ "${SECONDS}" -lt "${deadline}" ]; do
        current="$(phase 2>/dev/null || true)"
        if [ "${current}" = "${want}" ]; then
            printf 'fake-provider smoke: phase=%s\n' "${want}"
            return 0
        fi
        sleep 2
    done
    printf 'fake-provider smoke: wanted phase=%s, got=%s\n' "${want}" "${current}" >&2
    return 1
}

cleanup() {
    curl -fsS -X DELETE -H "${AUTH_HEADER}" \
        "${BASE_URL}/api/v1/machines/${MACHINE}?confirm=${MACHINE}" >/dev/null 2>&1 || true
	for _ in $(seq 1 30); do
		code="$(curl -sS -o /dev/null -w '%{http_code}' -H "${AUTH_HEADER}" \
			"${BASE_URL}/api/v1/machines/${MACHINE}" 2>/dev/null || true)"
		[ "${code}" = "404" ] && return 0
		sleep 1
	done
	return 1
}
trap cleanup EXIT

config_provider="$(request "${BASE_URL}/api/v1/config" | jq -r '.compute.provider')"
[ "${config_provider}" = "fake" ] || {
    echo "fake-provider smoke: stack provider is ${config_provider}, want fake" >&2
    exit 1
}

cleanup
request -X POST "${BASE_URL}/api/v1/machines" -d "$(jq -nc --arg name "${MACHINE}" '{
    name: $name,
    provider: "fake",
    machineType: "e2-small",
    diskSizeGb: 20,
    spot: true,
    zone: "local-a"
}')" >/dev/null
wait_phase Ready

request -X POST "${BASE_URL}/api/v1/machines/${MACHINE}/stop" -d '{}' >/dev/null
wait_phase Stopped

request -X POST "${BASE_URL}/api/v1/machines/${MACHINE}/start" -d '{}' >/dev/null
wait_phase Ready

cleanup
trap - EXIT
printf 'fake-provider smoke: create/stop/start/delete passed\n'
