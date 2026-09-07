#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME:-/home/kyber}"
export HERMES_HOME="${HERMES_HOME:-$HOME/.hermes}"
export HERMES_PROVIDER="${HERMES_PROVIDER:-openrouter}"
export HERMES_YOLO_MODE="${HERMES_YOLO_MODE:-1}"
export HERMES_ACCEPT_HOOKS="${HERMES_ACCEPT_HOOKS:-1}"
export HERMES_DISABLE_LAZY_INSTALLS="${HERMES_DISABLE_LAZY_INSTALLS:-1}"
PERSIST_ROOT="${KYBER_PERSIST_ROOT:-/persist}"

mkdir -p "$HERMES_HOME" "$PERSIST_ROOT/var/log" "$PERSIST_ROOT/var/lock"
chmod 0700 "$HERMES_HOME"

if [ -z "${OPENROUTER_API_KEY:-}" ]; then
    echo "[kyber] Hermes OpenRouter credential is missing" >&2
    exit 42
fi

if _runtime_probe_output="$(hermes --version 2>&1)"; then
    _runtime_probe_status=0
else
    _runtime_probe_status=$?
fi
HERMES_VERSION="$(printf '%s\n' "$_runtime_probe_output" | grep -Eo '[0-9]+\.[0-9]+\.[0-9]+([0-9A-Za-z.+-]*)?' | head -1 || true)"
if [ "$_runtime_probe_status" -ne 0 ] || [ -z "$HERMES_VERSION" ]; then
    _runtime_probe_message="$(printf '%s' "${_runtime_probe_output:-hermes executable produced no version}" | tr '\n\r\t' '   ' | tr -cd '[:print:]' | cut -c1-300)"
    echo "[kyber] FATAL: Hermes runtime probe failed: $_runtime_probe_message" >&2
    exit 43
fi
unset _runtime_probe_output _runtime_probe_status _runtime_probe_message

if mkdir -p /var/run/kyber 2>/dev/null; then
    printf '%s\n' "$HERMES_VERSION" > /var/run/kyber/runtime-version 2>/dev/null || true
fi
curl -fsS --max-time 5 --retry 5 --retry-connrefused -H 'Content-Type: application/json' -X POST \
    -d "{\"version\":\"${HERMES_VERSION}\",\"runtime\":\"hermes\",\"usable\":true}" \
    http://127.0.0.1:8091/runtime-version >/dev/null 2>&1 || true

HERMES_CONFIGURATOR="${KYBER_HERMES_CONFIGURATOR:-/usr/local/bin/kyber-configure-hermes}"
if ! "$HERMES_CONFIGURATOR"; then
    echo "[kyber] FATAL: Hermes managed configuration could not be written" >&2
    exit 43
fi

_kyber_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
_kyber_identity_script="${KYBER_IDENTITY_REPO_SCRIPT:-}"
if [ -z "$_kyber_identity_script" ]; then
    for _candidate in "${_kyber_dir}/kyber-identity-repo.sh" "${_kyber_dir}/../shared/kyber-identity-repo.sh"; do
        [ -r "$_candidate" ] && { _kyber_identity_script="$_candidate"; break; }
    done
fi
if [ -r "$_kyber_identity_script" ]; then
    . "$_kyber_identity_script"
else
    echo "[kyber] WARNING: shared identity-repo script not found; skipping identity repo setup" >&2
fi

LAUNCH_DIR="$HOME"
if [ -n "${KYBER_IDENTITY_REPO:-}" ]; then
    candidate="$HOME/dev/${KYBER_IDENTITY_REPO##*/}"
    if [ -d "$candidate/.git" ]; then
        LAUNCH_DIR="$candidate"
    fi
fi
if [ -r /opt/kyber/KYBER.md ]; then
    mkdir -p "$LAUNCH_DIR/.runtime"
    cp /opt/kyber/KYBER.md "$LAUNCH_DIR/.runtime/KYBER.md"
fi

if command -v kyber-skills >/dev/null 2>&1; then
    nohup kyber-skills report --repo-dir "${REPO_DIR:-}" --home "$HOME" \
        >> "$PERSIST_ROOT/var/log/kyber-skills.log" 2>&1 &
fi

