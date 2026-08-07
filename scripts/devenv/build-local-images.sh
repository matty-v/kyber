#!/usr/bin/env bash
#
# Build ALL kyber images a live-agent cluster needs and import them into the
# k3d devenv. The base scripts/devenv/up.sh only handles the control-plane image
# (its mock path never runs agent workloads); a full local run also needs
# node-agent, status-sidecar, and the claude-code runtime (built on runtime-base).
#
# Usage:
#   scripts/devenv/build-local-images.sh [--cluster-name NAME] [--skip-build]
#     --cluster-name  target k3d cluster (default kyber-devenv)
#     --skip-build    skip the docker builds; just import the existing :local tags
#
# runtime-base is the build base for claude-code and is NOT deployed on its own,
# so it is built (when needed) but not imported.
#
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

CLUSTER="kyber-devenv"
SKIP_BUILD=""
while [ $# -gt 0 ]; do
  case "$1" in
    --cluster-name) CLUSTER="$2"; shift 2 ;;
    --skip-build)   SKIP_BUILD=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$SKIP_BUILD" ]; then
  echo ">> [1/8] control-plane"
  docker build -t kyber/control-plane:local -f images/control-plane/Dockerfile .
  echo ">> [2/8] node-agent"
  docker build -t kyber/node-agent:local -f images/node-agent/Dockerfile .
  echo ">> [3/8] status-sidecar"
  docker build -t kyber/status-sidecar:local -f images/status-sidecar/Dockerfile .
  echo ">> [4/8] runtime-base (base for agent runtimes; not deployed directly)"
  docker build -t kyber/runtime-base:local images/agent-base
  echo ">> [5/8] claude-code"
  docker build --build-arg BASE_IMAGE=kyber/runtime-base:local \
    -t kyber/claude-code:local -f images/claude-code/Dockerfile .
  echo ">> [6/8] codex"
  docker build --build-arg BASE_IMAGE=kyber/runtime-base:local \
    -t kyber/codex:local -f images/codex/Dockerfile .
  echo ">> [7/8] mcp-telegram"
  docker build -t kyber/mcp-telegram:local -f images/mcp-telegram/Dockerfile .
  echo ">> [8/8] mcp-discord"
  docker build -t kyber/mcp-discord:local -f images/mcp-discord/Dockerfile .
else
  echo ">> --skip-build: importing existing :local images"
fi

echo ">> importing into k3d cluster '$CLUSTER'"
k3d image import -c "$CLUSTER" \
  kyber/control-plane:local \
  kyber/node-agent:local \
  kyber/status-sidecar:local \
  kyber/mcp-telegram:local \
  kyber/mcp-discord:local \
  kyber/claude-code:local \
  kyber/codex:local

echo ">> done:"
docker images | grep -E "kyber/(control-plane|node-agent|status-sidecar|runtime-base|claude-code|codex|mcp-telegram|mcp-discord)" | awk '{print "   "$1":"$2}' | sort -u
