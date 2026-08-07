# Installing Kyber

Step-by-step guide to deploying a production Kyber instance on Google Cloud. This document is the source of truth for installation — if you hit a step that doesn't match reality, fix the doc.

> **Running Kyber on a single machine (laptop / home server)?** See [installation-wsl2.md](./installation-wsl2.md) for the standalone WSL2 install path — native k3s, no Terraform, Tailscale Funnel for HTTPS.

> **Cluster naming:** this guide installs the `kyber-gcp` cluster. Helm release and ArgoCD Application both use that same name. See [clusters.md](./clusters.md) for the naming convention.

> **Status:** V1 small profile — single-VM k3s cluster. Multi-node and HA are future work.

## Architecture

A production Kyber install is one GCE VM running k3s, with the Kyber control plane + PWA + node agent deployed via ArgoCD. Agent pods run on that same VM (or on additional VMs provisioned via the Machine Controller). PostgreSQL and Redis run in-cluster as first-party StatefulSets (`postgres:16-alpine` and `redis:7-alpine`) — no Bitnami sub-charts required.

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
│  │  │  (e2-standard-4, Ubuntu 24.04, 100GB disk) │  │  │
│  │  │                                             │  │  │
│  │  │  k3s (single-server)                        │  │  │
│  │  │  ├─ ArgoCD (namespace: argocd)              │  │  │
│  │  │  ├─ ArgoCD Image Updater                    │  │  │
│  │  │  ├─ Kyber control plane (Deployment)        │  │  │
│  │  │  │  ├─ Public API :8080 (PWA at /)          │  │  │
│  │  │  │  ├─ Internal API :8082                   │  │  │
│  │  │  │  ├─ Health probes :8081                  │  │  │
│  │  │  │  └─ Metrics :9090                        │  │  │
│  │  │  ├─ Kyber node-agent (DaemonSet)            │  │  │
│  │  │  ├─ PostgreSQL (StatefulSet, postgres:16-alpine) │  │  │
│  │  │  └─ Redis (StatefulSet, redis:7-alpine)     │  │  │
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

The k3s `LoadBalancer` service type uses klipper-lb, which binds directly to the VM's network interface — no separate cloud LB is needed.

## Prerequisites

Tools installed on your workstation:

- `gcloud` (authenticated against your project)
- `terraform` ≥ 1.7.0
- `kubectl` ≥ 1.30
- `helm` ≥ 3.14
- `git`
- `jq`

GCP project requirements:

- A project with billing enabled (this guide uses `your-gcp-project` as a placeholder — substitute your own project ID everywhere it appears)
- Compute Engine API enabled (`gcloud services enable compute.googleapis.com`)
- IAM role on your user: Owner, or Editor + Compute Admin + Service Account User
- gcloud authenticated: `gcloud auth login` (user credentials)

> **Important — credential source for Terraform.** The Google Terraform provider looks for credentials in this order: `GOOGLE_APPLICATION_CREDENTIALS` env var → `GOOGLE_OAUTH_ACCESS_TOKEN` env var → gcloud ADC at `~/.config/gcloud/application_default_credentials.json` → GCE metadata server.
>
> If your workstation has `GOOGLE_APPLICATION_CREDENTIALS` set to a service account key with **read-only** permissions on compute (common when the same machine runs monitoring tooling), `terraform apply` will fail with `403 Forbidden` on `compute.networks.create` and `iam.serviceAccounts.create`.
>
> **Workaround** (until ADC is properly configured): unset the env var and pass the user's access token directly for the apply:
>
> ```bash
> env -u GOOGLE_APPLICATION_CREDENTIALS \
>   GOOGLE_OAUTH_ACCESS_TOKEN="$(gcloud auth print-access-token)" \
>   terraform apply ...
> ```
>
> The token is short-lived (1 hour). For a single `terraform apply` this is fine; for longer-running workflows, run `gcloud auth application-default login` once to set up ADC properly. The long-term fix is to either (a) narrow `GOOGLE_APPLICATION_CREDENTIALS` to only shell sessions that need it, or (b) grant that service account owner-equivalent roles (not recommended — too broad).

Kyber prerequisites (one-time):

- A GitHub Personal Access Token with `write:packages` scope, stored as `GHCR_PAT` in the `matty-v/kyber` repo secrets. This enables CI to publish images to GHCR. The Helm chart pulls from `ghcr.io/matty-v/kyber-*`, so this must exist before the first install.
- A GitHub Personal Access Token with `read:packages` and `repo` (read) scopes for ArgoCD + Image Updater to pull GHCR images and read the kyber / kyber-deploy repos. This PAT is different from `GHCR_PAT` — it only needs read access.

## 1. Verify CI has published images AND make them public

Before installing, make sure the images the chart references actually exist and are publicly pullable.

### 1a. If you're using the upstream matty-v/kyber images

Visit each package page and verify at least a `:latest` tag exists:

- https://github.com/users/matty-v/packages/container/package/kyber-control-plane
- https://github.com/users/matty-v/packages/container/package/kyber-node-agent
- https://github.com/users/matty-v/packages/container/package/kyber-runtime-base
- https://github.com/users/matty-v/packages/container/package/kyber-claude-code

> **Note:** the agent runtime base image is published as `kyber-runtime-base`, not `kyber-agent-base`, to avoid collision with a package of the same name from the legacy Kyber platform. This is intentional.

If any are missing, CI is broken or the `GHCR_PAT` gate in `.github/workflows/build.yml` is still active — see `docs/upgrading.md` § "Image publishing" to fix.

**First-time only — make packages public.** New GHCR packages default to `private` visibility. User-owned packages can only be made public via the GitHub web UI (the REST API's visibility endpoints only work for org-owned packages). For each of the four packages above:

1. Click the package URL
2. Click "Package settings" in the right sidebar
3. Scroll to "Danger Zone" at the bottom
4. Click "Change visibility" → select "Public" → type the package name to confirm → Save

Alternative (if you want to keep them private): create a `docker-registry` Kubernetes secret in the `kyber-system` namespace named `ghcr-pull`, using a PAT with `read:packages` scope as the password. Then set `imagePullSecrets: [{name: ghcr-pull}]` in `values-prod.yaml`. This adds ongoing PAT-rotation overhead — public is simpler for a single-operator setup.

### 1b. If you're running your own fork

The upstream images are owned by `matty-v` — you cannot make them public or modify them. Instead:

1. **Fork kyber** on GitHub to your own account (e.g., `github.com/your-user/kyber`).
2. **Update image references** in your `values-prod.yaml` (and any environment overlays) to point at your GHCR namespace — change `ghcr.io/matty-v/kyber-*` to `ghcr.io/your-user/kyber-*`.
3. **Add `GHCR_PAT` to your fork's repo secrets** (Settings → Secrets → Actions). The PAT needs `write:packages` scope so CI can push images to your GHCR namespace.
4. **Run CI on your fork** (push a commit or trigger the build workflow manually). The build pipeline publishes images to `ghcr.io/your-user/kyber-*`.
5. **Make those packages public** following the same "Change visibility" steps in §1a, but on your own package pages at `https://github.com/users/your-user/packages/...`.

Once your images are public and tagged `:latest`, continue from §2.

## 2. Generate secrets

Kyber needs an API key and a webhook secret before first install. Generate both as strong random strings and save them to a gitignored file.

```bash
mkdir -p ~/.config/kyber
umask 077
cat > ~/.config/kyber/prod-secrets.env <<EOF
KYBER_API_KEY=$(openssl rand -hex 32)
KYBER_WEBHOOK_SECRET=$(openssl rand -hex 32)
EOF
```

The API key unlocks every `/api/v1/*` endpoint. Treat it like a root password. Store it in a password manager alongside the GCP project credentials.

The webhook secret is **mandatory** for Telegram inbound (kyber#564). The `/webhooks/telegram/*` routes sit outside the API-key wall and are authenticated solely by the `X-Telegram-Bot-Api-Secret-Token` header Telegram echoes back — so they **fail closed**: an install with an empty `webhookSecret` **rejects all webhook traffic** (no message buffered, no suspended agent woken) until one is configured. After setting it, the secret must also be registered with Telegram via `setWebhook` so the bot sends the matching header; Kyber's webhook auto-registration (see `api.publicURL` below) does this for you when both are set. Leaving `webhookSecret` empty does not disable webhook auth — it disables webhook *inbound*.

## 3. Provision infrastructure with Terraform

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
- Firewall rules for SSH (22), k3s API (6443), Kyber API (8080), HTTPS (443), ICMP, and intra-subnet traffic
- Service account with logging, monitoring, artifact-registry-reader, and compute.instanceAdmin.v1 roles
- GCE VM `kyber-small-k3s-server` running Ubuntu 24.04 LTS with a startup script that installs k3s server (traefik disabled)

Expect the apply to take 3–5 minutes. Watch for the VM status to reach `RUNNING` and verify k3s finished its startup script:

```bash
VM_IP=$(terraform output -raw control_plane_ip)
gcloud compute ssh kyber-small-k3s-server --zone=us-central1-a --command "sudo tail -20 /var/log/kyber-k3s-install.log"
```

The last line should read `Kyber k3s install complete. Cluster is ready.`

If the startup script errored, SSH in and inspect `/var/log/kyber-k3s-install.log`. Fix the script (`infra/terraform/scripts/k3s-install.sh`), then re-run. The script is idempotent.

> **Note on TLS SAN.** The startup script reads the VM's external IP from GCE instance metadata and writes it to `/etc/rancher/k3s/config.yaml` as a `tls-san` entry BEFORE installing k3s. This is required so the k3s server certificate is valid for the external IP — without it, kubectl from the operator's workstation fails TLS verification when talking to `https://<external-ip>:6443`. If you find yourself needing to add a SAN after the fact (e.g., the VM gets a new IP), SSH in and:
> ```bash
> sudo sed -i "s|external-ip|$(curl -sfH 'Metadata-Flavor: Google' http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip)|" /etc/rancher/k3s/config.yaml
> sudo rm -f /var/lib/rancher/k3s/server/tls/dynamic-cert.json
> sudo systemctl restart k3s
> ```

## 4. Fetch kubeconfig and join token

The kubeconfig and k3s join token only exist on the VM after k3s boots. Pull them locally:

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

Fetch the k3s join token (needed for the Machine Controller to provision additional k3s workers later):

```bash
K3S_JOIN_TOKEN=$(gcloud compute ssh kyber-small-k3s-server --zone=us-central1-a \
  --command "sudo cat /var/lib/rancher/k3s/server/node-token" \
  | tr -d '[:space:]')
K3S_SERVER_URL="https://${VM_IP}:6443"

# Append to prod-secrets.env so Helm install can read them
cat >> ~/.config/kyber/prod-secrets.env <<EOF
K3S_JOIN_TOKEN=$K3S_JOIN_TOKEN
K3S_SERVER_URL=$K3S_SERVER_URL
EOF
```

## 5. Set up kyber-deploy and configure environment values

Kyber uses a separate deployment repo ([matty-v/kyber-deploy](https://github.com/matty-v/kyber-deploy)) to hold per-environment values and ArgoCD Application manifests. Clone it:

```bash
git clone https://github.com/matty-v/kyber-deploy.git ~/dev/kyber-deploy
cd ~/dev/kyber-deploy
```

> **Choose a release lane.** Every environment renders the chart from
> `targetRevision: main` — the lane is decided by the **image pins** you write into
> `environments/<name>/values.yaml`, not by a branch:
>
> - **Release lane (recommended for production-like installs).** Pin semver digests,
>   `vX.Y.Z@sha256:…`, as `environments/gcp/application.yaml` + `values.yaml` do.
>   The cluster moves only when a release is cut.
> - **Canary lane.** Pin `latest@sha256:…` and let a sync job advance the digests, as
>   `environments/razer/` does. Good for a dev cluster that should track `main`. The
>   WSL2 install path in [`docs/installation-wsl2.md`](installation-wsl2.md) uses this.
>
> There is **no `stable` branch, no `:stable` tag, and no `promote-stable.yml`** —
> that framing is retired. See [`docs/upgrading.md`](upgrading.md) § "Release lanes"
> for how each lane advances.

Edit `environments/<name>/values.yaml` with your environment-specific config:

```bash
# Key fields to set:
# api.existingSecret — name of the kyber-api-credentials secret (created in § 2)
# api.publicURL — your Tailscale Funnel URL (set in § 10) or http://<VM_IP>:8080
# api.service.type — LoadBalancer so klipper-lb assigns node IPs (required
#   for Tailscale proxy / external reachability). Default is ClusterIP.
# compute.gce.project — your GCP project ID
# compute.gce.network / subnet — Terraform-created network names
# k3s.existingSecret — leave empty ("") to reuse kyber-api-credentials; that
#   secret holds k3s-join-token and k3s-server-url as extra keys by default
# postgresql.auth.existingSecret — recommended ArgoCD-friendly pattern.
#   Set this to the name of a pre-provisioned Secret (e.g.
#   "kyber-postgres-credentials") that contains a "postgres-password" key.
#   ArgoCD repo-server has no cluster access, so the old randAlphaNum+lookup
#   auto-generate pattern regenerated the password on every render, which
#   rotated the in-cluster Postgres password out from under running pods.
#   Create the secret manually (see § 5a below) and set this field instead.
```

#### Image pins — which ones you must set

Image tags are **required at render time**: an empty tag for a required image fails
`helm template` loudly rather than silently resolving to `Chart.AppVersion` and
rendering an unpullable reference (kyber#457, kyber#370). Set every tag in the
first group; the second group is opt-in per feature.

**Required — the install does not render without these:**

| Values key | Image | What it is |
|---|---|---|
| `image.controlPlane.tag` | `kyber-control-plane` | API + controllers |
| `image.nodeAgent.tag` | `kyber-node-agent` | per-node DaemonSet |
| `image.statusSidecar.tag` | `kyber-status-sidecar` | injected into every agent pod |
| `image.agentBase.tag` | `kyber-runtime-base` | base layer for both runtimes |
| `image.claudeCode.tag` | `kyber-claude-code` | Claude Code agent runtime |

**Optional — leave empty unless you use the feature.** These are deliberately not
required, because forcing every install to pin an image it never pulls would break
installs that don't use them. Each one now fails *at the point it matters* rather
than silently:

| Values key | Image | Needed for | If left empty |
|---|---|---|---|
| `image.codex.tag` | `kyber-codex` | Codex-runtime agents | `POST /api/v1/agents` rejects a `runtime: codex` agent with a **400** naming this value, and the controller sets a `RuntimeImageMissing` condition (kyber#674) |
| `image.telegramSidecar.tag` | `kyber-mcp-telegram` | Telegram on **any** runtime | agents get **no Telegram at all** and the control plane raises a `TelegramUnavailable` condition (kyber#684) |
| `image.discordSidecar.tag` | `kyber-mcp-discord` | two-way Discord | Discord sidecar injection is a no-op; `spec.channels.discord` does nothing (kyber#646) |

> **`telegramSidecar` is load-bearing if you use Telegram at all.** It used to be
> Codex-only, because Claude Code carried a native in-process channel plugin. That
> plugin is retired (kyber#684) — the runtime container no longer receives a bot
> token, and this sidecar is now the only Telegram implementation for **both**
> runtimes. An install that leaves it empty has no working Telegram.

Pins come from ArgoCD Image Updater, `release.yml`'s digest-pinned bump PR, the
`sync-razer-latest.yml` job (canary lane only), or an explicit
`--set image.<component>.tag=…`. Prefer digest-pinned references
(`vX.Y.Z@sha256:…`) over bare tags so a render is reproducible.

Create the `kyber-api-credentials` secret in the cluster (from the values generated in § 2):

```bash
export KUBECONFIG=~/.kube/kyber-gcp.yaml
source ~/.config/kyber/prod-secrets.env

kubectl create namespace kyber-system --dry-run=client -o yaml | kubectl apply -f -

kubectl -n kyber-system create secret generic kyber-api-credentials \
  --from-literal=api-key="$KYBER_API_KEY" \
  --from-literal=webhook-secret="$KYBER_WEBHOOK_SECRET" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n kyber-system create secret generic kyber-k3s-credentials \
  --from-literal=join-token="$K3S_JOIN_TOKEN" \
  --from-literal=server-url="$K3S_SERVER_URL" \
  --dry-run=client -o yaml | kubectl apply -f -
```

> See [`docs/api-keys.md`](api-keys.md) for the full lifecycle of the platform API key — programmatic + manual rotation, revocation on compromise, scope and threat model.

### 5a. Pre-provision the Postgres password secret

The chart's `postgresql.auth.existingSecret` field points to a Secret that
must exist **before** ArgoCD first renders the chart. Create it now so the
password never touches `values.yaml` or git:

```bash
POSTGRES_PASSWORD=$(openssl rand -hex 24)

# Save it alongside the other secrets — you'll need it if you ever rebuild.
echo "POSTGRES_PASSWORD=$POSTGRES_PASSWORD" >> ~/.config/kyber/prod-secrets.env

kubectl -n kyber-system create secret generic kyber-postgres-credentials \
  --from-literal=postgres-password="$POSTGRES_PASSWORD" \
  --dry-run=client -o yaml | kubectl apply -f -
```

In `environments/prod/values.yaml` set:
```yaml
postgresql:
  auth:
    existingSecret: kyber-postgres-credentials
```

Do **not** set `postgresql.auth.password` — if both are present the chart
prefers `existingSecret` but having a plaintext password in the values file
defeats the purpose.

### 5b. Register the Kyber GitHub App

Kyber gives every agent a private GitHub-backed **identity repo**
(memory, skills, `SOUL.md`, session state all live there and persist
across pod restarts). That repo is managed **exclusively** by a
per-install **Kyber Platform GitHub App** — nothing is hardcoded to a
particular app or account. Instead of handing agents a broad Personal
Access Token, the control plane authenticates as this App and, on
demand, mints a **short-lived token scoped to just that one repo**
(`contents:write`, ~1h) that the agent uses for both reads and writes of
its own identity repo. There is deliberately **no PAT fallback** for the
identity repo: if the App flow is misconfigured, identity-repo git fails
loudly instead of silently succeeding on a broader credential. The
generic PAT (if the agent has one) is used only for the agent's *other*
repos. See [agents-identity-repos.md](./agents-identity-repos.md) for
the full credential model.

Enabling the feature is a one-time setup per Kyber install with two
parts — (a) create the App and its `kyber-github-app` Secret (steps 1–5
below), and (b) point `identityRepo.defaultOwner` at the account the App
is installed on (step 6). **If either part is absent the identity-repo
feature disables cleanly** — agents are created and run normally, just
without a managed identity repo (their identity is never backfilled with
a PAT). Estimated time: ~5 min.

**1. Create the App.** Go to https://github.com/settings/apps/new
(logged in as the user whose account will own agent identity repos) and
fill in:

- **GitHub App name:** `Kyber` (must be globally unique — try
  `Kyber-<yourhandle>` if taken)
- **Homepage URL:** `https://github.com/matty-v/kyber` (or your fork)
- **Webhook → Active:** uncheck (V1 does not use webhooks)
- **Repository permissions:**
  - Administration: **Read and write** (required to create new repos
    from the agent template)
  - Contents: **Read and write** (required to seed and let agents push
    their own memory/state)
  - Metadata: Read-only (GitHub auto-enables this)
- **Account permissions:** none
- **Subscribe to events:** none
- **Where can this GitHub App be installed?** → **Only on this account**

Click **Create GitHub App**.

**2. Generate the private key.** On the App's settings page:

1. Note the **App ID** (6–7 digit number near the top).
2. Scroll to **Private keys** → **Generate a private key**. A `.pem`
   file downloads. Save it somewhere secure — you'll feed it to the
   cluster in step 4.

**3. Install the App.** Click **Install App** in the left sidebar,
click **Install** next to your account, select **All repositories**, and
confirm. GitHub redirects you to
`https://github.com/settings/installations/<number>` — the **Installation
ID** is the number in that URL.

> **"All repositories" is safe here.** The App's installation tokens
> are minted per-call and scoped to the single repo each agent needs.
> "All repositories" at install time just lets the App see newly
> created agent repos without a re-install each time.

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
`pkg/githubapp.LoadConfigFromSecret`. If the Secret is missing or
malformed, the control plane logs `GitHub App Secret not loaded —
identity-repo feature disabled` and continues to start normally with
the feature off — agents that don't use an identity repo are unaffected,
and the internal `identity-repo-token` endpoint returns `503` so a git
op against an identity repo fails loudly rather than falling back to a
PAT.

**5. Set `identityRepo.defaultOwner`.** In
`environments/prod/values.yaml` (from § 5), set this to the GitHub
account/org the App is installed on — the account under which agent
identity repos live:

```yaml
identityRepo:
  defaultOwner: "your-github-account"   # e.g. matty-v
```

This value flows to the control plane as `KYBER_IDENTITY_REPO_OWNER`.
The chart default is empty; leaving it empty (even with the Secret
present) keeps identity-repo scaffolding disabled. Commit and push so
ArgoCD renders it on the next sync.

**6. Shred the PEM copy on your workstation.** Once the Secret is
applied, the plaintext key on disk is redundant:

```bash
shred -u "$PEM_PATH"
```

> **Rotation.** When it's time to rotate, generate a new private key in
> the GitHub App UI, re-run step 4 with the new PEM, then delete the
> old key from the App UI. Installation tokens in flight continue to
> work until they expire (max 1h).

## 6. Install ArgoCD + Image Updater and apply the Application

> **Shared with WSL2 install.** This section is near-identical to [installation-wsl2.md § 6](./installation-wsl2.md#6-install-argocd--image-updater-and-apply-the-application). If you change the ArgoCD/Image Updater bootstrap flow here, update the sibling too — bug-fixes in one can silently miss the other.

### 6a. Bootstrap ArgoCD

```bash
cd ~/dev/kyber-deploy
KUBECONFIG=~/.kube/kyber-gcp.yaml bash bootstrap/install-argocd.sh
```

Verify all ArgoCD pods are running:

```bash
kubectl -n argocd get pods
```

Expected: `argocd-server`, `argocd-application-controller`, `argocd-repo-server`, `argocd-redis`, `argocd-image-updater` — all `Running`.

### 6b. Create GHCR credentials for Image Updater

ArgoCD Image Updater polls GHCR every ~2 minutes to detect new image digests. Give it a read PAT:

```bash
GHCR_READ_TOKEN="<your-ghcr-read-pat>"   # read:packages scope

kubectl -n argocd create secret generic ghcr-creds \
  --from-literal=creds="matty-v:${GHCR_READ_TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n argocd patch configmap argocd-image-updater-config --type merge \
  -p '{"data":{"registries.conf":"registries:\n- name: ghcr\n  api_url: https://ghcr.io\n  prefix: ghcr.io\n  credentials: secret:argocd/ghcr-creds#creds\n  default: true\n"}}'

kubectl -n argocd rollout restart deploy/argocd-image-updater
```

### 6c. Create GitHub repo credentials for ArgoCD

ArgoCD needs read access to `matty-v/kyber` (the Helm chart) and `matty-v/kyber-deploy` (the values). Create a secret with a GitHub PAT (`repo` read scope):

```bash
GITHUB_PAT="<your-github-read-pat>"   # repo read scope

kubectl -n argocd create secret generic github-repo-creds \
  --from-literal=url=https://github.com/matty-v \
  --from-literal=username=matty-v \
  --from-literal=password="${GITHUB_PAT}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n argocd label secret github-repo-creds argocd.argoproj.io/secret-type=repo-creds
```

### 6d. Apply the ArgoCD Application

```bash
KUBECONFIG=~/.kube/kyber-gcp.yaml \
  kubectl apply -f ~/dev/kyber-deploy/environments/prod/application.yaml
```

Watch ArgoCD sync:

```bash
kubectl -n argocd get applications kyber-gcp -w
```

Expected: the application transitions to `Synced` and `Healthy`. ArgoCD renders the Helm chart from the Kyber repo using values from kyber-deploy and applies it to the cluster.

Check status:

```bash
kubectl -n kyber-system get all
kubectl -n kyber-system logs deploy/kyber-gcp-control-plane --tail=50
```

## 7. Verify

> **Shared with WSL2 install.** The verify steps are near-identical to [installation-wsl2.md § 7](./installation-wsl2.md#7-verify) — keep healthz/fleet-summary assertions in sync across both docs.

The LoadBalancer service's external IP is the VM's static external IP (klipper-lb uses the node network). ArgoCD deploys with release name `kyber-gcp`, so resources are prefixed `kyber-gcp-*`. Once the service is `Ready`:

```bash
SERVICE_IP=$(kubectl -n kyber-system get svc kyber-gcp-control-plane -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl -s http://$SERVICE_IP:8080/healthz
# expect: 200 OK (empty body)

curl -s -H "Authorization: Bearer $KYBER_API_KEY" http://$SERVICE_IP:8080/api/v1/fleet/summary
# expect: {"machineCount":0,"agentCount":0,"agentsByPhase":{},"machinesByPhase":{}}
```

If healthz works but the fleet endpoint returns 401, double-check the API key. If healthz returns anything other than 200, inspect `kubectl -n kyber-system logs deploy/kyber-control-plane`.

## 8. Open the PWA

The PWA is served at `/` by the same control plane binary. Open in a browser:

```
http://<VM_IP>:8080/
```

On first load, the Settings page will ask for the API key. Paste the value from `~/.config/kyber/prod-secrets.env`. The key is stored in browser `localStorage` — treat the browser as a trusted device.

You should see the Fleet Overview page with zero machines and zero agents. Success.

## 9. Create your first agent

From the PWA:

1. Go to **Machines** → **Create Machine**
2. Fill in: name `laptop`, provider `gce` (or leave blank for local), machine type `e2-standard-2`, zone `us-central1-a`, spot `false`
3. Click **Create**. The machine's phase should move Provisioning → Ready.

> **V1 limitation:** the Machine Controller provisions GCE VMs via the GCE adapter when real credentials are configured. Without credentials, it uses the Mock adapter and the machine stays "virtual" — useful for testing the control plane but not for running real agent pods.

4. Go to **Agents** → **Create Agent**
5. Fill in: name `dave`, machine `<the machine you just created>`, runtime `claude-code`, model `claude-sonnet-4-5`
6. Click **Authorize with Claude**. The PWA generates a PKCE verifier/challenge and opens Anthropic's authorize URL in a new tab.
7. Sign in at Anthropic, approve the scope grant, copy the authorization code shown on the Anthropic-hosted page.
8. Return to the PWA, paste the code into the **Authorization code** field.
9. Click **Create Agent**. Kyber exchanges the code + verifier for access and refresh tokens, stores them in a `dave-oauth` k8s secret, and creates the Agent CRD.
10. The agent moves Creating → Starting → Running. On every boot, `start-claude.sh` refreshes the access token from the stored refresh token and writes `~/.claude/.credentials.json`, so Claude Code starts with full-scope credentials — no interactive `/login` needed.

> **Legacy / scripted path:** If you need to create an agent without the PWA (e.g., from a script), you can still pass `secrets.oauthToken` directly in the `POST /api/v1/agents` request body. This accepts a long-lived refresh token obtained by other means. The PKCE flow above is preferred for interactive use.

> **Old setup-token flow (superseded):** Prior to 2026-04-14, agents were created by running `claude setup-token` on a trusted device and manually creating a k8s secret. This no longer works reliably because setup-tokens have only `user:inference` scope — insufficient for Claude Code's API calls. Use the PKCE flow above.

If the agent doesn't reach Running, inspect:

```bash
kubectl -n kyber-system describe agent dave
kubectl -n kyber-system logs pod/agent-dave
```

### Scheduling recurring work on an agent

Agents ship with cron already running. `crontab -e` (user schedule) or a
file in `/etc/cron.d/` (system schedule) both persist across pod restarts
and start firing automatically on the new pod. See
[docs/agents-scheduled-jobs.md](./agents-scheduled-jobs.md) for the
supported surfaces and how persistence works under the hood.

## 10. HTTPS via Tailscale Funnel

> **Shared with WSL2 install.** The Tailscale Funnel setup is near-identical to [installation-wsl2.md § 10](./installation-wsl2.md#10-https-via-tailscale-funnel). When touching the `tailscale up` invocation or Funnel config, update both — the `--operator="$USER"` flag regression in #59 landed in wsl2 only and left this doc stale.

The quickest path to a working HTTPS URL — zero cost, no domain purchase, no DNS setup, no cert rotation — is Tailscale Funnel. The VM joins your existing tailnet, `tailscale funnel` terminates HTTPS with a Let's Encrypt cert automatically provisioned by Tailscale, and the public URL is `https://<hostname>.<tailnet>.ts.net`. Funnel is free on all Tailscale plans.

**Prerequisites:**
- You have a Tailscale account with Funnel enabled (free on all plans as of late 2023).
- Your tailnet ACL allows the VM to run Funnel (default ACLs allow this).

**Generate an auth key** at https://login.tailscale.com/admin/settings/keys:
- Reusable: **off** (single-use is more secure; the key is consumed by `tailscale up`)
- Ephemeral: **off** (the VM persists across reboots)
- Pre-authorized: **on**
- Tags: leave empty unless your ACL requires them
- Expiration: 1 day

Save the key value — you'll paste it into the `tailscale up` command below.

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
- `--accept-dns=false` keeps the VM's DNS resolution using the k3s ClusterDNS so in-cluster service lookups (`kyber-gcp-postgres`, `kyber-gcp-redis`, etc.) keep working.
- `--operator="$USER"` — **required on a fresh tailscaled install.** After `tailscale install.sh`, tailscaled sets a default `operator` preference. Any subsequent `tailscale up` invocation must mention it or the daemon rejects the command. See Troubleshooting below.

**Enable Funnel on port 8080:**

```bash
gcloud compute ssh kyber-small-k3s-server --zone=us-central1-a --command "
  sudo tailscale funnel --bg 8080
"
```

Tailscale prints the public URL — something like `https://kyber.<tailnet>.ts.net/`. The cert is issued by Let's Encrypt and auto-renewed every ~60 days by Tailscale.

> **Note:** the Tailscale CLI for Funnel changed in v1.66+. The older `tailscale funnel 443 on` syntax no longer works. Use `tailscale funnel --bg <port>` on v1.66 and newer. Our install script assumes v1.96+.

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

Open the PWA at `https://kyber.<tailnet>.ts.net/` in your browser. It loads over HTTPS with a valid cert, the Service Worker can register (PWA install prompt works), and API calls + WebSockets use `wss://` automatically.

### Why Tailscale Funnel over a custom domain?

- **Zero cost.** No domain purchase, no Cloud DNS zone, no LB fees.
- **Zero config.** Tailscale handles DNS, cert provisioning, and renewal.
- **Zero maintenance.** No cert rotation, no DNS TTL drift, no CA changes to track.
- **Public reachable.** Unlike `tailscale serve` (tailnet-only), Funnel exposes the service to the public internet — Telegram webhooks work, phones on cellular work, etc.
- **Upgradeable.** If you later want a custom domain (`kyber.yourdomain.com`), just install cert-manager + ingress-nginx in the cluster and point an A record at the VM IP. The plain HTTP API on `:8080` still works alongside Funnel.

### Alternative: cert-manager + ingress-nginx + custom domain

If you need a custom domain (e.g., for branding), replace step 10 with:

1. Buy a domain (Cloudflare Registrar is cheapest, ~$12/year)
2. Create an A record `kyber.yourdomain.com` → VM external IP
3. Install ingress-nginx: `helm install ingress-nginx ingress-nginx/ingress-nginx -n ingress-nginx --create-namespace`
4. Install cert-manager: `helm install cert-manager cert-manager/cert-manager -n cert-manager --create-namespace --set installCRDs=true`
5. Create a ClusterIssuer for Let's Encrypt (HTTP-01 or DNS-01 challenge)
6. Set `ingress.enabled=true` in `values-prod.yaml` with `ingress.hosts[0].host=kyber.yourdomain.com` and the cert-manager annotation

This path is more k8s-native but takes ~30 minutes and costs $12/year. The Funnel path takes ~5 minutes and costs nothing.

### Keeping the old HTTP access

The plain `http://<VM_IP>:8080` endpoint stays available alongside Funnel. Use it for local debugging or if Tailscale is temporarily unreachable. GCP firewall rule `kyber-small-allow-kyber-api` already exposes port 8080 to `0.0.0.0/0`. If you want to lock it down to tailnet-only, delete that firewall rule via `terraform destroy -target=module.profile[0].google_compute_firewall.allow_kyber_api` and update the Terraform config.

## Agent Pod Requirements

Agent pods require two capabilities that go beyond what a standard restricted-policy cluster allows. Both are set automatically by the Agent Controller in `pkg/controllers/agent/pod_builder.go` — you don't need to configure them manually. This section explains why they exist.

**Privileged mode (`Privileged: true`)**

Agent pods run with `securityContext.privileged: true`. This is required for two reasons:

1. **fuse-overlayfs** — the whole-disk persistence fallback. `SYS_ADMIN` alone is insufficient because the default seccomp profile blocks the `mount` syscall even when `SYS_ADMIN` is granted. Privileged mode lifts both the capability and seccomp restrictions.
2. **`mount(MS_BIND)`** in the bind-mount-HOME fallback path.

If your cluster enforces a Pod Security Standard of `restricted` or `baseline`, agent pods will be rejected. Use a dedicated namespace with `enforced=privileged` (the default `kyber-system` namespace is configured this way by the chart).

**`/dev/fuse` device mount**

The controller mounts the host's `/dev/fuse` character device into every agent pod. This is the standard pattern for FUSE-using containers and requires the host kernel to have the `fuse` module loaded. On GCE VMs running Ubuntu 24.04, `fuse` is loaded by default.

If you're deploying on a cloud provider that doesn't expose `/dev/fuse` to containers (uncommon, but possible on some managed Kubernetes offerings), fuse-overlayfs will fall back to bind-mount-HOME automatically — you lose whole-disk persistence but the pods still start and run.

## GCE Persistent Disk CSI Driver (Required for k3s)

k3s does not bundle the GCE PD CSI driver. Install it before deploying Kyber:

```bash
# Install the GCE PD CSI driver
kubectl apply -k "github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver/deploy/kubernetes/overlays/stable?ref=v1.15.0"

# Verify the driver is running
kubectl get pods -n gce-pd-csi-driver
```

The driver requires GCE credentials. On GCE VMs, it uses the instance's service account automatically. For non-GCE clusters, configure credentials per the [driver documentation](https://github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver).

## Troubleshooting

### ArgoCD sync fails or pods don't come up

Check ArgoCD Application status: `kubectl -n argocd get applications kyber-gcp`. If `Sync Status` is `OutOfSync` or `Health Status` is `Degraded`, describe the Application for details:

```bash
kubectl -n argocd describe application kyber-gcp
```

If pods are in `ImagePullBackOff`, the image doesn't exist in GHCR or the GHCR packages visibility has been set to private (the default prod setup pulls public images with no pull secret). See `docs/upgrading.md` § "Image publishing".

### Control plane CrashLoopBackOff

```bash
kubectl -n kyber-system logs deploy/kyber-gcp-control-plane --previous
```

Common causes:
- API key or webhook secret is empty → check the `kyber-api-credentials` secret
- Postgres or Redis connection refused → check those pods are `Running` and the DSN env vars are correct (`kyber-gcp-postgres:5432`, `kyber-gcp-redis:6379`)
- RBAC missing → check `kyber-gcp-control-plane` ClusterRole exists and is bound

### PWA loads but API calls return 401

The API key in the browser doesn't match the one in the k8s secret. Open the PWA Settings page and paste the correct key.

### Telegram messages don't reach agents (webhook drops)

The webhook routes **fail closed** (kyber#564): if `webhookSecret` is empty, every webhook request is rejected — Telegram still gets a `200` (so it doesn't retry-storm) but nothing is buffered and no suspended agent is woken. The control-plane log shows `telegram webhook rejected: no webhook secret configured (fail-closed, kyber#564)` and, at startup, `KYBER_WEBHOOK_SECRET not set — Telegram webhook routes will REJECT all traffic`. Fix: set a non-empty `webhookSecret` (or `existingSecret`) **and** re-register the bot with the same secret via Telegram `setWebhook` so it sends the matching `X-Telegram-Bot-Api-Secret-Token` header. A `telegram webhook secret mismatch` log instead means the registered secret and the configured one differ — re-register with the configured value.

### LoadBalancer stuck in Pending

On k3s, klipper-lb takes a few seconds to assign. Wait 30 seconds. If it stays Pending:

```bash
kubectl -n kube-system logs deploy/svclb-kyber-gcp-control-plane
```

Common cause: another service is already bound to port 8080 on the host. Free the port or change `api.service.port`.

### `tailscale up` rejects with "changing settings requires mentioning all non-default flags"

After a fresh `tailscale install.sh`, tailscaled ships with a default `operator` preference set. If you run `tailscale up` without `--operator`, the daemon compares your flags against its saved defaults and rejects the command:

```
Error: changing settings via 'tailscale up' requires mentioning all non-default flags. To proceed,
either re-run your command with --reset or use the command below to explicitly mention the current
value of all non-default settings:
  tailscale up --accept-dns=false --auth-key=... --hostname=... --operator=<username>
```

The fix is to always include `--operator="$USER"` in the `tailscale up` invocation. This flag is idempotent — re-running with it is safe.

### Control plane pod is Ready but `/healthz` returns 404

This was a latent bug through D2. If you hit it, `mgr.AddHealthzCheck` and `mgr.AddReadyzCheck` aren't being called in `cmd/control-plane/main.go`. Fix and rebuild.

## Destroying the install

```bash
# Remove the ArgoCD Application (stops ArgoCD from managing Kyber)
kubectl -n argocd delete application kyber-gcp

# Uninstall Kyber resources
helm uninstall kyber-gcp -n kyber-system
kubectl delete namespace kyber-system

# Optionally remove ArgoCD
helm uninstall argocd -n argocd
kubectl delete namespace argocd

# Destroy the VM + network
cd ~/dev/kyber/infra/terraform
terraform destroy -var="project_id=your-gcp-project" -var="profile=small"   # <-- your GCP project ID
```

Terraform destroys the VM, network, firewall rules, and static IP. Helm uninstalls the control plane and first-party StatefulSets. The Postgres and Redis PVCs are deleted with the namespace — **this wipes all agent session state**.
