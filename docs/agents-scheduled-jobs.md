# Scheduled jobs on agents (cron)

Agents run with cron available out of the box. Any cron job installed at the
user or system level survives pod restarts and is automatically picked up by
a fresh daemon on the next boot, in every persistence mode (GCE and
standalone). This page covers the supported surfaces, a worked example, and
the mental model for how persistence happens under the hood so you can debug
if a job goes missing.

## TL;DR

- Use `crontab -e` for per-user schedules (runs as the `kyber` user).
- Drop a file in `/etc/cron.d/<name>` for system-level jobs that need to run
  as a specific user (e.g. `root`).
- The daemon is already started for you — no `service cron start` needed.
- Everything installed this way persists across pod restarts.

## Supported install surfaces

| Surface | Runs as | Persists across restarts |
|---|---|---|
| `crontab -e` (user crontab) | `kyber` | ✅ yes |
| `sudo crontab -e` (root crontab) | `root` | ✅ yes |
| `/etc/crontab` | configurable per line | ✅ yes |
| `/etc/cron.d/<file>` | configurable per line | ✅ yes |
| `/etc/cron.hourly/`, `cron.daily/`, `cron.weekly/`, `cron.monthly/` | `root` | ✅ yes |

## Example — per-minute log heartbeat

```bash
# Inside the agent
mkdir -p ~/.kyber-logs

# Option 1: user crontab — runs as the kyber user.
(crontab -l 2>/dev/null; echo '* * * * * date -Iseconds >> ~/.kyber-logs/heartbeat.log') | crontab -

# Option 2: system cron — runs as root.
sudo tee /etc/cron.d/kyber-heartbeat > /dev/null <<'EOF'
* * * * * root date -Iseconds >> /var/log/kyber-heartbeat.log 2>&1
EOF
```

Verify the daemon is running:

```bash
pgrep -a cron
# -> /usr/sbin/cron
```

Delete and recreate the pod (or restart the agent from the PWA). After the
new pod comes up, both lines are still installed and the logs keep growing.

## How persistence works

Two persistence modes, same outcome from the agent's perspective.

### Overlay mode (kernel or fuse-overlayfs)

This is the default on GCE machines and on any standalone install where the
kernel supports nested overlayfs or `/dev/fuse` is exposed. `entrypoint.sh`
mounts an overlay over `/` with the upper layer living on the agent's
persistent volume at `/persist/overlay/upper`. Every write anywhere on the
root FS (including `/etc/crontab`, `/etc/cron.d/*`, and
`/var/spool/cron/crontabs/*`) lands in that upper layer and persists across
pod restarts. On the next boot the overlay re-mounts the same upper layer,
so state carries over.

### Bind-mount-home fallback mode

When overlay setup fails (e.g. no `/dev/fuse`, restrictive kernel), agents
run on an ephemeral root FS with `$HOME` bind-mounted from `/persist/home`.
Under this mode the root FS is otherwise **ephemeral** — normally
`/etc/crontab`, `/etc/cron.d/`, and `/var/spool/cron/crontabs/` would be
reset on every restart.

To preserve the acceptance contract, `entrypoint.sh` shadows each of those
paths from `/persist/cron/` via `mount --bind` (with a symlink fallback when
`mount` itself is blocked by seccomp):

```
/etc/crontab               ← /persist/cron/crontab
/etc/cron.d/               ← /persist/cron/cron.d/
/etc/cron.hourly/          ← /persist/cron/cron.hourly/
/etc/cron.daily/           ← /persist/cron/cron.daily/
/etc/cron.weekly/          ← /persist/cron/cron.weekly/
/etc/cron.monthly/         ← /persist/cron/cron.monthly/
/var/spool/cron/crontabs/  ← /persist/cron/crontabs/
```

First-boot seeds the persist side with the image defaults so the bind mount
doesn't mask system-shipped entries. `/var/spool/cron/crontabs` gets its
required `root:crontab` ownership and `1730` permissions before mounting.

### Daemon start

Containers don't run an init, so `entrypoint.sh` starts `/usr/sbin/cron`
directly as root just before `su`'ing to `kyber`. Stale
`/var/run/crond.pid` from the previous pod is unlinked first — in overlay
mode that pidfile persists in the upper layer across restarts and would
otherwise make a fresh daemon refuse to start.

## Debugging

- **Daemon not running**: `pgrep cron` — nothing → inspect
  `/persist/kyber-bootstrap.log` for the `cron daemon started` or
  `WARNING: cron daemon did not start` line from entrypoint.
- **Job installed but not firing**: `crontab -l` (or `sudo cat /etc/crontab`)
  to confirm the entry is really there. Cron polls every minute; wait at
  least 60 seconds before declaring it broken.
- **Job disappears after restart**: check the pod's persistence mode:
  ```bash
  jq -r .mode /persist/kyber/boot-metadata.json
  ```
  If `mode` is `kernel` or `fuse`, files live in `/persist/overlay/upper/`.
  If `mode` is `bind-mount-home`, system cron files are in `/persist/cron/`.
  If neither path shows your file, the write never persisted — confirm you
  weren't shelling into a detached container.
- **Permission denied on `/var/spool/cron/crontabs`**: that directory requires
  `root:crontab` ownership and mode `1730`. Recreate the pod — entrypoint
  resets these on every boot.
