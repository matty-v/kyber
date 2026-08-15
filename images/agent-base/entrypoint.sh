#!/bin/bash
set -euo pipefail

PERSIST_DIR="/persist"
UPPER_DIR="$PERSIST_DIR/overlay/upper"
WORK_DIR="$PERSIST_DIR/overlay/work"
MERGED_DIR="/merged"

# ---- Three-tier overlay dispatcher ----
#
# Security context (kyber#76): this runs de-privileged by default — NOT full
# Privileged. The pod retains CAP_SYS_ADMIN (for the overlay/bind mount(2) and
# chroot) plus the /dev/fuse device; that is all fuse-overlayfs needs. Every
# mount/chroot below therefore still works, but host block devices are absent, so
# the tier-1 kernel-overlay and tier-2 fuse paths behave exactly as before while
# the pod can no longer reach the node. When user namespaces are enabled
# (agent.security.userNamespaces), the same operations run against the pod's
# namespaced root. Do not reintroduce a `Privileged: true` assumption here.
#
# Attempts mounts in priority order:
#   1. Kernel overlayfs — fastest, but fails when the container root is
#      already on overlayfs (k3s/containerd nested-overlay case; kernel
#      rejects with "cannot mount overlay read-only").
#   2. fuse-overlayfs — userspace overlay, works on top of any FS
#      including another overlay. Requires /dev/fuse exposed to the pod
#      (added in commit f7d5798) and the fuse-overlayfs binary (added
#      in commit 4090a33). No kernel restriction on nesting.
#   3. Bind-mount HOME — last resort. System-level installs (apt,
#      /usr/lib) do NOT persist, but the agent user's HOME directory
#      (and everything under $PERSIST_DIR) does — sufficient for Claude
#      Code state.
#
# Each attempt logs its outcome to /persist/overlay-mount.log so we can
# diagnose failures in the field. The kernel-overlay error was previously
# silenced with 2>/dev/null — that's gone now.
mkdir -p "$PERSIST_DIR"
echo "=== boot $(date -Iseconds) ===" >> "$PERSIST_DIR/overlay-mount.log"

try_kernel_overlay() {
    mkdir -p "$UPPER_DIR" "$WORK_DIR" "$MERGED_DIR"
    if mount -t overlay overlay \
        -o "lowerdir=/,upperdir=$UPPER_DIR,workdir=$WORK_DIR" \
        "$MERGED_DIR" 2>>"$PERSIST_DIR/overlay-mount.log"; then
        return 0
    fi
    return 1
}

try_fuse_overlay() {
    mkdir -p "$UPPER_DIR" "$WORK_DIR" "$MERGED_DIR"
    if [ ! -c /dev/fuse ]; then
        echo "[kyber] /dev/fuse not available — skipping fuse-overlayfs" \
            >> "$PERSIST_DIR/overlay-mount.log"
        return 1
    fi
    if fuse-overlayfs -o "lowerdir=/,upperdir=$UPPER_DIR,workdir=$WORK_DIR" \
        "$MERGED_DIR" 2>>"$PERSIST_DIR/overlay-mount.log"; then
        return 0
    fi
    return 1
}

# Detect whether we are running inside a Linux user namespace (kyber#76 Phase 2,
# agent.security.userNamespaces). uid_map line "0 <host> <count>": a non-zero
# host base means in-pod root is remapped, i.e. we are in a user namespace.
in_user_namespace() {
    [ -r /proc/self/uid_map ] || return 1
    awk 'NR==1 { exit !($1 == 0 && $2 != 0) }' /proc/self/uid_map
}

