# Agent pod isolation design

**Status:** Phase 1 (de-privileging) implemented and default-on; Phase 2 (user
namespaces) implemented behind a per-cluster toggle, default-off pending
per-cluster validation.
**Scope:** The security envelope of the main agent runtime container and its
pod — the `Privileged`/capability/seccomp posture, host-device exposure, and
user-namespace remapping. Whole-disk persistence semantics are preserved, not
changed. Agent-to-agent network isolation (G5) and per-agent privilege as an
Agent-CRD field are explicitly out of scope (see §7).

Resolves kyber#76.

## 1. Problem

`docs/runtimes.md` claimed "the Kubernetes pod is the agent's isolation
boundary." kyber#76 verified, from inside a live agent pod, that the boundary
does not hold: agent pods run `Privileged: true` (`pod_builder.go`), which makes
the container root-equivalent on its node. The reported gaps:

- **G1** — Fully privileged: `Seccomp: 0` (no filter), full capability bounding
  set, AppArmor unconfined.
- **G2** — Passwordless root in-pod: the agent user can `sudo -n id` to uid 0.
- **G3** — Host filesystem reachable: host block devices `/dev/sda`–`/dev/sdd`
  are present in the pod and mountable, exposing the node root filesystem
  (k3s state/credentials, every co-located pod's data).
- **G4** — The one boundary that holds (no ServiceAccount token is projected;
  the k8s API returns 401 to the pod as itself) is bypassable given G3, because
  the node's k3s admin kubeconfig can be read off the host filesystem.
- **G5** — No agent-to-agent network isolation.

The single root cause of G1–G3 is that `Privileged: true` was used as a
shortcut to satisfy the requirements of **whole-`/` overlay persistence**. The
persistence design mounts an overlay over `/` so an agent's entire filesystem —
including system-level `apt`/`/usr` installs — survives pod restarts. On
k3s/containerd the kernel rejects overlay-over-overlay, so it falls back to
`fuse-overlayfs`, which needs `/dev/fuse` plus the `mount`/FUSE syscalls that a
confined profile blocks. `Privileged: true` grants those — and, as collateral,
everything else that constitutes a host escape.

## 2. Goals and non-goals

Product constraint (owner decision, kyber#76 thread): **agents must remain
completely unrestricted _inside their own pod_** — they install and manage
arbitrary software, including system-level packages, and that software must
persist across pod reboots. In-pod root is a required capability, not a defect.

Reframed, the two objectives are not in tension:

- **Goal:** an agent can do anything it likes _inside its pod_ (root, `apt
  install`, whole-`/` persistence).
- **Goal:** an agent cannot reach the _host node_ — its filesystem, its
  credentials, its other pods.

Non-goals: changing what persists (whole-`/` overlay stays); per-agent
privilege tiers on the Agent CRD; agent-to-agent network policy (G5). See §7.

## 3. Key insight

`fuse-overlayfs` does not need `Privileged: true`. It needs exactly:

1. the `/dev/fuse` character device (already delivered as a `HostPath` mount,
   independent of `Privileged`), and
2. `CAP_SYS_ADMIN` for the `mount(2)` call, plus a seccomp profile that admits
   the mount/FUSE syscalls.

`Privileged: true` additionally does three things the persistence feature never
needed, and that are precisely the host-escape surface:

- populates the pod's `/dev` with **all host devices** (this is why
  `/dev/sda`–`/dev/sdd` are present — the G3 escape),
- sets the container's **device cgroup to `allow a *:* rwm`** (so even a
  hand-`mknod`'d block device would be openable), and
- disables **seccomp and AppArmor** entirely (G1).

Dropping `Privileged` while keeping `CAP_SYS_ADMIN` + `/dev/fuse` therefore
keeps overlay persistence working while removing the host-device exposure: the
host block devices no longer appear in the pod, and the device cgroup reverts to
the default allow-list (the runtime default set plus `/dev/fuse`), so
`mount /dev/sda` — the G3 escape — has no device to act on. This holds **even
with `CAP_SYS_ADMIN`**, because the device cgroup, not the capability, is what
gates access to a block device.

The residual after de-privileging is `CAP_SYS_ADMIN`'s own (much narrower,
kernel-level) escape surface. The clean way to neutralize that is a **Linux user
namespace** (`pod.spec.hostUsers: false`): the container's uid 0 (and its
`CAP_SYS_ADMIN`) is remapped to an unprivileged, per-pod uid range on the host,
so in-pod root is powerless against host-owned resources while remaining fully
root _inside the pod_. This is the true host boundary and requires no change to
what the agent can do.

