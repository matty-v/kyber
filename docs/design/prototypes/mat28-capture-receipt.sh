#!/usr/bin/env bash
set -euo pipefail

# Disposable MAT-28 evidence hook. It records only task/attempt and native
# receipt identifiers; it never persists the submitted prompt or transcript.
payload="$(cat)"
prompt="$(jq -r '.prompt // empty' <<<"$payload")"
header="${prompt%%$'\n'*}"

if [[ ! "$header" =~ ^\[kyber-task:(task_[a-f0-9]{32})\]\ attempt=(attempt_[a-f0-9]{32})$ ]]; then
  exit 0
fi

task_id="${BASH_REMATCH[1]}"
attempt_id="${BASH_REMATCH[2]}"
session_id="$(jq -r '.session_id // empty' <<<"$payload")"
turn_id="$(jq -r '.turn_id // empty' <<<"$payload")"
event="$(jq -r '.hook_event_name // empty' <<<"$payload")"

[[ "$session_id" =~ ^[A-Za-z0-9._:-]{1,256}$ ]] || exit 2
[[ -z "$turn_id" || "$turn_id" =~ ^[A-Za-z0-9._:-]{1,256}$ ]] || exit 2
[[ "$event" == "UserPromptSubmit" ]] || exit 2

receipt_dir=/persist/mat28-receipts
install -d -m 0700 "$receipt_dir"
tmp="$(mktemp "$receipt_dir/.${attempt_id}.XXXXXX")"
jq -nc \
  --arg task_id "$task_id" \
  --arg attempt_id "$attempt_id" \
  --arg session_id "$session_id" \
  --arg turn_id "$turn_id" \
  --arg event "$event" \
  '{task_id:$task_id,attempt_id:$attempt_id,session_id:$session_id,turn_id:$turn_id,event:$event}' >"$tmp"
chmod 0600 "$tmp"
mv -f -- "$tmp" "$receipt_dir/${attempt_id}.json"

