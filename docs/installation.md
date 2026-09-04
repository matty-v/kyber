# Installing Kyber on GCP

Step-by-step guide to deploying a production Kyber instance on Google Cloud.

This guide describes the direct-GCE/k3s installation. For GKE Standard with a
dedicated platform pool and per-Machine node pools, use
[Install Kyber on GKE](installation-gke.md).
This document is the source of truth for installation — if you hit a step that
doesn't match reality, fix the doc.

> **Just want to see Kyber run?** [the quickstart](./product/getting-started/quickstart.md) gets you a
> working instance with one live agent in ~15 minutes on any cluster, with no
> cloud account. This guide is the permanent version of the same install.

> **Running Kyber on a single machine (laptop / home server)?** See
> [installation-wsl2.md](./installation-wsl2.md) for the standalone WSL2 path —
> native k3s, no Terraform, Tailscale Funnel for HTTPS.

> **Cluster naming:** this guide installs the `kyber-gcp` cluster. The Helm
> release uses that same name. See [clusters.md](./clusters.md) for the
> convention.

> **Status:** V1 small profile — single-VM k3s cluster. Multi-node and HA are
> future work.

## Architecture

A production Kyber install is one GCE VM running k3s, with the Kyber control
plane + PWA + node agent installed from the published Helm chart. Agent pods run
on that same VM (or on additional VMs provisioned via the Machine Controller).
PostgreSQL and Redis run in-cluster as first-party StatefulSets
(`postgres:16-alpine` and `redis:7-alpine`) — no Bitnami sub-charts required.

```
┌─────────────────────────────────────────────────────────┐
│  GCP Project (your-gcp-project)                         │
│                                                         │
│  ┌───────────────────────────────────────────────────┐  │
│  │  VPC: kyber-small-network                         │  │
│  │  Subnet: 10.10.0.0/24                             │  │
│  │  Static external IP                               │  │
│  │                                                   │  │
│  │  ┌─────────────────────────────────────────────┐  │  │
│  │  │  VM: kyber-small-k3s-server                 │  │  │
│  │  │  (e2-standard-4, Ubuntu 24.04, 100GB disk)  │  │  │
│  │  │                                             │  │  │
│  │  │  k3s (single-server)                        │  │  │
│  │  │  ├─ Kyber control plane (Deployment)        │  │  │
│  │  │  │  ├─ Public API :8080 (PWA at /)          │  │  │
│  │  │  │  ├─ Internal API :8082                   │  │  │
│  │  │  │  ├─ Health probes :8081                  │  │  │
│  │  │  │  └─ Metrics :9090                        │  │  │
│  │  │  ├─ Kyber node-agent (DaemonSet)            │  │  │
│  │  │  ├─ PostgreSQL (StatefulSet)                │  │  │
│  │  │  └─ Redis (StatefulSet)                     │  │  │
│  │  └─────────────────────────────────────────────┘  │  │
│  └───────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘

External access: http://<static-ip>:8080 (via GCP firewall rule on port 8080)
```

### Ports

| Port | Exposed | Purpose |
|------|---------|---------|
| 22 | Firewall-gated | SSH (operator access) |
| 6443 | Open to world | k3s API — required by Machine Controller to register new VMs |
| 8080 | Open to world | Kyber public API + PWA |
| 443 | Open to world | Reserved for future HTTPS ingress |

The k3s `LoadBalancer` service type uses klipper-lb, which binds directly to the
VM's network interface — no separate cloud LB is needed.

## Prerequisites

Tools installed on your workstation:

- `gcloud` (authenticated against your project)
- `terraform` ≥ 1.7.0
- `kubectl` ≥ 1.30
- `helm` ≥ 3.14 (needs OCI registry support, on by default since 3.8)
- `git`, `jq`, `openssl`

GCP project requirements:

- A project with billing enabled (this guide uses `your-gcp-project` as a
  placeholder — substitute your own project ID everywhere it appears)
- Compute Engine API enabled (`gcloud services enable compute.googleapis.com`)
- IAM role on your user: Owner, or Editor + Compute Admin + Service Account User
- gcloud authenticated: `gcloud auth login` (user credentials)

That is the whole list. You do **not** need a GitHub account, a Personal Access
Token, or access to any private repository to install Kyber: the chart and all
container images are published publicly to GHCR and pull anonymously.

> **Important — credential source for Terraform.** The Google Terraform provider
> looks for credentials in this order: `GOOGLE_APPLICATION_CREDENTIALS` env var →
> `GOOGLE_OAUTH_ACCESS_TOKEN` env var → gcloud ADC at
> `~/.config/gcloud/application_default_credentials.json` → GCE metadata server.
>
> If your workstation has `GOOGLE_APPLICATION_CREDENTIALS` set to a service
> account key with **read-only** permissions on compute (common when the same
> machine runs monitoring tooling), `terraform apply` will fail with
> `403 Forbidden` on `compute.networks.create` and `iam.serviceAccounts.create`.
>
> **Workaround** (until ADC is properly configured): unset the env var and pass
> the user's access token directly for the apply:
>
> ```bash
> env -u GOOGLE_APPLICATION_CREDENTIALS \
>   GOOGLE_OAUTH_ACCESS_TOKEN="$(gcloud auth print-access-token)" \
>   terraform apply ...
> ```
>
> The token is short-lived (1 hour). For a single `terraform apply` this is fine;
> for longer-running workflows, run `gcloud auth application-default login` once
> to set up ADC properly.

