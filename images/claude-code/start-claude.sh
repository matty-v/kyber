#!/bin/bash
set -euo pipefail

mkdir -p ~/.claude ~/.claude/channels/telegram ~/.claude/plugins ~/.claude/statsig

# ---- Onboarding bypass (the CRITICAL piece) ----
# Claude Code's onboarding is controlled by ~/.claude.json (NOT ~/.claude/settings.json!).
# Without hasCompletedOnboarding:true, the onboarding flow runs — which includes
# an interactive OAuth sign-in step that blocks headless startup.
# This was the root cause of early auth failures; the bypass must always be written.
#
# This pattern matches the legacy Kyber agents (R2-D2 etc.) — see
# kyber-legacy/images/bootstrap-claude-code.sh for the source of truth.
if [ ! -f ~/.claude.json ]; then
    cat > ~/.claude.json <<EOF
{
  "hasCompletedOnboarding": true,
  "projects": {}
}
EOF
    echo "[kyber] ~/.claude.json written (onboarding bypass)"
fi

# Accept terms
touch ~/.claude/.terms-accepted 2>/dev/null || true

# ---- Settings ----
if [ ! -f ~/.claude/settings.json ]; then
    cat > ~/.claude/settings.json <<EOF
{
  "theme": "dark",
  "skipDangerousModePermissionPrompt": true
}
EOF
    echo "[kyber] settings.json written"
fi

# ---- Scheduled-job post-run hook ----
# kyber-cron-postrun is the platform's Stop hook for scheduled jobs: it clears
# the agent's context when a job declared clearContextAfter, and removes the
# job's pending marker, which is what releases the --exclusive agent-busy guard
# in kyber-job-dispatch.
#
# Registered in the USER settings (~/.claude/settings.json) rather than in the
# identity repo's project settings, so it merges with whatever hooks the agent
# owns instead of two writers competing for one file.
#
# The sentinel is the contract with the dispatcher: no sentinel means nothing
# will ever clear a pending marker, so the dispatcher must not write one — an
# --exclusive job would otherwise skip every fire until the staleness TTL. That
# is what keeps this feature inert rather than harmful on a runtime with no
# Stop-hook equivalent.
KYBER_POSTRUN_CMD="/usr/local/bin/kyber-cron-postrun"
KYBER_TURNSTART_CMD="/usr/local/bin/kyber-cron-turn-start"
KYBER_POSTRUN_SENTINEL="${KYBER_CRON_POSTRUN_SENTINEL:-/persist/var/run/kyber-cron-postrun-enabled}"

# register_kyber_hook <event> <command> — idempotent jq merge into the user
# settings. Returns non-zero if the hook is not present afterwards.
register_kyber_hook() {
    local event="$1" cmd="$2"
    if grep -qF "$cmd" ~/.claude/settings.json 2>/dev/null; then
        return 0
    fi
    if jq --arg ev "$event" --arg cmd "$cmd" \
        '.hooks //= {} | .hooks[$ev] //= []
         | .hooks[$ev] += [{"hooks":[{"type":"command","command":$cmd,"timeout":20}]}]' \
        ~/.claude/settings.json > ~/.claude/settings.json.tmp 2>/dev/null \
        && [ -s ~/.claude/settings.json.tmp ]; then
        mv ~/.claude/settings.json.tmp ~/.claude/settings.json
        return 0
    fi
    # A truncated settings.json would break the runtime entirely, not just this
    # feature — same guard as the legacy-plugin strip below.
    rm -f ~/.claude/settings.json.tmp
    return 1
}

if [ -x "$KYBER_POSTRUN_CMD" ] && [ -x "$KYBER_TURNSTART_CMD" ] && command -v jq >/dev/null 2>&1; then
    register_kyber_hook Stop "$KYBER_POSTRUN_CMD" \
        || echo "[kyber] WARNING: could not register cron post-run hook" >&2
    register_kyber_hook UserPromptSubmit "$KYBER_TURNSTART_CMD" \
        || echo "[kyber] WARNING: could not register cron turn-start hook" >&2

    # Both or neither. The pair is the mechanism: arming without consuming
    # leaks markers and mutes --exclusive; consuming without arming is the
    # mis-correlation bug (clearing on an unrelated turn). Claiming the
    # capability with only half of it registered would be worse than not
    # claiming it at all.
    if grep -qF "$KYBER_POSTRUN_CMD" ~/.claude/settings.json 2>/dev/null \
        && grep -qF "$KYBER_TURNSTART_CMD" ~/.claude/settings.json 2>/dev/null; then
        mkdir -p "$(dirname "$KYBER_POSTRUN_SENTINEL")" 2>/dev/null || true
        : > "$KYBER_POSTRUN_SENTINEL" 2>/dev/null || true
        echo "[kyber] cron context hooks registered"
    else
        rm -f "$KYBER_POSTRUN_SENTINEL" 2>/dev/null || true
        echo "[kyber] WARNING: cron context hooks incomplete; feature disabled" >&2
    fi
fi

# ---- Telegram: MCP sidecar (kyber#684) ----
# The native channel plugin is GONE from this path. Telegram is served by the
# kyber-mcp-telegram sidecar for every runtime now, and the controller no longer
# injects TELEGRAM_BOT_TOKEN here — without a token the plugin cannot poll, so
# the two can never race for the same bot. That race (#678/#679) is why a
# rebooted agent sometimes went permanently deaf: whichever poller won decided
# whether Telegram worked at all.
#
# The agent keeps a real tool surface (reply/react/edit_message/
# download_attachment) because the sidecar speaks MCP over loopback HTTP.
if [ -n "${KYBER_TELEGRAM_MCP_URL:-}" ]; then
    # Retire a plugin install left on /persist by a pre-#684 pod. Left in place
    # it would still be listed in enabledPlugins from that older settings.json
    # and would start a second poller on the next boot.
    if [ -f ~/.claude/plugins/installed_plugins.json ]; then
        claude plugins uninstall telegram@claude-plugins-official >/dev/null 2>&1 \
            && echo "[kyber] removed the legacy Telegram plugin — the sidecar owns this channel now" || true
    fi
    rm -f ~/.claude/channels/telegram/bot.pid 2>/dev/null || true
    if [ -f ~/.claude/settings.json ] && grep -q 'telegram@claude-plugins-official' ~/.claude/settings.json 2>/dev/null; then
        # Strip the stale enablement without disturbing anything else in the
        # file. jq, not python3: jq is an explicit dependency in
        # images/agent-base/Dockerfile, whereas python3 only happens to be
        # present transitively on ubuntu:24.04 — and this is the code enforcing
        # the one-bridge invariant, so it should not rest on a package nobody
        # declared. Written to a temp file and moved, so an interrupted boot
        # cannot leave a truncated settings.json behind.
        if jq 'del(.enabledPlugins["telegram@claude-plugins-official"])
               | if (.enabledPlugins | length) == 0 then del(.enabledPlugins) else . end' \
               ~/.claude/settings.json > ~/.claude/settings.json.tmp 2>/dev/null \
           && [ -s ~/.claude/settings.json.tmp ]; then
            mv ~/.claude/settings.json.tmp ~/.claude/settings.json
            echo "[kyber] stripped the legacy Telegram plugin from settings.json"
        else
            rm -f ~/.claude/settings.json.tmp
            echo "[kyber] WARNING: could not strip legacy plugin from settings.json" >&2
        fi
    fi

    # Register the sidecar. `claude mcp add` is idempotent per scope, but a
    # re-add after a URL change must win, so remove first and ignore the
    # not-found case on a fresh agent.
    claude mcp remove kyber-telegram --scope user >/dev/null 2>&1 || true
    if claude mcp add kyber-telegram "$KYBER_TELEGRAM_MCP_URL" \
            --transport http --scope user >/dev/null 2>&1; then
        echo "[kyber] Telegram MCP sidecar registered at $KYBER_TELEGRAM_MCP_URL"
    else
        echo "[kyber] WARNING: could not register the Telegram MCP server — the agent will not be able to reply" >&2
    fi
fi

# ---- Discord: MCP sidecar ----
if [ -n "${KYBER_DISCORD_MCP_URL:-}" ]; then
    claude mcp remove kyber-discord --scope user >/dev/null 2>&1 || true
    if claude mcp add kyber-discord "$KYBER_DISCORD_MCP_URL" \
            --transport http --scope user >/dev/null 2>&1; then
        echo "[kyber] Discord MCP sidecar registered at $KYBER_DISCORD_MCP_URL"
    else
        echo "[kyber] WARNING: could not register the Discord MCP server — the agent will use the HTTP fallback" >&2
    fi
