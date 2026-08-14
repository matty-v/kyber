#!/bin/bash
# kyber-tmux-paste.sh — shared helper for delivering text into the agent's
# interactive runtime via tmux. Sourced (not executed) by kyber-job-dispatch
# and kyber-compact-session.
#
# This exists so the delivery mechanism lives in exactly ONE place. The
# bracketed-paste detail below was learned the hard way against Codex's TUI;
# a second copy of it would drift the moment one caller is fixed and the
# other is not.
#
# Usage:
#   source "$(dirname "$0")/kyber-tmux-paste.sh"
#   if ! kyber_tmux_paste "$TMUX_SESSION" "$BUFFER_NAME" "$TEXT"; then
#       # $KYBER_TMUX_PASTE_REASON holds a stable reason token
#   fi
#
# On failure the function returns non-zero and sets
# KYBER_TMUX_PASTE_REASON to one of:
#   tmux_load_buffer_failed | tmux_paste_buffer_failed | tmux_send_keys_enter_failed
# Callers map that token to their own reporting channel (a job event, an
# API error body) rather than the helper assuming one.

# KYBER_TMUX_PASTE_REASON is set by kyber_tmux_paste on failure. Declared
# here so `set -u` callers can read it unconditionally.
KYBER_TMUX_PASTE_REASON=""

# kyber_tmux_paste <session> <buffer-name> <text>
#
# Delivers <text> into <session> as a single bracketed paste, then sends
# Enter so the interactive runtime submits it. The text travels through a
# tmux buffer (a data channel) and is never interpolated into shell code.
kyber_tmux_paste() {
    local session="$1"
    local buffer_name="$2"
    local text="$3"

    KYBER_TMUX_PASTE_REASON=""

    # Use tmux's explicit bracketed-paste path instead of a rapid send-keys
    # burst. Codex's TUI can keep a heuristic paste burst open long enough to
    # absorb the following Enter, leaving the entire payload stranded in its
    # editor. paste-buffer -p supplies an unambiguous paste-end marker.
    if ! printf '%s' "$text" | tmux load-buffer -b "$buffer_name" -; then
        KYBER_TMUX_PASTE_REASON="tmux_load_buffer_failed"
        return 1
    fi
    if ! tmux paste-buffer -p -d -b "$buffer_name" -t "$session"; then
        tmux delete-buffer -b "$buffer_name" 2>/dev/null || true
        KYBER_TMUX_PASTE_REASON="tmux_paste_buffer_failed"
        return 1
    fi
    # Let the TUI render the completed bracketed paste before submitting.
    sleep 0.1
    if ! tmux send-keys -t "$session" Enter; then
        KYBER_TMUX_PASTE_REASON="tmux_send_keys_enter_failed"
        return 1
    fi
    return 0
}
