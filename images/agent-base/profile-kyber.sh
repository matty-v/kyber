# Kyber agent debug helpers — sourced by login shells via /etc/profile.d
# Available to root and to the kyber user. Aliases assume sudo is available
# (the kyber user has passwordless sudo per agent-base Dockerfile).

alias agent='sudo nsenter --target 1 --mount --uts --ipc --net --pid --root --wd -- runuser -u kyber -- tmux -u attach -t agent'
alias peek='sudo nsenter --target 1 --mount --uts --ipc --net --pid --root --wd -- runuser -u kyber -- tmux -u attach -t agent -r'
alias as-agent='sudo nsenter --target 1 --mount --uts --ipc --net --pid --root --wd -- runuser -u kyber -- bash -l'
creds() {
    if [ -f /home/kyber/.codex/auth.json ]; then
        echo "Codex credentials are installed (contents are intentionally hidden)."
    elif [ -f /home/kyber/.claude/.credentials.json ]; then
        sudo jq '{claudeAiOauth: (.claudeAiOauth | keys)}' /home/kyber/.claude/.credentials.json
    else
        echo "No runtime credential file found."
    fi
}
tg() {
    if curl -fsS http://127.0.0.1:14003/healthz >/dev/null 2>&1; then
        echo "Codex Telegram sidecar is healthy."
    elif [ -f /home/kyber/.claude/channels/telegram/access.json ]; then
        sudo jq . /home/kyber/.claude/channels/telegram/access.json
    else
        echo "Telegram is not configured for this agent."
    fi
}
alias boot='tail -n 200 /persist/kyber-bootstrap.log'
alias tokens='tail -f /persist/var/log/kyber-*-reporter.log'
alias cron-log='tail -f /persist/var/log/kyber-jobs.log'
alias jobs='cat /kyber/jobs-src/crontab 2>/dev/null || echo "(no jobs configured — empty spec.jobs)"'
alias restart-agent='sudo nsenter --target 1 --mount --uts --ipc --net --pid --root --wd -- runuser -u kyber -- tmux kill-session -t agent'

# MOTD — printed on every login shell entry (one print per Shell tab open).
cat <<'KYBER_MOTD'

[Kyber agent shell — debug helpers]

  agent          attach to the agent runtime's tmux session (read/write)
  peek           attach read-only — safer if the agent is mid-task
  as-agent       become the kyber user (where the agent runs)
  creds          confirm runtime credentials without exposing secret values
  tg             show Telegram channel health/access configuration
  boot           tail runtime bootstrap log
  tokens         tail token-reporter log
  cron-log       tail /persist/var/log/kyber-jobs.log (from #135)
  jobs           show the rendered agent-jobs crontab (from #135)
  restart-agent  kill the tmux session — pod will restart cleanly

Pod runs the selected agent runtime as the 'kyber' user in tmux session 'agent'.
The Shell tab lands you as root for debugging; switch to the agent with `as-agent`.

KYBER_MOTD
