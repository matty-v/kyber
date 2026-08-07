#!/bin/bash
set -euo pipefail

LOG=/var/log/kyber-k3s-install.log
exec > >(tee -a "$LOG") 2>&1

# Emit the failing line number to stderr on any unhandled error.
trap 'echo "[$(date -u +%FT%TZ)] ERROR: startup script failed at line $LINENO — see $LOG" >&2' ERR

echo "[$(date -u +%FT%TZ)] Starting Kyber k3s install..."

# ---- Discover the VM's external IP via GCE metadata ------------------------
# Needed so the k3s server cert includes this IP in its SAN. Without it,
# kubectl from the operator's workstation fails TLS verification when talking
# to the external endpoint.
EXTERNAL_IP=$(curl -sfH "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip" || true)

if [ -z "$EXTERNAL_IP" ]; then
  echo "[$(date -u +%FT%TZ)] WARNING: could not fetch external IP from metadata; proceeding with default SANs only" >&2
else
  echo "[$(date -u +%FT%TZ)] External IP: $EXTERNAL_IP — adding to tls-san"
fi

# ---- Write k3s config with tls-san -----------------------------------------
mkdir -p /etc/rancher/k3s
cat > /etc/rancher/k3s/config.yaml <<CONFIG
write-kubeconfig-mode: "0644"
disable:
  - traefik
CONFIG

if [ -n "$EXTERNAL_IP" ]; then
  cat >> /etc/rancher/k3s/config.yaml <<CONFIG
tls-san:
  - $EXTERNAL_IP
CONFIG
fi

# ---- System prep ------------------------------------------------------------
echo "[$(date -u +%FT%TZ)] Updating system packages..."
apt-get update -qq
apt-get install -yq curl wget jq

# ---- Install k3s server -----------------------------------------------------
# TODO: pin k3s version (e.g. INSTALL_K3S_VERSION=v1.31.x+k3s1) before production upgrades
# so that VM reprovisions are deterministic across time.
# k3s reads /etc/rancher/k3s/config.yaml for disable + tls-san; INSTALL_K3S_EXEC is empty.
if [ -x /usr/local/bin/k3s ] && systemctl is-active --quiet k3s; then
  echo "[$(date -u +%FT%TZ)] k3s already installed and running, skipping install"
else
  echo "[$(date -u +%FT%TZ)] Installing k3s server..."
  curl -sfL https://get.k3s.io | sh -
fi

echo "[$(date -u +%FT%TZ)] Waiting for k3s to become ready..."
# The leading space in " Ready" matches the STATUS column, not a partial hostname.
timeout 120 bash -c 'until /usr/local/bin/k3s kubectl get nodes 2>/dev/null | grep -q " Ready"; do sleep 3; done'
echo "[$(date -u +%FT%TZ)] k3s is ready."

# Confirm the join token file exists without printing its value.
if [ -r /var/lib/rancher/k3s/server/node-token ]; then
  echo "[$(date -u +%FT%TZ)] k3s join token is available at /var/lib/rancher/k3s/server/node-token"
else
  echo "[$(date -u +%FT%TZ)] ERROR: k3s join token file not found" >&2
  exit 1
fi

# ---- Done -------------------------------------------------------------------
# Note: PostgreSQL, Redis, and the Kyber control plane are installed via the
# Helm chart from the operator's workstation — see docs/installation.md step 6.
# This VM's job is to provide a working k3s API server (port 6443) and node.

echo "[$(date -u +%FT%TZ)] Kyber k3s install complete. Cluster is ready."
echo "[$(date -u +%FT%TZ)] kubeconfig path: /etc/rancher/k3s/k3s.yaml"
echo "[$(date -u +%FT%TZ)] Next step: fetch kubeconfig + join token from your workstation and run 'helm install kyber'."