## 1. Generate secrets

Kyber needs three secrets. Generate them all now and save them to a gitignored
file.

```bash
mkdir -p ~/.config/kyber
umask 077
cat > ~/.config/kyber/gcp-secrets.env <<EOF
KYBER_API_KEY=$(openssl rand -hex 32)
KYBER_SIGNING_KEY=$(openssl rand -hex 32)
POSTGRES_PASSWORD=$(openssl rand -hex 24)
EOF
```

**The API key** unlocks every `/api/v1/*` endpoint. Treat it like a root
password. Store it in a password manager alongside the GCP project credentials.
Its full lifecycle — rotation, revocation, scopes — is in
[`api-keys.md`](api-keys.md).

**The signing key** authenticates the internal API on `:8082`, which every agent
pod uses to report status. It is delivered out of band on purpose, so a
cluster's signing key never lives in a values file or a git repo. The chart does
not generate it. Without it the internal API **fails closed**: the control plane
starts and serves normally, but agents stay blank and never report activity. See
[internal-api-auth.md](./architecture/internal-api-auth.md).

## 2. Provision infrastructure with Terraform

Clone the repo if you haven't already:

```bash
git clone https://github.com/matty-v/kyber.git
cd kyber
```

Initialize and apply the small profile:

```bash
cd infra/terraform
terraform init

# See the Prerequisites callout above for why we override credentials here.
env -u GOOGLE_APPLICATION_CREDENTIALS \
  GOOGLE_OAUTH_ACCESS_TOKEN="$(gcloud auth print-access-token)" \
  terraform apply -auto-approve \
  -var="project_id=your-gcp-project" \
  -var="profile=small" \
  -var="region=us-central1" \
  -var="zone=us-central1-a" \
  -var="machine_type=e2-standard-4" \
  -var="disk_size_gb=100"
```

Terraform creates:

- VPC `kyber-small-network` and subnet `kyber-small-subnet`
- Static external IP `kyber-small-ip`
- Firewall rules for SSH (22), k3s API (6443), Kyber API (8080), HTTPS (443),
  ICMP, and intra-subnet traffic
- Service account with logging, monitoring, artifact-registry-reader, and
  compute.instanceAdmin.v1 roles
- GCE VM `kyber-small-k3s-server` running Ubuntu 24.04 LTS with a startup script
  that installs k3s server (traefik disabled)

Expect the apply to take 3–5 minutes. Watch for the VM status to reach `RUNNING`
and verify k3s finished its startup script:

```bash
VM_IP=$(terraform output -raw control_plane_ip)
gcloud compute ssh kyber-small-k3s-server --zone=us-central1-a \
  --command "sudo tail -20 /var/log/kyber-k3s-install.log"
```

The last line should read `Kyber k3s install complete. Cluster is ready.`

If the startup script errored, SSH in and inspect
`/var/log/kyber-k3s-install.log`. Fix the script
(`infra/terraform/scripts/k3s-install.sh`), then re-run. The script is
idempotent.

> **Note on TLS SAN.** The startup script reads the VM's external IP from GCE
> instance metadata and writes it to `/etc/rancher/k3s/config.yaml` as a
> `tls-san` entry BEFORE installing k3s. This is required so the k3s server
> certificate is valid for the external IP — without it, kubectl from the
> operator's workstation fails TLS verification when talking to
> `https://<external-ip>:6443`. If you find yourself needing to add a SAN after
> the fact (e.g., the VM gets a new IP), SSH in and:
> ```bash
> sudo sed -i "s|external-ip|$(curl -sfH 'Metadata-Flavor: Google' http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip)|" /etc/rancher/k3s/config.yaml
> sudo rm -f /var/lib/rancher/k3s/server/tls/dynamic-cert.json
> sudo systemctl restart k3s
> ```

## 3. Fetch kubeconfig and join token

The kubeconfig and k3s join token only exist on the VM after k3s boots. Pull them
locally:

```bash
mkdir -p ~/.kube

# The k3s.yaml is root-owned on the VM. gcloud ssh with sudo cat is the
# most reliable way to get it (scp needs the file to be readable first).
gcloud compute ssh kyber-small-k3s-server --zone=us-central1-a \
  --command "sudo cat /etc/rancher/k3s/k3s.yaml" \
  > ~/.kube/kyber-gcp.yaml

# Rewrite the server URL from the in-cluster 127.0.0.1 to the external IP.
# The startup script already added the external IP as a tls-san, so the
# certificate is valid for it.
VM_IP=$(cd ~/dev/kyber/infra/terraform && \
  env -u GOOGLE_APPLICATION_CREDENTIALS \
  GOOGLE_OAUTH_ACCESS_TOKEN="$(gcloud auth print-access-token)" \
  terraform output -raw control_plane_ip)
sed -i "s|127.0.0.1|$VM_IP|" ~/.kube/kyber-gcp.yaml

export KUBECONFIG=~/.kube/kyber-gcp.yaml
kubectl get nodes  # should show the k3s node as Ready
```

