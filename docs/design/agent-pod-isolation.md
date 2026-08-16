# Agent pod host-isolation design

Issues: kyber#76 (escape report), kyber#77 (de-privileging), kyber#78 (hard
isolation). The architectural decision and the measurements behind it live in
[ADR 0003](../adr/0003-agent-sandbox-isolation.md); this page is the operator's
view of the resulting boundary.

## The boundary

An agent is unrestricted **inside its own sandbox** — it may be root, install
packages, edit any system file, run services, and keep all of it across pod
replacement. None of that authority is valid **outside** the sandbox.

The default agent pod is:

```yaml
hostUsers: false                 # in-pod root maps to an unprivileged host uid
privileged: false
capabilities:
  add: [SYS_ADMIN]               # namespaced — no authority on the node
seccompProfile:
  type: RuntimeDefault
automountServiceAccountToken: false
```

No host devices. No hostPath volumes. Persistence is a directory on the agent's
own PersistentVolume, seeded from the base image and entered with `chroot` — not
an overlay mounted in-pod, and not `/dev/fuse`.

`SYS_ADMIN` is still needed for the bind, `proc` and `tmpfs` mounts that
assemble the chroot. Inside a user namespace those are namespaced operations.
That is the entire isolation argument: the capability is real inside the
sandbox and meaningless against the node.

## Cluster requirements

User namespaces need **Kubernetes >= 1.33 and containerd >= 2.0** on every node
that schedules agents.

Below that, Kubernetes **accepts `pod.spec.hostUsers: false` and silently
ignores it**. The pod is admitted, runs with no user namespace, and reports
nothing wrong — `/proc/self/uid_map` reads `0 0 4294967295`. A control plane
that trusted the PodSpec would report an isolated agent that is not isolated.

So the agent checks its own uid map at boot and refuses to start if it is not
remapped. On an unsupported cluster you will see the agent fail with a message
naming the cause, rather than an agent that looks healthy.

The other requirement is a CNI that actually **enforces NetworkPolicy**. k3s
does (its kube-router-based controller, unless started with
`--disable-network-policy`); GKE needs Dataplane V2 or Calico. An unenforcing
CNI does not fail — it makes every isolation check pass for free, which is
worse.

## Settings

| Helm value | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `agent.security.userNamespaces` | `KYBER_AGENT_USER_NAMESPACES` | `true` | Set `pod.spec.hostUsers: false`. Disable only to deliberately accept an unisolated agent. |
| `agent.security.requireUserNamespace` | `KYBER_AGENT_REQUIRE_USER_NAMESPACE` | `true` | Refuse to start outside a user namespace instead of running unisolated. |
| `agent.security.persistenceMode` | `KYBER_AGENT_PERSISTENCE_MODE` | `rootfs` | `rootfs` is the durable root. `overlay` is the pre-#78 in-pod mount, rollback only. |
| `agent.security.privileged` | `KYBER_AGENT_PRIVILEGED` | `false` | Break-glass restoration of the legacy profile. |
| `agent.security.seccompProfile` | `KYBER_AGENT_SECCOMP_PROFILE` | `RuntimeDefault` | `Unconfined` only as a mount-compatibility fallback. |
| `agent.security.egress.enabled` | — | `true` | Deny agents the infrastructure ranges. |
| `agent.security.egress.blockedCIDRs` | — | private + link-local + CGNAT | IPv4 ranges withheld. Cluster DNS and the control plane are allowed back by their own rules. |
| `agent.security.egress.blockedCIDRsV6` | — | ULA + link-local + loopback | The IPv6 half. Emptying it removes the v6 rule entirely, which denies ALL v6 egress. |
| `agent.security.egress.platformTrustAgents` | — | `[]` | Named agents exempted from the egress policy. |

These are cluster-scoped because every agent has the same sandbox requirements.
The one per-agent knob is the platform-trust list, and it is deliberately a
values entry rather than a CRD field so that granting it shows up in every diff.

## Network isolation

