#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME:-/home/kyber}"
export CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
PERSIST_ROOT="${KYBER_PERSIST_ROOT:-/persist}"
mkdir -p "$CODEX_HOME" "$PERSIST_ROOT/var/log" "$PERSIST_ROOT/var/lock"
chmod 0700 "$CODEX_HOME"

# Optional per-agent harness pin. The API validates the same conservative
# character set; repeat it here because persisted CRs may predate validation.
# Installation failure is non-fatal: keep the baked-in CLI, report its actual
# version below, and let RuntimeVersionMismatch tell the operator what happened.
KYBER_CODEX_REQUESTED_SATISFIED=""
if [ -n "${KYBER_REQUESTED_CODEX_VERSION:-}" ]; then
    if [ "${#KYBER_REQUESTED_CODEX_VERSION}" -le 64 ] && [[ "$KYBER_REQUESTED_CODEX_VERSION" =~ ^[0-9A-Za-z.-]+$ ]]; then
        CURRENT_CODEX_VERSION="$(codex --version 2>/dev/null | awk '{print $NF; exit}' || true)"
        _npm="$(command -v npm || echo /usr/bin/npm)"
        _resolved="$KYBER_REQUESTED_CODEX_VERSION"
        if [ "$KYBER_REQUESTED_CODEX_VERSION" = "latest" ]; then
            _resolved="$("$_npm" view @openai/codex@latest version 2>/dev/null | tail -1 || true)"
        fi
        if ! [[ "$_resolved" =~ ^[0-9]+\.[0-9]+\.[0-9]+([0-9A-Za-z.+-]*)?$ ]]; then
            echo "[kyber] WARNING: could not resolve requested Codex harness version ${KYBER_REQUESTED_CODEX_VERSION}; keeping current version"
            KYBER_CODEX_REQUESTED_SATISFIED="false"
        elif [ "$CURRENT_CODEX_VERSION" = "$_resolved" ]; then
            echo "[kyber] requested Codex harness already installed (${_resolved}); skipping install"
            KYBER_CODEX_REQUESTED_SATISFIED="true"
        else
            echo "[kyber] installing requested Codex harness version ${KYBER_REQUESTED_CODEX_VERSION} (resolved=${_resolved})"
            _installer="/usr/local/bin/kyber-harness-install"
            if [ "${SKIP_CODEX_LAUNCH:-}" = "1" ] && [ -n "${KYBER_HARNESS_INSTALLER:-}" ]; then
                _installer="${KYBER_HARNESS_INSTALLER}"
            fi
            if [ ! -x "$_installer" ] || ! sudo "$_installer" @openai/codex "$_resolved" codex 2>&1; then
                echo "[kyber] WARNING: requested Codex harness install failed; previous install preserved"
                KYBER_CODEX_REQUESTED_SATISFIED="false"
            else
                KYBER_CODEX_REQUESTED_SATISFIED="true"
            fi
        fi
        unset _npm _resolved _installer
    else
        echo "[kyber] WARNING: requested Codex harness version is invalid; using baked-in version"
        KYBER_CODEX_REQUESTED_SATISFIED="false"
    fi
fi

KYBER_MANAGED_CODEX_CONFIG="${KYBER_MANAGED_CODEX_CONFIG:-/etc/codex/managed_config.toml}"

# Set $1 to mode $2, but only when it is not already that mode. A chmod on a
# path this user does not own fails even when the mode is already correct, so
# checking first stops an already-fine directory from failing the whole write.
kyber_ensure_mode() {
    _path="$1"
    _want="$2"
    _have="$(stat -c '%a' "$_path" 2>/dev/null || echo '')"
    [ "$_have" = "${_want#0}" ] && return 0
    chmod "$_want" "$_path" 2>/dev/null || sudo chmod "$_want" "$_path" 2>/dev/null || return 1
}

# True when this shell can write $1 directly — the file exists and is writable,
# or it does not exist and its directory is. Never attempts the write itself.
kyber_managed_config_writable() {
    if [ -e "$1" ]; then
        [ -w "$1" ]
    else
        [ -w "$(dirname "$1")" ]
    fi
}

# Write via sudo only when a direct write is not possible: the tests run this
# script unprivileged against a temp path, and the image grants kyber
# passwordless sudo for exactly this kind of boot-time maintenance.
#
# Both the directory and the file must be readable BY THE AGENT USER, not just
# by root. `sudo mkdir` and `sudo tee` create 0700/0600 under root's umask, and
# codex then cannot traverse /etc/codex: its probe for requirements.toml comes
# back EACCES instead of ENOENT, and it refuses to load ANY configuration. Every
# codex command fails, which reads as "credentials missing or invalid" and
# crash-loops the agent. Found on kyber-canary 2026-08-28, reproduced as:
#
#   /etc/codex mode 700 -> "failed to load bootstrap configuration: Failed to
#                           read requirements file ...: Permission denied"
#   /etc/codex mode 755 -> codex works normally
#
# The mode is therefore part of the contract, not a detail: creating this
# directory at all changes how codex boots, so creating it wrong is worse than
# not creating it.
#
# Writability is probed with `[ -w ]` rather than `: >> "$file"`. A failed
# REDIRECTION is reported by the shell itself, before the command's own
# `2>/dev/null` applies, so the probe leaked "Permission denied" into every
# boot log even when the sudo fallback then succeeded. start-codex.sh already
# documents that trap in the token-reporter block below; this is the same one.
kyber_write_managed_config() {
    _target="$1"
    _dir="$(dirname "$_target")"
    if [ ! -d "$_dir" ]; then
        mkdir -p "$_dir" 2>/dev/null || sudo mkdir -p "$_dir" 2>/dev/null || return 1
    fi
    kyber_ensure_mode "$_dir" 0755 || return 1
    if kyber_managed_config_writable "$_target"; then
        cat > "$_target"
    else
        sudo tee "$_target" >/dev/null || return 1
    fi
    kyber_ensure_mode "$_target" 0644 || return 1
}

