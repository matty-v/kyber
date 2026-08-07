#!/usr/bin/env bash
# Resolve the GHCR manifest digest for a given kyber-* image:tag pair.
# Used by .github/workflows/release.yml deploy-bump-pr job to write
# `tag@sha256:...` into kyber-deploy values.yaml (Strategic-B from
# kyber#364 — digest pinning at the deploy layer is the consequence-
# side fix for the tag-mutability failure mode).
#
# Usage:
#   resolve-image-digest.sh <image-name> <tag>
#
# Output: prints `sha256:...` to stdout on success.
# Exit 0 on success; exit 1 if the tag isn't visible (race against
# build-push or a wrong tag); exit 2 on usage error.

set -uo pipefail

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <image-name> <tag>" >&2
    exit 2
fi

IMAGE="$1"
TAG="$2"
OWNER="${GHCR_OWNER:-matty-v}"

digest=$(gh api "/users/${OWNER}/packages/container/${IMAGE}/versions" \
    --jq ".[] | select(.metadata.container.tags[]? == \"${TAG}\") | .name" 2>/dev/null \
    | head -n 1)

if [ -z "$digest" ]; then
    echo "::error::Could not resolve digest for ${OWNER}/${IMAGE}:${TAG} on GHCR." >&2
    echo "::error::Tag may not be visible yet (race against build-push) or may be wrong." >&2
    exit 1
fi

echo "$digest"