# Prefer fuse-overlayfs over kernel overlayfs when in a user namespace — but
# keep kernel overlayfs as a fallback, never skip it. Rationale (kyber#76 Phase
# 2, validated on k3d/linuxkit): with root remapped, kernel overlayfs restricts
# redirect_dir/metacopy, so a dpkg install that renames a directory across the
# copy-up boundary can fail with EXDEV ("Invalid cross-device link") — e.g.
# `apt-get install figlet` (ships /etc/emacs). fuse-overlayfs implements rename
# correctly, so it is tried first under userns. HOWEVER, unprivileged FUSE mounts
# are not available on every host (opening /dev/fuse inside a user namespace
# returns EPERM where the kernel/fusermount setup does not permit it), so when
# fuse-overlayfs cannot mount we fall back to kernel overlayfs — which still
# persists whole-/ and handles the large majority of installs — rather than
# collapsing to bind-mount-HOME. Outside userns, kernel overlayfs stays the fast
# default. See the pod-isolation design doc's Phase-2 validation notes.
PREFER_FUSE=false
if in_user_namespace; then
    PREFER_FUSE=true
    echo "[kyber] user namespace detected — trying fuse-overlayfs first, kernel overlayfs as fallback" \
        >> "$PERSIST_DIR/overlay-mount.log"
fi

OVERLAY_MODE=""
USE_OVERLAY=true

if mountpoint -q "$MERGED_DIR" 2>/dev/null; then
    echo "[kyber] Overlay already mounted — skipping"
    OVERLAY_MODE="already-mounted"
elif [ "$PREFER_FUSE" = true ] && try_fuse_overlay; then
    echo "[kyber] Overlay mounted (fuse-overlayfs, userns-preferred)"
    OVERLAY_MODE="fuse"
elif try_kernel_overlay; then
    echo "[kyber] Overlay mounted (kernel overlayfs)"
    OVERLAY_MODE="kernel"
elif [ "$PREFER_FUSE" != true ] && try_fuse_overlay; then
    echo "[kyber] Overlay mounted (fuse-overlayfs)"
    OVERLAY_MODE="fuse"
else
    echo "[kyber] Both kernel + fuse overlay failed — falling back to bind-mount HOME"
    echo "[kyber] (System-level installs will not persist; HOME directory will via bind-mount.)"
    cat "$PERSIST_DIR/overlay-mount.log" >&2 || true
    USE_OVERLAY=false
    OVERLAY_MODE="bind-mount-home"
fi