# Append to the managed config, keeping it readable by the agent user and
# keeping failed redirections out of the boot log.
kyber_append_managed_config() {
    _target="$1"
    if kyber_managed_config_writable "$_target"; then
        cat >> "$_target"
    else
        sudo tee -a "$_target" >/dev/null || return 1
    fi
    kyber_ensure_mode "$_target" 0644 || return 1
}

# Repair an already-poisoned managed config BEFORE anything asks codex to run.
#
# The full managed-settings write lives further down, after the credential and
# device-auth blocks. That ordering is fine on a healthy agent and useless on a
# broken one: an agent whose /etc/codex was created 0700 by the version of this
# script shipped in #160 cannot run ANY codex command, so `codex login status`
# fails, the device-auth branch runs, it cannot complete either, and the script
# exits 42 at that point — 170 lines before the chmod that would have fixed it.
# Every subsequent boot repeats it. #165 therefore fixed new agents and could
# not reach the ones already broken.
#
# /etc lives on /persist under rootfs persistence, so a directory created wrong
# once survives every image update. Repairing it here, before the first codex
# invocation, is what makes the fix self-healing rather than forward-only.
# Silent and best-effort by design: on a healthy agent there is nothing to do.
if [ -d "$(dirname "$KYBER_MANAGED_CODEX_CONFIG")" ]; then
    kyber_ensure_mode "$(dirname "$KYBER_MANAGED_CODEX_CONFIG")" 0755 || \
        echo "[kyber] WARNING: could not make $(dirname "$KYBER_MANAGED_CODEX_CONFIG") readable; codex may fail to load its configuration" >&2
    if [ -f "$KYBER_MANAGED_CODEX_CONFIG" ]; then
        kyber_ensure_mode "$KYBER_MANAGED_CODEX_CONFIG" 0644 || \
            echo "[kyber] WARNING: could not make $KYBER_MANAGED_CODEX_CONFIG readable" >&2
    fi
fi

# The API transports the opaque Codex credential document through a Kubernetes
# Secret. Never print it.
#
# SEED-IF-CHANGED, NOT UNCONDITIONALLY (kyber#681). This block used to write the
# Secret's copy over auth.json on every boot, with a comment asserting that the
# CLI-refreshed copy "survives pod replacement through Kyber's whole-disk
# persistence". It did not: this very write was what destroyed it.
#
# That is fatal because ChatGPT refresh tokens are SINGLE USE. Codex refreshes,
# OpenAI burns the old refresh token and returns a new one, the CLI saves it to
# auth.json — and the next boot restored the burnt one from the Secret. From
# then on every refresh fails with "your refresh token was already used" and the
# agent can never reach the model again. HK-47 died exactly this way on
# 2026-08-04, ten days after his credential was captured.
#
# We cannot simply skip the write when auth.json exists: re-authorising from the
# PWA updates the Secret, and that MUST still take effect. So we track which
# Secret payload we last seeded, in a marker beside auth.json on the same
# persistent overlay:
#
#   Secret differs from the marker -> the operator supplied a new credential,
#                                     seed it and record it.
#   Secret matches the marker      -> we already seeded this one; whatever is on
#                                     disk is that credential or a newer refresh
#                                     of it. Leave it alone.
#
# Comparing payloads rather than parsing last_refresh keeps this robust against
# credential documents whose shape we do not control. The marker holds only a
# hash, never the credential.
_device_auth_pending=false
if [ -n "${CODEX_AUTH_JSON:-}" ]; then
    # `codex login status` currently treats an empty JSON object as ChatGPT
    # auth, so the CLI status alone cannot distinguish Kyber's device-login
    # marker from a usable credential. Preserve that distinction before the
    # Secret payload is unset below.
    if [ "$CODEX_AUTH_JSON" = "{}" ]; then
        _device_auth_pending=true
        # The reporter normally treats the credential present at its own
        # startup as already represented by the Secret. Here the Secret held
        # only Kyber's marker, and device login will replace it before the
        # reporter starts, so force exactly one initial write-back.
        export KYBER_CODEX_PUSH_INITIAL=1
    fi
    umask 077
    _seed_marker="$CODEX_HOME/.kyber-seeded-auth"
    _secret_hash="$(printf '%s' "$CODEX_AUTH_JSON" | sha256sum | cut -d' ' -f1)"
    _seeded_hash=""
    # Written as an if, not `[ -r … ] && x=…`: under `set -e` the exit status of
    # a short-circuited && list is the test's failure, and relying on bash's
    # exemption rules here would make a missing marker — i.e. EVERY first boot —
    # depend on a subtlety. Be explicit instead.
    if [ -r "$_seed_marker" ]; then
        _seeded_hash="$(cat "$_seed_marker" 2>/dev/null || true)"
    fi

    if [ ! -s "$CODEX_HOME/auth.json" ]; then
        printf '%s' "$CODEX_AUTH_JSON" > "$CODEX_HOME/auth.json"
        printf '%s' "$_secret_hash" > "$_seed_marker"
        echo "[kyber] seeded Codex credentials (no local copy)"
    elif [ -z "$_seeded_hash" ]; then
        # ADOPTION PATH — first boot on a version that has this fix, for an agent
        # that predates it. There is a local credential but no marker yet.
        #
        # Do NOT seed here. The local copy was originally installed from this
        # same Secret and may have been refreshed since, which makes it newer
        # than what the Secret holds. Seeding would perform exactly the clobber
        # this change exists to stop, and would burn a healthy agent's working
        # credential once on upgrade. Adopt the current Secret hash as the
        # baseline instead and leave the live credential alone.
        printf '%s' "$_secret_hash" > "$_seed_marker"
        echo "[kyber] adopted existing Codex credentials (first boot with seed tracking)"
    elif [ "$_secret_hash" != "$_seeded_hash" ]; then
        printf '%s' "$CODEX_AUTH_JSON" > "$CODEX_HOME/auth.json"
        printf '%s' "$_secret_hash" > "$_seed_marker"
        echo "[kyber] seeded Codex credentials (secret changed since last boot)"
    else
        echo "[kyber] keeping locally refreshed Codex credentials (secret unchanged)"
    fi
    chmod 0600 "$CODEX_HOME/auth.json" "$_seed_marker" 2>/dev/null || true
    unset CODEX_AUTH_JSON _secret_hash _seeded_hash _seed_marker
