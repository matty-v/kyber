#!/usr/bin/env bash
set -euo pipefail

# Disposable MAT-21 cancellation adapter prototype.
# Exit 0: the exact foreground attempt was interrupted, or an identical cancel
#         already sent the interrupt.
# Exit 3: the requested attempt is not the foreground task; no key was sent.
# Exit 4: the marker is stale; no key was sent.
# Other:  interrupt transport failed; marker remains retryable.
task_id="${1:?task id required}"
attempt_id="${2:?attempt id required}"
interrupt_key="${3:-Escape}"
state_dir="${MAT21_STATE_DIR:-/persist/mat21-cancel}"
lock_file="$state_dir/foreground.lock"
marker_file="$state_dir/foreground.json"
max_age="${MAT21_MARKER_MAX_AGE_SECONDS:-86400}"

[[ "$task_id" =~ ^task_[a-f0-9]{32}$ ]] || exit 2
[[ "$attempt_id" =~ ^attempt_[a-f0-9]{32}$ ]] || exit 2
[[ "$interrupt_key" == "Escape" || "$interrupt_key" == "C-c" ]] || exit 2

install -d -m 0700 "$state_dir"
exec 9>"$lock_file"
flock -x 9

[[ -f "$marker_file" ]] || exit 3
marker_task="$(jq -r '.task_id // empty' "$marker_file")"
marker_attempt="$(jq -r '.attempt_id // empty' "$marker_file")"
[[ "$marker_task" == "$task_id" && "$marker_attempt" == "$attempt_id" ]] || exit 3

observed_at="$(jq -r '.observed_at // 0' "$marker_file")"
(( $(date +%s) - observed_at <= max_age )) || exit 4

if [[ "$(jq -r '.interrupt_sent // false' "$marker_file")" == "true" ]]; then
    exit 0
fi

if [[ -n "${MAT21_INTERRUPT_CMD:-}" ]]; then
    # Test seam. Production adapters call a native turn interrupt where
    # available or tmux send-keys inside the runtime namespace.
    "$MAT21_INTERRUPT_CMD" "$interrupt_key"
else
    tmux send-keys -t "${MAT21_TMUX_SESSION:-agent}" "$interrupt_key"
fi

tmp="$(mktemp "$state_dir/.foreground.XXXXXX")"
jq '.interrupt_sent = true | .interrupt_sent_at = now | .interrupt_key = $key' \
    --arg key "$interrupt_key" "$marker_file" >"$tmp"
chmod 0600 "$tmp"
mv -f -- "$tmp" "$marker_file"

