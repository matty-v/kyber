#!/usr/bin/env bash
# Fail-fast guard used by .github/workflows/release.yml: errors if the
# given GHCR image already has the given semver tag published. The
# release pipeline calls this once per kyber-* image before any build
# job runs. Exit 0 means "safe to publish"; exit 1 means "tag exists,
# cut a new semver."
#
# Why: kyber#364 — re-pushing a published semver tag can poison the
# kubelet's IfNotPresent cache on long-lived nodes (the original blob
# stays resident under the same tag identity), producing a digest
# drift that ArgoCD's manifest-only sync check can't see. Treating
# published tags as immutable closes that class of failure at the
# source.
#
# Usage:
#   preflight-tag-not-published.sh <image-name> <tag>
#
# Examples:
#   preflight-tag-not-published.sh kyber-node-agent v1.3.4
#
# Test fixtures invoke a fake `gh` via PATH override. See
# preflight-tag-not-published_test.sh.

set -uo pipefail

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <image-name> <tag>" >&2
    exit 2
fi

IMAGE="$1"
TAG="$2"
OWNER="${GHCR_OWNER:-matty-v}"

# Query GHCR for any version whose tags array contains TAG.
# `gh api` exits non-zero on 404 (package does not exist yet — that's
# the first-ever-push case, which must be allowed).
output=$(gh api "/users/${OWNER}/packages/container/${IMAGE}/versions" \
    --jq ".[] | select(.metadata.container.tags[]? == \"${TAG}\")" 2>&1)
rc=$?

if [ "$rc" -ne 0 ]; then
    # 404 (package not yet published) → first-ever release of this
    # image; not a re-push hazard. Any other gh error → log and pass
    # (we don't want a transient registry blip to block a release;
    # the build-push step will surface a real auth/network failure
    # with a clearer message).
    if echo "$output" | grep -q "HTTP 404"; then
        echo "preflight: ${IMAGE} not yet published on GHCR — first release, OK"
        exit 0
    fi
    echo "preflight: gh api error querying ${IMAGE} (continuing, build step will surface real failures):" >&2
    echo "$output" >&2
    exit 0
fi

if [ -n "$output" ]; then
    echo "::error::Tag ${TAG} is already published for ${OWNER}/${IMAGE} on GHCR." >&2
    echo "::error::Published semver tags are immutable (kyber#364) — cut a new patch instead of re-pushing." >&2
    echo "::error::See docs/operator/release-runbook.md § Tag immutability." >&2
    exit 1
fi

echo "preflight: ${IMAGE}:${TAG} is not yet published — OK to build"
exit 0