fi

if [ -z "${OPENAI_API_KEY:-}" ]; then
    if [ "$_device_auth_pending" = true ] || ! codex login status >/dev/null 2>&1; then
        echo "[kyber] Codex ChatGPT credentials are missing or invalid — starting device authorization"
        tmux kill-session -t auth 2>/dev/null || true
        tmux new-session -d -s auth 'codex login --device-auth'
        echo "[kyber] device authorization is waiting in tmux session 'auth'"
        while tmux has-session -t auth 2>/dev/null; do sleep 2; done
        # Codex 0.146 reports `login status` as successful for an auth.json
        # containing only `{}`. That is Kyber's pending marker, not a usable
        # credential, so require the device flow to replace it as well.
        _auth_after_login="$(tr -d '[:space:]' < "$CODEX_HOME/auth.json" 2>/dev/null || true)"
        if [ "$_auth_after_login" = "{}" ] || ! codex login status >/dev/null 2>&1; then
            echo "[kyber] Codex device authorization did not complete"
            exit 42
        fi
        unset _auth_after_login
        echo "[kyber] Codex device authorization completed"
    fi
fi
unset _device_auth_pending

# Kyber's own Codex settings live in the SYSTEM managed config, NOT in the
# agent's ~/.codex/config.toml.
#
# Until kyber#MCPFIX this block rewrote the agent's config.toml from scratch on
# every boot. That truncation silently deleted any MCP server the agent had
# registered itself with `codex mcp add` — the entry was gone from the very
# next session, and re-adding it only survived until the next restart, so a
# durable integration could never be established. Observed on `atlas` with an
# Atlassian MCP, repeatedly, across restarts.
#
# /etc/codex/managed_config.toml is read by Codex underneath the user config,
# is owned by Kyber rather than the agent, and can be rewritten freely because
# nothing else writes it. Settings placed here also apply to a bare `codex`
# the agent runs itself, which command-line flags would not cover.
#
# Claude Code has always done the equivalent — see start-claude.sh's
# "Merge instead of replacing" jq path for ~/.claude.json.

# ---- Scheduled-job turn-boundary hooks (FAL-8) ----
# Codex gets the SAME two platform signals Claude Code registers in
# images/claude-code/start-claude.sh:38-98, wired to the same runtime-neutral
# scripts rather than to a Codex-specific path:
#
#   UserPromptSubmit -> kyber-cron-turn-start  (arm the job's pending marker)
#   Stop             -> kyber-cron-postrun     (clear context, release the marker)
#
# Neither approach the issue proposed was needed. Codex 0.146.0 ships real
# `UserPromptSubmit` and `Stop` hooks whose payloads already match what those
# two scripts read — UserPromptSubmit carries `prompt`, which is exactly the
# key kyber-cron-turn-start hashes. Verified against the pinned binary; the
# spike record is in images/codex/INSTALL_NOTES.md.
#
# These ride in the managed layer THIS BLOCK'S NEIGHBOUR already owns, and for
# a second reason on top of that one: hooks declared in the agent's own
# config.toml are gated behind an interactive "Hooks need review" prompt and do
# not run until a human answers it, which on a headless pod is worse than an
# inert feature. Managed hooks are policy and load with no prompt.
#
# Each hook is rendered independently, on its own command's availability. The
# both-or-neither rule is then enforced by checking the WRITTEN file below, so
# a runtime that can only register half never claims the capability.
KYBER_POSTRUN_CMD="${KYBER_CRON_POSTRUN_CMD:-/usr/local/bin/kyber-cron-postrun}"
KYBER_TURNSTART_CMD="${KYBER_CRON_TURNSTART_CMD:-/usr/local/bin/kyber-cron-turn-start}"
KYBER_POSTRUN_SENTINEL="${KYBER_CRON_POSTRUN_SENTINEL:-/persist/var/run/kyber-cron-postrun-enabled}"
# MUST be a fresh-conversation command, NOT a compaction one. `/compact`
# summarizes the thread and carries the summary forward — which is exactly what
# CompactSessionCommand in pkg/runtimes/codex/adapter.go is for, and exactly
# what ClearContextAfter promises NOT to do ("begins from a clean conversation
# instead of accumulating every previous run"). Measured against 0.146.0 by
# planting a token in one turn and reading what the next turn actually sent
# upstream: after `/compact` the token was still there, after `/clear` it was
# gone. Codex's own TUI says the same thing after a compaction — "Long threads
# and multiple compactions can cause the model to be less accurate. Start a new
# thread when possible." An unattended job firing hourly is the worst case for
# that, so compaction would have quietly re-introduced the cross-contamination
# this flag exists to stop. See images/codex/INSTALL_NOTES.md.
#
# `/clear` is also the literal Claude Code uses (kyber-cron-postrun's own
# default), so the two runtimes are now genuinely equivalent rather than
# merely both non-inert. Kept explicit anyway: the runtime declaring its own
# clear command is the contract kyber-cron-postrun documents, and being
# explicit is what lets a test assert the VALUE.
#
# It rides inside the hook command rather than depending on this script's
# environment reaching the hook process, which is spawned by codex, which is
# spawned by a tmux server that may have outlived an earlier boot. Exported
# too, for any other consumer.
KYBER_CODEX_CLEAR_TEXT="${KYBER_CLEAR_SESSION_TEXT:-/clear}"
export KYBER_CLEAR_SESSION_TEXT="$KYBER_CODEX_CLEAR_TEXT"

