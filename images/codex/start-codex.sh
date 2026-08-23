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
        if [ "$CURRENT_CODEX_VERSION" != "$KYBER_REQUESTED_CODEX_VERSION" ]; then
            echo "[kyber] installing requested Codex harness version ${KYBER_REQUESTED_CODEX_VERSION}"
            # The baked-in package and /usr/bin/codex are root-owned. Runtime
            # startup runs as the unprivileged kyber user, so a plain global
            # npm install fails with EACCES while trying to replace them. The
            # image grants kyber passwordless sudo for this kind of boot-time
            # maintenance; resolve npm first because sudo's secure_path may
            # differ from the runtime PATH.
            _npm="$(command -v npm || echo /usr/bin/npm)"
            if ! sudo "$_npm" install -g "@openai/codex@${KYBER_REQUESTED_CODEX_VERSION}" 2>&1; then
                echo "[kyber] WARNING: requested Codex harness install failed; using baked-in version"
                KYBER_CODEX_REQUESTED_SATISFIED="false"
            else
                INSTALLED_CODEX_VERSION="$(codex --version 2>/dev/null | awk '{print $NF; exit}' || true)"
                if [ -n "$INSTALLED_CODEX_VERSION" ] && { [ "$KYBER_REQUESTED_CODEX_VERSION" = "latest" ] || [ "$INSTALLED_CODEX_VERSION" = "$KYBER_REQUESTED_CODEX_VERSION" ]; }; then
                    KYBER_CODEX_REQUESTED_SATISFIED="true"
                else
                    echo "[kyber] WARNING: Codex harness install completed but requested=${KYBER_REQUESTED_CODEX_VERSION} observed=${INSTALLED_CODEX_VERSION:-unknown}"
                    KYBER_CODEX_REQUESTED_SATISFIED="false"
                fi
            fi
            unset _npm INSTALLED_CODEX_VERSION
        else
            KYBER_CODEX_REQUESTED_SATISFIED="true"
        fi
    else
        echo "[kyber] WARNING: requested Codex harness version is invalid; using baked-in version"
        KYBER_CODEX_REQUESTED_SATISFIED="false"
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

cat > "$CODEX_HOME/config.toml" <<EOF
approval_policy = "never"
sandbox_mode = "danger-full-access"
check_for_update_on_startup = false
tui.resume_cwd = "current"
EOF
if [ -n "${CODEX_MODEL:-}" ]; then
    printf 'model = "%s"\n' "$CODEX_MODEL" >> "$CODEX_HOME/config.toml"
fi

# Telegram MCP sidecar (kyber#684). Same server the Claude Code runtime
# registers, so both runtimes get the same tool surface instead of Codex
# curl-ing a bare HTTP endpoint. The /send endpoint stays available for the
# inbound-binding action text, so this is additive — nothing breaks if an
# older binding is still telling the agent to curl.
if [ -n "${KYBER_TELEGRAM_MCP_URL:-}" ]; then
    cat >> "$CODEX_HOME/config.toml" <<EOF

[mcp_servers.kyber_telegram]
url = "${KYBER_TELEGRAM_MCP_URL}"
EOF
    echo "[kyber] Telegram MCP sidecar registered at ${KYBER_TELEGRAM_MCP_URL}"
fi
if [ -n "${KYBER_DISCORD_MCP_URL:-}" ]; then
    cat >> "$CODEX_HOME/config.toml" <<EOF

[mcp_servers.kyber_discord]
url = "${KYBER_DISCORD_MCP_URL}"
EOF
    echo "[kyber] Discord MCP sidecar registered at ${KYBER_DISCORD_MCP_URL}"
fi
chmod 0600 "$CODEX_HOME/config.toml"

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

cat >> "$CODEX_HOME/config.toml" <<EOF

[projects."$LAUNCH_DIR"]
trust_level = "trusted"
EOF

CODEX_ARGS=(--ask-for-approval never --sandbox danger-full-access)
if [ -n "${CODEX_MODEL:-}" ]; then
    CODEX_ARGS=(--model "$CODEX_MODEL" "${CODEX_ARGS[@]}")
fi
# kyber#118: build the resume command from the arg set BEFORE the startup
# prompt lands — the prompt is the initial turn of a NEW session, and a
# resumed session is not a new session. `resume --last` picks the newest
# recorded session; config.toml's tui.resume_cwd="current" scopes that to
# this launch dir.
CODEX_RESUME_CMD="codex resume --last $(printf '%q ' "${CODEX_ARGS[@]}")"
if [ -n "${KYBER_STARTUP_PROMPT:-}" ]; then
    CODEX_ARGS+=(-- "$KYBER_STARTUP_PROMPT")
fi

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
while true; do
    session_started=$(date +%s 2>/dev/null || echo 0)
    while tmux has-session -t agent 2>/dev/null; do sleep 5; done
    now=$(date +%s 2>/dev/null || echo 0)
    # A session that ran healthily (>=60s) before dying is not a crash loop.
    if [ $(( now - session_started )) -ge 60 ]; then
        relaunch_count=0
    fi
    relaunch_count=$((relaunch_count + 1))
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