fi

# ---- Identity repo ----
# Clone-or-sync the identity repo and install the git credential helper. The
# implementation is SHARED with every other runtime (kyber#676) — it used to
# live inline here, which is why the Codex runtime shipped without it and
# HK-47, the first prod Codex agent, booted with no identity repo at all.
#
# Sourced, not executed: it sets REPO_DIR and KYBER_SYNC_SCRIPT, which the rest
# of this script uses below.
# Resolve the shared script across BOTH layouts, because both are real:
#   - in the image, it is installed beside this script in /usr/local/bin
#   - in the repo, this lives in images/<runtime>/ and it lives in images/shared/
#     (the layout the boot test suite runs against)
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

# Install Kyber's cookbook only for agents with Telegram enabled. Preserve an
# identity-repo skill of the same name so an operator's customization wins.
TELEGRAM_SKILL_SRC="${KYBER_PLATFORM_SKILLS_DIR:-/opt/kyber/skills}/telegram-messaging"
TELEGRAM_SKILL_DST="$HOME/.claude/skills/telegram-messaging"
if [ -n "${KYBER_TELEGRAM_MCP_URL:-}" ] && [ -r "$TELEGRAM_SKILL_SRC/SKILL.md" ] && [ ! -e "$TELEGRAM_SKILL_DST" ]; then
    mkdir -p "$HOME/.claude/skills"
    ln -s "$TELEGRAM_SKILL_SRC" "$TELEGRAM_SKILL_DST"
    echo "[kyber] Telegram messaging skill installed"
elif [ -n "${KYBER_TELEGRAM_MCP_URL:-}" ] && [ ! -r "$TELEGRAM_SKILL_SRC/SKILL.md" ]; then
    echo "[kyber] WARNING: Telegram messaging skill is missing or unreadable at $TELEGRAM_SKILL_SRC" >&2
fi

# Install the Discord capability cookbook only when this pod has the Discord
# MCP sidecar. An identity-repo skill of the same name already linked by the
# shared setup wins, matching the Telegram override contract above.
DISCORD_SKILL_SRC="${KYBER_PLATFORM_SKILLS_DIR:-/opt/kyber/skills}/discord-messaging"
DISCORD_SKILL_DST="$HOME/.claude/skills/discord-messaging"
if [ -n "${KYBER_DISCORD_MCP_URL:-}" ] && [ -r "$DISCORD_SKILL_SRC/SKILL.md" ] && [ ! -e "$DISCORD_SKILL_DST" ]; then
    mkdir -p "$HOME/.claude/skills"
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

# ---- gh CLI bootstrap ----
# If the agent has a user-secret named `github-token` (surfaced as
# $USER_GITHUB_TOKEN), expose it as GH_TOKEN so the `gh` CLI works out of the
# box. Otherwise agents have to discover the variable and prepend it to every
# command. Same convention used by Falcon Dev Team agents (luke/han/chewie).
if [ -n "${USER_GITHUB_TOKEN:-}" ] && [ -z "${GH_TOKEN:-}" ]; then
    export GH_TOKEN="$USER_GITHUB_TOKEN"
    echo "[kyber] exported GH_TOKEN from USER_GITHUB_TOKEN for gh CLI"
fi

# ---- npm/GHCR auth ----
# If the agent has a user-secret named `npm-token` (surfaced as
# $USER_NPM_TOKEN), expose it as NODE_AUTH_TOKEN so npm/yarn/pnpm can
# install @matty-v/* packages from GitHub Packages without the agent
# having to remember the prepend. Repos using @matty-v/* npm deps have
# an `.npmrc` referencing ${NODE_AUTH_TOKEN}. Without this bridge,
# every install fails 401 on the first @matty-v/* package.
if [ -n "${USER_NPM_TOKEN:-}" ] && [ -z "${NODE_AUTH_TOKEN:-}" ]; then
    export NODE_AUTH_TOKEN="$USER_NPM_TOKEN"
    echo "[kyber] exported NODE_AUTH_TOKEN from USER_NPM_TOKEN for npm/GHCR auth"
fi

# ---- OAuth ----
# Credentials are injected as CLAUDE_ACCESS_TOKEN + CLAUDE_REFRESH_TOKEN +
# CLAUDE_ACCESS_TOKEN_EXPIRES_AT from the <agent>-oauth k8s Secret (PKCE flow).
# The credential-handling block below refreshes if needed and writes .credentials.json.

# ---- Credential handling ----
# On every boot we reach this block with three potentially-set env vars from
# the <agent>-oauth Secret:
#   CLAUDE_ACCESS_TOKEN, CLAUDE_REFRESH_TOKEN, CLAUDE_ACCESS_TOKEN_EXPIRES_AT (ms epoch)
#
# Strategy:
#   1. If the cached access_token is still valid (> 5 min remaining), reuse it.
#      Skip the Anthropic refresh entirely. This is the common path on restart.
#   2. Otherwise refresh via Anthropic. Anthropic always rotates the refresh_token,
#      so we MUST persist the new credentials back to the Secret BEFORE continuing.
#      Failing to persist would leave a stale secret that breaks the next boot.

# Skip the entire OAuth refresh block for api-key agents. They don't have
# access_token/refresh_token, so running this path just produces misleading
# "no refresh token" warnings. The --bare flag (added below) handles auth.
if [ -n "${ANTHROPIC_API_KEY:-}" ] && [ -z "${CLAUDE_ACCESS_TOKEN:-}" ]; then
    echo "[kyber] api-key auth — skipping OAuth credential refresh"
else

ANTHROPIC_TOKEN_URL="${ANTHROPIC_TOKEN_URL:-https://platform.claude.com/v1/oauth/token}"

if [ -z "${KYBER_REFRESH_TOKEN_URL:-}" ]; then
    echo "[kyber] FATAL: KYBER_REFRESH_TOKEN_URL not set — controller did not inject the rotation endpoint" >&2
    exit 2
fi

NOW_MS=$(($(date +%s) * 1000))
BUFFER_MS=$((5 * 60 * 1000))
USE_CACHED=false

if [ -n "${CLAUDE_ACCESS_TOKEN:-}" ] && [ -n "${CLAUDE_ACCESS_TOKEN_EXPIRES_AT:-}" ]; then
    if [ "$CLAUDE_ACCESS_TOKEN_EXPIRES_AT" -gt "$((NOW_MS + BUFFER_MS))" ]; then
        USE_CACHED=true
        echo "[kyber] cached access_token still valid (expires_at=$CLAUDE_ACCESS_TOKEN_EXPIRES_AT) — skipping refresh"
        access="$CLAUDE_ACCESS_TOKEN"
        expires_at="$CLAUDE_ACCESS_TOKEN_EXPIRES_AT"
        effective_refresh="$CLAUDE_REFRESH_TOKEN"
    fi
fi

if [ "$USE_CACHED" = false ]; then
    if [ -z "${CLAUDE_REFRESH_TOKEN:-}" ]; then
        echo "[kyber] no refresh token in env — Claude Code will prompt for /login" >&2
    else
        echo "[kyber] refreshing access token from stored refresh token"
        refresh_body=$(jq -n --arg rt "$CLAUDE_REFRESH_TOKEN" \
          '{grant_type:"refresh_token", client_id:"9d1c250a-e61b-44d9-88ed-5944d1962f5e", refresh_token:$rt}')
        if ! resp=$(curl -fsS --max-time 15 -X POST "$ANTHROPIC_TOKEN_URL" \
             -H "Content-Type: application/json" \
             -H "User-Agent: kyber-claude-code/1.0" \
             -d "$refresh_body"); then
            echo "[kyber] refresh failed — exiting 2 so node-agent transitions to NeedsAuth" >&2
            exit 2
        fi
        access=$(echo "$resp" | jq -r .access_token)
        new_refresh=$(echo "$resp" | jq -r '.refresh_token // empty')
        expires_in=$(echo "$resp" | jq -r '((.expires_in // 3600) | floor)')
        if ! [[ "$expires_in" =~ ^[0-9]+$ ]]; then
            echo "[kyber] invalid expires_in: $expires_in" >&2
            exit 2
        fi
        if [ -z "$access" ] || [ "$access" = "null" ]; then
            echo "[kyber] refresh returned empty access_token" >&2
            exit 2
        fi
        expires_at=$(( ($(date +%s) + expires_in) * 1000 ))
        effective_refresh="${new_refresh:-$CLAUDE_REFRESH_TOKEN}"

        # Rotation push — BLOCKING. Anthropic has consumed the old refresh_token by
        # now. If we don't persist the new credentials, the next boot's secret will
        # be stale and Anthropic will return invalid_grant. Better to crash loud
        # here than corrupt the secret silently.
        if [ -n "${AGENT_NAME:-}" ]; then
            POD_TOKEN=$(cat /var/run/secrets/kyber/pod-token 2>/dev/null || echo "")
            rot_body=$(jq -n \
              --arg at "$access" \
              --arg rt "$effective_refresh" \
              --argjson ex "$expires_at" \
              '{access_token:$at, refresh_token:$rt, expires_at:$ex}')
            if ! curl -fsS --max-time 10 \
                 -H "Authorization: Bearer $POD_TOKEN" \
                 -H "Content-Type: application/json" \
                 -X POST \
                 "$KYBER_REFRESH_TOKEN_URL" \
                 -d "$rot_body"; then
                echo "[kyber] FATAL: rotation push to control-plane failed" >&2
                echo "[kyber] Anthropic has consumed the old refresh_token, but Kyber's secret was not updated." >&2
                echo "[kyber] To recover: re-auth this agent via the PWA (delete + recreate, or patch the secret manually)." >&2
                exit 2
            fi
            echo "[kyber] rotation push succeeded — secret updated with new credentials"
        fi
    fi