KYBER_CODEX_HOOKS_TOML=""
if [ -x "$KYBER_TURNSTART_CMD" ]; then
    KYBER_CODEX_HOOKS_TOML="${KYBER_CODEX_HOOKS_TOML}
[[hooks.UserPromptSubmit]]
[[hooks.UserPromptSubmit.hooks]]
type = \"command\"
command = \"${KYBER_TURNSTART_CMD}\"
timeout = 20
"
fi
if [ -x "$KYBER_POSTRUN_CMD" ]; then
    KYBER_CODEX_HOOKS_TOML="${KYBER_CODEX_HOOKS_TOML}
[[hooks.Stop]]
[[hooks.Stop.hooks]]
type = \"command\"
command = \"env KYBER_CLEAR_SESSION_TEXT=${KYBER_CODEX_CLEAR_TEXT} ${KYBER_POSTRUN_CMD}\"
timeout = 20
"
fi

if kyber_write_managed_config "$KYBER_MANAGED_CODEX_CONFIG" <<EOF
# Managed by Kyber. Rewritten on every agent boot — do not edit.
# Agent-owned Codex settings belong in ~/.codex/config.toml, which Kyber
# never rewrites.
approval_policy = "never"
sandbox_mode = "danger-full-access"
check_for_update_on_startup = false
tui.resume_cwd = "current"
EOF
then
    if [ -n "${CODEX_MODEL:-}" ]; then
        printf 'model = "%s"\n' "$CODEX_MODEL" | \
            { kyber_append_managed_config "$KYBER_MANAGED_CODEX_CONFIG"; } || \
            echo "[kyber] WARNING: could not record the model in $KYBER_MANAGED_CODEX_CONFIG" >&2
    fi
    # The hook TABLES go last, after every top-level key above: a bare
    # `model = ...` appended after a [[table]] header would be parsed as a
    # member of that table, not as a top-level setting.
    if [ -n "$KYBER_CODEX_HOOKS_TOML" ]; then
        printf '%s' "$KYBER_CODEX_HOOKS_TOML" | \
            { kyber_append_managed_config "$KYBER_MANAGED_CODEX_CONFIG"; } || \
            echo "[kyber] WARNING: could not register the cron hooks in $KYBER_MANAGED_CODEX_CONFIG" >&2
    fi
    echo "[kyber] Codex managed settings written to $KYBER_MANAGED_CODEX_CONFIG"
else
    # Non-fatal: the launch command below still passes --ask-for-approval and
    # --sandbox explicitly, so a Kyber-started session is unaffected. Only a
    # bare `codex` the agent runs itself would fall back to defaults.
    echo "[kyber] WARNING: could not write $KYBER_MANAGED_CODEX_CONFIG; Codex settings apply to Kyber-launched sessions only" >&2
fi

# Both or neither, for the reason start-claude.sh:84-97 gives: arming without
# consuming leaks markers and mutes --exclusive; consuming without arming
# clears context on an unrelated turn. The sentinel is the contract with
# kyber-job-dispatch (pending_tracking_enabled), so it is written ONLY when
# both hooks are really in the file Codex will load, and actively REMOVED when
# they are not — a sentinel left over from a previous boot would otherwise keep
# claiming a capability this boot does not have.
if grep -q '^\[\[hooks\.UserPromptSubmit\]\]' "$KYBER_MANAGED_CODEX_CONFIG" 2>/dev/null \
    && grep -q '^\[\[hooks\.Stop\]\]' "$KYBER_MANAGED_CODEX_CONFIG" 2>/dev/null \
    && grep -qF "$KYBER_TURNSTART_CMD" "$KYBER_MANAGED_CODEX_CONFIG" 2>/dev/null \
    && grep -qF "$KYBER_POSTRUN_CMD" "$KYBER_MANAGED_CODEX_CONFIG" 2>/dev/null; then
    mkdir -p "$(dirname "$KYBER_POSTRUN_SENTINEL")" 2>/dev/null || true
    : > "$KYBER_POSTRUN_SENTINEL" 2>/dev/null || true
    echo "[kyber] cron context hooks registered"
else
    rm -f "$KYBER_POSTRUN_SENTINEL" 2>/dev/null || true
    echo "[kyber] WARNING: cron context hooks incomplete; feature disabled" >&2
fi

# The agent's own config is created if absent and otherwise left alone.
if [ ! -f "$CODEX_HOME/config.toml" ]; then
    : > "$CODEX_HOME/config.toml"
fi
chmod 0600 "$CODEX_HOME/config.toml"

