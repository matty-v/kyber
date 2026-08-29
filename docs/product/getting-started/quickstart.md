# Quickstart

Get a running Kyber with one live agent in about 15 minutes, on any Kubernetes
cluster, using the published Helm chart. No cloud account, no Terraform, no
GitOps tooling, no fork. Every stage has a verify step, plus a recovery pointer
when something does not match.

This is the shortest honest path. When you want a permanent install with a real
public URL and cloud compute, pick a path from the
[install options](./installation.md): the [full GCP guide](../../installation.md)
and the [WSL2 guide](../../installation-wsl2.md) both start from the same
`helm install` you run here.

> [!NOTE]
> **What you get.** A Kyber control plane, its PWA, Postgres, Redis, and one
> Claude Code agent running in a pod with whole-disk persistence. Agents
> schedule onto the cluster node you already have, and no VMs are provisioned.
> Cloud machine provisioning is the only part of Kyber this quickstart does not
> exercise.

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| `kubectl` | ≥ 1.33 | within one minor of the cluster; the cluster floor is 1.33 |
| `helm` | ≥ 3.14 | needs OCI registry support, which 3.14 has by default |
| `openssl` | any | generating secrets |
| `curl`, `jq` | any | the verify steps |

Plus **a Kubernetes cluster running 1.33 or newer with containerd 2.0 or
newer**, which admits `CAP_SYS_ADMIN`. Agents run in a user namespace so that
in-pod root maps to an unprivileged uid on the node, and that needs those
versions. Below them Kubernetes accepts the setting, ignores it, and the
agents refuse to start rather than run unisolated. See the
[deployment threat model](../../../.github/SECURITY.md#deployment-threat-model)
and the [agent pod isolation design](../../design/agent-pod-isolation.md).

Check an existing cluster with:

```bash
kubectl get nodes -o custom-columns=\
NAME:.metadata.name,K8S:.status.nodeInfo.kubeletVersion,RUNTIME:.status.nodeInfo.containerRuntimeVersion
```

No cluster? [k3d](https://k3d.io) gives you one on Docker in about 30 seconds.
Pin an image new enough to satisfy the above, because the k3d default tracks
its own release, which may be older than your k3d binary suggests:

```bash
k3d cluster create kyber --no-lb --wait --image rancher/k3s:v1.34.6-k3s1
```

Everything below assumes `kubectl` is pointed at that cluster.

## 1. Create the namespace and secrets

**What this does:** creates `kyber-system` and the secrets Kyber needs before
it starts. Everything is generated locally. Nothing is fetched, and nothing
here is committed anywhere.

```bash
kubectl create namespace kyber-system --dry-run=client -o yaml | kubectl apply -f -

# Keep the API key. It unlocks every /api/v1/* endpoint, so treat it as a root
# password and put it in your password manager now.
export KYBER_API_KEY=$(openssl rand -hex 32)

kubectl -n kyber-system create secret generic kyber-internal-signing-key \
  --from-literal=signing-key="$(openssl rand -hex 32)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

> [!IMPORTANT]
> **The signing key is not optional.** It authenticates the internal API on
> `:8082`, which is how each agent pod reports its status. The chart does not
> generate it; it is delivered out of band by design, so a cluster's signing
> key never lives in a values file. Without it that API **fails closed**: the
> control plane starts and serves normally, but every agent stays blank and
> never reports activity. See
> [internal-api-auth.md](../../architecture/internal-api-auth.md).

**Verify:**

```bash
kubectl -n kyber-system get secret kyber-internal-signing-key \
  -o jsonpath='{.data}' | jq -r 'keys | join(",")'
```

Expected: `signing-key`.

## 2. Install Kyber

**What this does:** installs the published chart. It carries its own image tags,
stamped at release time, so there is nothing to pin by hand.

```bash
helm install kyber oci://ghcr.io/matty-v/charts/kyber \
  --version 1.3.0 \
  --namespace kyber-system \
  --set namespace.create=false \
  --set api.apiKey="$KYBER_API_KEY" \
  --wait --timeout 10m
```

Use the newest version from [Releases](https://github.com/matty-v/kyber/releases)
in place of `1.3.0`; the chart version and the release tag are the same number.

Every other default is already right for this install: compute is `mock`,
the node-agent DaemonSet is on, agent volumes bind against your cluster's
default StorageClass, and the internal API starts in grace mode.

> [!NOTE]
> **Pull the chart, not the repo.** `deploy/helm/kyber` in the git tree is the
> same chart *before* release stamping: its image tags are deliberately empty
> and it refuses to render without you supplying every one of them. That is
> correct for a GitOps repo pinning digests, and wrong for a first install.
> Install from `oci://` unless you specifically want to pin images yourself.

**Verify:** all four pods `Running`, and the control plane answering.

```bash
kubectl -n kyber-system get pods
```

Expected: `kyber-control-plane`, `kyber-node-agent`, `kyber-postgres-0`, and
`kyber-redis-0`, all `Running`.

```bash
kubectl -n kyber-system port-forward svc/kyber-control-plane 8080:8080 &
sleep 3

curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/healthz
curl -s -H "Authorization: Bearer $KYBER_API_KEY" \
  http://localhost:8080/api/v1/fleet/summary
```

Expected: `200`, then
`{"machineCount":0,"agentCount":0,"agentsByPhase":{},"machinesByPhase":{}}`.

A `401` on the second call means the key in your shell and the key in the
cluster disagree; re-check `$KYBER_API_KEY`. Anything but `200` on the first
means the control plane itself is unhealthy:
`kubectl -n kyber-system logs deploy/kyber-control-plane`.

## 3. Open the PWA

The PWA is served at `/` by the same control plane, on the port-forward you
already have open:

```
http://localhost:8080/
```

On first load it asks for the API key. Paste `$KYBER_API_KEY`. The console
exchanges it once for a session cookie rather than keeping the key in browser
storage; even so, use a browser you trust on a machine you trust.

Lost the key already? It is in the cluster. The Secret is named after the Helm
release, so it is `kyber-api-credentials` for the `kyber` release used here:

```bash
kubectl -n kyber-system get secret kyber-api-credentials \
  -o jsonpath='{.data.api-key}' | base64 -d; echo
```

You should land on the Fleet Overview with zero machines and zero agents.

## 4. Create a machine

A machine is where agents run. With the `mock` provider it is a record that
attaches to the Kubernetes node you already have, so creating one provisions
nothing and reaches `Ready` immediately. Nothing about the *agent* is
simulated; only VM provisioning is skipped.

In the PWA: **Machines → Create Machine**, name it `local`, provider `mock`,
and create it. Or over the API:

```bash
curl -sS -X POST http://localhost:8080/api/v1/machines \
  -H "Authorization: Bearer $KYBER_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"local","provider":"mock","capacity":{"cpu":"2","memory":"4Gi"}}'
```

> [!NOTE]
> **`mock` is being renamed to `static`,** which says what it actually does.
> Releases after 1.0.4 accept both; `mock` keeps working as an alias, so the
> command above is correct either way.

**Verify:**

```bash
curl -s -H "Authorization: Bearer $KYBER_API_KEY" \
  http://localhost:8080/api/v1/machines/local | jq -r '.status.phase'
```

Expected: `Ready`.

## 5. Create your first agent

Agents need real Anthropic credentials, so this step needs you and a browser.
The PWA runs the whole OAuth exchange:

1. **Agents → Create Agent.**
2. Name it `dave`, machine `local`, runtime `claude-code`. There is no model to
   pick here: a new agent inherits the fleet default model, and you can change
   it later from the agent page.
3. Click **Authorize with Claude.** The PWA generates a PKCE verifier and opens
   Anthropic's authorize page in a new tab.
4. Sign in, approve the grant, and copy the authorization code Anthropic shows
   you.
5. Paste it into **Authorization code**, then click **Create Agent**.

Kyber exchanges the code for access and refresh tokens, stores them in a
`dave-oauth` Secret, and creates the Agent. Every later pod boot refreshes the
access token from the stored refresh token on its own, so the agent comes up
already logged in; there is no interactive `/login`, now or ever again.

**Verify:** the agent reaches `Running` and its pod reports all containers
ready.

```bash
kubectl -n kyber-system get agent dave
kubectl -n kyber-system get pod agent-dave
```

Expected: the Agent's phase moves `Creating` → `Starting` → `Running`, and the
pod's `READY` column shows both numbers equal.

Don't be surprised by the count. One container is the agent itself; the other
four are sidecars Kyber injects for status reporting, transcript tailing,
session saving, and transcript pruning, so a channel-free agent is `5/5` today,
plus one more container per chat channel you enable.

Confirm it really authenticated rather than merely started:

```bash
kubectl -n kyber-system logs agent-dave -c agent | grep -iE 'credentials|model probe'
```

Expected: `credentials.json written` and `pre-flight model probe: ok`. A
`Running` pod whose log shows neither is an agent that booted without working
credentials; it will sit idle rather than fail. See
[wedged-agent-recovery.md](../../operator/wedged-agent-recovery.md).

Open the agent in the PWA and use the terminal tab to talk to it. One heads-up:
the agent's Activity tab reads from a durable log archive bucket that this
quickstart does not configure, so expect an error there; the terminal and live
logs work without it.

## Where to go next

| You want | Read |
|---|---|
| All install options at a glance | [installation.md](./installation.md) |
| A permanent install on GCP | [the full GCP guide](../../installation.md) |
| A permanent install on a Windows laptop | [the WSL2 guide](../../installation-wsl2.md) |
| A public HTTPS URL for the PWA | [the GCP guide § 11](../../installation.md#11-https-via-tailscale-funnel) |
| Talk to agents from Telegram or Discord | [agents-comms.md](../../agents-comms.md) |
| Give agents persistent memory and a persona | [agents-identity-repos.md](../../agents-identity-repos.md) |
| Upgrade this install | [upgrading.md](../../upgrading.md) |
| Hack on Kyber itself | [the local dev environment guide](../../../scripts/devenv/full-local.md) |

## Teardown

> [!CAUTION]
> **Delete the agents first.** Each Agent carries a `kyber.io/agent-cleanup`
> finalizer that only the control plane can clear. Uninstall the release while
> Agents still exist and you remove the very controller that would release
> them; the namespace then hangs in `Terminating` forever.

```bash
kubectl -n kyber-system delete agent --all
kubectl -n kyber-system delete machine --all

helm uninstall kyber -n kyber-system
kubectl delete namespace kyber-system
```

That deletes the Postgres and Redis PVCs with the namespace, which **wipes all
agent state**, including each agent's persistent disk. If you created agents
with identity repos, those live on GitHub and survive.

If you already stranded a namespace this way, clear the finalizer by hand and it
finishes on its own:

```bash
kubectl -n kyber-system patch agent <name> --type merge -p '{"metadata":{"finalizers":[]}}'
```

If you made a k3d cluster for this, `k3d cluster delete kyber` removes
everything at once and sidesteps all of the above.