fi

# ---- Always write credentials.json from the resolved trio ----
if [ -n "${access:-}" ] && [ -n "${effective_refresh:-}" ] && [ -n "${expires_at:-}" ]; then
    mkdir -p "$HOME/.claude"
    cat > "$HOME/.claude/.credentials.json" <<EOF
{
  "claudeAiOauth": {
    "accessToken": "$access",
    "refreshToken": "$effective_refresh",
    "expiresAt": $expires_at,
    "scopes": ["org:create_api_key","user:profile","user:inference","user:sessions:claude_code","user:mcp_servers","user:file_upload"]
  }
}
EOF
    chmod 600 "$HOME/.claude/.credentials.json"
    echo "[kyber] credentials.json written"
fi

fi # end of OAuth/api-key guard

# ---- Agent manual — render the platform's own manual into the pod on boot ----
# docs/agent-manual.md is baked into the image at /opt/kyber/KYBER.md (see the
# Dockerfile) and copied here into <launch dir>/.runtime/KYBER.md, the same
# platform-owned continuity location as session-recall.md below. Rendering it at
# boot rather than scaffolding it into each identity repo means every agent reads
# the manual for the platform version it is ACTUALLY running on — a repo copy
# freezes on the day the repo was created and then quietly lies. Read-only to the
# agent: it is overwritten every boot.
# Placed BEFORE the SKIP_CLAUDE_LAUNCH short-circuit (so it is unit-testable) and
# BEFORE claude launches (so it is in place for the CLAUDE.md walk-up). The base
# dir resolution mirrors the recall block below. A missing source is not fatal —
# an older base image, or a runtime that doesn't bake the manual, must still boot
# cleanly — but it is never silent: the first cut of this block said nothing at
# all when the source was unreadable, and the manual then failed to render on the
# entire fleet with no line in any boot log to show for it. See the else arm.
MANUAL_SRC="${KYBER_AGENT_MANUAL_PATH:-/opt/kyber/KYBER.md}"
MANUAL_BASE="${HOME:-/home/kyber}"
if [ -n "${REPO_DIR:-}" ] && [ -d "${REPO_DIR:-}" ]; then
    MANUAL_BASE="$REPO_DIR"
fi
if [ -s "$MANUAL_SRC" ]; then
    mkdir -p "$MANUAL_BASE/.runtime" 2>/dev/null || true
    if cp "$MANUAL_SRC" "$MANUAL_BASE/.runtime/KYBER.md" 2>/dev/null; then
        echo "[kyber] Agent manual written to .runtime/KYBER.md"
        # Keep the platform's copy out of the agent's git history. It is
        # byte-identical to the image's, so committing it only churns every
        # identity repo on each image bump — and an agent running a `git add -A`
        # save (the /sync-identity skill does) would sweep it in without meaning
        # to. .git/info/exclude is local-only, so this holds for every EXISTING
        # agent repo without waiting for each one to add a .gitignore entry —
        # which is the same reason the manual is rendered rather than scaffolded.
        # Idempotent: appended once, re-checked every boot.
        if [ -d "$MANUAL_BASE/.git" ]; then
            _excl="$MANUAL_BASE/.git/info/exclude"
            mkdir -p "$MANUAL_BASE/.git/info" 2>/dev/null || true
            if ! grep -qxF '.runtime/KYBER.md' "$_excl" 2>/dev/null; then
                printf '%s\n' '.runtime/KYBER.md' >> "$_excl" 2>/dev/null || true
            fi
        fi
    else
        echo "[kyber] WARNING: could not write .runtime/KYBER.md (continuing)"
    fi
else
    # Say WHY there is no manual. `[ -s ]` is false for "absent" and for "present
    # but I can't reach it", and those are very different problems: the first is
    # an old image, the second is a packaging bug on the current one. This block
    # runs as the unprivileged `kyber` user, so a non-traversable parent
    # directory hides the file completely — exactly how kyber#653 shipped broken
    # (the Dockerfile's `COPY --chmod=0644` applied 0644 to the /opt/kyber
    # directory it implicitly created, and root-run tests never noticed).
    _mdir=$(dirname "$MANUAL_SRC")
    if [ -d "$_mdir" ] && [ ! -x "$_mdir" ]; then
        echo "[kyber] WARNING: agent manual unreachable — $_mdir is not traversable by $(id -un) (mode $(stat -c %a "$_mdir" 2>/dev/null || echo '?')); .runtime/KYBER.md NOT written"
    else
        echo "[kyber] no agent manual baked at $MANUAL_SRC — skipping"
    fi
fi

# ---- Session recall — surface the previous session's state on boot ----
# The session-saver sidecar continuously writes /persist/session-state.json (last
# activity + recent turns). /persist survives pod recreation, so on the next boot
# we render it into <launch dir>/.runtime/session-recall.md — a stable file the
# agent's CLAUDE.md reads on startup (the same continuity-state convention the
# kyber-agent-template already uses). Rendered into a SEPARATE file so the
# sidecar's ongoing updates to session-state.json never race the boot read.
# Placed BEFORE the SKIP_CLAUDE_LAUNCH short-circuit (so it is unit-testable) and
# BEFORE claude launches (so it is in place for the CLAUDE.md walk-up). The base
# dir mirrors LAUNCH_DIR's resolution (REPO_DIR when the identity repo is present,
# else $HOME) but is computed here with set -u-safe defaults since LAUNCH_DIR is
# defined later. Absent/empty state (first boot) is a silent no-op.
# STATE_FILE must match ClaudeCodeAdapter.SessionStatePath() (pkg/runtimes/
# claudecode/adapter.go) — the session-saver sidecar writes there.
STATE_FILE="${KYBER_SESSION_STATE_FILE:-/persist/session-state.json}"
RECALL_BASE="${HOME:-/home/kyber}"
if [ -n "${REPO_DIR:-}" ] && [ -d "${REPO_DIR:-}" ]; then
    RECALL_BASE="$REPO_DIR"
fi
if [ -f "$STATE_FILE" ]; then
    RECALL_LAST_ACTIVITY=$(jq -r '.last_activity // ""' "$STATE_FILE" 2>/dev/null || echo "")
    RECALL_N=$(jq -r '(.recent_exchanges | length) // 0' "$STATE_FILE" 2>/dev/null || echo 0)
    case "$RECALL_N" in (''|*[!0-9]*) RECALL_N=0 ;; esac
    if [ -n "$RECALL_LAST_ACTIVITY" ] || [ "$RECALL_N" -gt 0 ]; then
        # Render the body with a null/type-safe jq program — tolerate a missing/null
        # recent_exchanges and non-string timestamp/content, since durable /persist
        # state may come from a different session-saver version across a rolling
        # upgrade. Render into a variable FIRST and write only when non-empty, so a
        # jq failure never truncates a previously-good recall file nor logs a false
        # "written" success.
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
            mkdir -p "$RECALL_BASE/.runtime" 2>/dev/null || true
            {
                echo "# Session recall"
                echo
                echo "_Rendered at boot from the previous session's state (/persist/session-state.json) — what I was doing before this restart._"
                echo
                printf '%s\n' "$RECALL_BODY"
            } > "$RECALL_BASE/.runtime/session-recall.md" 2>/dev/null || true
            echo "[kyber] Session recall written to .runtime/session-recall.md (last activity: ${RECALL_LAST_ACTIVITY:-none})"
        fi
    fi
fi

