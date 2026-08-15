# Agent pod host-isolation design

Issue: kyber#76

## Decision

Agents remain unrestricted inside their own pod: they may become root, install
packages, and persist changes across pod replacement. That does not require the
container to be privileged on the host.

The default agent security context is therefore:

```yaml
privileged: false
capabilities:
  add: [SYS_ADMIN]
seccompProfile:
  type: RuntimeDefault
```

`/dev/fuse` remains the only explicitly mounted host device. `SYS_ADMIN` is
retained because the persistence entrypoint mounts overlayfs, FUSE, bind mounts,
and a chroot. Containerd's runtime-default seccomp profile is capability-aware
and admits `mount` when `SYS_ADMIN` is present.

Dropping privileged mode is the critical boundary change. Privileged containers
receive all capabilities, bypass seccomp/AppArmor/SELinux confinement, see host
devices, and receive an unrestricted device cgroup. A non-privileged container
with one added capability no longer receives those implicit privileges. In
particular, host block devices are not populated into `/dev`, and a fabricated
block-device node is rejected by the runtime's device allow-list.

Three cluster-level settings are exposed:

| Helm value | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `agent.security.privileged` | `KYBER_AGENT_PRIVILEGED` | `false` | Break-glass restoration of the legacy profile. |
| `agent.security.userNamespaces` | `KYBER_AGENT_USER_NAMESPACES` | `false` | Set `pod.spec.hostUsers: false` after target validation. |
| `agent.security.seccompProfile` | `KYBER_AGENT_SECCOMP_PROFILE` | `RuntimeDefault` | Permit `Unconfined` only as a mount-compatibility fallback. |

The settings are cluster-scoped because all agents have the same persistence
requirements and no supported workload needs host access. A per-Agent privilege
field would add CRD/API/PWA surface without a current product use case.

## User namespaces

User namespaces are the stronger second layer. With `hostUsers: false`, uid 0
and `SYS_ADMIN` inside the pod map to an unprivileged uid and namespaced
capability on the host. This keeps in-pod root useful while neutralizing it
against host-owned resources.

They are opt-in because Kubernetes requires runtime support and idmapped-mount
support from every filesystem used by the pod. Existing persist PVCs must be
verified on a canary before fleet enablement. A target lacking that support can
fail container creation before Kyber's entrypoint runs.

Inside a user namespace, the persistence dispatcher tries fuse-overlayfs first.
Kernel overlayfs remains the fallback because FUSE device access can itself be
unavailable under user namespaces. Outside a user namespace, kernel overlayfs
remains first for performance.

Local k3d validation found that this fallback is not fully transparent:
`/dev/fuse` could not be opened (`EPERM`), and kernel overlayfs then rejected a
dpkg directory replacement while installing `figlet` with `EXDEV`. User
namespaces must therefore remain opt-in until a target proves both FUSE access
and representative system-package installs. Host isolation that makes user
namespaces mandatory requires a different persistence or runtime boundary.

## Gap dispositions

| Issue gap | Disposition |
| --- | --- |
| G1: fully privileged, no seccomp, full capability set | Closed for privilege, seccomp, and full capabilities. The runtime/node retains responsibility for its default AppArmor or SELinux policy; Kyber does not ship a mount-compatible custom MAC profile in this change. |
| G2: passwordless in-pod root | Accepted. Root is required for the agent product contract; user namespaces make it non-root on the host. |
| G3: host filesystem reachable through host block devices | Closed by default by removing privileged mode and its host-device/device-cgroup grants. User namespaces provide the stronger defense against capability misuse. |
| G4: Kubernetes API boundary bypassed through G3 | Closed with G3. Agent pod specs also set `automountServiceAccountToken: false`. |
| G5: no agent-to-agent network isolation | Closed when the cluster CNI enforces NetworkPolicy. Every agent pod has default-deny ingress; loopback communication inside the pod is unaffected. |

The runtime probes are exec probes and are unaffected by NetworkPolicy. Status
and messaging sidecars use HTTP probes from the kubelet; Kubernetes normally
allows traffic from the pod's node regardless of ingress policy, but operators
must verify that behavior on the target CNI along with general NetworkPolicy
enforcement.

## Rollout and verification

Roll out `privileged=false` to a canary and verify:

1. `/persist/kyber/boot-metadata.json` reports `kernel` or `fuse`, not the
   HOME-only fallback.
2. A package installed with `sudo apt-get` survives pod recreation.
3. `/dev/sd*` is absent, `Seccomp` in `/proc/self/status` is non-zero, and the
   capability bounding set is no longer the privileged all-capabilities set.
4. The issue's read-only host-device mount reproduction fails.

If the overlay fails specifically because of the runtime seccomp profile, use
`seccompProfile: Unconfined` temporarily; this does not restore host devices or
the privileged device cgroup. `privileged: true` is the final break-glass
rollback only.

Enable user namespaces separately on a fresh canary, then an existing-PVC
canary. Verify `/proc/self/uid_map` maps container uid 0 to a non-zero host uid,
and repeat the persistence and package-install checks before fleet rollout.
