#!/usr/bin/env bash
#
# Deploy a published Kyber build to the shared GKE dev install (kyber-dev in
# datawire-dev) for manual testing, and wait until the control plane serves it.
#
# Usage:
#   scripts/devenv/deploy-gke-dev.sh main          # current main head
#   scripts/devenv/deploy-gke-dev.sh pr/274        # a PR's head build
#   scripts/devenv/deploy-gke-dev.sh <commit-sha>  # a specific main commit
#   scripts/devenv/deploy-gke-dev.sh --dry-run ... # resolve and print, change nothing
#
# Images are resolved per image. Main pushes retag every image with the
# commit's `git describe` version (minus the leading "v"), so a main commit
# resolves uniformly. A PR build publishes only the images it rebuilt, tagged
# with the head SHA; every other image falls back to the merge-base's main tag.
#
# The chart comes from the checked-out tree at the resolved commit, and the
# release's existing values are reused. Only image tags are overridden. The
# chart's CRDs are applied before the upgrade, since Helm never upgrades them.
#
# Env overrides: KYBER_DEV_PROJECT, KYBER_DEV_LOCATION, KYBER_DEV_CLUSTER,
# KYBER_DEV_RELEASE, KYBER_DEV_NAMESPACE, KYBER_DEV_URL.
set -euo pipefail
cd "$(dirname "$0")/../.."

PROJECT="${KYBER_DEV_PROJECT:-datawire-dev}"
LOCATION="${KYBER_DEV_LOCATION:-us-central1-a}"
CLUSTER="${KYBER_DEV_CLUSTER:-kyber-dev}"
RELEASE="${KYBER_DEV_RELEASE:-kyber-dev}"
NAMESPACE="${KYBER_DEV_NAMESPACE:-kyber-system}"
URL="${KYBER_DEV_URL:-https://kyber-dev-gcp.voget.io}"
CONTEXT="gke_${PROJECT}_${LOCATION}_${CLUSTER}"
REGISTRY_OWNER="matty-v"

# chart value key -> GHCR package
IMAGES=(
  "controlPlane:kyber-control-plane"
  "nodeAgent:kyber-node-agent"
  "agentBase:kyber-runtime-base"
  "claudeCode:kyber-claude-code"
  "codex:kyber-codex"
  "hermes:kyber-hermes"
  "statusSidecar:kyber-status-sidecar"
  "discordSidecar:kyber-mcp-discord"
  "telegramSidecar:kyber-mcp-telegram"
  "slackSidecar:kyber-mcp-slack"
)

log() { printf '=== deploy-gke-dev: %s ===\n' "$*"; }
die() { printf 'deploy-gke-dev: error: %s\n' "$*" >&2; exit 1; }

DRY_RUN=""
[ "${1:-}" = "--dry-run" ] && { DRY_RUN=1; shift; }
REF="${1:-}"
[ -n "$REF" ] || die "usage: $0 [--dry-run] main|pr/<number>|<commit-sha>"

for bin in gcloud kubectl helm gh git curl; do
  command -v "$bin" >/dev/null || die "$bin is required on PATH"
done

git fetch -q --tags origin main