# A config.toml that does not parse makes every `codex mcp` call below fail, so
# it has to be recovered — but a failing probe is NOT on its own evidence that
# THIS file is the problem. `codex mcp list` loads the MERGED configuration
# (the managed file written above, this file, CLI state) and fails just as
# readily on a malformed managed setting, an I/O or permission error, or a
# broken codex binary. Resetting the agent's file on any of those would destroy
# every custom MCP server and unrelated setting it holds — precisely the
# data loss this change exists to prevent.
#
# So attribute the failure before touching anything: re-probe against a
# throwaway CODEX_HOME whose config.toml is empty. If the empty one parses,
# the difference is this file and it is genuinely bad. If the empty one fails
# too, the fault lies elsewhere: leave the agent's file exactly as it is, and
# skip convergence entirely, because `codex mcp` cannot be trusted to edit a
# file it cannot read.
kyber_probe_codex_config() {
    CODEX_HOME="$1" codex mcp list >/dev/null 2>&1
}

KYBER_CODEX_CONFIG_USABLE=true
if ! kyber_probe_codex_config "$CODEX_HOME"; then
    _probe_home="$(mktemp -d)"
    : > "$_probe_home/config.toml"
    chmod 0600 "$_probe_home/config.toml"
    if kyber_probe_codex_config "$_probe_home"; then
        cp "$CODEX_HOME/config.toml" "$CODEX_HOME/config.toml.corrupt" 2>/dev/null || true
        : > "$CODEX_HOME/config.toml"
        chmod 0600 "$CODEX_HOME/config.toml"
        echo "[kyber] WARNING: recovered unparseable $CODEX_HOME/config.toml (saved as config.toml.corrupt)" >&2
    else
        KYBER_CODEX_CONFIG_USABLE=false
        echo "[kyber] WARNING: codex cannot read its configuration even with an empty user config; leaving $CODEX_HOME/config.toml untouched and skipping MCP convergence" >&2
    fi
    rm -rf "$_probe_home"
fi

# Converge Kyber-managed MCP entries through `codex mcp`, which edits
# config.toml as TOML and leaves every other entry byte-identical. `add` is an
# upsert (re-running it with a new URL updates in place) and `remove` exits 0
# when the entry is already absent, so both directions are idempotent — a
# channel that gets disabled does not leave a stale managed entry behind.
kyber_converge_mcp() {
    _name="$1"
    _url="$2"
    _label="$3"
    if [ -n "$_url" ]; then
        if codex mcp add "$_name" --url "$_url" >/dev/null 2>&1; then
            echo "[kyber] $_label MCP sidecar registered at $_url"
        else
            echo "[kyber] WARNING: could not register $_label MCP server '$_name'" >&2
        fi
    else
        codex mcp remove "$_name" >/dev/null 2>&1 || true
    fi
}

# Telegram MCP sidecar (kyber#684). Same server the Claude Code runtime
# registers, so both runtimes get the same tool surface instead of Codex
# curl-ing a bare HTTP endpoint. The /send endpoint stays available for the
# inbound-binding action text, so this is additive — nothing breaks if an
# older binding is still telling the agent to curl.
if [ "$KYBER_CODEX_CONFIG_USABLE" = "true" ]; then
    kyber_converge_mcp kyber_telegram "${KYBER_TELEGRAM_MCP_URL:-}" "Telegram"
    kyber_converge_mcp kyber_discord "${KYBER_DISCORD_MCP_URL:-}" "Discord"
    kyber_converge_mcp kyber_request_reply "${KYBER_REQUEST_MCP_URL:-}" "Request/reply"
fi


# Report the version actually running through the localhost status sidecar.
# Keep this best-effort: telemetry must never prevent the runtime from booting.
CODEX_VERSION="$(codex --version 2>/dev/null | awk '{print $NF; exit}' || true)"
if [ -z "$CODEX_VERSION" ]; then CODEX_VERSION="unknown"; fi
# Guard the write on the mkdir SUCCEEDING. `> file 2>/dev/null || true`
# suppresses the command's stderr, but a failed redirection is the SHELL's
# error, emitted before the command runs and before `|| true` applies — so
# when /var/run/kyber could not be created every boot printed
#   start-codex.sh: line NN: /var/run/kyber/runtime-version: No such file or directory
# despite the line being deliberately best-effort. Harmless but misleading:
# the next line reports the version successfully, so the log shows a
# scary-looking error immediately followed by success. Observed on HK-47's
# first boot (kyber#674).
if mkdir -p /var/run/kyber 2>/dev/null; then
    printf '%s\n' "$CODEX_VERSION" > /var/run/kyber/runtime-version 2>/dev/null || true
fi
RUNTIME_BODY="{\"version\":\"${CODEX_VERSION}\",\"requestedVersion\":\"${KYBER_REQUESTED_CODEX_VERSION:-}\"}"
if [ -n "$KYBER_CODEX_REQUESTED_SATISFIED" ]; then
    RUNTIME_BODY="${RUNTIME_BODY%?},\"requestedSatisfied\":${KYBER_CODEX_REQUESTED_SATISFIED}}"
fi
if curl -fsS --max-time 5 -H 'Content-Type: application/json' -X POST \
    -d "$RUNTIME_BODY" http://127.0.0.1:8091/runtime-version >/dev/null 2>&1; then
    echo "[kyber] runtime version reported: $CODEX_VERSION"
else
    echo "[kyber] WARNING: failed to report runtime version (best-effort; continuing)"
fi