## 4. Design

Two layered, independently-shippable phases, both configured at the **cluster**
level (not per-agent — see §7):

### Phase 1 — De-privilege (default on)

The main agent container's security context changes from:

```
Privileged: true
Capabilities.Add: [SYS_ADMIN]
# (seccomp/apparmor implicitly unconfined via Privileged)
```

to the hardened default:

```
Privileged: false
Capabilities.Add: [SYS_ADMIN]        # retained solely for fuse-overlayfs mount
SeccompProfile.Type: RuntimeDefault  # configurable; see below
# /dev/fuse HostPath mount unchanged
# no host block devices; device cgroup back to the default allow-list
```

Notes on the specific choices:

- **`CAP_SYS_ADMIN` is retained, not dropped.** It is the one capability
  fuse-overlayfs's `mount(2)` requires. The rest of the container's capability
  set is the container-runtime default (which is what a normal, unprivileged
  container gets, and is sufficient for `apt` and `su`); we neither drop-all nor
  add anything beyond `SYS_ADMIN`, so package installs that expect the standard
  set keep working.
- **`AllowPrivilegeEscalation` is left unset (true).** Setting it false would
  impose `no_new_privs`, which blocks setuid binaries and would break in-pod
  `sudo apt install` — directly violating the product constraint. G2
  (passwordless in-pod root) is therefore **accepted, by design**; it is the
  agent's own workload, and under Phase 2 that root is host-neutered anyway.
- **Seccomp is configurable** via `KYBER_AGENT_SECCOMP_PROFILE`
  (`RuntimeDefault` | `Unconfined`), default `RuntimeDefault`. With
  `CAP_SYS_ADMIN` held, the container runtime's default seccomp profile admits
  the `mount`/`umount2` family, so fuse-overlayfs should mount under
  `RuntimeDefault`. If a target's profile proves stricter and the overlay fails
  to mount, the entrypoint degrades to bind-mount-HOME (persistence of `$HOME`
  only, not system installs); the documented remedy is to set the profile to
  `Unconfined` on that cluster (still de-privileged — the host-escape win comes
  from dropping `Privileged`, not from seccomp). This is the primary Phase-1
  validation gate (§5).

Because today's pods run `Privileged` (implicitly `Unconfined` seccomp),
`RuntimeDefault` is a net tightening; the only functional risk is the
mount-syscall gate above.

### Phase 2 — User namespaces (per-cluster opt-in, default off)

When `KYBER_AGENT_USER_NAMESPACES=true`, the pod spec sets `hostUsers: false`.
The kubelet/runtime allocate a per-pod uid/gid range; in-pod uid 0 maps to an
unprivileged host uid. In-pod `CAP_SYS_ADMIN` becomes namespaced — it grants
power only over resources the namespace owns, not the host — so the residual
Phase-1 escape surface closes while the agent still runs as root in-pod.

Default off because enabling it safely depends on properties this change cannot
verify from the control plane:

- **Runtime/kernel support.** Kubernetes 1.36 (this repo's client-go line) has
  user namespaces GA, but transparent access to the pre-existing persist PVC
  requires **idmapped mounts** (kernel ≥ 6.3 and a runtime that requests them).
  Without idmap, files on an existing PVC — written as real host uid 0/1001 by
  a pre-Phase-2 pod — appear as `nobody` inside the namespace and are not
  writable, so the overlay upper layer becomes inaccessible and the agent loses
  its persisted state.
- **fuse-overlayfs under userns** is the rootless-podman configuration and is
  expected to work, but the overlay-upper-on-PVC + uid-remap combination must be
  proven on each target.

Rollout is therefore per-cluster and staged (§5): validate on a canary
(ideally a fresh agent, avoiding the migration edge), then enable fleet-wide on
that cluster.

### Configuration surface

| Env var (control-plane) | Chart value | Default | Effect |
| --- | --- | --- | --- |
| `KYBER_AGENT_PRIVILEGED` | `agent.security.privileged` | `false` | `true` restores the legacy full-`Privileged` profile (break-glass rollback). |
| `KYBER_AGENT_USER_NAMESPACES` | `agent.security.userNamespaces` | `false` | `true` sets `pod.spec.hostUsers: false`. |
| `KYBER_AGENT_SECCOMP_PROFILE` | `agent.security.seccompProfile` | `RuntimeDefault` | Seccomp profile type for the de-privileged agent container. |

All three are read in `pod_builder.go` via the same `os.Getenv` helper pattern
as `controlPlaneInternalURL()` / `agentJobTimezone()`. Sidecars
(status/transcript/telegram/discord) are unaffected — they were never
privileged.

## 5. Rollout and validation

Ordered, per-cluster, reversible at every step (`KYBER_AGENT_PRIVILEGED=true`
is the break-glass revert to today's behavior):

1. **Phase 1, canary agent.** Roll one agent with `privileged=false`,
   `seccompProfile=RuntimeDefault`. Verify from inside the pod:
   - `cat /persist/kyber/boot-metadata.json` → `"mode": "fuse"` (overlay still
     mounts; **not** `"bind-mount-home"`).
   - `apt-get install -y <pkg>` succeeds, then the package survives a pod
     restart.
   - `ls /dev/sd*` → absent; `grep Seccomp /proc/self/status` → non-zero.
   - If the overlay dropped to bind-mount-HOME, set
     `seccompProfile=Unconfined` and re-verify.
2. **Phase 1, fleet.** Enable `privileged=false` cluster-wide. Agents roll on
   their normal recreate path.
3. **Phase 2, canary.** On a cluster with kernel ≥ 6.3, enable
   `userNamespaces=true` for a **new** agent (fresh PVC avoids the migration
   edge). Verify overlay mode `fuse`, `apt` install + persistence, and that
   in-pod `id` is 0 while `cat /proc/self/uid_map` shows a non-zero host base.
4. **Phase 2, existing agents.** Only after confirming idmapped mounts work on
   that cluster (existing PVC still readable under the namespace). If idmap is
   unavailable, keep Phase 2 off for that cluster and rely on Phase 1.
5. **Phase 2, fleet** per cluster, once its canary holds.

Because `helm upgrade --reuse-values` silently ignores new chart defaults
(AGENTS.md gotcha #6), verify the rendered env on live control-plane pods after
each step (`kubectl set env --list` / `helm get values`).

## 6. Gap dispositions (kyber#76 "Done when")

| Gap | Disposition | Rationale |
| --- | --- | --- |
| **G1** — no seccomp/AppArmor, full caps | **Closed (Phase 1)** | De-privileged: seccomp `RuntimeDefault`, capability set reduced to runtime-default + `SYS_ADMIN`. AppArmor follows the runtime default rather than unconfined. |
| **G2** — passwordless in-pod root | **Accepted, by design** | Required: agents must install/manage software (`apt` needs root). It is the agent's own workload; host-neutered under Phase 2. |
| **G3** — host filesystem reachable | **Closed (Phase 1)** | Dropping `Privileged` removes host block devices from the pod and restores the device cgroup allow-list; `mount /dev/sda` has no device. Fully closed against `CAP_SYS_ADMIN` misuse under Phase 2. |
| **G4** — k8s-API boundary bypassable via G3 | **Closed (follows G3)** | With G3 closed the node kubeconfig is no longer reachable; the existing no-token + 401 posture (unchanged) becomes load-bearing again. |
| **G5** — no agent-to-agent network isolation | **Deferred** | Independent workstream (NetworkPolicy + CNI enforcement). Tracked as a follow-up; not gated on this change. |

The `docs/runtimes.md` claim is reconciled in the same change: the pod is the
agent's isolation boundary **for host access** once Phase 1 (and, fully, Phase
2) is in effect; it is explicitly **not** an agent-to-agent boundary until G5 is
addressed.

## 7. Alternatives considered

- **Keep `Privileged`, narrow only seccomp.** Rejected: leaves host devices +
  device cgroup wide open, so G3 stays open. The device exposure, not seccomp,
  is the escape.
- **Drop whole-`/` overlay; persist only `$HOME`.** Rejected: violates the
  product constraint that system-level installs persist.
- **Per-agent privilege as an Agent-CRD field** (`spec.security.privileged`).
  Deferred: every agent needs the same "unlimited in-pod" posture and none need
  host access, so a per-agent tier adds CRD schema surface (regeneration,
  OpenAPI, PWA type) for no current use case. Cluster-level configuration is
  sufficient; revisit if a genuine host-access agent (nested containers, GPU
  passthrough, raw device) appears — that is the natural trigger for the field.
- **`hostUsers: false` on by default.** Rejected for now: unvalidated userns +
  idmap support on a target would wedge scheduling or silently strand existing
  PVCs. Ships as a per-cluster toggle instead.