Fetch the k3s join token (needed for the Machine Controller to provision
additional k3s workers later):

```bash
K3S_JOIN_TOKEN=$(gcloud compute ssh kyber-small-k3s-server --zone=us-central1-a \
  --command "sudo cat /var/lib/rancher/k3s/server/node-token" \
  | tr -d '[:space:]')
K3S_SERVER_URL="https://${VM_IP}:6443"

# Append so the install step can read them
cat >> ~/.config/kyber/gcp-secrets.env <<EOF
K3S_JOIN_TOKEN=$K3S_JOIN_TOKEN
K3S_SERVER_URL=$K3S_SERVER_URL
EOF
```

## 4. Create the cluster secrets

All four secrets go into the `kyber-system` namespace before the install. None of
them are ever written to a values file.

```bash
export KUBECONFIG=~/.kube/kyber-gcp.yaml
source ~/.config/kyber/gcp-secrets.env

kubectl create namespace kyber-system --dry-run=client -o yaml | kubectl apply -f -

kubectl -n kyber-system create secret generic kyber-api-credentials \
  --from-literal=api-key="$KYBER_API_KEY" \
  --dry-run=client -o yaml | kubectl apply -f -

# The key names matter: the control-plane Deployment reads exactly
# k3s-join-token and k3s-server-url from whichever Secret k3s.existingSecret
# names. A Secret with differently-named keys leaves the Machine Controller
# unable to provision k3s workers, with nothing obviously wrong in the logs.
kubectl -n kyber-system create secret generic kyber-k3s-credentials \
  --from-literal=k3s-join-token="$K3S_JOIN_TOKEN" \
  --from-literal=k3s-server-url="$K3S_SERVER_URL" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n kyber-system create secret generic kyber-internal-signing-key \
  --from-literal=signing-key="$KYBER_SIGNING_KEY" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n kyber-system create secret generic kyber-postgres-credentials \
  --from-literal=postgres-password="$POSTGRES_PASSWORD" \
  --dry-run=client -o yaml | kubectl apply -f -
```

**Verify** all four exist with the right keys:

```bash
for s in kyber-api-credentials kyber-k3s-credentials \
         kyber-internal-signing-key kyber-postgres-credentials; do
  printf '%-28s %s\n' "$s" \
    "$(kubectl -n kyber-system get secret $s -o jsonpath='{.data}' | jq -r 'keys|join(",")')"
done
```

Expected:

```
kyber-api-credentials        api-key
kyber-k3s-credentials        k3s-join-token,k3s-server-url
kyber-internal-signing-key   signing-key
kyber-postgres-credentials   postgres-password
```

> **Why Postgres uses a pre-provisioned Secret.** The chart's
> `postgresql.auth.existingSecret` points at a Secret that must exist before the
> first render. Generating the password inside the chart regenerates it on every
> render, which rotates the in-cluster Postgres password out from under running
> pods. Do **not** also set `postgresql.auth.password` — if both are present the
> chart prefers `existingSecret`, but a plaintext password in the values file
> defeats the purpose.

## 5. Write your values file

The chart's defaults are correct for most of an install. You only need to state
what is specific to *your* cluster. Write this on your workstation — it holds no
secrets, so keep it wherever you keep infrastructure config:

```bash
cat > ~/.config/kyber/values-gcp.yaml <<'EOF'
namespace:
  create: false

api:
  # The Secret created in § 4. Overrides apiKey in the chart.
  existingSecret: kyber-api-credentials
  # Externally-reachable HTTPS URL. Used to build the signed inbound
  # webhook URLs shown in the console. Set this to your Funnel URL from
  # § 10, or to http://<VM_IP>:8080 if you are not using HTTPS yet.
  publicURL: "https://kyber.<your-tailnet>.ts.net"
  service:
    # LoadBalancer so klipper-lb binds the node IP and the API is reachable
    # from outside the cluster. The chart default is ClusterIP.
    type: LoadBalancer

compute:
  provider: gce
  gce:
    project: your-gcp-project
    # Must match the k3s server's network, or kubelet probes and pod-to-pod
    # traffic break across nodes. These are the small profile's names.
    network: kyber-small-network
    subnet: kyber-small-subnet

# Leave empty to reuse kyber-api-credentials; we created a dedicated Secret
# in § 4, so name it.
k3s:
  existingSecret: kyber-k3s-credentials

postgresql:
  auth:
    existingSecret: kyber-postgres-credentials

# GCE persistent disks for agent volumes. Requires the CSI driver — see
# "GCE Persistent Disk CSI Driver" below.
storage:
  gcePD:
    enabled: true
  agentStorageClass: kyber-pd

# Set this to enable git-backed agent identity repos — see § 6.
identityRepo:
  defaultOwner: ""
EOF
```

Then substitute your own project ID and tailnet name.

> **You do not pin image tags.** The published chart carries all eight image
> tags, stamped at release time, so it installs pin-free. That is the difference
> between the published chart and `deploy/helm/kyber` in the git tree: the git
> chart deliberately ships empty tags and refuses to render until you supply
> every one of them, which is correct for a GitOps repo pinning digests and
> wrong for an install. Pin explicitly only if you are running your own images —
> `--set image.controlPlane.tag=...` and friends.