# ---- Identity repo ----
# Clone-or-sync the identity repo and install the git credential helper, using
# the SAME implementation Claude Code agents use (kyber#676). This is what was
# missing: the LAUNCH_DIR block below already looked for $HOME/dev/<repo>/.git,
# but nothing ever created it, so the check could never be true and HK-47 — the
# first prod Codex agent — booted with no identity repo, no memory and no sync
# script while status.identityRepo reported "Ready".
#
# Sourced, not executed: it sets REPO_DIR, which the launch-dir logic uses.
#
# Resolve across BOTH layouts, because both are real:
#   - in the image, it is installed beside this script in /usr/local/bin
#   - in the repo, this lives in images/<runtime>/ and it lives in images/shared/
# KYBER_IDENTITY_REPO_SCRIPT overrides both, for tests that want a stub.
_kyber_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
_kyber_identity_script="${KYBER_IDENTITY_REPO_SCRIPT:-}"
if [ -z "$_kyber_identity_script" ]; then
    for _c in "${_kyber_dir}/kyber-identity-repo.sh" "${_kyber_dir}/../shared/kyber-identity-repo.sh"; do
        [ -r "$_c" ] && { _kyber_identity_script="$_c"; break; }
    done
fi
if [ -r "$_kyber_identity_script" ]; then
    . "$_kyber_identity_script"
else
    echo "[kyber] WARNING: shared identity-repo script not found (looked beside $_kyber_dir and in ../shared) — skipping identity repo setup" >&2
fi

LAUNCH_DIR="$HOME"
if [ -n "${KYBER_IDENTITY_REPO:-}" ]; then
    candidate="$HOME/dev/${KYBER_IDENTITY_REPO##*/}"
    if [ -d "$candidate/.git" ]; then LAUNCH_DIR="$candidate"; fi
fi
if [ -r /opt/kyber/KYBER.md ]; then
    mkdir -p "$LAUNCH_DIR/.runtime"
    cp /opt/kyber/KYBER.md "$LAUNCH_DIR/.runtime/KYBER.md"
fi

