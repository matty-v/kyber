#!/usr/bin/env bash
set -euo pipefail

root="$(mktemp -d)"
trap 'rm -rf -- "$root"' EXIT
export MAT21_STATE_DIR="$root/state"
export MAT21_INTERRUPT_CMD="$root/interrupt"
marker="$(dirname "$0")/mat21-task-marker.sh"
interrupt="$(dirname "$0")/mat21-guarded-interrupt.sh"
log="$root/interrupt.log"

cat >"$MAT21_INTERRUPT_CMD" <<SCRIPT
#!/usr/bin/env bash
printf '%s\n' "\$1" >>"$log"
SCRIPT
chmod 0700 "$MAT21_INTERRUPT_CMD"

task=task_11111111111111111111111111111111
attempt=attempt_22222222222222222222222222222222

submit() {
    jq -nc --arg prompt "$1" --arg turn "${2:-turn-1}" \
        '{hook_event_name:"UserPromptSubmit",session_id:"session-1",turn_id:$turn,prompt:$prompt}' | "$marker"
}

stop() {
    jq -nc --arg turn "${1:-turn-1}" \
        '{hook_event_name:"Stop",session_id:"session-1",turn_id:$turn}' | "$marker"
}

submit "[kyber-task:$task] attempt=$attempt"$'\nwork'
"$interrupt" "$task" "$attempt" Escape
"$interrupt" "$task" "$attempt" Escape
[[ "$(wc -l <"$log")" -eq 1 ]] || { echo "duplicate cancel sent twice" >&2; exit 1; }

stop
if "$interrupt" "$task" "$attempt" Escape; then
    echo "cancel after Stop unexpectedly sent" >&2
    exit 1
else
    [[ "$?" -eq 3 ]] || exit 1
fi

submit "ordinary operator turn" turn-2
if "$interrupt" "$task" "$attempt" Escape; then
    echo "non-task foreground was interrupted" >&2
    exit 1
else
    [[ "$?" -eq 3 ]] || exit 1
fi

submit "[kyber-task:$task] attempt=$attempt"$'\nwork' turn-3
jq '.observed_at = 0' "$MAT21_STATE_DIR/foreground.json" >"$root/stale"
mv "$root/stale" "$MAT21_STATE_DIR/foreground.json"
if MAT21_MARKER_MAX_AGE_SECONDS=1 "$interrupt" "$task" "$attempt" Escape; then
    echo "stale marker was interrupted" >&2
    exit 1
else
    [[ "$?" -eq 4 ]] || exit 1
fi

echo "PASS: exact attempt interrupted once; stopped, non-task, and stale targets were not interrupted"
