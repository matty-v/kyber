# Installing Kyber on WSL2 (Standalone)

Step-by-step guide for the standalone single-box deployment shape: native k3s in WSL2, Tailscale Funnel for HTTPS, and the same ArgoCD + Image Updater stack as GCP prod. Companion to [installation.md](./installation.md) (GCP multi-VM install).

> **Aside.** Cluster naming: this guide installs the `kyber-laptop` cluster. Helm release and ArgoCD Application both use that same name. See [clusters.md](./clusters.md) for the naming convention.

> **Release lane:** this profile follows the **canary** lane — chart from `main` HEAD plus `latest@sha256:…` image pins, advanced automatically (ArgoCD Image Updater for control-plane/node-agent; a scheduled latest-sync job in the deploy repo for the controller-injected runtime images). Suited to dev clusters where the newest behavior is what you want. For production-like installs that move only on a cut release, see the GCP install in [installation.md](./installation.md), which pins semver digests. There is no `stable` branch or `:stable` tag — see [upgrading.md](./upgrading.md) § "Release lanes".

> **Aside.** Status: Phase A complete (2026-04-16) — PWA reachable on phone via Tailscale Funnel. Agent creation (Phase B) and agent runtime with privileged pods (Phase C) are documented separately.

## Architecture

```
┌─────────────────────── Windows host (windows-laptop) ─────────────────────┐
│  Tailscale IPN (windows-laptop-1)                                          │
│  ⋮ (independent tailnet identity, used for SSH recovery)                │
│  ──────────────────────────────────────────────────────────────────── │
│                                                                         │
│  ┌──────────── WSL2 (Ubuntu 22.04, hostname kyber-wsl) ─────┐  │
│  │                                                                    │  │
│  │  tailscaled (systemd unit, separate tailnet node)                  │  │
│  │    │                                                               │  │
│  │    └── tailscale funnel --bg 8080                                  │  │
│  │            → https://kyber-wsl.<tailnet>.ts.net           │  │
│  │                                                                    │  │
│  │  ┌────────── k3s (single server, traefik disabled) ─────────────┐  │  │
│  │  │  namespace argocd/                                            │  │  │
│  │  │  ├─ argocd-server, repo-server, application-controller, …    │  │  │
│  │  │  └─ argocd-image-updater                                      │  │  │
│  │  │  namespace kyber-system/                                      │  │  │
│  │  │  ├─ kyber-laptop-control-plane   (Deployment, 1 replica)      │  │  │
│  │  │  ├─ kyber-laptop-control-plane   (Service, type=LB :8080)     │  │  │
│  │  │  ├─ kyber-laptop-node-agent      (DaemonSet, 1 pod)           │  │  │
│  │  │  ├─ kyber-laptop-postgres        (StatefulSet, 1 replica)     │  │  │
│  │  │  └─ kyber-laptop-redis           (StatefulSet, 1 replica)     │  │  │
│  │  │  storage: local-path (k3s default) — no GCE PD needed         │  │  │
│  │  └───────────────────────────────────────────────────────────────┘  │  │
│  └────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────┘

Phone → HTTPS → Tailscale Funnel (kyber-wsl node, inside WSL2)
      → klipper-lb :8080 → control-plane Service → pod
```

**Why native k3s, not k3d/kind?** Agent pods (Phase C) require `/dev/fuse` and `mount(MS_BIND)` — both work far more reliably directly on the WSL2 kernel than nested in Docker. Native k3s also exactly matches the prod stack (same k3s version, same klipper-lb, same Helm chart).

**Why tailscaled inside WSL2, not Windows-side forwarding?** The Windows Tailscale client keeps its own identity (`windows-laptop-1`) for SSH recovery. Running a separate tailscaled inside WSL2 gives the cluster a clean identity (`kyber-wsl`) and lets Funnel terminate directly on the kube LoadBalancer address — no `netsh portproxy` drift to debug.

### Ports

| Port | Exposed | Purpose |
|------|---------|---------|
| 6443 | WSL2-local only | k3s API server (kubectl from WSL2) |
| 8080 | Public via Tailscale Funnel | Kyber public API + PWA |

Port 6443 is not exposed outside WSL2 in Phase A (mock compute adapter; no external workers). Port 8080 is forwarded to the internet via Funnel — no firewall rules, no static IP, no DNS setup required.

## Prerequisites

**External accounts and tokens (gather before starting — § 0 verifies they exist):**
- A Tailscale account with Funnel enabled (free on all plans). Sign up at https://login.tailscale.com if you don't have one.
- A GitHub Personal Access Token with `read:packages` scope (for ArgoCD Image Updater to poll GHCR).
- A GitHub Personal Access Token with `repo` read scope (for ArgoCD to read `matty-v/kyber` and `matty-v/kyber-deploy`).
- A GitHub account on which you are willing to install the Kyber GitHub App (for agent identity-repos). Same account or a different one is fine; § 5a walks through the App registration.

**Workstation / WSL2 environment:** § 0 brings WSL2 up. Skip Prerequisites if you have not yet completed § 0 — the verify gates inside § 0 are authoritative.

## 0. Bring up WSL2

A non-technical operator with no WSL2 yet starts here. If WSL2 is already up with Ubuntu 22.04+ and systemd enabled, sub-steps 0.1–0.4 are no-ops (their verify commands pass), and you proceed at 0.5 to install required tools.

> **Aside.** This section assumes the agent is initially running with shell access to Windows PowerShell. After 0.4 we move execution into WSL2 (Ubuntu) for the rest of the install.

### 0.1 Detect existing WSL2

**What this does:** Checks whether a usable WSL2 + Ubuntu is already installed. If yes, the rest of § 0 is largely no-op.

**Preconditions:**
- Operator is on a Windows 10 (build 19041, "21H1") or later, or Windows 11.

**Run:** *(no run command — this is a verify-only step)*

**Verify:**
```powershell
wsl -l -v
```
Expected output: at least one line listing an Ubuntu distro with VERSION `2`. Example:
```
NAME      STATE    VERSION
* Ubuntu  Running  2
```

**If it fails (no Ubuntu listed, or VERSION is `1`):** → § Troubleshooting / "WSL2 not installed or Ubuntu missing".

**Human input needed:** None.

### 0.2 Install WSL2 + Ubuntu

**What this does:** Installs the WSL2 feature and the default Ubuntu distro.

**Preconditions:**
- 0.1 verify failed (no usable Ubuntu).
- Operator can run PowerShell as Administrator.

**Run** *(in PowerShell, run as Administrator)*:
```powershell
wsl --install
```

**Verify:**
```powershell
wsl -l -v
```
Expected output: an Ubuntu distro with VERSION `2`. May say `STATE: Stopped` until first launch (covered by Human input below).

**If it fails:** → § Troubleshooting / "WSL2 not installed or Ubuntu missing".

**Human input needed:** Reboot Windows (the agent should ask the operator to reboot and confirm), then open Ubuntu from the Start menu. The first launch creates a Linux user — the operator picks a username and password. Once the prompt is at a `$` shell, the operator tells the agent the chosen username and confirms first-launch is complete.

### 0.3 Enable systemd in /etc/wsl.conf

**What this does:** Turns on systemd inside WSL2's Ubuntu so k3s can run as a systemd service.

**Preconditions:**
- 0.2 complete (Ubuntu launched once, user created).

**Run** *(inside WSL2 Ubuntu — agent runs via `wsl -e bash -c '...'` from PowerShell or directly if running in WSL2)*:
```bash
sudo tee -a /etc/wsl.conf > /dev/null <<'EOF'
[boot]
systemd=true
EOF
```

**Verify:**
```bash
grep -A1 '\[boot\]' /etc/wsl.conf | grep -q 'systemd=true' && echo OK
```
Expected output:
```
OK
```

**If it fails:** → § Troubleshooting / "systemd block missing or malformed in wsl.conf".

**Human input needed:** After the edit, the operator runs `wsl --shutdown` from PowerShell (closing all WSL2 windows first), then reopens Ubuntu. The agent should ask the operator to confirm the restart and then re-verify.