# Surface the previous Codex session before launching a fresh one. The
# session-saver sidecar continuously normalizes the active runtime transcript
# into this durable JSON file; a pod replacement preserves it on /persist.
STATE_FILE="${KYBER_SESSION_STATE_FILE:-$PERSIST_ROOT/session-state.json}"
if [ -f "$STATE_FILE" ]; then
    RECALL_LAST_ACTIVITY=$(jq -r '.last_activity // ""' "$STATE_FILE" 2>/dev/null || echo "")
    RECALL_N=$(jq -r '(.recent_exchanges | length) // 0' "$STATE_FILE" 2>/dev/null || echo 0)
    case "$RECALL_N" in (''|*[!0-9]*) RECALL_N=0 ;; esac
    if [ -n "$RECALL_LAST_ACTIVITY" ] || [ "$RECALL_N" -gt 0 ]; then
        RECALL_BODY=$(jq -r '
            "**Last activity:** " + (((.last_activity // "") | tostring) | if . == "" then "(none)" else . end) + "\n" +
            "**As of:** " + (((.updated_at // "") | tostring) | if . == "" then "(unknown)" else . end) + "\n\n" +
            "## Recent turns (oldest first)\n\n" +
            ( [ (.recent_exchanges // [])[]
                | "### " + (if .role == "user" then "User" else "Assistant" end)
                  + " · " + ((.timestamp // "") | tostring) + "\n\n"
                  + ((.content // "") | tostring) + "\n"
              ] | join("\n") )
        ' "$STATE_FILE" 2>/dev/null || echo "")
        if [ -n "$RECALL_BODY" ]; then
            mkdir -p "$LAUNCH_DIR/.runtime" 2>/dev/null || true
            {
                echo "# Session recall"
                echo
                echo "_Rendered at boot from the previous session's state (/persist/session-state.json) — what I was doing before this restart._"
                echo
                printf '%s\n' "$RECALL_BODY"
            } > "$LAUNCH_DIR/.runtime/session-recall.md" 2>/dev/null || true
            echo "[kyber] Session recall written to .runtime/session-recall.md (last activity: ${RECALL_LAST_ACTIVITY:-none})"
        fi
    fi
fi

# Install Kyber's cookbook only for agents with Telegram enabled. Preserve an
# identity-repo skill of the same name so an operator's customization wins.
TELEGRAM_SKILL_SRC="${KYBER_PLATFORM_SKILLS_DIR:-/opt/kyber/skills}/telegram-messaging"
TELEGRAM_SKILL_DST="$CODEX_HOME/skills/telegram-messaging"
# Identity-repo skills are operator-owned and override the platform cookbook.
if [ -n "${REPO_DIR:-}" ] && [ -r "$REPO_DIR/skills/telegram-messaging/SKILL.md" ]; then
    mkdir -p "$CODEX_HOME/skills"
    rm -rf "$TELEGRAM_SKILL_DST"
    ln -s "$REPO_DIR/skills/telegram-messaging" "$TELEGRAM_SKILL_DST"
fi
if [ -n "${KYBER_TELEGRAM_MCP_URL:-}" ] && [ -r "$TELEGRAM_SKILL_SRC/SKILL.md" ] && [ ! -e "$TELEGRAM_SKILL_DST" ]; then
    mkdir -p "$CODEX_HOME/skills"
    ln -s "$TELEGRAM_SKILL_SRC" "$TELEGRAM_SKILL_DST"
    echo "[kyber] Telegram messaging skill installed"
elif [ -n "${KYBER_TELEGRAM_MCP_URL:-}" ] && [ ! -r "$TELEGRAM_SKILL_SRC/SKILL.md" ]; then
    echo "[kyber] WARNING: Telegram messaging skill is missing or unreadable at $TELEGRAM_SKILL_SRC" >&2
fi

DISCORD_SKILL_SRC="${KYBER_PLATFORM_SKILLS_DIR:-/opt/kyber/skills}/discord-messaging"
DISCORD_SKILL_DST="$CODEX_HOME/skills/discord-messaging"
if [ -n "${REPO_DIR:-}" ] && [ -r "$REPO_DIR/skills/discord-messaging/SKILL.md" ]; then
    mkdir -p "$CODEX_HOME/skills"
    rm -rf "$DISCORD_SKILL_DST"
    ln -s "$REPO_DIR/skills/discord-messaging" "$DISCORD_SKILL_DST"
fi
if [ -n "${KYBER_DISCORD_MCP_URL:-}" ] && [ -r "$DISCORD_SKILL_SRC/SKILL.md" ] && [ ! -e "$DISCORD_SKILL_DST" ]; then
    mkdir -p "$CODEX_HOME/skills"
    ln -s "$DISCORD_SKILL_SRC" "$DISCORD_SKILL_DST"
    echo "[kyber] Discord messaging skill installed"
elif [ -n "${KYBER_DISCORD_MCP_URL:-}" ] && [ ! -r "$DISCORD_SKILL_SRC/SKILL.md" ]; then
    echo "[kyber] WARNING: Discord messaging skill is missing or unreadable at $DISCORD_SKILL_SRC" >&2
fi

# ---- Skill inventory ----
# Converge and report what this agent can ACTUALLY invoke, now that both the
# identity-repo skills and the image-bundled capability skills above are linked.
#
# This is a LOOP, not a one-shot. An agent asked to save a skill does the
# obvious thing — write skills/<name>/SKILL.md and sync its identity — and if
# linking only ran at boot, that skill would be committed, pushed, invisible to
# the UI, and not loadable in the very session that created it. Making the
# platform converge on a timer is what removes the "works if the agent
# remembers the extra command" dependency; `kyber-skills install` stays as the
# make-it-live-right-now fast path.
#
# Backgrounded: boot must not wait on it, and the reporter retries a sidecar
# that is still binding.
#
# This runs under `set -e`, well before the token-reporter block creates
# /persist/var/log, so the log path is resolved defensively — an unwritable
# redirect target here would kill the whole boot.
if command -v kyber-skills >/dev/null 2>&1; then
    KYBER_SKILLS_LOG="${KYBER_SKILLS_LOG:-/persist/var/log/kyber-skills.log}"
    mkdir -p "$(dirname "$KYBER_SKILLS_LOG")" 2>/dev/null || true
    [ -w "$(dirname "$KYBER_SKILLS_LOG")" ] || KYBER_SKILLS_LOG="$HOME/.kyber-skills.log"
    nohup kyber-skills report --repo-dir "${REPO_DIR:-}" --home "$HOME" \
        >> "$KYBER_SKILLS_LOG" 2>&1 &
    echo "[kyber] skill reporter started (pid=$!, log=$KYBER_SKILLS_LOG)"
fi

# Trust the launch directory. This goes in Kyber's managed config, not the
# agent's config.toml — Kyber decides which directory it launches the agent in,
# so the trust that follows from it is Kyber's to assert, and asserting it here
# keeps Kyber out of a file the agent owns.
{
    echo ""
    echo "[projects.\"$LAUNCH_DIR\"]"
    echo 'trust_level = "trusted"'
} | { kyber_append_managed_config "$KYBER_MANAGED_CODEX_CONFIG"; } || \
    echo "[kyber] WARNING: could not trust Codex launch directory $LAUNCH_DIR" >&2

CODEX_ARGS=(--ask-for-approval never --sandbox danger-full-access)
if [ -n "${CODEX_MODEL:-}" ]; then
    CODEX_ARGS=(--model "$CODEX_MODEL" "${CODEX_ARGS[@]}")
fi
if [ -n "${KYBER_STARTUP_PROMPT:-}" ]; then
    CODEX_ARGS+=(-- "$KYBER_STARTUP_PROMPT")
fi
# kyber#118: build the resume command from the arg set AFTER the startup
# prompt lands — a resumed session restores its conversation but has no turn
# to act on, so the prompt (`codex resume [SESSION_ID] [PROMPT]`) is what
# makes an agent interrupted mid-task pick the task back up. `resume --last`
# picks the newest recorded session; config.toml's tui.resume_cwd="current"
# scopes that to this launch dir.
CODEX_RESUME_CMD="codex resume --last $(printf '%q ' "${CODEX_ARGS[@]}")"

# Build the tmux command string once, shell-quoting every argument. CODEX_MODEL
# comes from spec.model, which the API does not charset-validate, and it is
# interpolated into the generated relaunch script below — an unquoted expansion
# there would let a crafted model string run arbitrary commands on every
# restart-session. Both launch paths use this single definition so they cannot
# drift apart.
CODEX_LAUNCH_CMD="codex $(printf '%q ' "${CODEX_ARGS[@]}")"

# kyber#118: native session resume. When spec.sessionResume is enabled, the
# pod-boot and crash-relaunch paths resume the previous session; an
# intentional restart-session passes --fresh to the generated relaunch
# script below and always starts clean. Resume only when the session store
# already holds a transcript — `codex resume --last` with nothing recorded
# would die immediately and read as a crash loop.
SESSION_RESUME_ENABLED=0
case "${KYBER_SESSION_RESUME:-}" in
    1|true|True|TRUE) SESSION_RESUME_ENABLED=1 ;;
esac
codex_has_prior_session() {
    [ -n "$(find "$CODEX_HOME/sessions" -name '*.jsonl' -print -quit 2>/dev/null)" ]
}

# Test hook, mirroring SKIP_CLAUDE_LAUNCH in start-claude.sh: run the whole boot
# path (credential seeding, identity repo, config render) and stop before
# anything long-lived starts, so tests can assert on the resulting filesystem
# without a reporter process or a tmux session to reap.
if [ -n "${SKIP_CODEX_LAUNCH:-}" ]; then
    echo "[kyber] SKIP_CODEX_LAUNCH set — boot path complete, not launching"
    exit 0
fi

nohup /usr/local/bin/kyber-codex-reporter >> "$PERSIST_ROOT/var/log/kyber-codex-reporter.log" 2>&1 &

cat > "$PERSIST_ROOT/last-codex-launch.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
# kyber#118: --fresh (the restart-session API's argv passes it) forces a
# fresh session; the crash watchdog calls this script bare, which is where
# resume applies.
KYBER_FRESH=0
[ "\${1:-}" = "--fresh" ] && KYBER_FRESH=1
exec 9>"$PERSIST_ROOT/var/lock/session.lock"
flock -x 9
if [ "\$(id -u)" -eq 0 ]; then
    TMUX=(runuser -u kyber -- tmux)
else
    TMUX=(tmux)
fi
# kyber#118: bare (crash-watchdog) invocations only revive a DEAD session —
# if one is alive under the lock, a concurrent restart-session already
# relaunched and killing it could resurrect the discarded conversation.
if [ "\$KYBER_FRESH" = "0" ] && "\${TMUX[@]}" has-session -t agent 2>/dev/null; then
    echo "[kyber] relaunch: session already alive — a concurrent restart won the race, nothing to do (kyber#118)"
    exit 0
fi
"\${TMUX[@]}" kill-session -t agent 2>/dev/null || true
# The restart lock lives on fd 9 in this shell. Close it in the tmux child:
# tmux becomes the long-lived server process on the first relaunch, and an
# inherited locked descriptor would otherwise make every later inbound prompt
# look like it arrived during a restart (and make the next restart hang).
# kyber#118: decide fresh-vs-resume at RUN time — the store may have gained
# its first transcript since boot, and --fresh must always win. Command
# strings and paths are baked at boot (unquoted heredoc); only the decision
# runs here.
RELAUNCH_CMD=$(printf '%q' "$CODEX_LAUNCH_CMD")
if [ "$SESSION_RESUME_ENABLED" = "1" ] && [ "\$KYBER_FRESH" = "0" ] \\
   && [ -n "\$(find $(printf '%q' "$CODEX_HOME")/sessions -name '*.jsonl' -print -quit 2>/dev/null)" ]; then
    RELAUNCH_CMD=$(printf '%q' "$CODEX_RESUME_CMD")
    echo "[kyber] relaunch: resuming previous session (kyber#118)"
fi
"\${TMUX[@]}" new-session -d -s agent -c $(printf '%q' "$LAUNCH_DIR") "\$RELAUNCH_CMD" 9>&-
EOF
chmod 0755 "$PERSIST_ROOT/last-codex-launch.sh"

BOOT_LAUNCH_CMD="$CODEX_LAUNCH_CMD"
if [ "$SESSION_RESUME_ENABLED" = "1" ] && codex_has_prior_session; then
    # kyber#118: pod boot after a recreate/preemption/crash — pick the
    # previous conversation back up instead of starting fresh.
    BOOT_LAUNCH_CMD="$CODEX_RESUME_CMD"
    echo "[kyber] session resume: continuing previous session (kyber#118)"
fi
echo "[kyber] Starting Codex ${CODEX_VERSION} in tmux (cwd=$LAUNCH_DIR)"
tmux new-session -d -s agent -c "$LAUNCH_DIR" "$BOOT_LAUNCH_CMD"

relaunch_count=0
DISK_EXHAUSTED_MARKER=/var/run/kyber/disk-exhausted
while true; do
    session_started=$(date +%s 2>/dev/null || echo 0)
    while tmux has-session -t agent 2>/dev/null; do
        if [ -f "$DISK_EXHAUSTED_MARKER" ]; then
            echo "[kyber] disk reserve reached; pausing Codex while Shell remains available"
            tmux kill-session -t agent 2>/dev/null || true
            break
        fi
        sleep 5
    done
	if [ -f "$DISK_EXHAUSTED_MARKER" ]; then
		while [ -f "$DISK_EXHAUSTED_MARKER" ]; do sleep 5; done
		echo "[kyber] disk reserve recovered; relaunching Codex"
		relaunch_count=0
	fi
    now=$(date +%s 2>/dev/null || echo 0)
    # A session that ran healthily (>=60s) before dying is not a crash loop.
    if [ $(( now - session_started )) -ge 60 ]; then
        relaunch_count=0
    fi
    relaunch_count=$((relaunch_count + 1))
    # kyber#118: give-up rung, mirroring start-claude.sh (kyber#563). The
    # liveness probe (`pgrep -f codex`) matches this start script itself, so
    # a permanently-dying session would otherwise loop here forever with the
    # pod reporting healthy. Exit so the controller recreates the pod.
    if [ "$relaunch_count" -gt 5 ]; then
        echo "[kyber] Codex session died ${relaunch_count}x within seconds — exiting so the controller recreates the pod" >&2
        exit 1
    fi
    echo "[kyber] Codex tmux session ended; relaunching"
    RELAUNCH_FLAG=""
    if [ "$relaunch_count" -ge 3 ] && [ "$SESSION_RESUME_ENABLED" = "1" ]; then
        # kyber#118: repeated fast deaths with resume on — the resumed
        # transcript itself may be the poison (e.g. corrupted by a hard
        # kill mid-write). Drop to a fresh session instead of looping.
        echo "[kyber] session resume: ${relaunch_count} fast deaths — falling back to a fresh session (kyber#118)"
        RELAUNCH_FLAG="--fresh"
    fi
    "$PERSIST_ROOT/last-codex-launch.sh" $RELAUNCH_FLAG || true
    sleep 2
done
