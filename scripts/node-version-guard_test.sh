#!/usr/bin/env bash
# Keep the repository's Node development version, package contract, control-plane
# PWA builder, and the holocron publishing job on the same major release.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

expected="$(tr -d '[:space:]' < .nvmrc)"
engine="$(node -p "require('./package.json').engines.node")"
builder="$(sed -nE 's/^FROM node:([0-9]+)-alpine AS pwa-builder$/\1/p' images/control-plane/Dockerfile)"
publisher="$(sed -nE "s/^[[:space:]]*node-version: '([0-9]+)'$/\1/p" .github/workflows/publish-pwa-views.yml)"

fail=0
check() {
  local label="$1" actual="$2" wanted="$3"
  if [ "$actual" != "$wanted" ]; then
    echo "ERROR: ${label} is '${actual:-missing}', expected '${wanted}'." >&2
    fail=1
  fi
}

check "package.json engines.node" "$engine" ">=${expected} <$(($expected + 1))"
check "control-plane PWA builder Node major" "$builder" "$expected"
check "holocron publisher Node major" "$publisher" "$expected"

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "Node ${expected} is aligned across development, CI, publishing, and the control-plane PWA build."