if [ "$USE_OVERLAY" = true ]; then
    # ---- Overlay mode: chroot into merged root ----

    # Reuse or initialize upper layer
    if [ -d "$UPPER_DIR" ] && [ "$(ls -A "$UPPER_DIR" 2>/dev/null)" ]; then
        echo "[kyber] Returning pod — upper layer has state"
    else
        echo "[kyber] First boot — overlay upper is empty"
    fi

    # Bind mount the PV into merged so /persist is accessible inside the chroot
    mkdir -p "$MERGED_DIR/persist"
    if ! mountpoint -q "$MERGED_DIR/persist"; then
        mount --bind "$PERSIST_DIR" "$MERGED_DIR/persist"
    fi

    # Bind mount kernel filesystems.
    # Use --rbind for /dev so /dev/pts (a submount) comes with it — otherwise
    # tmux can't allocate PTYs and fails with "fork failed: No such file or
    # directory" when starting a session.
    for fs in proc sys; do
        if ! mountpoint -q "$MERGED_DIR/$fs"; then
            mount --bind "/$fs" "$MERGED_DIR/$fs"
        fi
    done
    if ! mountpoint -q "$MERGED_DIR/dev"; then
        mount --rbind /dev "$MERGED_DIR/dev"
        mount --make-rslave "$MERGED_DIR/dev"
    fi

    # k8s-injected files
    for f in /etc/resolv.conf /etc/hosts /etc/hostname; do
        if [ -f "$f" ] && ! mountpoint -q "$MERGED_DIR$f" 2>/dev/null; then
            mount --bind "$f" "$MERGED_DIR$f" 2>/dev/null || true
        fi
    done

    # Secrets
    if [ -d /secrets ] && ! mountpoint -q "$MERGED_DIR/secrets" 2>/dev/null; then
        mkdir -p "$MERGED_DIR/secrets"
        mount --bind /secrets "$MERGED_DIR/secrets"
    fi

    # User-secrets (#75 / #514) — kubelet mounts the <agent>-user-secrets-files
    # Secret at /user-secrets on the pod. Like /secrets, the chroot doesn't
    # inherit it, so bind it in. Without this bind the agent sees only the empty
    # boot-time overlay snapshot of /user-secrets and NEVER kubelet's live
    # content — so a file-mode user-secret written via the API (e.g. the minted
    # FALCON_ISSUE_TOKEN) is invisible in-pod (kyber#514). Clean top-level path,
    # so no /var/run → /run symlink complication (cf. the identity token below).
    # File-mode rotations follow the underlying dir: kubelet's atomic ~60s
    # re-sync is visible inside the chroot without a pod restart.
    if [ -d /user-secrets ] && ! mountpoint -q "$MERGED_DIR/user-secrets" 2>/dev/null; then
        mkdir -p "$MERGED_DIR/user-secrets"
        mount --bind /user-secrets "$MERGED_DIR/user-secrets"
    fi

    # Agent jobs ConfigMap (#135) — kubelet mounts the <agent>-jobs ConfigMap
    # at /kyber/jobs-src on the pod. Inside the chroot the path doesn't exist
    # unless we bind it in: the dispatcher and the crontab-install step both
    # read from this directory. Bind-mount is used (not copy) so ConfigMap
    # rotations — kubelet re-writes the mount atomically in place — flow
    # through to the in-chroot view without a pod restart. Target is under
    # /kyber (not /etc/cron.d) so the mount doesn't fight the overlay's own
    # /etc/cron.d layer. install-agent-jobs-crontab below copies the rendered
    # file into /etc/cron.d/ as a separate step.
    if [ -d /kyber/jobs-src ] \
        && ! mountpoint -q "$MERGED_DIR/kyber/jobs-src" 2>/dev/null; then
        mkdir -p "$MERGED_DIR/kyber/jobs-src"
        mount --bind /kyber/jobs-src "$MERGED_DIR/kyber/jobs-src"
    fi

    # Identity-repo token — kubelet populates /var/run/secrets/kyber-github on
    # the pod filesystem when the Agent CRD declares spec.identityRepo.repo.
    # The chroot doesn't inherit /var/run/secrets/* automatically, so bind-
    # mount it in so start-claude.sh's credential helper can read the token.
    # File-level rotations inside the bind-mount follow the underlying dir
    # (kubelet's ~60s re-sync is visible without a pod restart).
    #
    # Target $MERGED_DIR/run (not /var/run): /merged/var/run is a symlink to
    # /run on the HOST, so binding to $MERGED_DIR/var/run/secrets/kyber-github
    # resolves to the host's /run/secrets/kyber-github and becomes an invisible
    # self-bind that never appears inside the chroot. Binding to $MERGED_DIR/run
    # lands the Secret in the chroot's real /run; /var/run → /run inside the
    # chroot makes it visible at both paths.
    if [ -d /var/run/secrets/kyber-github ] \
        && ! mountpoint -q "$MERGED_DIR/run/secrets/kyber-github" 2>/dev/null; then
        mkdir -p "$MERGED_DIR/run/secrets/kyber-github"
        mount --bind /var/run/secrets/kyber-github "$MERGED_DIR/run/secrets/kyber-github"
    fi

    # Pod-token (kyber#566) — kubelet mounts the control-plane-signed pod-token
    # at /var/run/secrets/kyber/pod-token. Same story as kyber-github above: the
    # chroot doesn't inherit /var/run/secrets/*, so bind it into $MERGED_DIR/run
    # so processes inside the chroot can present it. Needed so the git credential
    # helper (kyber#508 Stage 3/4) can authenticate its scoped-identity-repo-token
    # request, and so start-claude.sh's own authenticated calls work. Bind to
    # $MERGED_DIR/run (not /var/run) for the same symlink reason as above.
    if [ -d /var/run/secrets/kyber ] \
        && ! mountpoint -q "$MERGED_DIR/run/secrets/kyber" 2>/dev/null; then
        mkdir -p "$MERGED_DIR/run/secrets/kyber"
        mount --bind /var/run/secrets/kyber "$MERGED_DIR/run/secrets/kyber"
    fi