# SKIP_CLAUDE_LAUNCH short-circuits for unit tests.
if [ -n "${SKIP_CLAUDE_LAUNCH:-}" ]; then
    exit 0
fi

if [ -f ~/.claude/.credentials.json ]; then
    echo "[kyber] credentials.json present — Claude Code will authenticate via keychain"
else
    echo "[kyber] No credentials.json — Claude Code will prompt for /login on first use"
fi

# ---- Session brief — preemption detection ----
BRIEF_FILE="/persist/session-brief.json"
if [ -f "$BRIEF_FILE" ]; then
    SHUTDOWN_TYPE=$(jq -r '.shutdownType // ""' "$BRIEF_FILE" 2>/dev/null || echo "")
    if [ "$SHUTDOWN_TYPE" = "preemption" ]; then
        export KYBER_PREEMPTION_RESTART=true
        GRACEFUL=$(jq -r '.metadata.preemptionContext.gracefulDrain // false' "$BRIEF_FILE" 2>/dev/null || echo "false")
        echo "[kyber] Restarting after preemption (graceful=$GRACEFUL)"
    else
        echo "[kyber] Session brief: shutdownType=$SHUTDOWN_TYPE"
    fi
fi

# ---- Build arguments ----
CLAUDE_ARGS="--dangerously-skip-permissions"

# API-key agents use --bare mode which accepts ANTHROPIC_API_KEY without the
# interactive "Use this API key?" prompt that blocks headless startup.
# --bare also disables OAuth/keychain, so --channels is not supported.
if [ -n "${ANTHROPIC_API_KEY:-}" ] && [ -z "${CLAUDE_ACCESS_TOKEN:-}" ]; then
    CLAUDE_ARGS="$CLAUDE_ARGS --bare"
    echo "[kyber] api-key auth detected — using --bare mode"
    if [ -n "${TELEGRAM_BOT_TOKEN:-}" ]; then
        echo "[kyber] WARNING: --channels not supported with api-key auth (requires OAuth). Telegram will not be available."
    fi
elif [ -n "${TELEGRAM_BOT_TOKEN:-}" ]; then
    # Pass token as env var (like the laptop agents do), NOT inline in --channels.
    # Inline :token=... changes the channel identifier from "telegram@claude-plugins-official"
    # to "telegram@claude-plugins-official:token=...", which breaks plugin and allowlist lookup.
    export TELEGRAM_BOT_TOKEN
    CLAUDE_ARGS="$CLAUDE_ARGS --channels plugin:telegram@claude-plugins-official"
fi

# ---- Boot-time Claude Code install (kyber#377 / PR-C) --------------------
#
# The controller may set KYBER_REQUESTED_CC_VERSION to override the
# build-time CC pin baked into the image (KYBER_RUNTIME_DEFAULT_VERSION).
# When the requested value is non-empty AND differs from the default,
# install it now via npm; otherwise the boot path is byte-equivalent to
# the pre-PR-C behavior.
#
# SECURITY CRITICAL: the value flows into a shell command (`npm install
# @anthropic-ai/claude-code@<value>`). Validate against a strict charset
# BEFORE any interpolation — mirrors the KYBER_IDENTITY_REPO slug guard at
# :108-112. The kubebuilder CRD pattern enforces the same shape at admit
# time; this is defense-in-depth so a future CRD change can't accidentally
# open a shell-injection path on a pod that holds OAuth tokens.
#
# Install failure is non-fatal: keep the baked-in version and continue
# boot. PR-E surfaces the mismatch via the runtime report so operators
# see it in the PWA without grepping pod logs.
KYBER_CC_INSTALL_OUTCOME="not-requested"
if [ -n "${KYBER_REQUESTED_CC_VERSION:-}" ]; then
    if ! printf '%s' "${KYBER_REQUESTED_CC_VERSION}" | grep -Eq '^[0-9A-Za-z.\-]+$'; then
        echo "[kyber] WARNING: KYBER_REQUESTED_CC_VERSION='${KYBER_REQUESTED_CC_VERSION}' rejected by charset guard (^[0-9A-Za-z.-]+\$); continuing on baked-in version ${KYBER_RUNTIME_DEFAULT_VERSION:-unset}"
        KYBER_CC_INSTALL_OUTCOME="rejected-charset"
    elif [ "${#KYBER_REQUESTED_CC_VERSION}" -gt 64 ]; then
        echo "[kyber] WARNING: KYBER_REQUESTED_CC_VERSION exceeds 64 chars; rejected; continuing on baked-in version ${KYBER_RUNTIME_DEFAULT_VERSION:-unset}"
        KYBER_CC_INSTALL_OUTCOME="rejected-length"
    elif [ "${KYBER_REQUESTED_CC_VERSION}" = "${KYBER_RUNTIME_DEFAULT_VERSION:-}" ]; then
        echo "[kyber] CC install: requested version matches baked-in (${KYBER_REQUESTED_CC_VERSION}); skipping npm install"
        KYBER_CC_INSTALL_OUTCOME="skipped-equal"
    else
        echo "[kyber] CC install: installing @anthropic-ai/claude-code@${KYBER_REQUESTED_CC_VERSION} (baked-in=${KYBER_RUNTIME_DEFAULT_VERSION:-unset})"
        # Install as ROOT. The baked-in claude is `npm install -g`'d as root at
        # image build, so /usr/lib/node_modules and /usr/bin/claude are
        # root-owned; running this install as the (non-root) kyber user can't
        # replace them — it fails with EACCES and the upgrade silently no-ops.
        # kyber has passwordless sudo. Resolve npm's path explicitly because
        # sudo's secure_path may not include it.
        _npm="$(command -v npm || echo /usr/bin/npm)"
        # Self-heal stale npm staging dirs before installing. npm upgrades a
        # global package by renaming the current dir aside to a hidden staging
        # dir (.<pkg>-<hash>), unpacking the new version, then swapping. If a
        # prior install was interrupted (OOM, pod kill, SIGTERM mid-unpack) the
        # staging dir is left behind — and on whole-disk-persistence agents it
        # is saved to the PVC, so it survives every reboot. The next install
        # then fails forever with `ENOTEMPTY: rename ... .claude-code-<hash>`
        # and the agent is permanently pinned to the last good version,
        # ignoring every requestedVersion bump (kyber#483). These dirs are
        # failed-install garbage, never the live install (that's `claude-code`,
        # no leading dot), so removing them is safe.
        _ccparent="$(dirname "$("${_npm}" root -g 2>/dev/null)/@anthropic-ai/claude-code")"
        if [ -d "${_ccparent}" ]; then
            for _stale in "${_ccparent}"/.claude-code-*; do
                [ -e "${_stale}" ] || continue
                echo "[kyber] CC install: clearing stale npm staging dir $(basename "${_stale}") before install"
                sudo rm -rf "${_stale}" 2>&1 || true
            done
        fi
        _npm_install_ok="false"
        if sudo "${_npm}" install -g "@anthropic-ai/claude-code@${KYBER_REQUESTED_CC_VERSION}" 2>&1; then
            _npm_install_ok="true"
        fi
        # Determine success from the RUNNING binary, not npm's exit code alone:
        # `claude --version` must actually run, and for a concrete request it
        # must report exactly that version. Trusting the exit code by itself
        # lets requestedSatisfied lie when an install reports OK but didn't
        # take effect. `latest` has no version string to compare against, so
        # there it is npm's exit code AND a binary that still answers
        # --version — an empty answer means the install broke the CLI and must
        # NOT be reported as satisfied.
        _installed="$(claude --version 2>/dev/null | awk '{print $1; exit}')"
        if [ "${_npm_install_ok}" = "true" ] && { { [ "${KYBER_REQUESTED_CC_VERSION}" = "latest" ] && [ -n "${_installed}" ]; } || [ "${_installed}" = "${KYBER_REQUESTED_CC_VERSION}" ]; }; then
            if [ "${KYBER_REQUESTED_CC_VERSION}" = "latest" ]; then
                echo "[kyber] CC install: succeeded (latest -> ${_installed})"
            else
                echo "[kyber] CC install: succeeded (${KYBER_REQUESTED_CC_VERSION})"
            fi
            KYBER_CC_INSTALL_OUTCOME="installed"
        else
            echo "[kyber] CC install: FAILED — claude --version reports '${_installed:-unknown}', not ${KYBER_REQUESTED_CC_VERSION}; falling back to baked-in ${KYBER_RUNTIME_DEFAULT_VERSION:-unset} (mismatch surfaced via the runtime report / PWA badge)"
            KYBER_CC_INSTALL_OUTCOME="failed"
        fi
    fi
fi
export KYBER_CC_INSTALL_OUTCOME

