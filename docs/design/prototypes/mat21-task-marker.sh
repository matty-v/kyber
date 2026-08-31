#!/usr/bin/env bash
set -euo pipefail

# Disposable MAT-21 hook prototype. UserPromptSubmit records the exact
# foreground Kyber task attempt; Stop clears it under the same lock used by the
# interrupt helper. It never stores prompt content.
state_dir="${MAT21_STATE_DIR:-/persist/mat21-cancel}"
lock_file="$state_dir/foreground.lock"
marker_file="$state_dir/foreground.json"
payload="$(cat)"
event="$(jq -r '.hook_event_name // empty' <<<"$payload")"
session_id="$(jq -r '.session_id // empty' <<<"$payload")"
turn_id="$(jq -r '.turn_id // empty' <<<"$payload")"

install -d -m 0700 "$state_dir"
exec 9>"$lock_file"
flock -x 9

case "$event" in
UserPromptSubmit)
    prompt="$(jq -r '.prompt // empty' <<<"$payload")"
    header="${prompt%%$'\n'*}"
    if [[ "$header" =~ ^\[kyber-task:(task_[a-f0-9]{32})\]\ attempt=(attempt_[a-f0-9]{32})$ ]]; then
        task_id="${BASH_REMATCH[1]}"
        attempt_id="${BASH_REMATCH[2]}"
        tmp="$(mktemp "$state_dir/.foreground.XXXXXX")"
        jq -nc \
            --arg task_id "$task_id" \
            --arg attempt_id "$attempt_id" \
            --arg session_id "$session_id" \
            --arg turn_id "$turn_id" \
            --argjson observed_at "$(date +%s)" \
            '{task_id:$task_id,attempt_id:$attempt_id,session_id:$session_id,
              turn_id:$turn_id,observed_at:$observed_at,interrupt_sent:false}' >"$tmp"
        chmod 0600 "$tmp"
        mv -f -- "$tmp" "$marker_file"
    else
        # A non-task turn must never inherit an old task's cancellation target.
        rm -f -- "$marker_file"
    fi
    ;;
Stop)
    if [[ -f "$marker_file" ]]; then
        marker_session="$(jq -r '.session_id // empty' "$marker_file")"
        marker_turn="$(jq -r '.turn_id // empty' "$marker_file")"
        if [[ "$marker_session" == "$session_id" &&
              ( -z "$marker_turn" || -z "$turn_id" || "$marker_turn" == "$turn_id" ) ]]; then
            rm -f -- "$marker_file"
        fi
    fi
    ;;
*)
    exit 0
    ;;
esac