else
    # ---- Bind-mount HOME mode: run on ephemeral root, bind-mount HOME from PV ----
    #
    # Kernel overlay mount failed, so we can't persist arbitrary writes across
    # the entire root FS. Instead, bind-mount the agent user's HOME from the
    # PV — anything an agent writes under $HOME survives pod restarts, which
    # matches operator/agent intuition.
    #
    # System-level state (apt, /usr/lib) still doesn't persist in this mode —
    # that requires a working overlay mount.

    # Determine the runtime user. If the 'kyber' user exists (agent-base
    # image creates it), use its home. Otherwise fall back to root.
    if id kyber &>/dev/null; then
        AGENT_USER="kyber"
        AGENT_HOME="/home/kyber"
    else
        AGENT_USER="root"
        AGENT_HOME="/root"
    fi

    HOME_PERSIST="$PERSIST_DIR/home"
    mkdir -p "$HOME_PERSIST"

    # First-boot seed: if the persist target is empty (no .bashrc — using that
    # as a sentinel for "looks like a fresh HOME"), copy the image's initial
    # HOME contents in. This gives the persistent HOME a sane starting state
    # (bash dotfiles, default config) on first boot. Subsequent boots skip
    # this step because .bashrc will already exist.
    if [ ! -f "$HOME_PERSIST/.bashrc" ] && [ -d "$AGENT_HOME" ]; then
        echo "[kyber] First boot in symlink mode — seeding $HOME_PERSIST from $AGENT_HOME"
        cp -a "$AGENT_HOME/." "$HOME_PERSIST/" 2>/dev/null || true
    fi

    # Ownership: chown the persist tree BEFORE binding, so the chown affects
    # the underlying PV (not the bind-mounted view, where it would be redundant).
    if [ "$AGENT_USER" != "root" ]; then
        chown -R "$AGENT_USER:$AGENT_USER" "$PERSIST_DIR" 2>/dev/null || true
    fi

    # Bind-mount $HOME_PERSIST over $AGENT_HOME. After this, every read/write
    # to $AGENT_HOME goes directly to the PV — no symlink traversal.
    #
    # On security profiles that block mount(MS_BIND) (default seccomp without
    # privileged), this fails — we tolerate that and fall back to symlinking
    # known persistent subdirs into HOME so SOMETHING survives. Pod stays alive.
    if ! mountpoint -q "$AGENT_HOME"; then
        if mount --bind "$HOME_PERSIST" "$AGENT_HOME" 2>/dev/null; then
            echo "[kyber] HOME bind-mounted: $AGENT_HOME → $HOME_PERSIST"
        else
            echo "[kyber] WARNING: bind-mount of HOME failed (likely seccomp restriction)" >&2
            echo "[kyber] Falling back to selective symlinks (~/.claude, ~/.config, ~/.local, ~/.npm)" >&2
            mkdir -p "$HOME_PERSIST/.claude" \
                     "$HOME_PERSIST/.config" \
                     "$HOME_PERSIST/.local" \
                     "$HOME_PERSIST/.npm"
            for dir in .claude .config .local .npm; do
                target="$HOME_PERSIST/$dir"
                link="$AGENT_HOME/$dir"
                if [ -L "$link" ]; then continue; fi
                rm -rf "$link"
                ln -s "$target" "$link"
            done
            echo "[kyber] Selective symlinks in place: ~$AGENT_USER/{.claude,.config,.local,.npm} → $HOME_PERSIST/"
        fi
    else
        echo "[kyber] HOME already bind-mounted at $AGENT_HOME"
    fi

    # System-cron persistence in bind-mount-home mode. Overlay mode covers
    # /etc/crontab, /etc/cron.d, and /var/spool/cron/crontabs for free via
    # the upper layer — in the bind-mount-home fallback we'd lose them, so
    # shadow each one from /persist/cron. Result: any cron job installed at
    # the user (`crontab -e`) or system (`/etc/crontab`, `/etc/cron.d/*`)
    # level survives pod restarts in every persistence mode.
    CRON_PERSIST="$PERSIST_DIR/cron"
    mkdir -p "$CRON_PERSIST/cron.d" \
             "$CRON_PERSIST/cron.hourly" \
             "$CRON_PERSIST/cron.daily" \
             "$CRON_PERSIST/cron.weekly" \
             "$CRON_PERSIST/cron.monthly" \
             "$CRON_PERSIST/crontabs"
    [ -f "$CRON_PERSIST/crontab" ] || touch "$CRON_PERSIST/crontab"

    # First-boot seed the image defaults so bind-mount doesn't mask them.
    if [ ! -s "$CRON_PERSIST/crontab" ] && [ -s /etc/crontab ]; then
        cp /etc/crontab "$CRON_PERSIST/crontab" 2>/dev/null || true
    fi

    # /var/spool/cron/crontabs demands root:crontab ownership + 1730 or cron
    # refuses to read it.
    chown root:crontab "$CRON_PERSIST/crontabs" 2>/dev/null || true
    chmod 1730 "$CRON_PERSIST/crontabs" 2>/dev/null || true

    # Bind-mount each path; fall back to symlink when mount --bind is blocked
    # (same seccomp scenario the HOME fallback handles above). Helper is
    # local to this block.
    _kyber_persist_cron_path() {
        local src="$1" dest="$2"
        if mountpoint -q "$dest" 2>/dev/null; then return 0; fi
        mkdir -p "$(dirname "$dest")" 2>/dev/null || true
        if mount --bind "$src" "$dest" 2>/dev/null; then
            echo "[kyber] cron path bind-mounted: $dest → $src"
            return 0
        fi
        [ -L "$dest" ] && return 0
        rm -rf "$dest" 2>/dev/null || true
        if ln -s "$src" "$dest" 2>/dev/null; then
            echo "[kyber] cron path symlinked (bind-mount blocked): $dest → $src"
        else
            echo "[kyber] WARNING: could not persist cron path $dest" >&2
        fi
    }
    _kyber_persist_cron_path "$CRON_PERSIST/crontab"  /etc/crontab
    for dir in cron.d cron.hourly cron.daily cron.weekly cron.monthly; do
        if [ -z "$(ls -A "$CRON_PERSIST/$dir" 2>/dev/null)" ] && [ -d "/etc/$dir" ]; then
            cp -a "/etc/$dir/." "$CRON_PERSIST/$dir/" 2>/dev/null || true
        fi
        _kyber_persist_cron_path "$CRON_PERSIST/$dir" "/etc/$dir"
    done
    _kyber_persist_cron_path "$CRON_PERSIST/crontabs" /var/spool/cron/crontabs