## 6. Register the Kyber GitHub App (optional)

Kyber gives every agent a private GitHub-backed **identity repo** (memory,
skills, `SOUL.md`, session state all live there and persist across pod
restarts). That repo is managed **exclusively** by a per-install **Kyber
Platform GitHub App** — nothing is hardcoded to a particular app or account.
Instead of handing agents a broad Personal Access Token, the control plane
authenticates as this App and, on demand, mints a **short-lived token scoped to
just that one repo** (`contents:write`, ~1h) that the agent uses for both reads
and writes of its own identity repo. There is deliberately **no PAT fallback**
for the identity repo: if the App flow is misconfigured, identity-repo git fails
loudly instead of silently succeeding on a broader credential. The generic PAT
(if the agent has one) is used only for the agent's *other* repos. See
[agents-identity-repos.md](./agents-identity-repos.md) for the full credential
model.

Enabling the feature has two parts — (a) create the App and its
`kyber-github-app` Secret, and (b) point `identityRepo.defaultOwner` at the
account the App is installed on. **If either part is absent the feature disables
cleanly** — agents are created and run normally, just without a managed identity
repo (their identity is never backfilled with a PAT). Skip this section entirely
if you don't want it. Estimated time: ~5 min.

**1. Create the App.** Go to https://github.com/settings/apps/new (logged in as
the user whose account will own agent identity repos) and fill in:

- **GitHub App name:** `Kyber` (must be globally unique — try
  `Kyber-<yourhandle>` if taken)
- **Homepage URL:** `https://github.com/matty-v/kyber` (or your fork)
- **Webhook → Active:** uncheck (V1 does not use webhooks)
- **Repository permissions:**
  - Administration: **Read and write** (required to create new repos from the
    agent template)
  - Contents: **Read and write** (required to seed and let agents push their own
    memory/state)
  - Metadata: Read-only (GitHub auto-enables this)
- **Account permissions:** none
- **Subscribe to events:** none
- **Where can this GitHub App be installed?** → **Only on this account**

Click **Create GitHub App**.

**2. Generate the private key.** On the App's settings page:

1. Note the **App ID** (6–7 digit number near the top).
2. Scroll to **Private keys** → **Generate a private key**. A `.pem` file
   downloads. Save it somewhere secure.

**3. Install the App.** Click **Install App** in the left sidebar, click
**Install** next to your account, select **All repositories**, and confirm.
GitHub redirects you to `https://github.com/settings/installations/<number>` —
the **Installation ID** is the number in that URL.

> **"All repositories" is safe here.** The App's installation tokens are minted
> per-call and scoped to the single repo each agent needs. "All repositories" at
> install time just lets the App see newly created agent repos without a
> re-install each time.

**4. Create the `kyber-github-app` Secret.** From your workstation:

```bash
APP_ID=<paste from step 2>
INSTALLATION_ID=<paste from step 3>
PEM_PATH=<absolute path to the .pem downloaded in step 2>

kubectl -n kyber-system create secret generic kyber-github-app \
  --from-literal=app-id="$APP_ID" \
  --from-literal=installation-id="$INSTALLATION_ID" \
  --from-file=private-key.pem="$PEM_PATH" \
  --dry-run=client -o yaml | kubectl apply -f -
```

The control plane reads this Secret at startup via
`pkg/githubapp.LoadConfigFromSecret`. If the Secret is missing or malformed, the
control plane logs `GitHub App Secret not loaded — identity-repo feature
disabled` and continues to start normally with the feature off — agents that
don't use an identity repo are unaffected, and the internal
`identity-repo-token` endpoint returns `503` so a git op against an identity repo
fails loudly rather than falling back to a PAT.

**5. Set `identityRepo.defaultOwner`** in `~/.config/kyber/values-gcp.yaml` to
the GitHub account/org the App is installed on:

```yaml
identityRepo:
  defaultOwner: "your-github-account"   # e.g. matty-v
```

This value flows to the control plane as `KYBER_IDENTITY_REPO_OWNER`. The chart
default is empty; leaving it empty (even with the Secret present) keeps
identity-repo scaffolding disabled.

**6. Shred the PEM copy on your workstation.** Once the Secret is applied, the
plaintext key on disk is redundant:

```bash
shred -u "$PEM_PATH"
```

> **Rotation.** When it's time to rotate, generate a new private key in the
> GitHub App UI, re-run step 4 with the new PEM, then delete the old key from the
> App UI. Installation tokens in flight continue to work until they expire
> (max 1h).

## 7. Install Kyber

Install the GCE PD CSI driver first — k3s does not bundle it, and the values file
above asks for GCE persistent disks:

```bash
kubectl apply -k "github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver/deploy/kubernetes/overlays/stable?ref=v1.15.0"
kubectl get pods -n gce-pd-csi-driver   # wait for Running
```

On GCE VMs the driver uses the instance's service account automatically.

