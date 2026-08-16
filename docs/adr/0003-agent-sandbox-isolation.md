# ADR 0003 — Agent Sandbox Isolation and Durable Root

**Status:** Proposed · 2026-08-15
**Context issue:** [kyber#78](https://github.com/matty-v/kyber/issues/78)
**Decider:** Matt (via Dave)
**Related:** [kyber#76](https://github.com/matty-v/kyber/issues/76) (escape report), [kyber#77](https://github.com/matty-v/kyber/pull/77) (de-privileging), [agent-pod-isolation.md](../design/agent-pod-isolation.md) (superseded in part by this ADR)

## Summary

Kyber#78 asks for a sandbox where an agent has unrestricted administrative
control over its own durable environment while holding no authority that is
valid against the Kubernetes node.

PR #77 removed privileged mode but left the agent holding **host-valid
`CAP_SYS_ADMIN`**, because whole-root persistence was implemented as an
overlayfs mount performed inside the pod. Enabling user namespaces on top of
that stack broke package installation, so #77 shipped user namespaces as an
opt-in that no production profile could actually use.

**This ADR removes the overlay.**

The decision is to run agent pods in a **user namespace** and to make the
agent's root filesystem a **plain directory on its own PersistentVolume**,
seeded from the base image on first boot and entered with `chroot`. The mounts
the entrypoint still performs — bind mounts, `proc`, `tmpfs` — are all
permitted to a *namespaced* `CAP_SYS_ADMIN`, which carries no authority on the
host.

This closes the autonomy/isolation tradeoff rather than choosing a side of it:
`apt` works because the root is an ordinary filesystem, and `SYS_ADMIN` is
harmless because it is namespaced.

## The measurements

Every result below was produced on the supported runtime — k3s v1.34.6,
containerd 2.2.2, kernel 6.18 — in a live pod, not by reading a PodSpec.

### The user namespace works and is real

A pod with `hostUsers: false`, `privileged: false`, `RuntimeDefault` seccomp and
`SYS_ADMIN` starts normally and reports:

```
$ cat /proc/self/uid_map
         0 1091895296      65536
```

In-pod uid 0 is host uid 1091895296. No host block devices are present in
`/dev`.

### Option A (user namespace + FUSE) is a dead end

`/dev/fuse` cannot be delivered to a user-namespaced pod on this stack, and the
failure is not a configuration mistake:

| Delivery route | Result |
| --- | --- |
| `hostPath` volume (what #77 ships) | Pod fails to start: `failed to set MOUNT_ATTR_IDMAP on /dev/fuse: invalid argument`. The kubelet idmaps hostPath volumes for user-namespaced pods, and devtmpfs does not support idmapped mounts. |
| CDI injection via `cdi.k8s.io/*` annotation | No device injected. k3s does not configure CDI, and annotation-based injection is deprecated in containerd 2.x. |
| `mknod` from inside the pod | `EPERM`. `CAP_MKNOD` is namespaced and not host-valid, so the agent cannot fabricate the node at all. |

For completeness, the same `mknod` succeeds in a **non**-user-namespaced pod but
the subsequent `open()` returns `EPERM` — the device cgroup denies it. That is
the fabricated-device defense from #77 working as designed, and it confirms the
device cgroup, not the filesystem, is the gate.

A device-plugin DaemonSet could in principle satisfy the cgroup, but it would
reintroduce a node-level component whose whole job is handing agents a
kernel-facing device. Option C makes the device unnecessary, so the cost is not
worth paying.

### Kernel overlayfs is unavailable in the user namespace

```
$ mount -t overlay overlay -o lowerdir=/,upperdir=...,workdir=...,userxattr /tmp/ov/m
mount: wrong fs type, bad option, bad superblock on overlay
```

This is the root cause of #77's `figlet` failure. With neither FUSE nor kernel
overlayfs available, **no overlay-based persistence model can work inside a user
namespace.** The overlay has to go.

### Namespaced `SYS_ADMIN` is sufficient for everything else

In the same user-namespaced pod:

| Operation | Result |
| --- | --- |
| `mount --bind` | OK |
| `mount -t proc` | OK |
| `mount -t tmpfs` | OK |
| `chroot` (`CAP_SYS_CHROOT`) | Available |

Every mount the entrypoint performs after the overlay step is in this set.

### The decisive test

Seeding a full root onto the volume, bind-mounting `proc`/`sys`/`dev`, and
chrooting in:

```
1.3G    /persist/agentroot
Unpacking figlet (2.2.5-3.1) ...
Setting up figlet (2.2.5-3.1) ...
  ___  _  __
 / _ \| |/ /
| | | | ' /
| |_| | . \
 \___/|_|\_\
```

`figlet` — the exact package that failed in #77 — installs and runs, with no
privileged mode, no host-valid capability, no host device, and no overlay.

Agent PVCs are provisioned at 50–100Gi, so a 1.3G root seed is not a
meaningful capacity change.

### Kubernetes silently ignores `hostUsers` when unsupported

On k3s v1.31.5 / containerd 1.7.23, a pod with `hostUsers: false` is **admitted
and runs**, reporting:

```
$ cat /proc/self/uid_map
         0          0 4294967295
```

No user namespace, no warning, no event. A control plane that trusts the
PodSpec would report an isolated agent that is not isolated. This is why AC7
(fail closed) cannot be satisfied by rendering the field — Kyber must read the
effective uid map from inside the running agent and refuse to proceed if it is
not remapped.

## Decision

Agent pods run with `hostUsers: false`, `privileged: false`, `RuntimeDefault`
seccomp, and namespaced `SYS_ADMIN` + `SYS_CHROOT`. The agent's root filesystem
is a directory on its PersistentVolume, seeded from the base image on first
boot and entered with `chroot`. `/dev/fuse` and `fuse-overlayfs` are removed
from the agent pod spec and image. Kyber verifies the effective uid map at boot
and fails closed.

## Alternatives rejected

| Option | Disposition |
| --- | --- |
| **A. User namespace + FUSE delegation** | Rejected on evidence above: no delivery route for `/dev/fuse` on the supported stack, and a device plugin would reintroduce a node-level kernel-facing component that option C makes unnecessary. |
| **B. VM-isolated RuntimeClass (Kata/Firecracker)** | Rejected for now. It would satisfy the boundary, but it requires nested virtualization on every target (including the WSL2 dev box), adds seconds of cold start per agent, cuts density, and complicates PVC attachment, exec, and observability. Option C reaches the same host boundary — no host-valid capability — with none of that. Revisit if Kyber ever needs to defend against a *kernel* exploit rather than a capability escape. |
| **C. Persistence without overlay/FUSE** | **Accepted.** See above. |
| **D. gVisor / syscall-intercepting sandbox** | Rejected. gVisor's gofer does not implement the mount, `chroot`, and package-manager semantics the agent contract requires, and its syscall surface breaks tmux PTY handling. It also would not remove the persistence problem — an agent under gVisor still needs a durable root, which is option C again. |

## Consequences

**Good.** No host-valid administrative capability. `apt` works on an ordinary
filesystem, so the whole class of overlay rename/`EXDEV` bugs disappears. It
also removes overlay *shadowing*, where an agent's upper layer pins a stale copy
of a base-image library and a runtime bump then breaks only the agents that had
written to that path — the failure mode behind the v1.0.5 ncurses/tmux
regression. Persistence
becomes inspectable: the agent's root is a directory an operator can read.

**Cost.** First boot pays a ~1.3G seed. Base-image upgrades are no longer free —
delivering a new base image to an existing durable root requires a merge step
rather than swapping a lower layer. That merge is the significant new
engineering in this design and is specified in the implementation notes.

**Migration.** Existing agents hold their state in `/persist/overlay/upper`.
Migration composes that upper layer over the image root into the new
`/persist/agentroot` on the next boot, and keeps the old upper directory intact
so rollback is a flag flip.

## Acceptance criteria coverage

Scored against kyber#78's normative criteria. "Design met" means the mechanism
is in place and the suite is still owed; it is not a claim that the criterion
has been verified.

| AC | Verdict | Notes |
| --- | --- | --- |
| AC1 full software autonomy | Design met | Plain filesystem root; `figlet` proven. Toolchain/service/port sweep owed. |
| AC2 durable autonomy | **One clause at risk** | Restart, pod recreate, suspend/resume, base-image upgrade are met. **Node loss / machine replacement is not met on `local-path` volumes** — node-local disk, and this predates #78. |
| AC3 host isolation | **Gap found** | Identity, capabilities, devices, mounts, `setns`, modules, BPF all closed. **The kubelet (`:10250`) and API server (`:443`) are reachable from a pod today** — #77 added default-deny *ingress* only. Needs an egress policy. |
| AC4 agent-to-agent | **Partly blocked** | No Services, so no DNS discovery; ingress deny is enforced. Pod-CIDR scanning needs the same egress policy. The cross-node half cannot be run on single-node targets. |
| AC5 credential isolation | Design met | No SA token; per-agent secrets and pod token. Cross-agent replay test owed. |
| AC6 enforced network isolation | Enforcement proven | k3s enforces NetworkPolicy — proven by removing the policy and watching the same connection succeed. Node-local/metadata clauses need the egress policy. |
| AC7 fail closed | Implemented | uid-map assertion; the HOME-only silent downgrade is gone. Control-plane status surfacing owed. |
| AC8 adversarial suite | To build | Versioned escape suite as a gate. |
| AC9 completion gate | Blocked | On AC2 node loss and AC4 cross-node. |

### Verification methodology

AC6 is the criterion most likely to be satisfied by a test that proves nothing.
A blocked connection to a port that was never listening is indistinguishable
from an enforced policy — the first version of this check "passed" that way. The
suite therefore proves enforcement positively: bind a listener, confirm
same-pod loopback reaches it, confirm a second pod cannot, then remove the
policy and confirm the same connection succeeds.

### Platform prerequisite

User namespaces require Kubernetes >= 1.33 and containerd >= 2.0 on every node
that schedules agents. Razer satisfies this (v1.34.6 / containerd 2.2.2). The
nested dev cluster does not (v1.31.5 / containerd 1.7.23) and silently ignores
`hostUsers`, so it must be upgraded before it can verify the central claim. The
canary cluster must be checked for the same.

## Assumptions worth stating

The egress half of the boundary is expressed as CIDR ranges, so it withholds the
node, the kubelet, the API server and the metadata endpoint **only where those
addresses are private**. Every cluster we run satisfies that; a cluster with
publicly addressed nodes does not, and neither the rendered policy nor a passing
isolation suite on an RFC1918 cluster would reveal it. Operators on such a
topology have to add their ranges explicitly.

## Non-goals

Node-management agents are out of scope, per #78. If Kyber later supports them
they get a separate, conspicuous trust tier — not a flag on the normal agent
profile.