# Map model names to Claude Code aliases. Claude Code with Max subscription
# expects aliases (sonnet, opus, haiku) or full 4.6+ names (claude-sonnet-4-6).
# The CRD may store older names like "claude-sonnet-4" that no longer resolve.
#
# For models that support a 1M context window, opt in via the [1m] suffix.
# Without it, Claude Code runs the 200K variant — auto-compaction then fires
# around 165K and Kyber's budget card (which reads pkg/tokenreport/limits.go
# for the same model IDs) silently overreports the headroom by ~5x. We're on
# Max plans; paying for 1M and running at 200K is both misleading and slow.
#
# kyber#377 (PR-C): the [1m] gate is now data-driven via
# KYBER_MODEL_CONTEXT_WINDOW (resolved by the controller from
# tokenreport.LimitFor → operator-editable map in PR-D). The hardcoded
# model-id `case` arm is gone; a brand-new 1M model can be adopted without
# editing this script. Family-alias compat-mapping stays — Claude Code's
# Max subscription path expects `sonnet`/`opus`/`haiku` for older IDs.
if [ -n "${CLAUDE_MODEL:-}" ]; then
    case "$CLAUDE_MODEL" in
        claude-sonnet-4|claude-sonnet-4-5) CLAUDE_MODEL="sonnet" ;;
        claude-opus-4|claude-opus-4-5)     CLAUDE_MODEL="opus" ;;
        claude-haiku-4-5*)                 CLAUDE_MODEL="haiku" ;;
    esac
    # Append [1m] iff the resolved context window is >= 1M tokens. The
    # alias forms (sonnet/opus/haiku) don't take [1m] — Claude Code maps
    # the alias to the highest-context concrete model for that family
    # already. Apply the suffix only to concrete IDs (i.e., when
    # CLAUDE_MODEL still contains the `claude-` prefix after the case).
    if [ "${KYBER_MODEL_CONTEXT_WINDOW:-0}" -ge 1000000 ] 2>/dev/null; then
        case "$CLAUDE_MODEL" in
            claude-*) CLAUDE_MODEL="${CLAUDE_MODEL}[1m]" ;;
        esac
    fi
    CLAUDE_ARGS="$CLAUDE_ARGS --model $CLAUDE_MODEL"
fi

# A configured startup prompt is the initial user turn for every new harness
# session. Quote it as one shell argument because the tmux command is a command
# string; never interpolate its raw contents into generated shell source.
CLAUDE_LAUNCH_ARGS="$CLAUDE_ARGS"
if [ -n "${KYBER_STARTUP_PROMPT:-}" ]; then
    CLAUDE_LAUNCH_ARGS="$CLAUDE_LAUNCH_ARGS -- $(printf '%q' "$KYBER_STARTUP_PROMPT")"
fi

# Resume launches (kyber#118) also deliver the startup prompt, as the first
# turn AFTER the restored conversation: a resumed session otherwise sits idle
# with no turn to act on, so an agent interrupted mid-task would never pick
# the task back up. --continue sits before the `--` separator because flags
# must precede the positional prompt.
CLAUDE_RESUME_ARGS="$CLAUDE_ARGS --continue"
if [ -n "${KYBER_STARTUP_PROMPT:-}" ]; then
    CLAUDE_RESUME_ARGS="$CLAUDE_RESUME_ARGS -- $(printf '%q' "$KYBER_STARTUP_PROMPT")"
fi

# Test-only early exit (kyber#377 / PR-C). Lets start_claude_test.go
# exercise the boot-prep block above (charset guard, install branch, [1m]
# gate) without spinning up tmux + claude. Gated behind an env var so
# production boot is unaffected.
if [ "${KYBER_BOOTPREP_DRY_RUN:-}" = "1" ]; then
    echo "[kyber] KYBER_BOOTPREP_DRY_RUN=1 — exiting after boot-prep (CLAUDE_MODEL=${CLAUDE_MODEL:-} CLAUDE_ARGS=${CLAUDE_ARGS:-} CC_INSTALL_OUTCOME=${KYBER_CC_INSTALL_OUTCOME:-})"
    exit 0
fi

# Token-usage reporter — isolated from the boot chain. `|| true` ensures a
# bug in the reporter never prevents Claude Code from starting.
#
# Env-var fallbacks:
#   - HOME: may be unset when start-claude runs under entrypoint.sh; default
#     to /home/kyber which is where Claude writes its JSONL transcripts.
#   - KYBER_CONTROL_PLANE_INTERNAL_URL: controller injects KYBER_REFRESH_TOKEN_URL
#     (…/internal/agents/{name}/refresh-token) but not the base URL yet. Derive
#     by stripping the suffix. Followup: add explicit base-URL env var to
#     pkg/controllers/agent/pod_builder.go so the reporter (and future
#     pod-side services) don't have to parse.
if [ -x /usr/local/bin/kyber-token-reporter ]; then
    mkdir -p /persist/var/log && chown kyber:kyber /persist/var/log 2>/dev/null || true
    export HOME="${HOME:-/home/kyber}"
    if [ -z "${KYBER_CONTROL_PLANE_INTERNAL_URL:-}" ] && [ -n "${KYBER_REFRESH_TOKEN_URL:-}" ]; then
        export KYBER_CONTROL_PLANE_INTERNAL_URL="${KYBER_REFRESH_TOKEN_URL%/internal/agents/*}"
    fi
    nohup /usr/local/bin/kyber-token-reporter \
        >> /persist/var/log/kyber-token-reporter.log 2>&1 &
    echo "[kyber] token-reporter launched (pid=$!) cp_url=${KYBER_CONTROL_PLANE_INTERNAL_URL:-unset} home=${HOME}"
else
    echo "[kyber] token-reporter binary not found — skipping"
fi

# ---- Runtime version reporting (kyber#175) ----
# Writes the installed Claude Code version to /var/run/kyber/runtime-version
# for in-pod consumers (future node-agent probes, debugging) and best-effort
# POSTs it to the control plane so Agent.status.runtime.installedVersion stays
# accurate. Non-blocking; any failure here never aborts startup.
RUNTIME_VERSION_DIR="/var/run/kyber"
RUNTIME_VERSION_FILE="${RUNTIME_VERSION_DIR}/runtime-version"
# Captured so the write below can be guarded on it. A failed redirection is a
# SHELL error printed before `|| true` can apply, so an unwritable
# /var/run/kyber would emit a bogus "No such file or directory" on a line that
# is deliberately best-effort. Same defect the Codex start script hit on HK-47's
# first boot (kyber#674); fixed here too rather than waiting for it to surface.
RUNTIME_VERSION_DIR_OK=false
if mkdir -p "$RUNTIME_VERSION_DIR" 2>/dev/null; then
    RUNTIME_VERSION_DIR_OK=true
fi

# `claude --version` emits "1.2.3 (Claude Code)". Take the first whitespace-
# separated token. Empty output → "unknown" so the reported value is always
# a parseable string.
CLAUDE_VERSION="$(claude --version 2>/dev/null | awk '{print $1; exit}' || true)"
if [ -z "$CLAUDE_VERSION" ]; then
    CLAUDE_VERSION="unknown"
    echo "[kyber] WARNING: claude --version returned empty output; reporting 'unknown'"
fi
if [ "$RUNTIME_VERSION_DIR_OK" = true ]; then
    printf '%s\n' "$CLAUDE_VERSION" > "$RUNTIME_VERSION_FILE" 2>/dev/null || true
fi
echo "[kyber] runtime version: $CLAUDE_VERSION (default=${KYBER_RUNTIME_DEFAULT_VERSION:-unset})"