Now install the chart. Pick the newest version from
[Releases](https://github.com/matty-v/kyber/releases) — the chart version and
the release tag are the same number:

```bash
helm install kyber-gcp oci://ghcr.io/matty-v/charts/kyber \
  --version 1.4.1 \
  --namespace kyber-system \
  -f ~/.config/kyber/values-gcp.yaml \
  --wait --timeout 15m
```

Resources are prefixed with the release name, so everything is `kyber-gcp-*`.

**Verify** the four workloads are up:

```bash
kubectl -n kyber-system get pods
```

Expected: `kyber-gcp-control-plane`, `kyber-gcp-node-agent`,
`kyber-gcp-postgres-0`, and `kyber-gcp-redis-0`, all `Running`.

## 8. Verify

> **Shared with the WSL2 install.** These assertions are near-identical to
> [installation-wsl2.md § 7](./installation-wsl2.md#7-verify) — keep them in sync.

The LoadBalancer service's external IP is the VM's static external IP (klipper-lb
uses the node network). Once the service has an IP:

```bash
SERVICE_IP=$(kubectl -n kyber-system get svc kyber-gcp-control-plane \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

curl -s -o /dev/null -w '%{http_code}\n' http://$SERVICE_IP:8080/healthz
# expect: 200

curl -s -H "Authorization: Bearer $KYBER_API_KEY" \
  http://$SERVICE_IP:8080/api/v1/fleet/summary
# expect: {"machineCount":0,"agentCount":0,"agentsByPhase":{},"machinesByPhase":{}}
```

If healthz works but the fleet endpoint returns 401, double-check the API key. If
healthz returns anything other than 200, inspect
`kubectl -n kyber-system logs deploy/kyber-gcp-control-plane`.

## 9. Open the PWA

The PWA is served at `/` by the same control plane binary. Open in a browser:

```
http://<VM_IP>:8080/
```

On first load, the Settings page will ask for the API key. Paste the value from
`~/.config/kyber/gcp-secrets.env`. The key is stored in browser `localStorage` —
treat the browser as a trusted device.

You should see the Fleet Overview page with zero machines and zero agents.
Success.

## 10. Create your first agent

From the PWA:

1. Go to **Machines** → **Create Machine**
2. Fill in: name `laptop`, provider `gce`, machine type `e2-standard-2`, zone
   `us-central1-a`, spot `false`
3. Click **Create**. The machine's phase should move Provisioning → Ready.

> **No cloud credentials handy?** Provider `mock` attaches to the Kubernetes node
> you already have instead of provisioning a VM, so you can create a working
> agent without exercising GCE at all. Releases after 1.0.4 also accept `static`,
> which is the same thing under a clearer name.

4. Go to **Agents** → **Create Agent**
5. Fill in: name `dave`, machine `<the machine you just created>`, runtime
   `claude-code`, and pick a model
6. Click **Authorize with Claude**. The PWA generates a PKCE verifier/challenge
   and opens Anthropic's authorize URL in a new tab.
7. Sign in at Anthropic, approve the scope grant, copy the authorization code
   shown on the Anthropic-hosted page.
8. Return to the PWA, paste the code into the **Authorization code** field.
9. Click **Create Agent**. Kyber exchanges the code + verifier for access and
   refresh tokens, stores them in a `dave-oauth` k8s secret, and creates the
   Agent CRD.
10. The agent moves Creating → Starting → Running. On every boot,
    `start-claude.sh` refreshes the access token from the stored refresh token
    and writes `~/.claude/.credentials.json`, so Claude Code starts with
    full-scope credentials — no interactive `/login` needed.

**Verify it authenticated**, rather than merely started:

```bash
kubectl -n kyber-system logs agent-dave -c agent | grep -iE 'credentials|model probe'
```

Expected: `credentials.json written` and `pre-flight model probe: ok`. A pod that
reaches Ready without those lines booted with credentials that don't work — it
will sit idle rather than fail. See
[wedged-agent-recovery.md](./operator/wedged-agent-recovery.md).

If the agent doesn't reach Running at all:

```bash
kubectl -n kyber-system describe agent dave
kubectl -n kyber-system logs pod/agent-dave
```

> **Legacy / scripted path:** If you need to create an agent without the PWA
> (e.g., from a script), you can still pass `secrets.oauthToken` directly in the
> `POST /api/v1/agents` request body. This accepts a long-lived refresh token
> obtained by other means. The PKCE flow above is preferred for interactive use.

### Scheduling recurring work on an agent

Agents ship with cron already running. `crontab -e` (user schedule) or a file in
`/etc/cron.d/` (system schedule) both persist across pod restarts and start
firing automatically on the new pod. See
[agents-scheduled-jobs.md](./agents-scheduled-jobs.md) for the supported surfaces
and how persistence works under the hood.

## 11. HTTPS via Tailscale Funnel

> **Shared with the WSL2 install.** This is near-identical to
> [installation-wsl2.md § 10](./installation-wsl2.md#10-https-via-tailscale-funnel).
> When touching the `tailscale up` invocation or Funnel config, update both.

The quickest path to a working HTTPS URL — zero cost, no domain purchase, no DNS
setup, no cert rotation — is Tailscale Funnel. The VM joins your existing tailnet,
`tailscale funnel` terminates HTTPS with a Let's Encrypt cert automatically
provisioned by Tailscale, and the public URL is
`https://<hostname>.<tailnet>.ts.net`. Funnel is free on all Tailscale plans.

**Prerequisites:**
- A Tailscale account with Funnel enabled (free on all plans as of late 2023).
- Your tailnet ACL allows the VM to run Funnel (default ACLs allow this).

**Generate an auth key** at https://login.tailscale.com/admin/settings/keys:
- Reusable: **off** (single-use is more secure; the key is consumed by
  `tailscale up`)
- Ephemeral: **off** (the VM persists across reboots)
- Pre-authorized: **on**
- Tags: leave empty unless your ACL requires them
- Expiration: 1 day

**Install and bring tailscale up on the VM:**

```bash
AUTHKEY='tskey-auth-XXXXXXXXXX'   # paste yours here, do not commit
gcloud compute ssh kyber-small-k3s-server --zone=us-central1-a --command "
  curl -fsSL https://tailscale.com/install.sh | sudo sh
  sudo tailscale up --authkey=$AUTHKEY --hostname=kyber --accept-dns=false --operator=\"\$USER\"
  sudo tailscale status
"
```

Flag rationale:
- `--accept-dns=false` keeps the VM's DNS resolution using the k3s ClusterDNS so
  in-cluster service lookups (`kyber-gcp-postgres`, `kyber-gcp-redis`, etc.) keep
  working.
- `--operator="$USER"` — **required on a fresh tailscaled install.** After
  `tailscale install.sh`, tailscaled sets a default `operator` preference. Any
  subsequent `tailscale up` invocation must mention it or the daemon rejects the
  command. See Troubleshooting below.

**Enable Funnel on port 8080:**

```bash
gcloud compute ssh kyber-small-k3s-server --zone=us-central1-a --command "
  sudo tailscale funnel --bg 8080
"
```

Tailscale prints the public URL — something like
`https://kyber.<tailnet>.ts.net/`. The cert is issued by Let's Encrypt and
auto-renewed every ~60 days by Tailscale.

> **Note:** the Tailscale CLI for Funnel changed in v1.66+. The older
> `tailscale funnel 443 on` syntax no longer works. Use
> `tailscale funnel --bg <port>` on v1.66 and newer.

**Set `api.publicURL`** to the Funnel URL in
`~/.config/kyber/values-gcp.yaml`, then apply it:

```bash
helm upgrade kyber-gcp oci://ghcr.io/matty-v/charts/kyber \
  --version 1.4.1 \
  --namespace kyber-system \
  -f ~/.config/kyber/values-gcp.yaml \
  --wait
```

**Verify from your workstation (or phone):**

```bash
curl -s https://kyber.<tailnet>.ts.net/healthz    # empty 200
curl -s https://kyber.<tailnet>.ts.net/assets/index-*.js -o /dev/null -w '%{http_code} %{content_type}\n'
# expect: 200 text/javascript; charset=utf-8

curl -s -H "Authorization: Bearer $KYBER_API_KEY" \
  https://kyber.<tailnet>.ts.net/api/v1/fleet/summary
# expect: {"machineCount":N,"agentCount":N,"agentsByPhase":{...},"machinesByPhase":{...}}

echo | openssl s_client -servername kyber.<tailnet>.ts.net \
  -connect kyber.<tailnet>.ts.net:443 2>/dev/null | \
  openssl x509 -noout -issuer -subject -dates
# expect: issuer=Let's Encrypt, subject=kyber.<tailnet>.ts.net, valid ~90 days
```

Open the PWA at `https://kyber.<tailnet>.ts.net/` in your browser. It loads over
HTTPS with a valid cert, the Service Worker can register (PWA install prompt
works), and API calls + WebSockets use `wss://` automatically.

### Why Tailscale Funnel over a custom domain?

- **Zero cost.** No domain purchase, no Cloud DNS zone, no LB fees.
- **Zero config.** Tailscale handles DNS, cert provisioning, and renewal.
- **Zero maintenance.** No cert rotation, no DNS TTL drift, no CA changes.
- **Public reachable.** Unlike `tailscale serve` (tailnet-only), Funnel exposes
  the service to the public internet — signed inbound webhooks work, phones on
  cellular work, etc.
- **Upgradeable.** If you later want a custom domain
  (`kyber.yourdomain.com`), install cert-manager + ingress-nginx in the cluster
  and point an A record at the VM IP. The plain HTTP API on `:8080` still works
  alongside Funnel.

### Alternative: cert-manager + ingress-nginx + custom domain

If you need a custom domain (e.g., for branding), replace this section with:

1. Buy a domain (Cloudflare Registrar is cheapest, ~$12/year)
2. Create an A record `kyber.yourdomain.com` → VM external IP
3. Install ingress-nginx:
   `helm install ingress-nginx ingress-nginx/ingress-nginx -n ingress-nginx --create-namespace`
4. Install cert-manager:
   `helm install cert-manager cert-manager/cert-manager -n cert-manager --create-namespace --set installCRDs=true`
5. Create a ClusterIssuer for Let's Encrypt (HTTP-01 or DNS-01 challenge)
6. Set `ingress.enabled=true` in your values file with
   `ingress.hosts[0].host=kyber.yourdomain.com` and the cert-manager annotation

This path is more k8s-native but takes ~30 minutes and costs $12/year. The Funnel
path takes ~5 minutes and costs nothing.

### Keeping the old HTTP access

The plain `http://<VM_IP>:8080` endpoint stays available alongside Funnel. Use it
for local debugging or if Tailscale is temporarily unreachable. GCP firewall rule
`kyber-small-allow-kyber-api` already exposes port 8080 to `0.0.0.0/0`. If you
want to lock it down to tailnet-only, delete that firewall rule via
`terraform destroy -target=module.profile[0].google_compute_firewall.allow_kyber_api`
and update the Terraform config.

## Upgrading

Once installed, a Kyber instance upgrades **itself**: Settings → Updates in the
PWA shows the current version, the latest on your channel, and an Install button.
There is no automatic apply and no fleet-wide action — each cluster is upgraded
on its own, deliberately.

To use it, set `selfUpgrade.enabled: true` in your values file; that is what
creates the upgrade Job's ServiceAccount. Full detail, including the guards and
what to do when an upgrade fails, is in [upgrading.md](./upgrading.md).

Rolling an upgrade by hand is just the install command with a newer
`--version`:

```bash
helm upgrade kyber-gcp oci://ghcr.io/matty-v/charts/kyber \
  --version <newer> -n kyber-system -f ~/.config/kyber/values-gcp.yaml --wait
```

> **Helm never upgrades CRDs.** `helm upgrade` installs CRDs only when they are
> absent, and silently leaves existing ones alone — so a cluster installed at an
> older version keeps that version's CRD schema forever, and new fields are
> rejected by the API server with no obvious cause. When a release notes a CRD
> change, apply them explicitly:
> ```bash
> helm pull oci://ghcr.io/matty-v/charts/kyber --version <newer> --untar
> kubectl apply -f kyber/crds/
> ```

## Optional: GitOps with ArgoCD

Nothing above needs ArgoCD, and a single-operator install is simpler without it.
If you already run ArgoCD and want Kyber under it, the chart is a normal Helm
source — point an `Application` at
`oci://ghcr.io/matty-v/charts/kyber` with your values file, and keep image tags
either unset (inherit the chart's stamped tags) or digest-pinned in the
Application.

Two things to know before you do:

- **Self-upgrade and GitOps are mutually exclusive.** If the control plane runs
  `helm upgrade` on itself while ArgoCD reconciles against a pinned revision,
  ArgoCD's `selfHeal` reverts the upgrade. Pick one delivery model per cluster —
  [upgrading.md § Two delivery models](./upgrading.md#two-delivery-models)
  explains both.
- **Preview/ApplicationSet installs must not own the Namespace**, or a prune can
  take the whole namespace with it.

## Agent Pod Requirements

Agent pods require two capabilities that go beyond what a standard
restricted-policy cluster allows. Both are set automatically by the Agent
Controller in `pkg/controllers/agent/pod_builder.go` — you don't need to
configure them manually. This section explains why they exist.

**User namespaces (required)**

Agent pods run with `hostUsers: false`, which maps in-pod uid 0 to an
unprivileged host uid. This needs **Kubernetes >= 1.33 and containerd >= 2.0**
on every node that schedules agents.

Below those versions Kubernetes accepts the setting and silently ignores it —
the pod runs unisolated and looks healthy. Kyber therefore checks the agent's
effective uid map at boot and refuses to start rather than run an agent that
believes it is isolated and is not. If your cluster cannot meet the version
requirement, set `agent.security.requireUserNamespace=false` to accept that
deliberately; nothing falls back to it on its own.

**Mount capability (`CAP_SYS_ADMIN`)**

Agent pods are not privileged. The runtime container receives only the
additional `SYS_ADMIN` capability needed for the bind, `proc` and `tmpfs`
mounts that assemble its chroot, together with a `RuntimeDefault` seccomp
profile. Inside the user namespace that capability is namespaced and carries no
authority over the node.

`SYS_ADMIN` is outside the Kubernetes `baseline` and `restricted` Pod Security
Standards, so agent pods still need a namespace that admits this capability. See
[`design/agent-pod-isolation.md`](design/agent-pod-isolation.md) for cluster
requirements and how to verify the boundary without fooling yourself.

**No host devices**

Agent pods receive no host devices and no hostPath volumes. Earlier versions
mounted `/dev/fuse` for overlay persistence; that is gone. The agent's root
filesystem is a directory on its own PersistentVolume, which needs nothing from
the host — and a hostPath device could not be delivered to a user-namespaced pod
in any case, since the kubelet idmaps hostPath volumes and devtmpfs rejects
idmapped mounts.

## Running your own images

The upstream images and chart are public and pull anonymously, so most installs
never need this. If you want to run your own build:

1. **Fork kyber** on GitHub to your own account.
2. **Add `GHCR_PAT`** to your fork's repo secrets (Settings → Secrets → Actions)
   with `write:packages` scope, so CI can push images to your GHCR namespace.
3. **Run CI on your fork** — the build pipeline publishes images to
   `ghcr.io/your-user/kyber-*` and the chart to `oci://ghcr.io/your-user/charts`.
4. **Make those packages public** (GitHub → package → Package settings → Danger
   Zone → Change visibility). New GHCR packages default to private, and
   user-owned packages can only be flipped in the web UI — the REST API's
   visibility endpoints only work for org-owned packages.
5. **Install from your own chart**, or point `image.*.repository` at your
   namespace in your values file.

If you'd rather keep the packages private, create a `docker-registry` Secret
named `ghcr-pull` in `kyber-system` using a `read:packages` PAT, and set
`imagePullSecrets: [{name: ghcr-pull}]` in your values file. That adds ongoing
PAT-rotation overhead — public is simpler for a single-operator setup.

## Troubleshooting

### Pods don't come up after install

```bash
kubectl -n kyber-system get pods
kubectl -n kyber-system describe pod <name>
```

If pods are in `ImagePullBackOff`, the image tag doesn't exist in GHCR or you
pinned a tag by hand that was never published. The published chart's stamped tags
always exist; check whether your values file overrides `image.*.tag`.

### Control plane CrashLoopBackOff

```bash
kubectl -n kyber-system logs deploy/kyber-gcp-control-plane --previous
```

Common causes:
- API key is empty → check the `kyber-api-credentials` secret
  and that `api.existingSecret` names it
- Postgres or Redis connection refused → check those pods are `Running` and the
  DSN env vars are correct (`kyber-gcp-postgres:5432`, `kyber-gcp-redis:6379`)
- RBAC missing → check the `kyber-gcp-control-plane` ClusterRole exists and is
  bound

### Agents stay blank and never report activity

The control plane is healthy and the PWA works, but agents show no status. The
`kyber-internal-signing-key` Secret is missing or wasn't picked up — the internal
API on `:8082` fails closed without it. Create it (§ 4) and restart the control
plane. A Secret patched *after* a pod starts never reaches that pod, so the
restart is required:

```bash
kubectl -n kyber-system rollout restart deploy/kyber-gcp-control-plane
```

### Agent pod stuck Pending on an unbound PVC

`describe pod` shows `unbound immediate PersistentVolumeClaims`. The
`storage.agentStorageClass` in your values file names a StorageClass the cluster
doesn't have. On GCE that means the GCE PD CSI driver isn't installed (§ 7); on
any other cluster, leave `agentStorageClass` empty so the cluster's default
StorageClass binds the volume.

On k3s, the default `local-path` StorageClass is directory-backed and does not
enforce the capacity requested by a PVC. Kyber treats an Agent's disk value as
a scheduling reservation and usage threshold in that configuration, not as a
filesystem quota. The Agent resource panel labels it **Disk reservation
(soft)** and separately shows pressure on the shared backing filesystem. Leave
headroom for the operating system and platform workloads; an Agent that writes
past its reservation can otherwise consume shared node space. Use a
quota-capable StorageClass when a hard per-Agent limit is required.

### PWA loads but API calls return 401

The API key in the browser doesn't match the one in the k8s secret. Open the PWA
Settings page and paste the correct key.

### Telegram messages don't reach agents

Telegram delivery is the in-pod sidecar long-polling the Bot API — there is no
server-side webhook route involved. Check that the agent is `Running`, that the
Telegram sidecar container in the agent pod is up, and that the sender's user ID
is on the agent's allowlist. A second process polling the same bot token (a
`409 Conflict` in the sidecar log) also silences delivery.

### LoadBalancer stuck in Pending

On k3s, klipper-lb takes a few seconds to assign. Wait 30 seconds. If it stays
Pending:

```bash
kubectl -n kube-system logs deploy/svclb-kyber-gcp-control-plane
```

Common cause: another service is already bound to port 8080 on the host. Free the
port or change `api.service.port`.

### `tailscale up` rejects with "changing settings requires mentioning all non-default flags"

After a fresh `tailscale install.sh`, tailscaled ships with a default `operator`
preference set. If you run `tailscale up` without `--operator`, the daemon
compares your flags against its saved defaults and rejects the command:

```
Error: changing settings via 'tailscale up' requires mentioning all non-default flags. To proceed,
either re-run your command with --reset or use the command below to explicitly mention the current
value of all non-default settings:
  tailscale up --accept-dns=false --auth-key=... --hostname=... --operator=<username>
```

The fix is to always include `--operator="$USER"` in the `tailscale up`
invocation. This flag is idempotent — re-running with it is safe.

## Destroying the install

```bash
# Delete Agents and Machines FIRST. Each Agent carries a
# kyber.io/agent-cleanup finalizer that only the control plane clears —
# uninstall the release while Agents exist and you delete the controller
# that would release them, leaving the namespace stuck Terminating forever.
# (Recover from that with: kubectl -n kyber-system patch agent <name> \
#   --type merge -p '{"metadata":{"finalizers":[]}}')
kubectl -n kyber-system delete agent --all
kubectl -n kyber-system delete machine --all

# Uninstall Kyber resources
helm uninstall kyber-gcp -n kyber-system
kubectl delete namespace kyber-system

# Destroy the VM + network
cd ~/dev/kyber/infra/terraform
terraform destroy -var="project_id=your-gcp-project" -var="profile=small"
```

Terraform destroys the VM, network, firewall rules, and static IP. Helm
uninstalls the control plane and first-party StatefulSets. The Postgres and Redis
PVCs are deleted with the namespace — **this wipes all agent session state**.
Agent identity repos live on GitHub and survive.

Helm does not delete CRDs. If you are rebuilding from scratch rather than walking
away, remove them too, or the next install inherits this install's schema:

```bash
kubectl delete crd agents.kyber.io machines.kyber.io
```