fi

# ---- Boot metadata (both modes) ----
BOOT_TIME=$(date -Iseconds)
FIRST_BOOT="false"
if [ ! -f "$PERSIST_DIR/.initialized" ]; then
    FIRST_BOOT="true"
    touch "$PERSIST_DIR/.initialized"
fi

mkdir -p "$PERSIST_DIR/kyber"
cat > "$PERSIST_DIR/kyber/boot-metadata.json.tmp" <<EOF
{
  "boot_time": "$BOOT_TIME",
  "hostname": "$(hostname)",
  "first_boot": $FIRST_BOOT,
  "mode": "$OVERLAY_MODE"
}
EOF
mv "$PERSIST_DIR/kyber/boot-metadata.json.tmp" "$PERSIST_DIR/kyber/boot-metadata.json"

echo "[kyber] Boot metadata written ($OVERLAY_MODE mode)"

if [ "$USE_OVERLAY" = true ]; then
    echo "[kyber] Filesystem ready. Entering chroot."
    # Ensure the log dir exists inside the merged root, then tee bootstrap output
    # so the in-pod `boot` alias has something to tail.
    mkdir -p "$MERGED_DIR/var/log"
    # Claude Code v2 refuses --dangerously-skip-permissions as root, and we need
    # a working dbus session for keytar credential storage. Same pattern as the
    # bind-mount-home-with-kyber-user branch below: chroot → dbus-run-session
    # → unlock keyring → su to kyber → run bootstrap with pipefail + tee.
    if id kyber &>/dev/null; then
        # Make /persist writable by kyber (the bind-mount-home branch already
        # does this; replicate here so the chroot's su-to-kyber can write the
        # bootstrap log + everything else under /persist).
        chown -R kyber:kyber "$PERSIST_DIR" 2>/dev/null || true
        KYBER_AGENT_CMD="$(printf '%q ' "$@")"
        export KYBER_AGENT_CMD
        exec chroot "$MERGED_DIR" env \
            "KYBER_AGENT_CMD=$KYBER_AGENT_CMD" \
            HOME=/home/kyber \
            dbus-run-session -- bash -c '
                mkdir -p /run/dbus 2>/dev/null || true
                # gnome-keyring-daemon runs as ROOT here (we have not dropped to
                # kyber yet) with HOME=/home/kyber, and creates ~/.local/share/
                # keyrings + ~/.cache/keyring-* on first use. Left to itself it
                # creates them root-owned 0700, and the boot then dies later as
                # kyber on `mkdir -p $HOME/.local/bin` in the shared identity-repo
                # helper — under `set -euo pipefail` that kills EVERY brand-new
                # agent, both runtimes (kyber#684). Pre-create them owned by kyber
                # so the daemon writes inside dirs the agent can still use, and
                # self-heal any PVC already wedged by the old behaviour.
                # NO APOSTROPHES OR SINGLE QUOTES ANYWHERE IN THIS BLOCK — it is
                # the body of `bash -c +...+` (single-quoted), so one stray quote
                # silently ends the string and kills the boot at "Entering
                # chroot". That is exactly how kyber#684 got broken once already.
                #
                # Deliberately NOT a recursive chown over ~/.cache: a real agent
                # has 4.6 GB / 22k files under there, and walking every inode
                # twice per boot is both slow and the kind of I/O that has put
                # the falcon node into NotReady. Only the keyring subtrees (a
                # handful of small files) get a recursive pass; parents are
                # fixed in place.
                #
                # Bare user, no :group — id kyber proves the USER exists, not a
                # like-named group, and -g against a missing group fails the
                # whole install silently.
                kyber_fix_home_dirs() {
                    install -d -o kyber -m 0700 /home/kyber/.local /home/kyber/.cache 2>/dev/null || true
                    chown kyber /home/kyber/.local /home/kyber/.cache /home/kyber/.local/share 2>/dev/null || true
                    chown -R kyber /home/kyber/.local/share/keyrings /home/kyber/.cache/keyring-* 2>/dev/null || true
                }
                kyber_fix_home_dirs
                echo "" | gnome-keyring-daemon --unlock --components=secrets 2>/dev/null || true
                # Re-assert after the daemon has run: it creates its own subdirs
                # as root, and those must be kyber-owned too.
                kyber_fix_home_dirs
                # Fail loudly rather than dying twenty lines later with a bare
                # Permission denied from mkdir. Every repair above is
                # best-effort by design, so this is the only thing standing
                # between a failed chown and an unexplained dead boot.
                if ! su -s /bin/sh kyber -c "test -w /home/kyber/.local" 2>/dev/null; then
                    echo "[kyber] WARNING: ~/.local is not writable by kyber — identity-repo setup will fail (kyber#684)" >&2
                fi
                echo "[kyber] dbus=${DBUS_SESSION_BUS_ADDRESS:-unset}"
                # Start cron as root before dropping to kyber. Containers have no
                # init so nothing else will start the daemon; without it scheduled
                # jobs in ~/.kyber-cron never fire. Remove any stale pidfile from
                # the previous pod (overlay upper persists /var/run/crond.pid
                # across restarts, which makes fresh cron refuse to start).
                rm -f /var/run/crond.pid 2>/dev/null || true
                # Install agent-jobs crontab from the ConfigMap mount (#135).
                # Copy (not symlink) so cron picks up the current contents on
                # boot even if the overlay upper layer persisted a stale file
                # from a previous pod. Overwrite every boot; the ConfigMap is
                # the source of truth. If the mount is missing — the Agent
                # CRD may be pre-#135 or the controller is still catching up
                # — drop any stale kyber-jobs file so cron does not fire old
                # definitions. cron'"'"'s mtime-every-minute dir scan picks
                # up the change within ~60s.
                if [ -r /kyber/jobs-src/crontab ]; then
                    mkdir -p /etc/cron.d
                    install -m 0644 /kyber/jobs-src/crontab /etc/cron.d/kyber-jobs
                    echo "[kyber] installed /etc/cron.d/kyber-jobs from ConfigMap"
                else
                    rm -f /etc/cron.d/kyber-jobs 2>/dev/null || true
                    echo "[kyber] no ConfigMap crontab at /kyber/jobs-src/crontab"
                fi
                if [ -x /usr/sbin/cron ] && /usr/sbin/cron; then
                    echo "[kyber] cron daemon started"
                else
                    echo "[kyber] WARNING: cron daemon did not start"
                fi
                # kyber-job-dispatch runs as user kyber from cron and appends to
                # /persist/var/log/kyber-jobs.log. Pre-create the file + parent
                # dirs with kyber ownership so the dispatcher can write. The
                # chown on the existing file is defensive: /persist is the PV
                # and an earlier root-side invocation (or a pre-fix image) may
                # have left the log root-owned, which silently swallows every
                # subsequent cron-fire log write.
                install -d -o kyber -g kyber -m 0755 /persist/var/log /persist/var/lock 2>/dev/null || true
                [ -e /persist/var/log/kyber-jobs.log ] || install -o kyber -g kyber -m 0644 /dev/null /persist/var/log/kyber-jobs.log
                chown kyber:kyber /persist/var/log/kyber-jobs.log 2>/dev/null || true
                # Background watcher: re-sync /etc/cron.d/kyber-jobs when the
                # ConfigMap changes without a pod restart (#160). Still running
                # as root here, so writes to /etc/cron.d work. Cron rescans
                # /etc/cron.d every minute; changes land within ~65s of a
                # ConfigMap update.
                (
                    prev_hash=""
                    while true; do
                        if [ -r /kyber/jobs-src/crontab ]; then
                            curr_hash=$(sha256sum /kyber/jobs-src/crontab 2>/dev/null | cut -d" " -f1)
                            if [ "$curr_hash" != "$prev_hash" ] || [ ! -f /etc/cron.d/kyber-jobs ]; then
                                mkdir -p /etc/cron.d
                                install -m 0644 /kyber/jobs-src/crontab /etc/cron.d/kyber-jobs
                                echo "[kyber] jobs-watcher: updated /etc/cron.d/kyber-jobs"
                                prev_hash="$curr_hash"
                            fi
                        else
                            if [ -f /etc/cron.d/kyber-jobs ]; then
                                rm -f /etc/cron.d/kyber-jobs
                                echo "[kyber] jobs-watcher: removed stale /etc/cron.d/kyber-jobs"
                                prev_hash=""
                            fi
                        fi
                        sleep 5
                    done
                ) &
                echo "[kyber] jobs-watcher started (pid=$!)"
                exec su --preserve-environment -s /bin/bash kyber -c \
                    "set -o pipefail; $KYBER_AGENT_CMD 2>&1 | tee /persist/kyber-bootstrap.log"
            '
    else
        # No kyber user in the image — fall back to root (agent-base dev image only).
        exec chroot "$MERGED_DIR" /bin/bash -c 'set -o pipefail; "$@" 2>&1 | tee /persist/kyber-bootstrap.log' _ "$@"
    fi