# ---- Pre-flight model probe (kyber#379 / PR-E) --------------------------
#
# The actual claude session launches detached in tmux (line ~620), so its
# exit code is unobservable to the platform — a wrong/unsupported model
# becomes a silent failure (R2-D2 incident class). Run a bounded one-shot
# probe BEFORE the detached launch and capture the outcome for the
# extended runtime-report body below.
#
# Hard timeout (default 10s) prevents a network blip from blocking boot
# past a defined budget. The probe MUST NOT crash-loop the pod — failure
# is surfaced via the report, never by aborting startup. The timeout
# itself is reported as nil/unknown (not false) because a network blip
# is operationally distinct from a model-rejection error and shouldn't
# flip the PWA badge. Set KYBER_PROBE_TIMEOUT_SECONDS to override.
#
# --strict-mcp-config is load-bearing, not tidiness (kyber#678). Originally the
# channel plugins were MCP STDIO servers, so a probe that loaded MCP config
# spawned a SECOND Telegram bot — one that polled getUpdates, wrote bot.pid, and
# was never reaped, then raced the real session's poller ~4s later. Whichever
# side of that race a boot landed on decided whether the agent could hear
# Telegram at all.
#
# kyber#684 removed that plugin, and the sidecar it replaced it with is a
# separate CONTAINER reached over HTTP — nothing the probe could spawn. So the
# original failure mode is gone. Keep the flag anyway: it is still correct (the
# probe needs a model round-trip, never tools), it costs nothing, and it keeps
# this path immune to the next stdio MCP server anyone adds.
#
# The script reports the RAW probe outcome (exit code + combined
# stdout/stderr, sanitized) and the control plane classifies it via
# pkg/modelprobe — a unit-tested Go table instead of an in-image grep
# heuristic. Rationale (canary regression 2026-08-22): the CLI prints its
# model-rejection message to STDOUT, which the old probe discarded, and
# the current phrasing matched none of the old stderr patterns — so an
# invalid model reported "unknown" and the platform showed green.
#
# KYBER_MODEL_SUPPORTED (true|false|unknown) is still computed for
# mixed-version windows where the control plane predates the raw fields:
# exit 0 → true, timeout → unknown, other failures → false only on a
# clear rejection phrase in the COMBINED output (both streams).
KYBER_MODEL_SUPPORTED="unknown"
KYBER_MODEL_PROBE_RAN="0"
KYBER_MODEL_PROBE_EXIT=""
KYBER_MODEL_PROBE_OUTPUT=""
PROBE_TIMEOUT="${KYBER_PROBE_TIMEOUT_SECONDS:-10}"
if [ -n "${CLAUDE_MODEL:-}" ] && [ "${KYBER_SKIP_MODEL_PROBE:-}" != "1" ]; then
    echo "[kyber] pre-flight model probe: claude --model ${CLAUDE_MODEL} --strict-mcp-config --print 'ping' (timeout=${PROBE_TIMEOUT}s)"
    PROBE_OUT_FILE="$(mktemp 2>/dev/null || echo /tmp/kyber-probe-out.$$)"
    set +e
    timeout "${PROBE_TIMEOUT}" claude --model "$CLAUDE_MODEL" --strict-mcp-config --print 'ping' >"$PROBE_OUT_FILE" 2>&1
    PROBE_EXIT=$?
    set -e
    PROBE_OUTPUT="$(cat "$PROBE_OUT_FILE" 2>/dev/null || true)"
    rm -f "$PROBE_OUT_FILE" 2>/dev/null || true

    KYBER_MODEL_PROBE_RAN="1"
    KYBER_MODEL_PROBE_EXIT="$PROBE_EXIT"
    # JSON-safe payload: drop control chars, double quotes and
    # backslashes (lossy but diagnostic-grade), cap at 300 bytes.
    KYBER_MODEL_PROBE_OUTPUT="$(printf '%s' "$PROBE_OUTPUT" | tr -d '\000-\037"\\' | head -c 300)"

    if [ "$PROBE_EXIT" = "0" ]; then
        KYBER_MODEL_SUPPORTED="true"
        echo "[kyber] pre-flight model probe: ok"
    elif [ "$PROBE_EXIT" = "124" ]; then
        # `timeout(1)` returns 124 on hard timeout. Network blip vs real
        # rejection is indistinguishable here — legacy field stays
        # unknown; the raw report lets the control plane decide.
        echo "[kyber] pre-flight model probe: timed out after ${PROBE_TIMEOUT}s"
    else
        # Legacy-field heuristic only (the control plane re-classifies
        # from the raw fields with the authoritative pattern table).
        if printf '%s' "$PROBE_OUTPUT" | grep -Eqi 'issue with the selected model|(unsupported|invalid|unknown|not found|no such|does not (exist|support|recognize))[^A-Za-z]+model|model[^A-Za-z]+.*?(unsupported|invalid|unknown|not found|not available|may not exist|does not (exist|support|recognize))'; then
            KYBER_MODEL_SUPPORTED="false"
            echo "[kyber] pre-flight model probe: model rejected (exit=${PROBE_EXIT}); reporting ModelUnsupported"
        else
            echo "[kyber] pre-flight model probe: probe failed (exit=${PROBE_EXIT}) without clear model-rejection signal; control plane will classify from raw output"
        fi
    fi
else
    echo "[kyber] pre-flight model probe: skipped (CLAUDE_MODEL empty or KYBER_SKIP_MODEL_PROBE=1)"
fi
export KYBER_MODEL_SUPPORTED

# Translate the boot-time CC install outcome (captured upstream in the
# PR-C block as KYBER_CC_INSTALL_OUTCOME) into the PR-E report-body
# field requestedSatisfied. Operator semantics:
#   installed | skipped-equal → true  (pod runs what the operator asked for)
#   failed | rejected-*       → false (fell back to baked-in; operator should know)
#   not-requested             → unknown/absent (operator didn't ask for anything,
#                                so "satisfied" doesn't apply — the field stays
#                                out of the body so the controller doesn't flip
#                                a mismatch badge on the baked-in default)
KYBER_REQUESTED_SATISFIED="unknown"
case "${KYBER_CC_INSTALL_OUTCOME:-}" in
    installed|skipped-equal) KYBER_REQUESTED_SATISFIED="true" ;;
    failed|rejected-charset|rejected-length) KYBER_REQUESTED_SATISFIED="false" ;;
esac
export KYBER_REQUESTED_SATISFIED

# Derive CP URL if only the refresh-token URL was injected (same pattern as
# the token-reporter block above — duplicate deliberately so this block
# works even when the reporter binary is missing).
if [ -z "${KYBER_CONTROL_PLANE_INTERNAL_URL:-}" ] && [ -n "${KYBER_REFRESH_TOKEN_URL:-}" ]; then
    export KYBER_CONTROL_PLANE_INTERNAL_URL="${KYBER_REFRESH_TOKEN_URL%/internal/agents/*}"
fi
if [ -n "${AGENT_NAME:-}" ] && [ -n "${KYBER_CONTROL_PLANE_INTERNAL_URL:-}" ]; then
    REPORTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    # Build the extended PR-E body. Each PR-E field is included ONLY when
    # known — absent = "unknown" on the server, which leaves any
    # previously-reported value untouched (kyber#379 §Sidecar
    # coordination). Plain string concatenation is fine because every
    # value here is sourced from controlled inputs (KYBER_REQUESTED_CC_VERSION
    # was charset-validated upstream; KYBER_MODEL_SUPPORTED /
    # KYBER_REQUESTED_SATISFIED are set to one of three literal strings).
    BODY="{\"version\":\"${CLAUDE_VERSION}\",\"reportedAt\":\"${REPORTED_AT}\""
    if [ -n "${KYBER_REQUESTED_CC_VERSION:-}" ]; then
        BODY="${BODY},\"requestedVersion\":\"${KYBER_REQUESTED_CC_VERSION}\""
    fi
    if [ "$KYBER_REQUESTED_SATISFIED" = "true" ]; then
        BODY="${BODY},\"requestedSatisfied\":true"
    elif [ "$KYBER_REQUESTED_SATISFIED" = "false" ]; then
        BODY="${BODY},\"requestedSatisfied\":false"
    fi
    if [ "$KYBER_MODEL_SUPPORTED" = "true" ]; then
        BODY="${BODY},\"modelSupported\":true"
    elif [ "$KYBER_MODEL_SUPPORTED" = "false" ]; then
        BODY="${BODY},\"modelSupported\":false"
    fi
    # Raw probe outcome — authoritative on control planes that know the
    # fields; older ones ignore unknown JSON keys. KYBER_MODEL_PROBE_OUTPUT
    # was sanitized above (no quotes/backslashes/control chars), so plain
    # concatenation stays valid JSON.
    if [ "$KYBER_MODEL_PROBE_RAN" = "1" ]; then
        BODY="${BODY},\"modelProbeExit\":${KYBER_MODEL_PROBE_EXIT}"
        BODY="${BODY},\"modelProbeOutput\":\"${KYBER_MODEL_PROBE_OUTPUT}\""
    fi
    BODY="${BODY}}"
    URL="${KYBER_CONTROL_PLANE_INTERNAL_URL}/internal/agents/${AGENT_NAME}/runtime-version"
    CURL_ARGS=(
        -fsS --max-time 5 --retry 2
        -H "Content-Type: application/json"
        -X POST -d "$BODY"
    )
    POD_TOKEN_PATH="/var/run/secrets/kyber/pod-token"
    if [ -r "$POD_TOKEN_PATH" ]; then
        CURL_ARGS+=( -H "Authorization: Bearer $(cat "$POD_TOKEN_PATH")" )
    fi
    if curl "${CURL_ARGS[@]}" "$URL" >/dev/null 2>&1; then
        echo "[kyber] runtime version reported to control plane"
    else
        echo "[kyber] WARNING: failed to report runtime version (best-effort; continuing)"
    fi