Agent pods get **default-deny ingress** — nothing on the pod network can open a
connection to them. Loopback inside the pod is unaffected, which is how the
runtime and its sidecars talk to each other.

Agent pods also get an **egress policy**: the public internet stays open, and
the private ranges are withheld — the node, the kubelet, the API server, the
cloud metadata service at `169.254.169.254`, and the pod CIDR where neighbouring
agents live. Denying ingress alone was not enough; before this an agent could
still reach the kubelet on `:10250` and scan the pod network for its neighbours.

Cluster DNS and the control plane fall inside the blocked ranges and are allowed
back by their own rules — a NetworkPolicy is the union of its rules, not a
sequence of overrides.

### The trust tier

`platformTrustAgents` names agents that keep unrestricted egress, including to
the node and the Kubernetes API. This exists for rescue agents that genuinely
hold cluster credentials. kyber#78 is explicit that host administration must not
be folded into the normal agent profile, so this is a separate and conspicuous
tier, empty by default.

## Verifying it, without fooling yourself

Every check below has a way of passing for the wrong reason. The methodology
matters more than the result.

**User namespace.** Read `/proc/self/uid_map` from inside a running agent. The
first line must map container uid 0 to a non-zero host uid. Reading the PodSpec
proves nothing — see above.

**NetworkPolicy enforcement.** Do not conclude "blocked" from a failed
connection. A closed port and an enforced policy are indistinguishable. Bind a
listener, confirm same-pod loopback reaches it, confirm a second pod cannot,
then **delete the policy and confirm the same connection succeeds**. Only the
last step proves enforcement.

**Persistence.** `/persist/kyber/boot-metadata.json` reports the mode. It must
read `rootfs*`, never `bind-mount-home`. Install a package with `apt`, delete
the pod, and confirm the binary is still there and still runs.

**Host access.** Confirm `/dev/sd*` is absent, `Seccomp` in `/proc/self/status`
is non-zero, the capability bounding set is not the privileged all-caps set, and
that creating a device node fails. Under a user namespace `mknod` itself returns
`EPERM`.

## Gap dispositions

| Issue gap | Disposition |
| --- | --- |
| G1: privileged, no seccomp, full capability set | Closed. The node retains responsibility for its own AppArmor/SELinux policy; Kyber does not ship a mount-compatible custom MAC profile. |
| G2: passwordless in-pod root | Accepted by design. Root is the agent product contract; the user namespace is what makes it harmless to the host. |
| G3: host filesystem reachable through host block devices | Closed. No privileged mode, no host devices, no hostPath volumes, and `CAP_MKNOD` is namespaced. |
| G4: Kubernetes API boundary bypassed through G3 | Closed with G3, plus `automountServiceAccountToken: false` and the egress policy. |
| G5: no agent-to-agent network isolation | Closed where the CNI enforces NetworkPolicy: default-deny ingress, plus egress that withholds the pod CIDR. |

## Known limits

**The egress guarantee is CIDR-shaped.** The policy withholds the node, the
kubelet, the API server and the metadata service by *address range*, so it holds
only where those sit inside the ranges in `blockedCIDRs`. That is true of every
normal cluster, but on a cluster with **publicly addressed nodes** the defaults
do not withhold them and the AC3/AC6 guarantees do not hold as written. Nothing
in the render can detect that, and an isolation suite passing on an RFC1918
cluster will not either — add your node and API-server ranges explicitly if that
is your topology.

Both address families are covered. An IPv4-only rule matches no IPv6 traffic at
all, which on a dual-stack cluster would deny every v6 connection — fail-closed,
but silently.


**Node loss.** Agent PVCs on `local-path` are node-local disk with hard node
affinity. If the node is replaced the volume does not come with it. This is a
storage-class property, unchanged by the sandbox work, and it means the durable
root survives pod replacement but not machine replacement on such targets.

**Kernel boundary.** The sandbox boundary is the user namespace, not a guest
kernel. A kernel exploit reachable from an unprivileged user namespace is not
defended against here. ADR 0003 records why a VM-isolated runtime was not
chosen and what would reopen that question.