else
    echo "[kyber] Filesystem ready. Running as $AGENT_USER (bind-mount-home mode)."
    if [ "$AGENT_USER" != "root" ]; then
        # Start a dbus session bus + gnome-keyring daemon so Claude Code's
        # keytar module has a working keychain backend for credential storage.
        # Without this, keytar fails silently and Claude Code falls back to
        # the interactive OAuth flow on every startup.
        #
        # dbus-run-session creates a session bus and exports DBUS_SESSION_BUS_ADDRESS
        # for all child processes. This replaces the previous dbus-launch approach
        # (dbus-launch requires dbus-x11 which is not installed).
        echo "[kyber] Starting dbus session + gnome-keyring for credential storage"
        mkdir -p /run/dbus 2>/dev/null || true
        dbus-daemon --system --fork 2>/dev/null || true
        export HOME="$AGENT_HOME"
        export KYBER_AGENT_USER="$AGENT_USER"
        export KYBER_AGENT_CMD="$(printf '%q ' "$@")"

        exec dbus-run-session -- bash -c '
            # Same root-owned-dotdir hazard as the overlay branch above
            # (kyber#684): the keyring daemon runs as root here with
            # HOME=$AGENT_HOME and would create ~/.local and ~/.cache root-owned,
            # killing the later `mkdir -p $HOME/.local/bin` once we drop to
            # $KYBER_AGENT_USER. Pre-create them owned by the agent user, and
            # re-assert after the daemon has made its own subdirs.
            # Bounded for the same reason as the overlay branch: recursive chown
            # only over the keyring subtrees, never over all of ~/.cache.
            kyber_fix_home_dirs() {
                install -d -o "$KYBER_AGENT_USER" -m 0700 "$HOME/.local" "$HOME/.cache" 2>/dev/null || true
                chown "$KYBER_AGENT_USER" "$HOME/.local" "$HOME/.cache" "$HOME/.local/share" 2>/dev/null || true
                chown -R "$KYBER_AGENT_USER" "$HOME/.local/share/keyrings" "$HOME"/.cache/keyring-* 2>/dev/null || true
            }
            kyber_fix_home_dirs
            echo "" | gnome-keyring-daemon --unlock --components=secrets 2>/dev/null || true
            kyber_fix_home_dirs
            if ! su -s /bin/sh "$KYBER_AGENT_USER" -c "test -w $HOME/.local" 2>/dev/null; then
                echo "[kyber] WARNING: ~/.local is not writable by $KYBER_AGENT_USER — identity-repo setup will fail (kyber#684)" >&2
            fi
            echo "[kyber] dbus=${DBUS_SESSION_BUS_ADDRESS:-unset}"
            # Start cron as root before dropping to the agent user — see the
            # overlay branch above for why. /var/run/crond.pid is ephemeral in
            # bind-mount-home mode so no stale-pidfile cleanup needed, but
            # keep the rm for symmetry / safety.
            rm -f /var/run/crond.pid 2>/dev/null || true
            # Install agent-jobs crontab from the ConfigMap mount (#135).
            # Bind-mount-home mode: /etc/cron.d is bind-mounted from
            # /persist/cron/cron.d, so writing here persists to the PV AND
            # appears to cron at /etc/cron.d/kyber-jobs. Overwrite every boot
            # so the ConfigMap stays the source of truth; drop stale files
            # when the mount is absent (pre-#135 pods).
            if [ -r /kyber/jobs-src/crontab ]; then
                mkdir -p /etc/cron.d
                install -m 0644 /kyber/jobs-src/crontab /etc/cron.d/kyber-jobs
                echo "[kyber] installed /etc/cron.d/kyber-jobs from ConfigMap"
            else
                rm -f /etc/cron.d/kyber-jobs 2>/dev/null || true
                echo "[kyber] no ConfigMap crontab at /kyber/jobs-src/crontab"
            fi
            if [ -x /usr/sbin/cron ] && /usr/sbin/cron; then
                echo "[kyber] cron daemon started"
            else
                echo "[kyber] WARNING: cron daemon did not start"
            fi
            # Background watcher: re-sync /etc/cron.d/kyber-jobs when the
            # ConfigMap changes without a pod restart (#160).
            (
                prev_hash=""
                while true; do
                    if [ -r /kyber/jobs-src/crontab ]; then
                        curr_hash=$(sha256sum /kyber/jobs-src/crontab 2>/dev/null | cut -d" " -f1)
                        if [ "$curr_hash" != "$prev_hash" ] || [ ! -f /etc/cron.d/kyber-jobs ]; then
                            mkdir -p /etc/cron.d
                            install -m 0644 /kyber/jobs-src/crontab /etc/cron.d/kyber-jobs
                            echo "[kyber] jobs-watcher: updated /etc/cron.d/kyber-jobs"
                            prev_hash="$curr_hash"
                        fi
                    else
                        if [ -f /etc/cron.d/kyber-jobs ]; then
                            rm -f /etc/cron.d/kyber-jobs
                            echo "[kyber] jobs-watcher: removed stale /etc/cron.d/kyber-jobs"
                            prev_hash=""
                        fi
                    fi
                    sleep 5
                done
            ) &
            echo "[kyber] jobs-watcher started (pid=$!)"
            exec su --preserve-environment -s /bin/bash "$KYBER_AGENT_USER" -c "$KYBER_AGENT_CMD"
        '
    else
        mkdir -p /var/log
        exec bash -c 'set -o pipefail; "$@" 2>&1 | tee /persist/kyber-bootstrap.log' _ "$@"
    fi
fi
