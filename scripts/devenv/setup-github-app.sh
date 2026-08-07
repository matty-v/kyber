#!/usr/bin/env bash
# Store a developer's GitHub App credentials outside tracked files so
# up-full.sh can configure identity-repo testing in the local k3d cluster.
set -euo pipefail
cd "$(dirname "$0")/../.."

CONFIG_DIR="${DEVENV_GITHUB_APP_DIR:-.kyber-local/github-app}"

usage() {
  cat <<'EOF'
Usage:
  scripts/devenv/setup-github-app.sh \
    --app-id ID --installation-id ID --owner OWNER --private-key PATH

Writes an ignored, mode-700 bundle under .kyber-local/github-app/. Re-run
scripts/devenv/up-full.sh --skip-build afterward to apply it to the cluster.
EOF
}

APP_ID=""
INSTALLATION_ID=""
OWNER=""
PRIVATE_KEY=""
while [ $# -gt 0 ]; do
  case "$1" in
    --app-id)           APP_ID="${2:-}"; shift 2 ;;
    --installation-id) INSTALLATION_ID="${2:-}"; shift 2 ;;
    --owner)            OWNER="${2:-}"; shift 2 ;;
    --private-key)      PRIVATE_KEY="${2:-}"; shift 2 ;;
    -h|--help)          usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ "$APP_ID" =~ ^[1-9][0-9]*$ ]] || { echo "--app-id must be a positive integer" >&2; exit 2; }
[[ "$INSTALLATION_ID" =~ ^[1-9][0-9]*$ ]] || { echo "--installation-id must be a positive integer" >&2; exit 2; }
[[ "$OWNER" =~ ^[A-Za-z0-9][A-Za-z0-9-]{0,38}$ ]] || { echo "--owner must be a GitHub account name" >&2; exit 2; }
[ -f "$PRIVATE_KEY" ] || { echo "private key not found: $PRIVATE_KEY" >&2; exit 2; }
grep -q -- 'BEGIN.*PRIVATE KEY' "$PRIVATE_KEY" || { echo "private key does not look like a PEM file: $PRIVATE_KEY" >&2; exit 2; }

mkdir -p "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR"
printf '%s\n' "$APP_ID" >"$CONFIG_DIR/app-id"
printf '%s\n' "$INSTALLATION_ID" >"$CONFIG_DIR/installation-id"
printf '%s\n' "$OWNER" >"$CONFIG_DIR/owner"
cp "$PRIVATE_KEY" "$CONFIG_DIR/private-key.pem"
chmod 600 "$CONFIG_DIR/app-id" "$CONFIG_DIR/installation-id" "$CONFIG_DIR/owner" "$CONFIG_DIR/private-key.pem"

cat <<EOF
Local GitHub App configuration saved to $CONFIG_DIR (ignored by git).
Apply it with:
  scripts/devenv/up-full.sh --skip-build
EOF
