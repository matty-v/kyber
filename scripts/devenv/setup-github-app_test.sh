#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")/../.."

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
key="$tmp/source.pem"
printf '%s\n' '-----BEGIN PRIVATE KEY-----' 'test-only' '-----END PRIVATE KEY-----' >"$key"

DEVENV_GITHUB_APP_DIR="$tmp/config" scripts/devenv/setup-github-app.sh \
  --app-id 123 --installation-id 456 --owner test-owner --private-key "$key" >/dev/null

[ "$(cat "$tmp/config/app-id")" = 123 ] || { echo "app ID mismatch" >&2; exit 1; }
[ "$(cat "$tmp/config/installation-id")" = 456 ] || { echo "installation ID mismatch" >&2; exit 1; }
[ "$(cat "$tmp/config/owner")" = test-owner ] || { echo "owner mismatch" >&2; exit 1; }
cmp -s "$key" "$tmp/config/private-key.pem" || { echo "private key mismatch" >&2; exit 1; }

if DEVENV_GITHUB_APP_DIR="$tmp/invalid" scripts/devenv/setup-github-app.sh \
  --app-id nope --installation-id 456 --owner test-owner --private-key "$key" >/dev/null 2>&1; then
  echo "invalid app ID unexpectedly accepted" >&2
  exit 1
fi

echo "PASS: local GitHub App configuration"