HERMES_ARGS=(--cli --yolo --accept-hooks chat --provider "$HERMES_PROVIDER")
if [ -n "${HERMES_INFERENCE_MODEL:-}" ]; then
    HERMES_ARGS+=(--model "$HERMES_INFERENCE_MODEL")
fi
if [ -n "${KYBER_STARTUP_PROMPT:-}" ]; then
    HERMES_ARGS+=(-q "$KYBER_STARTUP_PROMPT")
fi
HERMES_LAUNCH_CMD="hermes $(printf '%q ' "${HERMES_ARGS[@]}")"

HERMES_RESUME_ARGS=("${HERMES_ARGS[@]}")
HERMES_RESUME_ARGS+=(--continue)
HERMES_RESUME_CMD="hermes $(printf '%q ' "${HERMES_RESUME_ARGS[@]}")"

SESSION_RESUME_ENABLED=0
case "${KYBER_SESSION_RESUME:-}" in
    1|true|True|TRUE) SESSION_RESUME_ENABLED=1 ;;
esac
hermes_has_prior_session() {
    [ -s "$HERMES_HOME/state.db" ]
}

cat > "$PERSIST_ROOT/last-hermes-launch.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
KYBER_FRESH=0
[ "\${1:-}" = "--fresh" ] && KYBER_FRESH=1
exec 9>"$PERSIST_ROOT/var/lock/session.lock"
flock -x 9
if [ "\$(id -u)" -eq 0 ]; then
    TMUX=(runuser -u kyber -- tmux)
else
    TMUX=(tmux)
fi
if [ "\$KYBER_FRESH" = "0" ] && "\${TMUX[@]}" has-session -t agent 2>/dev/null; then
    exit 0
fi
"\${TMUX[@]}" kill-session -t agent 2>/dev/null || true
RELAUNCH_CMD=$(printf '%q' "$HERMES_LAUNCH_CMD")
if [ "$SESSION_RESUME_ENABLED" = "1" ] && [ "\$KYBER_FRESH" = "0" ] && [ -s $(printf '%q' "$HERMES_HOME/state.db") ]; then
    RELAUNCH_CMD=$(printf '%q' "$HERMES_RESUME_CMD")
fi
"\${TMUX[@]}" new-session -d -s agent -c $(printf '%q' "$LAUNCH_DIR") "\$RELAUNCH_CMD" 9>&-
EOF
chmod 0755 "$PERSIST_ROOT/last-hermes-launch.sh"

if [ -n "${SKIP_HERMES_LAUNCH:-}" ]; then
    echo "[kyber] SKIP_HERMES_LAUNCH set — boot path complete, not launching"
    exit 0
fi

BOOT_LAUNCH_CMD="$HERMES_LAUNCH_CMD"
if [ "$SESSION_RESUME_ENABLED" = "1" ] && hermes_has_prior_session; then
    BOOT_LAUNCH_CMD="$HERMES_RESUME_CMD"
    echo "[kyber] session resume: continuing previous Hermes session"
fi

echo "[kyber] Starting Hermes ${HERMES_VERSION} in tmux (cwd=$LAUNCH_DIR)"
tmux new-session -d -s agent -c "$LAUNCH_DIR" "$BOOT_LAUNCH_CMD"

relaunch_count=0
DISK_EXHAUSTED_MARKER=/var/run/kyber/disk-exhausted
while true; do
    session_started="$(date +%s 2>/dev/null || echo 0)"
    while tmux has-session -t agent 2>/dev/null; do
        if [ -f "$DISK_EXHAUSTED_MARKER" ]; then
            tmux kill-session -t agent 2>/dev/null || true
            break
        fi
        sleep 5
    done
    if [ -f "$DISK_EXHAUSTED_MARKER" ]; then
        while [ -f "$DISK_EXHAUSTED_MARKER" ]; do sleep 5; done
        relaunch_count=0
    fi
    now="$(date +%s 2>/dev/null || echo 0)"
    if [ $((now - session_started)) -ge 60 ]; then relaunch_count=0; fi
    relaunch_count=$((relaunch_count + 1))
    if [ "$relaunch_count" -ge 3 ]; then
        echo "[kyber] Hermes exited repeatedly; stopping watchdog" >&2
        exit 1
    fi
    sleep 2
    "$PERSIST_ROOT/last-hermes-launch.sh" || true
done