else
    echo "[kyber] runtime version: skipping control-plane report (AGENT_NAME or CP URL unset)"
fi

# Test-only early exit (kyber#379 / PR-E). Lets the integration tests
# observe what the script REPORTS without spinning up tmux + claude.
# Gated behind an env var so production boot is unaffected.
if [ "${KYBER_SKIP_LAUNCH_AFTER_REPORT:-}" = "1" ]; then
    echo "[kyber] KYBER_SKIP_LAUNCH_AFTER_REPORT=1 — exiting after report POST"
    exit 0
fi

# ---- Launch in tmux ----
# Launch from the identity repo dir when one is configured + cloned so Claude
# Code's built-in CLAUDE.md walk-up finds the agent's identity on session
# start. Falls back to $HOME when there's no identity repo or the clone was
# skipped — never to "/" (which is wherever entrypoint.sh left us).
LAUNCH_DIR="${HOME:-/home/kyber}"
if [ -n "${REPO_DIR:-}" ] && [ -d "$REPO_DIR" ]; then
    LAUNCH_DIR="$REPO_DIR"
fi

# kyber#118: native session resume. When spec.sessionResume is enabled, the
# pod-boot and crash-relaunch paths launch with `--continue` so the harness
# picks up its previous conversation; an intentional restart-session passes
# --fresh to the generated relaunch script below and always starts clean.
# Resume launches use CLAUDE_RESUME_ARGS, which carries the startup prompt
# when one is configured — the prompt is what makes a resumed agent act on
# its restored context instead of idling (see CLAUDE_RESUME_ARGS above).
SESSION_RESUME_ENABLED=0
case "${KYBER_SESSION_RESUME:-}" in
    1|true|True|TRUE) SESSION_RESUME_ENABLED=1 ;;
esac

# Claude Code keys its transcript store on the launch cwd with every
# non-alphanumeric byte mapped to '-' (~/.claude/projects/-home-kyber-…).
# Resume only when that store already holds a transcript: `claude --continue`
# with no prior conversation for the cwd exits immediately, which the
# kyber#563 watchdog would read as a crash loop and take the pod down with it.
CLAUDE_PROJECT_STORE="${HOME:-/home/kyber}/.claude/projects/$(printf '%s' "$LAUNCH_DIR" | sed 's|[^A-Za-z0-9]|-|g')"
claude_has_prior_session() {
    [ -n "$(find "$CLAUDE_PROJECT_STORE" -maxdepth 1 -name '*.jsonl' -print -quit 2>/dev/null)" ]
}

# Claude Code's workspace-trust decision is keyed by the exact launch path in
# ~/.claude.json. The old boot path wrote trust for $PWD near the top of this
# script (normally "/"), before identity-repo setup resolved REPO_DIR, and then
# launched Claude from /home/kyber/dev/<repo>. Fresh agents therefore stopped
# at an interactive trust prompt despite Kyber owning and cloning that repo.
#
# Merge instead of replacing: Claude stores session/UI state alongside these
# keys on the durable root. Re-assert on every boot so existing PVCs missing the
# entry self-heal, while preserving every unrelated field Claude has written.
CLAUDE_STATE="${HOME:-/home/kyber}/.claude.json"
CLAUDE_STATE_TMP="${CLAUDE_STATE}.kyber-tmp"
# Claude owns this durable file and may leave it truncated after an abrupt pod
# stop. Preserve the bad bytes for diagnosis, then rebuild the minimum valid
# object so a UI-state file cannot permanently crash-loop the runtime.
if ! jq empty "$CLAUDE_STATE" >/dev/null 2>&1; then
    CLAUDE_STATE_CORRUPT="${CLAUDE_STATE}.corrupt"
    cp "$CLAUDE_STATE" "$CLAUDE_STATE_CORRUPT" 2>/dev/null || true
    printf '{}\n' > "$CLAUDE_STATE"
    echo "[kyber] WARNING: recovered invalid $CLAUDE_STATE (saved as $CLAUDE_STATE_CORRUPT)" >&2
