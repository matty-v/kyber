FROM ubuntu:26.04

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
    attr \
    cron \
    && rm -rf /var/lib/apt/lists/*

# Create a non-root user for the agent runtime. Claude Code v2.x refuses
# --dangerously-skip-permissions when running as root ("cannot be used with
# root/sudo privileges for security reasons"). The kyber user has passwordless
# sudo so it can still install packages if needed.
RUN useradd -m -s /bin/bash kyber \
    && echo "kyber ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/kyber

# Entrypoint (runs as root for mount privileges, drops to kyber user).
# fuse3/fuse-overlayfs are deliberately absent: under a user namespace
# /dev/fuse cannot be delivered to the pod at all, so the FUSE stack is dead
# weight that only makes the persistence story look more capable than it is.
# See docs/adr/0003-agent-sandbox-isolation.md.
COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

# Durable-root manager (kyber#78) — seeds /persist/agentroot from this image,
# migrates legacy overlay upper layers into it, and three-way-merges a new base
# image into a root the agent has been editing. `attr` above provides getfattr,
# which the migration path needs to read overlay opaque-directory markers.
COPY --chmod=0755 scripts/kyber-rootfs /usr/local/bin/kyber-rootfs

# Debug helpers — sourced by every login shell (root or kyber user).
COPY --chmod=0644 profile-kyber.sh /etc/profile.d/kyber.sh

# System tmux config (#122) — enables mouse-wheel copy-mode and bumps
# scrollback. Per-agent overrides still possible via ~/.tmux.conf.
COPY --chmod=0644 tmux.conf /etc/tmux.conf

# Agent jobs dispatcher (#135) — invoked per-fire from /etc/cron.d/kyber-jobs
# to deliver a scheduled prompt into the agent's tmux session. See
# scripts/kyber-job-dispatch for contract and failure modes.
COPY --chmod=0755 scripts/kyber-job-dispatch /usr/local/bin/kyber-job-dispatch

# Shared tmux delivery helper, sourced by kyber-job-dispatch and
# kyber-compact-session. Must land in the same directory as both — they
# resolve it relative to their own path. Not executable: it is a library.
COPY --chmod=0644 scripts/kyber-tmux-paste.sh /usr/local/bin/kyber-tmux-paste.sh

# Session compaction (kyber#TBD) — asks the running runtime to compact its
# own context in place. Exec'd by the control-plane over SPDY via
# Adapter.CompactSessionCommand(); see scripts/kyber-compact-session.
COPY --chmod=0755 scripts/kyber-compact-session /usr/local/bin/kyber-compact-session

# Platform-owned Stop hook for scheduled jobs — clears the agent's context when
# a job declared clearContextAfter, and releases the --exclusive agent-busy
# guard by removing the job's pending marker. Registered into the runtime's
# settings by its start script; see scripts/kyber-cron-postrun.
COPY --chmod=0755 scripts/kyber-cron-postrun /usr/local/bin/kyber-cron-postrun

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
# Agent controller overrides CMD with args: [/usr/local/bin/start-claude.sh] on
# every claude-code pod — this placeholder is used only for bare-container dev/test.
CMD ["/bin/bash"]
