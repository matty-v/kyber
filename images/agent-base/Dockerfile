FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Core tools every agent needs.
# Retry apt-get update up to 3 times — Ubuntu mirrors occasionally return stale
# index files during mirror syncs, causing transient "File has unexpected size" errors.
RUN for i in 1 2 3; do apt-get update && break || { echo "apt-get update failed (attempt $i/3), retrying..."; sleep 5; }; done && \
    apt-get install -y --no-install-recommends \
    curl \
    git \
    sudo \
    ca-certificates \
    jq \
    tmux \
    dbus \
    gnome-keyring \
    libsecret-1-0 \
    fuse3 \
    fuse-overlayfs \
    cron \
    && rm -rf /var/lib/apt/lists/*

# Create a non-root user for the agent runtime. Claude Code v2.x refuses
# --dangerously-skip-permissions when running as root ("cannot be used with
# root/sudo privileges for security reasons"). The kyber user has passwordless
# sudo so it can still install packages if needed.
RUN useradd -m -s /bin/bash kyber \
    && echo "kyber ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/kyber

# Overlay entrypoint (runs as root for mount privileges, drops to kyber user)
COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

# Debug helpers — sourced by every login shell (root or kyber user).
COPY --chmod=0644 profile-kyber.sh /etc/profile.d/kyber.sh

# System tmux config (#122) — enables mouse-wheel copy-mode and bumps
# scrollback. Per-agent overrides still possible via ~/.tmux.conf.
COPY --chmod=0644 tmux.conf /etc/tmux.conf

# Agent jobs dispatcher (#135) — invoked per-fire from /etc/cron.d/kyber-jobs
# to deliver a scheduled prompt into the agent's tmux session. See
# scripts/kyber-job-dispatch for contract and failure modes.
COPY --chmod=0755 scripts/kyber-job-dispatch /usr/local/bin/kyber-job-dispatch

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
# Agent controller overrides CMD with args: [/usr/local/bin/start-claude.sh] on
# every claude-code pod — this placeholder is used only for bare-container dev/test.
CMD ["/bin/bash"]
