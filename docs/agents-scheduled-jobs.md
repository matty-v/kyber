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

The agent's root filesystem is a real directory on its persistent volume at
`/persist/agentroot`, seeded from the base image on first boot and entered with
`chroot`. Every write anywhere on the root FS — including `/etc/crontab`,
`/etc/cron.d/*`, and `/var/spool/cron/crontabs/*` — is simply a write to that
directory, so it persists across pod restarts with no special handling.

There is no overlay and no separate fallback mode to reason about. Earlier
versions mounted an overlayfs over `/` and kept the writes in an upper layer,
with a bind-mount-`$HOME` fallback that silently stopped persisting system-level
state. Both are gone: neither overlayfs nor FUSE works inside the user namespace
agents now run in, and a mode that quietly loses your cron files is worse than a
pod that refuses to start. See
[`design/agent-pod-isolation.md`](design/agent-pod-isolation.md).

A newer base image reaches an existing root through a three-way merge that never
overwrites a file the agent has touched, so a cron file you edited survives a
Kyber upgrade. Conflicts are listed in
`/persist/kyber/rootfs-upgrade-conflicts.log`.

One thing does not survive: on `local-path` volumes the PVC is node-local disk,
so the root does not follow the agent to a replacement node.

### Daemon start

Containers don't run an init, so `entrypoint.sh` starts `/usr/sbin/cron`
directly as root just before `su`'ing to `kyber`. Stale
`/var/run/crond.pid` from the previous pod is unlinked first — that pidfile
lives on the durable root and would otherwise make a fresh daemon refuse to
start.

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
  It should read `rootfs` (or `rootfs-seeded`, `rootfs-migrated`,
  `rootfs-upgraded` on the boot that did that work). Your file should then be
  visible at `/persist/agentroot/etc/cron.d/...`. If it isn't, the write never
  persisted — confirm you weren't shelling into a detached container, since
  `kubectl exec` does not land inside the agent's chroot.
  A `mode` of `bind-mount-home` means the agent fell back to the legacy
  overlay path and system-level state is NOT persisting; that mode is no longer
  reachable in the default configuration.
- **Permission denied on `/var/spool/cron/crontabs`**: that directory requires
  `root:crontab` ownership and mode `1730`. Recreate the pod — entrypoint
  resets these on every boot.