After restart, an additional verify:
```bash
systemctl is-system-running
```
Expected: `running` or `degraded` (degraded is acceptable — only fails if WSL2 didn't pick up systemd).

### 0.4 Move agent execution into WSL2

**What this does:** Establishes the agent's command channel into Ubuntu so all remaining install steps execute in the Linux environment.

**Preconditions:**
- 0.3 complete and `systemctl is-system-running` returns `running` or `degraded`.

**Run:** *(no command — this is a control-flow step for the agent)*

**Verify:**
```bash
wsl -e bash -c 'whoami && uname -a'
```
Expected output: the Linux username (from 0.2) on the first line and a kernel string containing `microsoft-standard-WSL2` on the second. Example:
```
ubuntu
Linux DESKTOP-XYZ 5.15.x.x-microsoft-standard-WSL2 #1 SMP ... GNU/Linux
```

**If it fails:** → § Troubleshooting / "wsl -e bash unavailable or returns wrong distro".

**Human input needed:** None — but the agent must from this point on either:
- (a) run all bash commands via `wsl -e bash -c '...'` (or a long-lived `wsl bash` session), keeping the agent session continuous, **or**
- (b) ask the operator to start a fresh agent session inside Ubuntu (open Ubuntu from Start, type `wsl`, then start the AI assistant from inside that shell) and resume from § 1 with the same launching prompt.

(a) is preferred. (b) is the fallback for agents that cannot fluidly straddle PowerShell and bash.

### 0.5 Install required tools

**What this does:** Installs `kubectl`, `helm`, `jq`, `git`, `curl`, and `openssl` inside Ubuntu.

**Preconditions:**
- 0.4 complete; bash commands are running inside Ubuntu.

**Run:**
```bash
sudo apt update
sudo apt install -y curl jq git openssl

# kubectl (official APT repo)
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key | \
  sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.30/deb/ /' | \
  sudo tee /etc/apt/sources.list.d/kubernetes.list
sudo apt update
sudo apt install -y kubectl

# helm (official install script)
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

**Verify:**
```bash
for tool in kubectl helm jq git curl openssl; do
  command -v "$tool" >/dev/null && echo "$tool: $($tool --version 2>&1 | head -1)" || echo "$tool: MISSING"
done
```
Expected output: every line ends with a version string, none with `MISSING`.

**If it fails:** → § Troubleshooting / "Tool install failed".

**Human input needed:** None.

### 0.6 Free port 8080

**What this does:** Confirms nothing else on the WSL2 host is bound to port 8080. klipper-lb (k3s's built-in service LB) needs it.

**Preconditions:** 0.5 complete.

**Run:** *(no run — verify-only)*

**Verify:**
```bash
ss -tln | grep ':8080 ' || echo "port free"
```
Expected output: `port free` (or no output before that line).

**If it fails (something is bound to 8080):** → § Troubleshooting / "Port 8080 already in use".

**Human input needed:** None.

## 1. Verify CI images are public

**What this does:** Confirms the four kyber images CI publishes to GHCR are publicly pullable. The chart pulls them with no pull secret.

**Preconditions:**
- § 0 complete (tools installed, working bash inside Ubuntu).

**Run:** *(no command — verify-only)*

**Verify:**
```bash
for img in kyber-control-plane kyber-node-agent kyber-runtime-base kyber-claude-code; do
  if curl -fsI "https://ghcr.io/v2/matty-v/$img/manifests/latest" >/dev/null 2>&1; then
    echo "$img: public"
  else
    echo "$img: NOT public or missing"
  fi
done
```
Expected output: every line ends with `public` (no `NOT public` or `missing`).

**If it fails:** → § Troubleshooting / "GHCR images private or missing".

**Human input needed** *(only if any image is missing or private)*:
- If missing: CI is broken or hasn't published yet. Direct the operator to https://github.com/matty-v/kyber/actions to confirm the latest `build.yml` run finished.
- If private: ask the operator (or someone with admin on `matty-v`) to make the package public via the GitHub UI: visit `https://github.com/users/matty-v/packages/container/<image-name>/settings`, scroll to "Danger Zone", click "Change visibility", choose Public, type the package name to confirm. Repeat for each private image.

**Notes:** *(aside)* This step is identical for both prod and laptop installs — both pull from the same public GHCR namespace.

## 2. Generate secrets

**What this does:** Generates a 64-hex-char API key and webhook secret and writes them to a gitignored file at `~/.config/kyber/laptop-secrets.env`.

**Preconditions:**
- § 0.5 complete (`openssl` installed).

**Run:**
```bash
mkdir -p ~/.config/kyber
umask 077
cat > ~/.config/kyber/laptop-secrets.env <<EOF
KYBER_API_KEY=$(openssl rand -hex 32)
KYBER_WEBHOOK_SECRET=$(openssl rand -hex 32)
EOF
chmod 600 ~/.config/kyber/laptop-secrets.env
```

**Verify:**
```bash
source ~/.config/kyber/laptop-secrets.env
test "${#KYBER_API_KEY}" = 64 && test "${#KYBER_WEBHOOK_SECRET}" = 64 && echo OK
```
Expected output: `OK`.

**If it fails:** → § Troubleshooting / "Secret generation produced wrong length".

**Human input needed:** None — but the agent should remind the operator that `~/.config/kyber/laptop-secrets.env` is the canonical home for these values. Anyone who can read the file can call any `/api/v1/*` endpoint. Tell the operator to save the API key in their password manager as a backup.

## 3. Install k3s

**What this does:** Installs k3s as a systemd service inside Ubuntu, with traefik disabled and the kubeconfig set to mode 644 so we can copy it without sudo.

**Preconditions:**
- § 0.6 verify passed (port 8080 free).
- § 2 complete.

**Run:**
```bash
curl -sfL https://get.k3s.io | sudo sh -s - --disable traefik --write-kubeconfig-mode 644
```

**Verify:**
```bash
sudo systemctl is-active k3s && sudo k3s kubectl get nodes -o wide
```
Expected output: first command prints `active`. Second command prints one line with STATUS `Ready` and ROLES containing `control-plane,master`.

**If it fails:** → § Troubleshooting / "k3s install failed or node not Ready".

**Human input needed:** None.

**Notes:** *(aside)* `--write-kubeconfig-mode 644` is what lets the next step copy `/etc/rancher/k3s/k3s.yaml` without root. The `--disable traefik` flag avoids a port collision with klipper-lb on 8080.

## 4. Fetch kubeconfig and join token

### 4a. Copy kubeconfig to user home

**What this does:** Copies `/etc/rancher/k3s/k3s.yaml` to `~/.kube/kyber-laptop.yaml` with the operator's user as owner and mode 600.

**Preconditions:** § 3 complete; `/etc/rancher/k3s/k3s.yaml` exists.

**Run:**
```bash
mkdir -p ~/.kube
sudo install -m 600 -o "$USER" -g "$USER" \
  /etc/rancher/k3s/k3s.yaml ~/.kube/kyber-laptop.yaml
```

**Verify:**
```bash
test -O ~/.kube/kyber-laptop.yaml && stat -c '%a' ~/.kube/kyber-laptop.yaml
```
Expected output: `600`.

**If it fails:** → § Troubleshooting / "kubeconfig copy permission error".

**Human input needed:** None.

### 4b. Export KUBECONFIG and persist to bashrc

**What this does:** Sets `$KUBECONFIG` for the current shell and adds a permanent export to `~/.bashrc` so future shells inherit it.

**Preconditions:** 4a complete.

**Run:**
```bash
export KUBECONFIG=~/.kube/kyber-laptop.yaml
grep -qF 'export KUBECONFIG=~/.kube/kyber-laptop.yaml' ~/.bashrc \
  || echo 'export KUBECONFIG=~/.kube/kyber-laptop.yaml' >> ~/.bashrc
```

**Verify:**
```bash
kubectl get nodes
```
Expected output: one node with STATUS `Ready`.

**If it fails:** → § Troubleshooting / "kubectl get nodes returns empty or error".

**Human input needed:** None.

### 4c. Capture k3s join token and server URL

**What this does:** Reads the k3s server's join token and stores it in `~/.config/kyber/laptop-secrets.env` so the Helm install in § 5 can mount them as secret keys. Phase A uses `127.0.0.1:6443` as the server URL — no external workers join.

**Preconditions:** § 3 complete.

**Run:**
```bash
K3S_JOIN_TOKEN=$(sudo cat /var/lib/rancher/k3s/server/node-token | tr -d '[:space:]')
K3S_SERVER_URL="https://127.0.0.1:6443"
cat >> ~/.config/kyber/laptop-secrets.env <<EOF
K3S_JOIN_TOKEN=$K3S_JOIN_TOKEN
K3S_SERVER_URL=$K3S_SERVER_URL
EOF
```

**Verify:**
```bash
( source ~/.config/kyber/laptop-secrets.env && \
  test -n "$K3S_JOIN_TOKEN" && test -n "$K3S_SERVER_URL" && echo OK )
```
Expected output: `OK`.

**If it fails:** → § Troubleshooting / "k3s join token empty or unreadable".

**Human input needed:** None.

## 5. Clone kyber-deploy and create the API credentials Secret

### 5.1 Clone kyber-deploy

**What this does:** Clones the kyber-deploy repo so ArgoCD can read the laptop environment's values.

**Preconditions:** § 0.5 (`git` installed).

**Run:**
```bash
git clone https://github.com/matty-v/kyber-deploy.git ~/dev/kyber-deploy
```

**Verify:**
```bash
test -f ~/dev/kyber-deploy/environments/laptop/values.yaml && echo OK
```
Expected output: `OK`.

**If it fails:** → § Troubleshooting / "kyber-deploy clone failed".

**Human input needed:** None.

### 5.2 Create the kyber-api-credentials Secret

**What this does:** Creates one Kubernetes Secret in the `kyber-system` namespace holding the API key, webhook secret, k3s join token, and k3s server URL. The chart's `kyber.k3sSecretName` helper falls through from an empty `k3s.existingSecret` to `api.existingSecret`, so this single Secret satisfies both.

**Preconditions:** § 4c complete (laptop-secrets.env has all four keys).

**Run:**
```bash
source ~/.config/kyber/laptop-secrets.env

kubectl create namespace kyber-system --dry-run=client -o yaml | kubectl apply -f -

kubectl -n kyber-system create secret generic kyber-api-credentials \
  --from-literal=api-key="$KYBER_API_KEY" \
  --from-literal=webhook-secret="$KYBER_WEBHOOK_SECRET" \
  --from-literal=k3s-join-token="$K3S_JOIN_TOKEN" \
  --from-literal=k3s-server-url="$K3S_SERVER_URL" \
  --dry-run=client -o yaml | kubectl apply -f -
```

**Verify:**
```bash
kubectl -n kyber-system get secret kyber-api-credentials \
  -o jsonpath='{.data}' | jq -r 'keys | join(",")'
```
Expected output: `api-key,k3s-join-token,k3s-server-url,webhook-secret`.

**If it fails:** → § Troubleshooting / "kyber-api-credentials Secret missing keys".

**Human input needed:** None.

### 5a. Register the Kyber GitHub App

**What this does:** Registers a GitHub App on the operator's GitHub account, captures App ID + Installation ID + private key, creates a Kubernetes Secret holding all three, and points `identityRepo.defaultOwner` at that account. Each agent gets a private **identity repo** (memory, skills, `SOUL.md`, session state) managed **exclusively** by this per-install **Kyber Platform GitHub App**: the control plane mints a **short-lived token scoped to just that one repo** (`contents:write`, ~1h) on demand, which the agent uses for both reads and writes of its own identity repo — no per-agent PATs, and **no PAT fallback** (a broken App flow fails loudly rather than silently succeeding on a broad credential). See [agents-identity-repos.md](./agents-identity-repos.md) for the full credential model.

Both parts are required to enable the feature: the `kyber-github-app` Secret **and** a non-empty `identityRepo.defaultOwner`. If either is absent the identity-repo feature **disables cleanly** — agents are still created and run, just without a managed identity repo (never backfilled with a PAT).

**Preconditions:** § 5.2 complete (`kyber-system` namespace exists); `~/dev/kyber-deploy` cloned (§ 5.1).

**Run** *(after the human-input block below has produced `APP_ID`, `INSTALLATION_ID`, `PEM_PATH`, and `IDENTITY_OWNER`)*:
```bash
kubectl -n kyber-system create secret generic kyber-github-app \
  --from-literal=app-id="$APP_ID" \
  --from-literal=installation-id="$INSTALLATION_ID" \
  --from-file=private-key.pem="$PEM_PATH" \
  --dry-run=client -o yaml | kubectl apply -f -

# Point identityRepo.defaultOwner at the account the App is installed on.
# This flows to the control plane as KYBER_IDENTITY_REPO_OWNER; the chart
# default is empty (feature disabled). ArgoCD picks it up on the next sync.
cd ~/dev/kyber-deploy
sed -i "s|^\(  defaultOwner:\).*|\1 \"$IDENTITY_OWNER\"|" environments/laptop/values.yaml
git add environments/laptop/values.yaml
git -c user.email=kyber-install@local -c user.name=Kyber-Install \
  commit -m "feat(laptop): set identityRepo.defaultOwner"
git push origin "$(git branch --show-current)"

# After applying, shred the local PEM:
shred -u "$PEM_PATH"
```

**Verify:**
```bash
kubectl -n kyber-system get secret kyber-github-app \
  -o jsonpath='{.data}' | jq -r 'keys | join(",")'
grep -E '^\s*defaultOwner:' ~/dev/kyber-deploy/environments/laptop/values.yaml
```
Expected output: first line `app-id,installation-id,private-key.pem`; second line shows `defaultOwner: "<your-account>"` (non-empty).

**If it fails:** → § Troubleshooting / "kyber-github-app Secret missing keys".

**Human input needed:** This step requires browser-only work in the GitHub UI. Walk the operator through the following sequence; do not proceed past each numbered item until the operator confirms.

1. **Create the App.** Visit https://github.com/settings/apps/new while signed in as the GitHub account that should own the agent identity-repos. Fill in:
   - GitHub App name: `Kyber` (try `Kyber-<theirhandle>` if globally taken).
   - Homepage URL: `https://github.com/matty-v/kyber`.
   - Webhook → Active: **uncheck**.
   - Repository permissions: Administration `Read and write`; Contents `Read and write`; Metadata `Read-only` (auto-enabled).
   - Account permissions: leave all None.
   - Subscribe to events: leave all unchecked.
   - Where can this GitHub App be installed?: **Only on this account**.
   Click `Create GitHub App`. Ask the operator to read back the App's URL — it will be `https://github.com/settings/apps/<app-name>`.

2. **Capture App ID.** On the App's settings page (the URL from step 1), the App ID is shown near the top — a 6–7 digit number. Ask the operator to send it back as `APP_ID`.

3. **Generate the private key.** Scroll to "Private keys" → click "Generate a private key". A `.pem` file downloads. Ask the operator to tell you the full absolute path of the downloaded file (e.g. `C:\Users\Friend\Downloads\kyber.YYYY-MM-DD.private-key.pem` on Windows; in WSL2 it's accessible at `/mnt/c/Users/Friend/Downloads/...`). Save the WSL2 path as `PEM_PATH`.

4. **Install the App.** Click "Install App" in the App's left sidebar. Click `Install` next to the operator's account. Choose `All repositories`. Confirm. GitHub redirects to `https://github.com/settings/installations/<number>`. Ask the operator to read back the number — that's `INSTALLATION_ID`.

5. **Bind variables** in the agent's bash shell with the values the operator returned. `IDENTITY_OWNER` is the GitHub account/org the operator installed the App on in step 4 (the account under which agent identity repos will live — usually the operator's own handle):
   ```bash
   APP_ID=<number from step 2>
   INSTALLATION_ID=<number from step 4>
   PEM_PATH=<absolute Linux path from step 3>
   IDENTITY_OWNER=<the GitHub account/org from step 4>
   ```

   Do not invent any of these values. If the operator did not produce a value for any of them, do not proceed.

After the variables are bound, run the **Run** block above.

**Notes:** *(aside)* "All repositories" at install time is safe — installation tokens are minted per-call and scoped to the single repo each agent needs. The App can be reused across multiple Kyber clusters; only the **Run** block (creating the Secret in the cluster) is per-cluster.

## 6. Install ArgoCD + Image Updater and apply the Application

> **Aside.** This section is near-identical to [installation.md § 6](./installation.md#6-install-argocd--image-updater-and-apply-the-application). If you change the ArgoCD/Image Updater bootstrap flow here, update the sibling too.

> **Shell prerequisite.** Every `kubectl` and `helm` command in this section (and § 5 onward) assumes `KUBECONFIG=~/.kube/kyber-laptop.yaml` is set in the shell. § 4b set this up in the operator's interactive bash, but agents invoking commands via non-interactive shells (e.g. `wsl -e bash -c '...'`) do not auto-source `~/.bashrc`. In that case, prefix each command with `KUBECONFIG=~/.kube/kyber-laptop.yaml`, or run `export KUBECONFIG=~/.kube/kyber-laptop.yaml` once per shell invocation.

### 6a. Bootstrap ArgoCD

**What this does:** Installs ArgoCD 9.5.1 and ArgoCD Image Updater 0.14.0 via Helm into the `argocd` namespace using the `bootstrap/install-argocd.sh` script in kyber-deploy.

**Preconditions:**
- § 5.2 complete (`kyber-system` namespace + `kyber-api-credentials` Secret exist).
- `kyber-deploy` is cloned to `~/dev/kyber-deploy` (§ 5.1).

**Run:**
```bash
cd ~/dev/kyber-deploy
bash bootstrap/install-argocd.sh
```

**Verify:**
```bash
for i in {1..18}; do
  ready=$(kubectl -n argocd get pods --no-headers 2>/dev/null | awk '$3=="Running"' | wc -l)
  echo "$i: $ready / 5 running"
  [ "$ready" -ge 5 ] && echo OK && break
  sleep 10
done
```
Expected output: ends with a line `OK` (5 pods running: `argocd-server`, `argocd-application-controller`, `argocd-repo-server`, `argocd-redis`, `argocd-image-updater`).

**If it fails:** → § Troubleshooting / "ArgoCD bootstrap failed".

**Human input needed:** None.

### 6b. Create GHCR credentials and configure Image Updater

**What this does:** Creates the `ghcr-creds` Secret in the `argocd` namespace and patches the Image Updater ConfigMap so it polls GHCR every ~2 minutes for new image digests.

**Preconditions:** 6a complete.

**Run** *(after the human-input block below has produced `GHCR_READ_TOKEN`)*:
```bash
kubectl -n argocd create secret generic ghcr-creds \
  --from-literal=creds="matty-v:${GHCR_READ_TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n argocd patch configmap argocd-image-updater-config --type merge \
  -p '{"data":{"registries.conf":"registries:\n- name: ghcr\n  api_url: https://ghcr.io\n  prefix: ghcr.io\n  credentials: secret:argocd/ghcr-creds#creds\n  default: true\n"}}'

kubectl -n argocd rollout restart deploy/argocd-image-updater
kubectl -n argocd rollout status deploy/argocd-image-updater --timeout=120s

unset GHCR_READ_TOKEN
```

**Verify:**
```bash
kubectl -n argocd get cm argocd-image-updater-config -o jsonpath='{.data.registries\.conf}' | grep -q 'ghcr' && \
  kubectl -n argocd get pods -l app.kubernetes.io/name=argocd-image-updater \
    --no-headers | awk '$3=="Running" && $2=="1/1"{print "OK"}'
```
Expected output: `OK`.

**If it fails:** → § Troubleshooting / "Image Updater configuration not applied".

**Human input needed:** This step needs a GitHub Personal Access Token with `read:packages` scope.

1. Walk the operator to https://github.com/settings/tokens/new (signed in as the GitHub account that owns the kyber fork — for the upstream install on `matty-v/kyber`, this is the operator's own GitHub account, used only to authenticate against GHCR).
2. Set:
   - Note: `kyber-ghcr-read`
   - Expiration: `90 days`
   - Permissions section → check **`read:packages`**.
3. Click `Generate token`. Ask the operator to copy the token value (it's shown only once) and paste it back to you.
4. Bind the value:
   ```bash
   GHCR_READ_TOKEN=<paste from operator>
   ```
   Do not invent this value. Do not log it.

After the variable is bound, run the **Run** block above. The trailing `unset GHCR_READ_TOKEN` clears it from the agent's shell after use.

### 6c. Create GitHub repo credentials for ArgoCD

**What this does:** Creates the `github-repo-creds` Secret in the `argocd` namespace and labels it as a repo-creds template so ArgoCD can read both `matty-v/kyber` (Helm chart source) and `matty-v/kyber-deploy` (values + Application manifest).

**Preconditions:** 6a complete.

**Run** *(after the human-input block below has produced `GITHUB_PAT`)*:
```bash
kubectl -n argocd create secret generic github-repo-creds \
  --from-literal=url=https://github.com/matty-v \
  --from-literal=username=matty-v \
  --from-literal=password="${GITHUB_PAT}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n argocd label secret github-repo-creds \
  argocd.argoproj.io/secret-type=repo-creds --overwrite

unset GITHUB_PAT
```

**Verify:**
```bash
kubectl -n argocd get secret github-repo-creds \
  -o jsonpath='{.metadata.labels.argocd\.argoproj\.io/secret-type}'
```
Expected output: `repo-creds`.

**If it fails:** → § Troubleshooting / "github-repo-creds Secret missing or unlabeled".

**Human input needed:** This step needs a second GitHub Personal Access Token, distinct from 6b's, with `repo` (read) scope.

1. Walk the operator to https://github.com/settings/tokens/new (in a browser, signed in as the same account).
2. Set:
   - Note: `kyber-argocd-repo`
   - Expiration: `90 days`
   - Permissions section → check **`repo`** (the top-level checkbox; the four sub-scopes auto-select).
3. Click `Generate token`, copy the token, paste back to you.
4. Bind the value:
   ```bash
   GITHUB_PAT=<paste from operator>
   ```
   Do not invent. Do not log.

After the variable is bound, run the **Run** block. The trailing `unset GITHUB_PAT` clears it.

### 6d. Apply the ArgoCD Application

**What this does:** Applies the ArgoCD Application manifest from `kyber-deploy/environments/laptop/application.yaml`. ArgoCD then syncs the manifest, renders the Helm chart, and creates the Kyber resources (control plane, node-agent, Postgres, Redis, services) inside `kyber-system`.

**Preconditions:**
- 6a, 6b, 6c complete.
- § 1 verify passed (GHCR images public).

**Run:**
```bash
kubectl apply -f ~/dev/kyber-deploy/environments/laptop/application.yaml
```

**Verify:**
```bash
for i in {1..30}; do
  status=$(kubectl -n argocd get application kyber-laptop \
    -o jsonpath='{.status.sync.status}/{.status.health.status}' 2>/dev/null)
  echo "$i: $status"
  [ "$status" = "Synced/Healthy" ] && echo OK && break
  sleep 10
done
```
Expected output: ends with a line `OK` (the Application reaches `Synced/Healthy` — first sync typically 60–180 seconds).

**If it fails:** → § Troubleshooting / "ArgoCD sync stuck on OutOfSync".

**Human input needed:** None.

**Notes:** *(aside)* Once the Application is `Synced/Healthy`, `kubectl -n kyber-system get pods,svc` should show the control plane Deployment, node-agent DaemonSet, Postgres + Redis StatefulSets, and a LoadBalancer Service. If the Service stays in `Pending`, see Troubleshooting / "LoadBalancer stuck in Pending".

## 7. Verify

> **Aside.** The verify steps here are near-identical to [installation.md § 7](./installation.md#7-verify) — keep healthz/fleet-summary assertions in sync across both docs.

**What this does:** Confirms the control plane is reachable on WSL2 localhost and answers an authenticated API call with the expected zero-state JSON.

**Preconditions:**
- § 6d verify passed (Application `Synced/Healthy`).
- `KUBECONFIG=~/.kube/kyber-laptop.yaml` set in the shell (see § 6 Shell prerequisite).

**Run:** *(no command — verify-only)*

**Verify:**
```bash
# /healthz returns HTTP 200 with an empty body
curl -fsS -o /dev/null -w '%{http_code}\n' http://localhost:8080/healthz

# fleet summary returns the zero-state JSON
source ~/.config/kyber/laptop-secrets.env
curl -fsS -H "Authorization: Bearer $KYBER_API_KEY" http://localhost:8080/api/v1/fleet/summary
```
Expected output:
- First curl prints `200` on its own line.
- Second curl prints exactly `{"machineCount":0,"agentCount":0,"agentsByPhase":{},"machinesByPhase":{}}`.

**If it fails:**
- If the first curl hangs or prints a non-200 status: → § Troubleshooting / "klipper-lb not binding port 8080".
- If the first curl is 200 but the second prints an error or 401 body: → § Troubleshooting / "PWA returns 401 from API".

**Human input needed:** None.

**Notes:** *(aside)* `/healthz` returns 200 with an *empty body*, not `ok`. The empty response does not mean the service is down — the 200 status is the signal. `kubectl get svc kyber-laptop-control-plane -n kyber-system` will report an `EXTERNAL-IP` in the WSL2-internal `172.x.x.x` range; that IP is only reachable from the WSL2 host. External and on-phone access goes through Tailscale Funnel in § 10.

## 8. Open the PWA

**What this does:** Opens the PWA in a browser on the Windows host and configures the API key in browser localStorage so future API calls authenticate.

**Preconditions:** § 7 verify passed.

**Run:** *(no command — operator-driven)*

**Verify:** Operator confirms the PWA Fleet Overview page loads at `http://localhost:8080/` and shows zero machines and zero agents.

**If it fails:** → § Troubleshooting / "PWA does not load on localhost:8080".

**Human input needed:** This step is browser-only.

1. Walk the operator: open a browser **on Windows** (not inside WSL2) and visit `http://localhost:8080/`. WSL2 bridges its network to Windows, so `localhost:8080` on Windows connects to klipper-lb inside WSL2 with no extra configuration.
2. The PWA renders the Settings page on first load and asks for the API key. Retrieve the key for the operator:
   ```bash
   source ~/.config/kyber/laptop-secrets.env
   echo "$KYBER_API_KEY"
   ```
   Paste the value into the **API key** field and click **Save**.
3. The PWA navigates to the Fleet Overview page. Operator confirms: zero machines, zero agents, no error banners.

**Notes:** *(aside)* The API key is stored in browser `localStorage`. Each browser profile has its own; opening the PWA in a different browser or a private/incognito window requires re-entering the key. Treat the browser as a trusted device.

## 9. Create your first Machine and Agent

> **Aside.** This section is distinct from [installation.md § 9](./installation.md#9-create-your-first-agent) — on standalone there is no VM to provision; the Machine is a `provider=mock` reference to the WSL2 host itself.

### 9.1 Create the Machine

**What this does:** Creates a Machine CRD (`provider=mock`) that represents the WSL2 host as the single available compute node. The Machine Controller short-circuits — no cloud adapter is invoked — and the Machine reaches `Phase=Ready` within seconds.

**Preconditions:**
- § 8 verify passed (PWA loaded; operator has API key in browser localStorage).
- `KUBECONFIG=~/.kube/kyber-laptop.yaml` set (see § 6 Shell prerequisite).

**Run:** *(no command — operator-driven via the PWA)*

**Verify:**
```bash
for i in {1..6}; do
  phase=$(kubectl -n kyber-system get machine local -o jsonpath='{.status.phase}' 2>/dev/null)
  echo "$i: phase=$phase"
  [ "$phase" = "Ready" ] && echo OK && break
  sleep 5
done
```
Expected output: ends with a line `OK` (Machine `local` is in Phase `Ready`).

**If it fails:** → § Troubleshooting / "Machine stuck in Provisioning or Failed".

**Human input needed:** This step is browser-only.

1. In the PWA (already open from § 8), navigate to **Machines → New Machine**.
2. Fill the form:
   - **Name:** `local`
   - **CPU:** a value up to the host's available CPUs (e.g., `4`)
   - **Memory:** a value up to the host's available memory (e.g., `16Gi`)
3. Click **Create Machine**. The form closes; the Machines list now shows `local` with phase transitioning Provisioning → Ready.

**Notes:** *(aside)* On standalone, only one Machine per cluster is permitted (`provider=mock` is host-bound). The PWA hides the "New Machine" button after the first Machine is created.

### 9.2 Open the Create Agent form and start the OAuth flow

**What this does:** Opens the Create Agent form, fills it in, and clicks `Authorize with Claude` — which opens Anthropic's OAuth authorize URL in a new browser tab and pauses for the operator to approve.

**Preconditions:** 9.1 verify passed.

**Run:** *(no command — operator-driven via the PWA)*

**Verify:** The OAuth authorize URL has loaded in a new browser tab. The URL begins with `https://claude.ai/oauth/authorize?` and shows Anthropic's "Authorize Kyber" page asking the operator to grant scope.

**If it fails:** → § Troubleshooting / "Authorize with Claude does not open the OAuth tab".

**Human input needed:** Browser-only.

1. In the PWA, navigate to **Agents → New Agent**.
2. Fill the form:
   - **Name:** a kebab-case agent name (e.g., `dave`)
   - **Runtime:** `claude-code`
   - **Model:** `claude-sonnet-4-5` (or another supported model from the dropdown)
   - **Machine:** the Machine created in 9.1 (only one entry, e.g. `local`)
   - **CPU / Memory / Disk:** must fit under the Machine's remaining budget. The PWA shows a live readout (e.g. `local: 3.75 cpu / 15.5 Gi free · 0 agent(s)`); pick smaller values than the displayed free amount.
3. Click **Authorize with Claude**. A new tab opens to `https://claude.ai/oauth/authorize?...`.

Pause here. Do not proceed to 9.3 until the operator confirms the OAuth tab has loaded.

### 9.3 Paste the authorization code and create the Agent

**What this does:** The operator approves the scope grant on Anthropic's OAuth page, copies the authorization code shown, pastes it back into the PWA's `Authorization code` field, and clicks `Create Agent`. The control plane exchanges the code + PKCE verifier for `{access_token, refresh_token}` and creates the Agent CRD.

**Preconditions:** 9.2 verify passed (OAuth tab open).

**Run:** *(no command — operator-driven via the PWA)*

**Verify:**
```bash
kubectl -n kyber-system get agent dave -o jsonpath='{.status.phase}'
```
Expected output: a phase string — `Creating`, `Starting`, or `Running` are all OK at this point. `NeedsAuth` or `Failed` mean the create did not land cleanly.

**If it fails:** → § Troubleshooting / "Agent CRD missing or in NeedsAuth phase".

**Human input needed:** Browser-only. The agent should NOT invent or guess the authorization code under any circumstances.

1. In the OAuth tab opened in 9.2, ask the operator to sign in to Anthropic (if not already), then click **Authorize**. Anthropic's page renders an authorization code (a random string like `code-abc123…`).
2. Operator copies the code and switches back to the PWA's Create Agent form.
3. Operator pastes the code into the **Authorization code** field, then clicks **Create Agent**.
4. The PWA shows "Creating agent…" then navigates to the Agents list. The new agent (`dave`) appears with phase `Creating`.

If the operator returns saying "the OAuth tab failed" or "Anthropic showed an error," do NOT retry by guessing — go back to 9.2's troubleshooting.

### 9.4 Wait for the Agent to reach Running

**What this does:** Polls the Agent's phase until it reaches `Running` — the agent pod has booted, refreshed its OAuth token, and started Claude Code.

**Preconditions:** 9.3 verify passed (phase is `Creating` or later).

**Run:** *(no command — verify-only polling)*

**Verify:**
```bash
for i in {1..36}; do
  phase=$(kubectl -n kyber-system get agent dave -o jsonpath='{.status.phase}' 2>/dev/null)
  echo "$i: phase=$phase"
  [ "$phase" = "Running" ] && echo OK && break
  case "$phase" in
    Failed|NeedsAuth|CrashLoopBackOff) echo "TERMINAL: $phase" && break ;;
  esac
  sleep 10
done
```
Expected output: ends with a line `OK` (Agent reached `Running` within ~6 minutes).

**If it fails (output ends with `TERMINAL: <phase>` or never reaches `OK`):** → § Troubleshooting / "Agent does not reach Running on standalone WSL2".

**Human input needed:** None.

**Notes:** *(aside)* On standalone WSL2, the agent pod's Phase C dependency on `/dev/fuse` + `Privileged: true` + `mount(MS_BIND)` is functional, but its end-to-end reliability across kernel updates is not yet covered by CI. If a pod stays in `CrashLoopBackOff`, inspect with `kubectl -n kyber-system logs pod/agent-<name> --previous`.

### Scheduling recurring work on an agent

Once an agent is Running, it has cron available out of the box. See [docs/agents-scheduled-jobs.md](./agents-scheduled-jobs.md) for the supported `crontab -e` and `/etc/cron.d/` install paths and how persistence works across pod restarts.

## 10. HTTPS via Tailscale Funnel

> **Aside.** This setup is near-identical to [installation.md § 10](./installation.md#10-https-via-tailscale-funnel). When changing `tailscale up` flags or Funnel config, update both — past sync drift has cost us debugging time.

Tailscale Funnel gives you a public HTTPS URL with automatic Let's Encrypt cert provisioning — no domain purchase, no cert rotation, no DNS setup.

### 10.1 Install Tailscale and join the tailnet

**What this does:** Installs the Tailscale package, enables `tailscaled` as a systemd service, and runs `tailscale up` with an auth key from the operator's tailnet, joining the WSL2 host as a node named `kyber-wsl`.

**Preconditions:**
- § 0.5 (`curl` installed); systemd running inside WSL2.
- Operator has a Tailscale account with Funnel enabled (see Prerequisites).

**Run** *(after the human-input block below has produced `AUTHKEY`)*:
```bash
curl -fsSL https://tailscale.com/install.sh | sudo sh
sudo systemctl enable --now tailscaled

sudo tailscale up \
  --auth-key="$AUTHKEY" \
  --hostname=kyber-wsl \
  --accept-dns=false \
  --operator="$USER"

unset AUTHKEY
```

**Verify:**
```bash
tailscale status | grep -q ' kyber-wsl ' && \
  tailscale ip -4 | grep -qE '^100\.' && echo OK
```
Expected output: `OK`.

**If it fails:** → § Troubleshooting / "tailscale up rejected for missing --operator flag".

**Human input needed:** This step needs a single-use Tailscale auth key.

1. Walk the operator to https://login.tailscale.com/admin/settings/keys (signed in to their Tailscale account).
2. Click `Generate auth key…` and set:
   - Reusable: **off**
   - Ephemeral: **off**
   - Pre-authorized: **on**
   - Tags: *(leave empty unless the operator's tailnet ACL requires them)*
   - Expiration: `1 day`
3. Click `Generate key`. Tailscale shows the value (`tskey-auth-...`) once. Operator copies it and pastes back to you.
4. Bind the value:
   ```bash
   AUTHKEY=<paste from operator>
   ```
   Do not invent. Do not log. The trailing `unset AUTHKEY` clears it after `tailscale up` consumes it.

After the variable is bound, run the **Run** block above.

**Notes:** *(aside)* Flag rationale: `--hostname=kyber-wsl` keeps a clean tailnet identity separate from any other Tailscale node on the operator's machine. `--accept-dns=false` keeps WSL2's DNS resolver using k3s ClusterDNS so in-cluster service names (`kyber-laptop-postgres`, `kyber-laptop-redis`) resolve. `--operator="$USER"` is required on a fresh tailscaled install — see Troubleshooting "tailscale up rejected for missing --operator flag" for the gotcha.

### 10.2 Enable Funnel on port 8080

**What this does:** Tells Tailscale to terminate HTTPS on a public Funnel URL and forward traffic to the local k3s service on `:8080`. Tailscale auto-provisions a Let's Encrypt cert for `kyber-wsl.<tailnet>.ts.net`.

**Preconditions:** 10.1 verify passed.

**Run:**
```bash
sudo tailscale funnel --bg 8080
```

**Verify:**
```bash
tailscale funnel status 2>/dev/null | grep -E 'https://kyber-wsl\.[a-z0-9]+\.ts\.net'
```
Expected output: a single line containing the public Funnel URL, like `https://kyber-wsl.<tailnet>.ts.net (...)`. Capture this URL — you'll need it in 10.3 and 10.4.

**If it fails:** → § Troubleshooting / "Tailscale Funnel did not bind 8080".

**Human input needed:** None.

### 10.3 Set api.publicURL in kyber-deploy values

**What this does:** Updates `api.publicURL` in the operator's clone of `kyber-deploy/environments/laptop/values.yaml` so the control plane knows its public URL, then commits and pushes the change. ArgoCD picks up the change within ~2 minutes and triggers a rolling restart of the control-plane pod with the new `KYBER_PUBLIC_URL`.

**Preconditions:** 10.2 captured the public URL.

**Run:**
```bash
TAILNET_URL=$(tailscale funnel status 2>/dev/null \
  | grep -oE 'https://kyber-wsl\.[a-z0-9]+\.ts\.net' | head -1)
test -n "$TAILNET_URL" || { echo "ERR: TAILNET_URL not captured"; exit 1; }

cd ~/dev/kyber-deploy
sed -i "s|publicURL: .*|publicURL: \"$TAILNET_URL\"|" environments/laptop/values.yaml
git add environments/laptop/values.yaml
git -c user.email=kyber-install@local -c user.name=Kyber-Install \
  commit -m "feat(laptop): set publicURL"
git push origin "$(git branch --show-current)"
```

**Verify:**
```bash
for i in {1..15}; do
  pub=$(kubectl -n kyber-system get deploy kyber-laptop-control-plane \
    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="KYBER_PUBLIC_URL")].value}' 2>/dev/null)
  echo "$i: $pub"
  [ "$pub" = "$TAILNET_URL" ] && echo OK && break
  sleep 20
done
```
Expected output: ends with a line `OK` (the control-plane Deployment's `KYBER_PUBLIC_URL` env var matches `$TAILNET_URL`).

**If it fails:** → § Troubleshooting / "publicURL did not propagate to control plane".

**Human input needed:** The `git push` will prompt for the operator's GitHub credentials if the local git config doesn't already have them. If prompted, ask the operator to provide a Personal Access Token with `repo` scope (it can be the same `kyber-argocd-repo` PAT created in 6c, *or* a new one — the operator may have rotated). Walk the operator to set the credentials inline before retrying:
```bash
cd ~/dev/kyber-deploy
git remote set-url origin "https://${GH_USER}:${GH_PAT}@github.com/${GH_USER}/kyber-deploy.git"
```
Where `GH_USER` is the operator's GitHub handle and `GH_PAT` is a `repo`-scoped PAT. Do not invent the PAT.

### 10.4 Verify external HTTPS reachability

**What this does:** Confirms the Funnel URL is reachable from the public internet — both from the WSL2 host (loopback through Funnel) and, more importantly, from the operator's phone (off-WiFi).

**Preconditions:** 10.3 verify passed (control-plane has rolled with the new public URL).

**Run:** *(no command — verify-only)*

**Verify (from inside WSL2):**
```bash
test -n "$TAILNET_URL" || TAILNET_URL=$(tailscale funnel status 2>/dev/null \
  | grep -oE 'https://kyber-wsl\.[a-z0-9]+\.ts\.net' | head -1)

source ~/.config/kyber/laptop-secrets.env

curl -fsS -o /dev/null -w '%{http_code}\n' "$TAILNET_URL/healthz"
curl -fsS -H "Authorization: Bearer $KYBER_API_KEY" "$TAILNET_URL/api/v1/fleet/summary"

HOST=${TAILNET_URL#https://}
echo | openssl s_client -servername "$HOST" -connect "$HOST":443 2>/dev/null \
  | openssl x509 -noout -issuer -dates
```
Expected output:
- First curl prints `200`.
- Second curl prints exactly `{"machineCount":1,"agentCount":1,"agentsByPhase":{...},"machinesByPhase":{...}}` (or zero counts if the operator skipped agent creation).
- openssl block prints an `issuer=` line containing `Let's Encrypt` and `notBefore=` / `notAfter=` lines bracketing today.

**Verify (from outside WSL2 — the on-phone smoke test):** Walk the operator: open `$TAILNET_URL/` on their phone, **over cellular** (not WiFi), and confirm the PWA Fleet Overview loads. This is the proof that Funnel reaches the public internet, not just localhost.

**If it fails (any check above):** → § Troubleshooting / "Funnel URL unreachable from outside WSL2".

**Human input needed:** Step out to the operator for the on-phone smoke test:

1. Operator opens their phone's data connection (turn off WiFi if needed).
2. Operator visits `<TAILNET_URL>/` in their phone's browser.
3. The PWA Settings page loads asking for the API key. Operator pastes the value from `$KYBER_API_KEY` (you can paste it back to them or have them retrieve it from `~/.config/kyber/laptop-secrets.env` on the laptop).
4. Operator confirms the Fleet Overview page loads on their phone.

This completes the install — the operator can now drive Kyber from anywhere with internet, not just the WSL2 host.

## Troubleshooting

### WSL2 not installed or Ubuntu missing

**Symptom:** Step 0.1 verify shows no Ubuntu distro, or the distro shows VERSION `1` instead of `2`.

**Likely cause:** WSL2 isn't installed on this Windows host, or only WSL1 is available.

**Fix:** Run `wsl --install` from PowerShell (as Administrator). This installs WSL2 plus the default Ubuntu distro. After install, reboot Windows and open Ubuntu from the Start menu to complete first-launch user creation. If a distro is on VERSION `1`, convert it: `wsl --set-version Ubuntu 2`.

If `wsl --install` itself fails (most common on older Windows), the underlying Windows version may pre-date WSL2 support. WSL2 requires Windows 10 build 19041 ("21H1") or later, or Windows 11. Confirm with `winver` (run from PowerShell) and upgrade Windows if needed.

**Verify the fix:** Re-run step 0.1's verify.

### systemd block missing or malformed in wsl.conf

**Symptom:** Step 0.3 verify (`grep -A1 '\[boot\]' /etc/wsl.conf | grep -q 'systemd=true' && echo OK`) prints nothing.

**Likely cause:** The `[boot]` block was not appended to `/etc/wsl.conf`, or the `systemd=true` line is malformed.

**Fix:**
```bash
sudo tee -a /etc/wsl.conf > /dev/null <<'EOF'
[boot]
systemd=true
EOF
```

If `/etc/wsl.conf` already had a `[boot]` block with a different setting, edit it manually to set `systemd=true` instead of appending a second `[boot]` block.

**Verify the fix:** Re-run step 0.3's verify (and the `systemctl is-system-running` check after `wsl --shutdown` + reopen).

### wsl -e bash unavailable or returns wrong distro

**Symptom:** Step 0.4 verify (`wsl -e bash -c 'whoami && uname -a'`) errors out, returns the wrong username, or returns a kernel string without `microsoft-standard-WSL2`.

**Likely cause:** Multiple WSL distros are installed and `wsl -e` is hitting the wrong default; or `wsl --shutdown` from step 0.3 hasn't been run since the systemd change.

**Fix:**
```powershell
wsl -l -v             # confirm Ubuntu shows STATE Running
wsl --set-default Ubuntu  # if a different distro is currently default
```

If Ubuntu has not started since the last `wsl --shutdown`, open Ubuntu from the Start menu (or run `wsl` in PowerShell) once before retrying.

**Verify the fix:** Re-run step 0.4's verify.

### Tool install failed

**Symptom:** Step 0.5 verify reports one or more `MISSING` lines.

**Likely cause:** `apt update` failed (network), an APT repo configuration mismatch (kubectl), or the helm install script returned non-zero.

**Fix:**
```bash
sudo apt update 2>&1 | tail
```

If `apt update` errors mention the kubernetes APT repo, re-run the keyring + sources.list.d block from step 0.5 — most common cause is a previous partial run that left a malformed entry. After fixing, retry the missing tool's install (`sudo apt install -y <tool>` or, for helm, re-run the install script).

**Verify the fix:** Re-run step 0.5's verify.

### Port 8080 already in use

**Symptom:** Step 0.6 verify shows a `LISTEN` line on `:8080` instead of `port free`.

**Likely cause:** Another local service (a dev server, an old k3s install, etc.) is bound to 8080. klipper-lb cannot bind alongside it.

**Fix:**
```bash
ss -tlnp | grep ':8080 '
# inspect the PID/process. Stop it via the appropriate mechanism
# (e.g. systemctl stop <unit>, kill <pid>, or close the dev server).
```

**Verify the fix:** Re-run step 0.6's verify.

### Secret generation produced wrong length

**Symptom:** `${#KYBER_API_KEY}` is 0 or some non-64 value after sourcing `laptop-secrets.env`.

**Likely cause:** `openssl rand -hex 32` failed silently (rare), or the heredoc didn't write to the file as expected.

**Fix:**
```bash
ls -la ~/.config/kyber/laptop-secrets.env  # check the file exists and is non-empty
cat ~/.config/kyber/laptop-secrets.env     # confirm both lines are well-formed KEY=VALUE
```
Re-run the step's `cat > ... <<EOF` block if the file is empty or malformed.

**Verify the fix:** Re-run step 2's verify.

### k3s install failed or node not Ready

**Symptom:** `systemctl is-active k3s` is not `active`, or `k3s kubectl get nodes` shows STATUS `NotReady` (or no nodes at all) after 60 seconds.

**Likely cause:** Install script failed mid-stream (e.g. apt lock, transient network), or the node hasn't finished startup.

**Fix:**
```bash
sudo journalctl -u k3s --no-pager | tail -50
```
Read the tail. Most common: rerun the install one-liner from step 3 — the install script is idempotent.

**Verify the fix:** Re-run step 3's verify.

### kubeconfig copy permission error

**Symptom:** Step 4a's `sudo install` errors, or the verify shows mode other than 600.

**Likely cause:** Source file `/etc/rancher/k3s/k3s.yaml` doesn't exist (k3s didn't finish installing) or the user's home directory doesn't have a writable `.kube`.

**Fix:** Confirm the source exists with `sudo ls -la /etc/rancher/k3s/k3s.yaml`. If missing, return to step 3. Otherwise, re-run step 4a.

**Verify the fix:** Re-run step 4a's verify.

### kubectl get nodes returns empty or error

**Symptom:** `kubectl get nodes` after `KUBECONFIG` export fails with connection refused, or returns no nodes.

**Likely cause:** k3s isn't running, or `$KUBECONFIG` points at the wrong path.

**Fix:**
```bash
echo "$KUBECONFIG"
sudo systemctl status k3s --no-pager | head
```
If `$KUBECONFIG` is empty, re-export it. If k3s isn't running, restart it: `sudo systemctl restart k3s`.

**Verify the fix:** Re-run step 4b's verify.

### k3s join token empty or unreadable

**Symptom:** `K3S_JOIN_TOKEN` ends up empty, or `sudo cat /var/lib/rancher/k3s/server/node-token` fails.

**Likely cause:** k3s server isn't running, or the install completed too recently for the token file to exist.

**Fix:** Wait 10s after `systemctl is-active k3s` returns `active`, then retry. If still empty, re-run step 3.

**Verify the fix:** Re-run step 4c's verify.

### kyber-deploy clone failed

**Symptom:** Step 5.1 verify fails: `~/dev/kyber-deploy/environments/laptop/values.yaml` is missing.

**Likely cause:** Network failure mid-clone, or `~/dev/` doesn't exist as a writable directory.

**Fix:** `mkdir -p ~/dev && rm -rf ~/dev/kyber-deploy`, then re-run step 5.1.

**Verify the fix:** Re-run step 5.1's verify.

### kyber-api-credentials Secret missing keys

**Symptom:** Step 5.2 verify shows fewer than four key names, or the comma-separated list is in a different order from expected.

**Likely cause:** One or more of `KYBER_API_KEY`, `KYBER_WEBHOOK_SECRET`, `K3S_JOIN_TOKEN`, `K3S_SERVER_URL` was empty when `kubectl create secret` ran.

**Fix:**
```bash
source ~/.config/kyber/laptop-secrets.env
for v in KYBER_API_KEY KYBER_WEBHOOK_SECRET K3S_JOIN_TOKEN K3S_SERVER_URL; do
  printf '%s=%s\n' "$v" "${!v:0:8}…"
done
```
Any var that prints `=…` (empty value) is the culprit. Re-do step 2 or 4c as needed, then re-run step 5.2.

**Verify the fix:** Re-run step 5.2's verify.

### kyber-github-app Secret missing keys

**Symptom:** Step 5a verify reports fewer than three keys.

**Likely cause:** `APP_ID`, `INSTALLATION_ID`, or `PEM_PATH` was empty / pointed at a non-existent file.

**Fix:**
```bash
echo "APP_ID=$APP_ID INSTALLATION_ID=$INSTALLATION_ID"
ls -la "$PEM_PATH"
```
Fix any empty/missing value (return to the relevant numbered item in 5a's Human input block) and re-run the Run block.

**Verify the fix:** Re-run step 5a's verify.

### ArgoCD bootstrap failed

**Symptom:** Step 6a verify counts fewer than 5 pods Running after several minutes, or the bootstrap script exits non-zero.

**Likely cause:** Helm install partial-failure (network, name collision with a previous install) or the script's prerequisite checks failed.

**Fix:**
```bash
kubectl -n argocd get pods                        # see which pods are unhealthy
kubectl -n argocd describe pod <unhealthy-pod>    # check Events
```

If a previous ArgoCD install left orphaned resources, clean before retrying:
```bash
helm uninstall argocd -n argocd 2>/dev/null
kubectl delete namespace argocd
```
Then re-run step 6a.

**Verify the fix:** Re-run step 6a's verify.

### Image Updater configuration not applied

**Symptom:** Step 6b verify produces no `OK` (the `registries.conf` patch didn't land or the Image Updater pod isn't Running 1/1).

**Likely cause:** The `kubectl patch configmap` command's JSON merge failed (typo in the patch payload) or the Image Updater pod failed to roll cleanly.

**Fix:**
```bash
kubectl -n argocd get cm argocd-image-updater-config -o yaml
kubectl -n argocd describe deploy argocd-image-updater | tail -30
```

If the `registries.conf` is empty or malformed, re-run the `patch configmap` block from step 6b. If the deploy didn't roll, run `kubectl -n argocd rollout restart deploy/argocd-image-updater` and `kubectl -n argocd rollout status deploy/argocd-image-updater --timeout=120s`.

**Verify the fix:** Re-run step 6b's verify.

### github-repo-creds Secret missing or unlabeled

**Symptom:** Step 6c verify outputs nothing instead of `repo-creds`, or the Secret doesn't exist.

**Likely cause:** The Secret was created but not labeled, or the create command failed silently because `$GITHUB_PAT` was empty when it ran.

**Fix:**
```bash
kubectl -n argocd get secret github-repo-creds -o yaml
```

If the Secret is missing, return to 6c's Human input block and confirm `$GITHUB_PAT` is set, then re-run the Run block. If the Secret exists without the label, just re-run the `kubectl label` command from 6c.

**Verify the fix:** Re-run step 6c's verify.

### ArgoCD sync stuck on OutOfSync

```bash
kubectl -n argocd describe application kyber-laptop | tail -40
```

If `Sync Status` is `OutOfSync`, check whether the repo credentials are working:
```bash
kubectl -n argocd get secret github-repo-creds \
  -o jsonpath='{.metadata.labels}' | jq .
```

The label `argocd.argoproj.io/secret-type: repo-creds` must be present. If it's missing, re-run the label command from step 6c.

### Control plane CrashLoopBackOff

```bash
kubectl -n kyber-system logs deploy/kyber-laptop-control-plane --previous
```

Common causes:
- API key or webhook secret is empty — check the `kyber-api-credentials` secret has all four keys
- Postgres or Redis connection refused — check those pods are `Running` (`kubectl -n kyber-system get pods`) and the DSN env vars are correct (`kyber-laptop-postgres:5432`, `kyber-laptop-redis:6379`)
- RBAC missing — check `kyber-laptop-control-plane` ClusterRole exists and is bound

### PWA returns 401 from API

The API key in the browser doesn't match the one in the k8s secret. Open the PWA Settings page and paste the correct key from:
```bash
source ~/.config/kyber/laptop-secrets.env
echo $KYBER_API_KEY
```

### GHCR images private or missing

**Symptom:** Step 1's verify reports `NOT public` for one or more images, or `manifests/latest` returns 404.

**Likely cause:** GHCR packages default to private; first-time forks must explicitly make them public.

**Fix:** Visit each affected package's settings page in the GitHub UI and change visibility to Public. URLs:
- https://github.com/users/matty-v/packages/container/kyber-control-plane/settings
- https://github.com/users/matty-v/packages/container/kyber-node-agent/settings
- https://github.com/users/matty-v/packages/container/kyber-runtime-base/settings
- https://github.com/users/matty-v/packages/container/kyber-claude-code/settings

Scroll to Danger Zone → Change visibility → Public → confirm.

**Verify the fix:** Re-run step 1's verify.

### Pods stuck in ImagePullBackOff

The GHCR packages aren't public. See [installation.md § 1](./installation.md#1-verify-ci-has-published-images-and-make-them-public) for the visibility steps. Package visibility changes take effect immediately — no pod restart needed, just wait for the next pull attempt (~30 seconds).

### LoadBalancer stuck in Pending

klipper-lb binds directly to the WSL2 eth0 interface on port 8080. If another process already holds port 8080, klipper-lb will loop in Pending.

```bash
ss -tlnp | grep :8080
```

Free the port or change `api.service.port` in `environments/laptop/values.yaml`.

### klipper-lb not binding port 8080

**Symptom:** Step 7's first curl (to `http://localhost:8080/healthz`) hangs or returns connection refused.

**Likely cause:** klipper-lb (k3s's built-in LoadBalancer) hasn't finished binding the eth0 interface, or another process holds 8080.

**Fix:**
```bash
kubectl -n kube-system logs -l app=svclb-kyber-laptop-control-plane --tail=50
ss -tlnp | grep ':8080 '
```
- If `ss` shows another PID bound to 8080: stop it (see § Troubleshooting / "Port 8080 already in use") and re-run § 6d's polling verify.
- If the svclb logs show errors: restart the LB DaemonSet — `kubectl -n kube-system rollout restart daemonset/svclb-kyber-laptop-control-plane`.

**Verify the fix:** Re-run step 7's verify.

### PWA does not load on localhost:8080

**Symptom:** Operator's browser on Windows shows "This site can't be reached" or a connection error when opening `http://localhost:8080/`.

**Likely cause:** Step 7 verify hadn't completed (klipper-lb not yet bound), or Windows host networking isn't bridging to WSL2.

**Fix:**
- First confirm step 7's verify still passes from inside WSL2: `curl -fsS -o /dev/null -w '%{http_code}\n' http://localhost:8080/healthz` should print `200`. If not, follow that step's recovery.
- From PowerShell on Windows, confirm the bridge works: `curl.exe http://localhost:8080/healthz` (Windows 10+ ships with curl). Should also print HTTP 200.
- If WSL2-side curl works but Windows-side doesn't, restart the WSL2 networking stack: `wsl --shutdown` from PowerShell, reopen Ubuntu, then re-run from § 7.

**Verify the fix:** Operator confirms `http://localhost:8080/` loads in their Windows browser.

### tailscale up rejected for missing --operator flag

This is the `--operator` gotcha documented in [§ 10](#10-https-via-tailscale-funnel). Always include `--operator="$USER"` in the `tailscale up` invocation. Full working command:

```bash
sudo tailscale up \
  --auth-key="$AUTHKEY" \
  --hostname=kyber-wsl \
  --accept-dns=false \
  --operator="$USER"
```

### tailscaled not starting on WSL2

WSL2 doesn't auto-start systemd services on package install. Run:
```bash
sudo systemctl enable --now tailscaled
sudo systemctl status tailscaled
```

If systemd itself isn't running (you see `System has not been booted with systemd`), check that `/etc/wsl.conf` contains:

```ini
[boot]
systemd=true
```

Then rerun `wsl --shutdown` from Windows.

### Machine stuck in Provisioning or Failed

**Symptom:** Step 9.1 verify never reaches `Ready`; phase stays `Provisioning` past 30 seconds, or transitions to `Failed`.

**Likely cause:** The Machine Controller is not reconciling — control plane pod is unhealthy — or the cluster has no Ready node for the mock provider to bind.

**Fix:**
```bash
kubectl -n kyber-system logs deploy/kyber-laptop-control-plane --tail=80 | grep -i machine
kubectl get nodes -o wide
```
If `kubectl get nodes` shows no Ready node, return to § 3. If the control plane logs show "machine reconciler" errors, return to § 6d's verify.

**Verify the fix:** Re-run step 9.1's verify.

### Authorize with Claude does not open the OAuth tab

**Symptom:** Step 9.2's `Authorize with Claude` button does not open a new tab, or the operator's browser blocks the popup.

**Likely cause:** Browser popup blocker, or the PWA is missing the OAuth client ID configuration (rare).

**Fix:** Ask the operator to allow popups for `localhost:8080` in their browser settings, then click `Authorize with Claude` again. If the button still doesn't open a tab:
```bash
kubectl -n kyber-system logs deploy/kyber-laptop-control-plane --tail=40 | grep -i oauth
```
The control-plane logs surface OAuth client configuration errors at startup.

**Verify the fix:** Re-run step 9.2's verify (OAuth tab loads).

### Agent CRD missing or in NeedsAuth phase

**Symptom:** Step 9.3's verify shows phase `NeedsAuth` or returns no output (Agent CRD wasn't created).

**Likely cause:** The PKCE token exchange failed (code was wrong / expired), or the operator dismissed the Create Agent form before the create POST landed.

**Fix:**
```bash
kubectl -n kyber-system get agents
kubectl -n kyber-system describe agent dave | tail -30
```
- If the Agent doesn't exist: the operator never clicked Create Agent. Return to 9.2's form.
- If phase is `NeedsAuth`: the OAuth code was invalid or already consumed. Return to 9.2 and run a fresh OAuth round-trip with a new authorization code.

**Verify the fix:** Re-run step 9.3's verify.

### Agent does not reach Running on standalone WSL2

**Symptom:** Step 9.4's polling loop ends with `TERMINAL: CrashLoopBackOff` or never reaches `Running`.

**Likely cause:** Agent pod's privileged-mode + `/dev/fuse` + bind-mount path is failing on this WSL2 substrate. Phase C reliability across kernel updates is not yet CI-covered.

**Fix:**
```bash
kubectl -n kyber-system logs pod/agent-dave --previous
kubectl -n kyber-system describe pod/agent-dave | tail -30
```

Common cases:
- `mount: ... operation not permitted` → privileged-pod policy is being rejected. Check the namespace's PodSecurity setting: `kubectl get ns kyber-system -o jsonpath='{.metadata.labels}'` should show `pod-security.kubernetes.io/enforce=privileged`.
- `Failed to setup overlayfs / bind mount` → fuse-overlayfs path failed; the agent will fall back to bind-mount-HOME. Check pod logs for the fallback.

If neither path completes, this is a known Phase C uncertainty on WSL2. Capture what you saw in a fresh issue against `matty-v/kyber`.

**Verify the fix:** Re-run step 9.4's verify.

### Tailscale Funnel did not bind 8080

**Symptom:** Step 10.2 verify produces no URL line.

**Likely cause:** Funnel is disabled on the operator's Tailscale plan, or 10.1's `tailscale up` did not register `--operator="$USER"`, blocking subsequent `tailscale funnel` commands.

**Fix:**
```bash
tailscale status                # confirm node is online
sudo tailscale funnel status    # check if any rules exist
sudo tailscale funnel --help    # confirm the binary supports --bg
```
If `tailscale funnel` reports a permission error, re-run 10.1's `tailscale up` line ensuring `--operator="$USER"` is included.

If Funnel is not available on the tailnet, the operator must enable it: https://login.tailscale.com/admin/dns → Funnel section → enable.

**Verify the fix:** Re-run step 10.2's verify.

### publicURL did not propagate to control plane

**Symptom:** Step 10.3's polling verify never lands on `OK` — the control-plane Deployment's `KYBER_PUBLIC_URL` env var stays empty or shows an old value.

**Likely cause:** The `git push` to kyber-deploy did not succeed (auth failure), or ArgoCD has not yet synced the change.

**Fix:**
```bash
cd ~/dev/kyber-deploy
git log -1 --oneline                                # confirm the publicURL commit landed locally
git status                                          # confirm clean
git push origin "$(git branch --show-current)"      # retry push if local commit isn't pushed

kubectl -n argocd get application kyber-laptop -o jsonpath='{.status.sync.status}'
# If "OutOfSync", trigger a manual sync:
kubectl -n argocd patch application kyber-laptop --type merge \
  -p '{"operation":{"sync":{"prune":true}}}'
```

**Verify the fix:** Re-run step 10.3's verify.

### Funnel URL unreachable from outside WSL2

**Symptom:** Step 10.4's curl from inside WSL2 hangs / fails, or the on-phone visit to `<TAILNET_URL>` errors.

**Likely cause:** klipper-lb is not bound on 8080 (see § Troubleshooting / "klipper-lb not binding port 8080"), Tailscale is reporting `Stopped` instead of `Running`, or the operator's tailnet ACLs are blocking Funnel traffic.

**Fix:**
```bash
tailscale status
ss -tln | grep ':8080 '
sudo tailscale funnel status
```
- If tailscale status reports `Stopped`: `sudo tailscale up --reset --auth-key="$AUTHKEY" --hostname=kyber-wsl --accept-dns=false --operator="$USER"`.
- If port 8080 isn't bound: return to § 7's troubleshooting.
- If Funnel rules are missing: re-run 10.2.
- If the on-phone test fails but inside-WSL2 curls succeed: the operator's tailnet ACLs may need a Funnel grant. Direct them to https://login.tailscale.com/admin/acls and confirm a `funnel` rule exists for the kyber-wsl node.

**Verify the fix:** Re-run step 10.4's verify.

## Destroying the install

```bash
# Remove the ArgoCD Application (stops ArgoCD from managing Kyber)
kubectl -n argocd delete application kyber-laptop

# Uninstall k3s — wipes the entire cluster state including all PVCs
# (this deletes all Postgres and Redis data)
sudo /usr/local/bin/k3s-uninstall.sh

# Log out and stop Tailscale
sudo tailscale logout
sudo systemctl stop tailscaled
sudo systemctl disable tailscaled
```

`k3s-uninstall.sh` is installed automatically by the k3s installer — it removes k3s binaries, config, and all cluster state. The Postgres and Redis PVCs stored in `/var/lib/rancher/k3s/storage/` are deleted along with it.

If you want to keep the cluster but remove just Kyber:
```bash
helm uninstall kyber-laptop -n kyber-system
kubectl delete namespace kyber-system
# Optionally remove ArgoCD
helm uninstall argocd -n argocd
kubectl delete namespace argocd
```
