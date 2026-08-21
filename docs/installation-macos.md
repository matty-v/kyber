# Installing Kyber on macOS

Kyber runs on a Mac the same way it runs anywhere else: in a Kubernetes cluster.
macOS cannot run Linux containers directly, so the cluster lives inside a Linux
VM on your Mac, and Kyber installs into that cluster from the published Helm
chart.

> **Verification status.** The maintainer's own fleets run on Linux (k3s) and on
> GCP. This guide is written from the same chart and the same verify gates as
> [the quickstart](./product/getting-started/quickstart.md), but the macOS path is not yet part of the
> maintainer's regular testing. If a step here does not behave as described,
> please [open an issue](https://github.com/matty-v/kyber/issues).

## Does Kyber support macOS?

As of v1.0.5, Kyber's container images are published for `linux/amd64` only.
That single fact drives everything below.

| Mac | Status | What it means |
|---|---|---|
| Intel | Works natively | The VM is `x86_64`, so the published images run as-is. |
| Apple Silicon (M1 and later) | Works through an `x86_64` VM | Run the Linux VM as `x86_64`. On macOS 13 and later, Apple's Virtualization framework plus Rosetta translates the images fast enough to be usable. An `arm64` VM cannot run the images at all: pods fail with `exec format error`. |

Native `arm64` images land in the release after v1.0.5. Until you are on a
version that has them, an Apple Silicon Mac is running translated `x86_64` code,
which costs performance and is the least-tested way to run Kyber. Check what a
version actually ships before assuming:

```bash
docker buildx imagetools inspect ghcr.io/matty-v/kyber-control-plane:v1.0.5
```

If the output lists only `linux/amd64`, the `x86_64` VM below is required.

## What you need

- macOS 13 (Ventura) or later, for the Virtualization framework and Rosetta
- At least 8 GB of RAM to spare and 60 GB of disk, since agents keep their whole
  filesystem
- Homebrew, plus `colima`, `kubectl` and `helm`:

  ```bash
  brew install colima kubectl helm
  ```

Colima runs a Linux VM with k3s inside it. This is the shape closest to the
maintainer's own Linux installs: agent pods sit directly on the VM's kernel
rather than nested inside Docker, which is where user namespaces and bind
mounts behave best. If you would rather use Docker Desktop or OrbStack, see
[Alternative: k3d on Docker Desktop or OrbStack](#alternative-k3d-on-docker-desktop-or-orbstack).

## 1. Start the cluster VM

**Apple Silicon.** Rosetta translation is what makes the `amd64` images usable:

```bash
colima start kyber \
  --arch x86_64 \
  --vm-type vz --vz-rosetta \
  --cpu 4 --memory 8 --disk 60 \
  --kubernetes
```

**Intel Mac.** No translation needed:

```bash
colima start kyber \
  --cpu 4 --memory 8 --disk 60 \
  --kubernetes
```

**Verify.** The node is `Ready` and reports an `amd64` architecture:

```bash
kubectl config use-context colima-kyber
kubectl get nodes -o wide
kubectl get node -o jsonpath='{.items[0].status.nodeInfo.architecture}{"\n"}'
```

Expected: one `Ready` node, and `amd64`. If that prints `arm64`, the VM is
running natively and Kyber's images will not start. Delete it
(`colima delete kyber`) and start again with `--arch x86_64`.

## 2. Check the node can host agent pods

Agent pods need two things from the node: a namespace that admits
`CAP_SYS_ADMIN` (k3s does by default), and **user-namespace support**, which
means Kubernetes 1.33 or newer with containerd 2.0 or newer.

```bash
kubectl get node -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion} {.items[0].status.nodeInfo.containerRuntimeVersion}{"\n"}'
```

Expected: `v1.33.x` or newer, and `containerd://2.x`. Below that, Kubernetes
accepts the user-namespace setting and silently ignores it, so Kyber's agents
will refuse to start rather than run without the isolation they claim to have.
If your Colima k3s is older, recreate the VM with a newer Kubernetes.

No host device is needed. Earlier versions required `/dev/fuse` for overlay
persistence; agents now keep their root filesystem on their own volume instead.

You can see how an agent's root was prepared in its boot log:

```bash
kubectl -n kyber-system logs <agent-pod> | grep -i 'durable root\|user namespace'
```

## 3. Install Kyber

From here the install is identical to every other cluster. Follow
[the quickstart](./product/getting-started/quickstart.md) from § 1 to the end: it creates the namespace
and secrets, installs the chart, opens the PWA, and walks you through your first
machine and your first agent, with a verify step at each stage.

Two macOS-specific notes as you go:

- **Port-forwarding** is how you reach the PWA. `kubectl -n kyber-system
  port-forward svc/kyber-control-plane 8080:8080` works the same on a Mac, and
  `http://localhost:8080` opens in Safari or Chrome normally.
- **Pods start slowly on Apple Silicon.** Every image is being translated, so
  first pull and first boot take noticeably longer than the quickstart's timings
  suggest. Give `helm install --wait` the full `--timeout 10m`, and expect an
  agent's first boot to take a few minutes.

## Alternative: k3d on Docker Desktop or OrbStack

If you already run Docker Desktop or OrbStack, you can use k3d instead of
Colima. The cluster then runs nested inside a Docker container, which is a less
proven arrangement for agent pods but works for trying Kyber out.

On Apple Silicon, enable Rosetta in your Docker runtime's settings first, then
force `amd64` so the k3s node and everything inside it match the published
images:

```bash
brew install k3d
export DOCKER_DEFAULT_PLATFORM=linux/amd64
k3d cluster create kyber --no-lb --wait --image rancher/k3s:v1.34.6-k3s1
```

The `--image` pin matters: agents need Kubernetes >= 1.33 with containerd >= 2.0
for user namespaces, and k3d's default node image tracks its own release rather
than the newest k3s.

Then continue from [§ 2](#2-check-the-node-can-host-agent-pods). The version
check there runs against the cluster, so it is the same command either way —
but note that agent pods nested inside Docker are the least reliable place for
user namespaces, which is why Colima is the recommended path.

## What is different on a Mac

- **No managed machines.** Kyber can provision real VMs on GCP. On a Mac there
  is no cloud provider, so your agents run on the single local node, the same as
  any other local install. See [clusters.md](./clusters.md).
- **The node-agent's machine actions do nothing useful.** Reboot and stop are
  aimed at cloud VMs.
- **Everything is on one disk.** Agent volumes come from k3s's `local-path`
  provisioner, so the VM disk you sized above is the ceiling for all agents
  combined.
- **The VM is the blast radius.** Agent pods are not privileged, and their
  `CAP_SYS_ADMIN` is namespaced by the user namespace so it carries no authority
  over the node. Whatever is left is contained inside the Colima VM rather than
  on macOS itself, which is one of the nicer properties of this shape. Still
  read the
  [threat model](../.github/SECURITY.md#deployment-threat-model).

## Teardown

```bash
helm uninstall kyber -n kyber-system
colima delete kyber
```

`colima delete` removes the VM and every agent's persistent volume with it.

## Where to go next

- [the quickstart](./product/getting-started/quickstart.md) for the install itself and your first agent
- [agents-comms.md](./agents-comms.md) to reach your agents from Telegram or
  Discord
- [upgrading.md](./upgrading.md) to move to a newer version later