fi
if ! jq --arg launch_dir "$LAUNCH_DIR" '
    .hasCompletedOnboarding = true
    | .projects = (.projects // {})
    | .projects[$launch_dir] = ((.projects[$launch_dir] // {}) + {
        hasTrustDialogAccepted: true,
        hasCompletedProjectOnboarding: true
      })
  ' "$CLAUDE_STATE" > "$CLAUDE_STATE_TMP"; then
    rm -f "$CLAUDE_STATE_TMP"
    echo "[kyber] FATAL: could not trust Claude Code launch directory $LAUNCH_DIR" >&2
    exit 2
fi
mv "$CLAUDE_STATE_TMP" "$CLAUDE_STATE"
chmod 600 "$CLAUDE_STATE"
if [ "$(id -u)" -eq 0 ]; then
    chown kyber:kyber "$CLAUDE_STATE"
fi
echo "[kyber] Claude Code workspace trusted: $LAUNCH_DIR"

# Dump a re-runnable launch script so POST /restart-session (#128) can
# kill-tmux + relaunch in place without rolling the pod. The heredoc is
# unquoted so $LAUNCH_DIR and $CLAUDE_ARGS expand to their resolved values
# at boot — the dumped script does not depend on this shell's environment.
# Escape (\$) any variables that should stay as literal references for the
# generated script to evaluate at restart time.

# Operator-uploaded user secrets (#75) are projected into the container
# env via envFrom with prefix USER_. They MUST survive the user-switch
# in the launch script below — without this, a normal restart-session
# strips things like USER_GITHUB_PAT (Han 2026-05-08) and the agent
# loses credentials it had at boot. Enumerate USER_* keys present at
# this shell's runtime (= pod-boot time, since envFrom is evaluated
# there) and bake the resolved list into the --preserve-env= allowlist
# below. Empty when no user secrets are configured — the suffix is then
# empty and the line behaves identically to before.
USER_ENV_KEYS=$(env | awk -F= '/^USER_[A-Z]/ { print $1 }' | sort | tr '\n' ',' | sed 's/,$//')
USER_PRESERVE_SUFFIX=""
if [ -n "$USER_ENV_KEYS" ]; then
    USER_PRESERVE_SUFFIX=",${USER_ENV_KEYS}"
fi

mkdir -p /persist/var/lock
cat > /persist/last-claude-launch.sh <<LAUNCH_SH
#!/bin/bash
# Generated by start-claude.sh at boot — do not edit.
# Kill the tmux "agent" session and re-launch Claude Code with the same
# args used at boot. Safe to re-run; the API handler for #128 calls this.

set -u

# kyber#118: --fresh (the restart-session API's argv passes it) forces a
# fresh session. Without it — the kyber#563 crash watchdog calls this script
# bare — resume applies when enabled and a prior transcript exists.
KYBER_FRESH=0
[ "\${1:-}" = "--fresh" ] && KYBER_FRESH=1

SESSION_LOCK="/persist/var/lock/session.lock"
mkdir -p "\$(dirname "\$SESSION_LOCK")"

(
    # flock -w 30: wait up to 30s for the session lock. Cron jobs from #135
    # that fire during this window will flock-fail and record
    # status=skipped_restart_in_progress instead of dispatching.
    flock -w 30 200 || { echo "[kyber] restart-session: could not acquire session.lock within 30s" >&2; exit 1; }

    # kyber#118: the bare (crash-watchdog) invocation only exists to revive a
    # DEAD session. If a session is alive by the time we hold the lock, a
    # concurrent restart-session already relaunched — proceeding would kill
    # that fresh session and, with resume enabled, could resurrect the very
    # conversation the intentional restart just discarded. --fresh (the API
    # path) still always kills + relaunches; that is its purpose.
    if [ "\$KYBER_FRESH" = "0" ] && sudo -iu kyber tmux has-session -t agent 2>/dev/null; then
        echo "[kyber] relaunch: session already alive — a concurrent restart won the race, nothing to do (kyber#118)"
        exit 0
    fi

    # Kill existing session. Ignore error — the agent may have crashed or
    # never started.
    sudo -iu kyber tmux kill-session -t agent 2>/dev/null || true

    # Ensure the Telegram bot process is fully gone before clearing bot.pid.
    # tmux kill-session sends SIGHUP to the session's process tree, but
    # propagation is async — bun server.ts may outlive the kill briefly.
    # A surviving bun process + a cleared bot.pid → two bun instances on
    # relaunch, and Telegram delivers updates to whichever wins the long-poll.
    pkill -f "bun.*server\.ts" 2>/dev/null || true

    # Clear stale bot.pid so the Telegram plugin re-initializes on the new
    # session. The plugin (v0.0.6) fails to reconnect when this file is
    # present pointing at a dead process. start-claude.sh clears it on pod
    # boot, but that code does not re-run on a session restart — hence here.
    rm -f ${HOME:-/home/kyber}/.claude/channels/telegram/bot.pid

    # Sync identity-repo code + skills BEFORE relaunch (kyber#596) — the session
    # is already dead here, so this is race-free. This is what makes a
    # restart-session (and the nightly falcon-session-reset that calls it) pick
    # up new skills/contract, not just clear context. Fail-soft: a sync error
    # never blocks the relaunch (the agent comes back on the existing tree).
    # NOTE: this whole block lives inside the UNQUOTED <<LAUNCH_SH heredoc, so
    # anything unescaped is expanded now, at boot, and baked into the generated
    # script. That is deliberate here: boot already resolved where it wrote the
    # sync script (/persist normally, $HOME when this pod has none), so we bake
    # that literal path in rather than re-deriving it at restart time — where
    # KYBER_SYNC_SCRIPT would not be in the environment anyway (the relaunch runs
    # under nsenter with PID 1's env). Do not add unescaped runtime variables here.
    if [ -x "${KYBER_SYNC_SCRIPT:-/persist/kyber-sync-identity.sh}" ]; then
        "${KYBER_SYNC_SCRIPT:-/persist/kyber-sync-identity.sh}" || echo "[kyber] restart-session: identity sync failed (relaunching on existing tree)"
    fi

    # Re-launch with the exact args resolved at boot.
    # --preserve-env keeps the container env (TELEGRAM_BOT_TOKEN, CLAUDE_*
    # OAuth tokens, ANTHROPIC_API_KEY, AGENT_NAME, KYBER_*, plus any
    # operator-uploaded USER_* secrets — see USER_PRESERVE_SUFFIX above)
    # across the user-switch. Plain "sudo -i" strips them (initial-login
    # env reset), which breaks Telegram and OAuth on the relaunch — the
    # new claude process starts with no token, the plugin can't
    # authenticate, and the bot is silently never spawned. Explicit
    # allowlist avoids relying on sudoers env_keep policy and documents
    # what crosses the boundary.
    #
    # HOME=/home/kyber is set INLINE (not preserved) because the parent
    # process (the dumped script under nsenter --target 1) inherits PID 1's
    # HOME, which is /root. Preserving that would make claude look for
    # /root/.claude.json, miss the onboarding bypass written by
    # start-claude.sh at /home/kyber/.claude.json, and fall into the
    # interactive theme picker on every restart-session.
    #
    # kyber#118: decide fresh-vs-resume at RUN time, not boot time — the
    # store may have gained its first transcript since boot, and --fresh
    # must always win. The enable flag, store path, and arg strings are
    # baked at boot (this heredoc is unquoted); only the decision runs here.
    RELAUNCH_CMD="claude $CLAUDE_LAUNCH_ARGS"
    if [ "$SESSION_RESUME_ENABLED" = "1" ] && [ "\$KYBER_FRESH" = "0" ] \\
       && [ -n "\$(find "$CLAUDE_PROJECT_STORE" -maxdepth 1 -name '*.jsonl' -print -quit 2>/dev/null)" ]; then
        RELAUNCH_CMD="claude $CLAUDE_RESUME_ARGS"
        echo "[kyber] relaunch: resuming previous session (kyber#118)"
    fi
    sudo HOME=/home/kyber --preserve-env=TELEGRAM_BOT_TOKEN,ANTHROPIC_API_KEY,CLAUDE_MODEL,CLAUDE_ACCESS_TOKEN,CLAUDE_REFRESH_TOKEN,CLAUDE_ACCESS_TOKEN_EXPIRES_AT,AGENT_NAME,KYBER_CONTROL_PLANE_INTERNAL_URL,KYBER_REFRESH_TOKEN_URL,KYBER_IDENTITY_REPO,KYBER_RUNTIME_DEFAULT_VERSION,TZ${USER_PRESERVE_SUFFIX} -u kyber tmux new-session -d -s agent -c "$LAUNCH_DIR" "\$RELAUNCH_CMD"

    echo "[kyber] restart-session: tmux 'agent' session restarted"
) 200>"\$SESSION_LOCK"
LAUNCH_SH
chmod 0755 /persist/last-claude-launch.sh

# Sweep any Telegram poller left over from earlier in THIS boot before the
# real session starts (kyber#678). The equivalent clear at the top of this
# script runs ~1000 lines earlier, so it can only ever catch a previous pod's
# pid — never one created since. --strict-mcp-config on the pre-flight probe
# should mean there is nothing here to sweep; this is the net under it, and it
# mirrors the restart-session path above so both launches share one shape.
if [ -n "${TELEGRAM_BOT_TOKEN:-}" ]; then
    pkill -f "bun.*server\.ts" 2>/dev/null || true
    rm -f "${HOME:-/home/kyber}/.claude/channels/telegram/bot.pid"
fi

BOOT_LAUNCH_CMD="claude $CLAUDE_LAUNCH_ARGS"
if [ "$SESSION_RESUME_ENABLED" = "1" ]; then
    if claude_has_prior_session; then
        # kyber#118: pod boot after a recreate/preemption/crash — pick the
        # previous conversation back up instead of starting fresh.
        BOOT_LAUNCH_CMD="claude $CLAUDE_RESUME_ARGS"
        echo "[kyber] session resume: continuing previous session (kyber#118)"
    else
        # Deliberately loud: the store path replicates Claude Code's
        # cwd-munging, and if a CC version changes that scheme this branch
        # is the only signal that resume silently stopped engaging.
        echo "[kyber] session resume: enabled but no prior transcript at $CLAUDE_PROJECT_STORE — starting fresh (kyber#118)"
    fi
fi
echo "[kyber] Starting Claude Code in tmux (cwd=$LAUNCH_DIR)"
tmux new-session -d -s agent -c "$LAUNCH_DIR" "$BOOT_LAUNCH_CMD"

echo "[kyber] tmux session 'agent' started — waiting"

# kyber#563: relaunch on session death instead of exiting. The old tail exited 0
# the moment the session ended, leaving the container Completed and NEVER
# restarted — a raw `tmux kill-session` (the restart-agent debug helper), a
# crash, or an OOM all kill the session. Re-run the dumped launch script (same
# env + args as boot) to bring the agent back in place. A crash-loop guard exits
# the script if the session keeps dying within seconds, so a genuinely broken
# agent falls through to a controller-driven pod recreation rather than spinning.
relaunch_count=0
while true; do
    session_started=$(date +%s 2>/dev/null || echo 0)
    while tmux has-session -t agent 2>/dev/null; do
        sleep 5
    done
    now=$(date +%s 2>/dev/null || echo 0)
    ran_for=$(( now - session_started ))
    # A session that ran healthily (>=60s) before dying is not a crash loop.
    if [ "$ran_for" -ge 60 ]; then
        relaunch_count=0
    fi
    relaunch_count=$((relaunch_count + 1))
    if [ "$relaunch_count" -gt 5 ]; then
        echo "[kyber] agent session died ${relaunch_count}x within seconds — exiting so the controller recreates the pod (kyber#563)" >&2
        exit 1
    fi
    echo "[kyber] tmux session 'agent' ended (ran ${ran_for}s) — relaunching #${relaunch_count} (kyber#563)"
    if [ -x /persist/last-claude-launch.sh ]; then
        RELAUNCH_FLAG=""
        if [ "$relaunch_count" -ge 3 ] && [ "$SESSION_RESUME_ENABLED" = "1" ]; then
            # kyber#118: three fast deaths in a row with resume on — the
            # resumed transcript itself may be the poison (e.g. corrupted
            # by a hard kill mid-write). Drop to a fresh session before the
            # crash-loop guard above gives up the whole pod.
            echo "[kyber] session resume: ${relaunch_count} fast deaths — falling back to a fresh session (kyber#118)"
            RELAUNCH_FLAG="--fresh"
        fi
        /persist/last-claude-launch.sh $RELAUNCH_FLAG || echo "[kyber] relaunch script returned non-zero — retrying" >&2
    else
        tmux new-session -d -s agent -c "$LAUNCH_DIR" "claude $CLAUDE_LAUNCH_ARGS" || true
    fi
    sleep 3
done