describe_tag() {
  local d
  d=$(git describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' "$1") || die "cannot describe $1 (release tags missing?)"
  echo "${d#v}"
}

case "$REF" in
  main)
    SHA=$(git rev-parse origin/main); BASE_SHA="$SHA"; HEAD_TAG="" ;;
  pr/*)
    PR="${REF#pr/}"
    SHA=$(gh pr view "$PR" --json headRefOid --jq .headRefOid) || die "cannot read PR $PR"
    git fetch -q origin "$SHA" || die "cannot fetch PR head $SHA"
    BASE_SHA=$(git merge-base "$SHA" origin/main); HEAD_TAG="$SHA" ;;
  *)
    SHA=$(git rev-parse --verify "$REF^{commit}") || die "unknown commit $REF"
    git merge-base --is-ancestor "$SHA" origin/main || die "$REF is not on main; use pr/<number> for PR builds"
    BASE_SHA="$SHA"; HEAD_TAG="" ;;
esac
BASE_TAG=$(describe_tag "$BASE_SHA")

published_tags() {
  gh api "/users/$REGISTRY_OWNER/packages/container/$1/versions?per_page=100" \
    --jq '.[].metadata.container.tags[]' 2>/dev/null
}

SETS=()
log "resolving images for $REF ($SHA)"
for entry in "${IMAGES[@]}"; do
  key="${entry%%:*}"; pkg="${entry#*:}"
  tags=$(published_tags "$pkg")
  if [ -n "$HEAD_TAG" ] && grep -qx "$HEAD_TAG" <<<"$tags"; then
    tag="$HEAD_TAG"
  elif grep -qx "$BASE_TAG" <<<"$tags"; then
    tag="$BASE_TAG"
  else
    die "$pkg has neither ${HEAD_TAG:+$HEAD_TAG nor }$BASE_TAG published — is CI still building?"
  fi
  printf '  %-16s ghcr.io/%s/%s:%s\n' "$key" "$REGISTRY_OWNER" "$pkg" "$tag"
  SETS+=(--set "image.$key.tag=$tag")
done
CP_TAG=$(printf '%s\n' "${SETS[@]}" | sed -n 's/^image\.controlPlane\.tag=//p')
# The control plane reports the commit its image was BUILT from. A main commit
# that did not change it gets a retag of an older build, so read the build SHA
# off the full-SHA tag GHCR lists on the same image version.
CP_SHA=$(gh api "/users/$REGISTRY_OWNER/packages/container/kyber-control-plane/versions?per_page=100" \
  --jq ".[] | select(.metadata.container.tags | index(\"$CP_TAG\")) | .metadata.container.tags[] | select(test(\"^[0-9a-f]{40}$\"))" 2>/dev/null | head -1)
if [ -z "$CP_SHA" ]; then
  CP_SHA="$BASE_SHA"; [ "$CP_TAG" = "$HEAD_TAG" ] && CP_SHA="$SHA"
fi

log "control plane will report build ${CP_SHA:0:7}"
[ -n "$DRY_RUN" ] && { log "dry run — nothing changed"; exit 0; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
CHART_DIR="$WORK/chart"; VALUES="$WORK/values.yaml"; mkdir "$CHART_DIR"
# A private kubeconfig: every call targets kyber-dev by construction, and the
# caller's current kubectl context is left untouched.
export KUBECONFIG="$WORK/kubeconfig"

log "fetching credentials for $CLUSTER"
gcloud container clusters get-credentials "$CLUSTER" --location "$LOCATION" --project "$PROJECT" >/dev/null 2>&1 \
  || die "gcloud get-credentials failed — run: gcloud auth login"
git archive "$SHA" deploy/helm/kyber | tar -x -C "$CHART_DIR"
# Helm installs crds/ once and never upgrades them, so a build with a schema
# change would run against the old CRDs and the API server would silently
# prune its new fields. Apply them first, the way the self-upgrade Job does
# (docs/upgrading.md): server-side, as the same field manager.
log "applying the chart's CRDs"
kubectl --context "$CONTEXT" apply --server-side --force-conflicts --field-manager=kyber-upgrade \
  -f "$CHART_DIR/deploy/helm/kyber/crds/" >/dev/null || die "applying CRDs failed"
helm --kube-context "$CONTEXT" -n "$NAMESPACE" get values "$RELEASE" -o yaml > "$VALUES"

log "helm upgrade $RELEASE on $CONTEXT"
helm --kube-context "$CONTEXT" -n "$NAMESPACE" upgrade "$RELEASE" "$CHART_DIR/deploy/helm/kyber" \
  -f "$VALUES" "${SETS[@]}" --wait --timeout 10m >/dev/null

log "waiting for the control plane to serve ${CP_SHA:0:7} ($CP_TAG)"
KEY=$(kubectl --context "$CONTEXT" -n "$NAMESPACE" get secret kyber-api-credentials -o jsonpath='{.data.api-key}' | base64 -d)
want="${CP_SHA:0:7}"
for _ in $(seq 1 60); do
  served=$(curl -fsS -H "Authorization: Bearer $KEY" "$URL/api/v1/version" 2>/dev/null | sed -n 's/.*"sha":"\([^"]*\)".*/\1/p' || true)
  [ -n "$served" ] && [[ "$want" == "$served"* || "$served" == "$want"* ]] && { log "serving $served at $URL"; exit 0; }
  sleep 5
done
die "control plane still not serving $want after 5 minutes (last seen: ${served:-none})"
