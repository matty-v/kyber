#!/usr/bin/env bash
# scripts/pwa-views-should-tag.sh — the single decision point for whether a
# pwa-views version needs a `pwa-views/v*` tag pushed (which is what triggers
# the package publish).
#
# Usage: pwa-views-should-tag.sh NEW PUBLISHED
#   NEW        the version at the candidate commit (package.json)
#   PUBLISHED  the registry's current latest version ('' when nothing has
#              ever been published)
#
# Prints exactly one line — "tag" or "skip: <reason>" — and exits 0 for
# either decision; exits 2 on usage error. Pure (no network, no git) so
# auto-publish-pwa-views.yml and release.yml share one set of semantics and
# scripts/pwa-views-should-tag_test.sh can pin them.
#
# Rules:
#   - NEW must be a final X.Y.Z — prereleases never auto-tag.
#   - Nothing published yet → tag.
#   - Versions compare numerically field by field. Never `sort -V`: GNU sort
#     orders a prerelease AFTER its final (0.28.0-rc.1 > 0.28.0), the inverse
#     of semver, which would strand the final release forever.
#   - NEW newer than PUBLISHED's numeric core → tag; older → skip (a reverted
#     bump must not republish an old version as the registry's latest).
#   - Cores equal: PUBLISHED is a prerelease → tag (the final supersedes its
#     own rc); PUBLISHED is the same final → skip (already out).

set -uo pipefail

[ $# -eq 2 ] || { echo "usage: $0 NEW PUBLISHED" >&2; exit 2; }
NEW="$1"; PUBLISHED="$2"

is_final() { [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; }
core() { local s="${1%%-*}"; echo "${s%%+*}"; }

if ! is_final "$NEW"; then
  echo "skip: NEW '$NEW' is not a final X.Y.Z version"
  exit 0
fi

if [ -z "$PUBLISHED" ]; then
  echo "tag"
  exit 0
fi

PCORE="$(core "$PUBLISHED")"
if ! is_final "$PCORE"; then
  echo "skip: cannot parse PUBLISHED version '$PUBLISHED'"
  exit 0
fi

IFS=. read -r n1 n2 n3 <<<"$NEW"
IFS=. read -r p1 p2 p3 <<<"$PCORE"
for pair in "$n1:$p1" "$n2:$p2" "$n3:$p3"; do
  n="${pair%%:*}"; p="${pair##*:}"
  if [ "$n" -gt "$p" ]; then echo "tag"; exit 0; fi
  if [ "$n" -lt "$p" ]; then echo "skip: $NEW is not newer than published $PUBLISHED"; exit 0; fi
done

# Numeric cores are equal.
if [ "$PUBLISHED" != "$PCORE" ]; then
  echo "tag"
else
  echo "skip: $NEW is already published"
fi
exit 0
