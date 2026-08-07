#!/usr/bin/env bash
# kyber#578 Layer 2 — the internal-auth key-presence DEPLOY GATE.
#
# Run by Ackbar on a `needs-deploy-review` / cluster-apply for any internal-auth
# (kyber#566) rollout. It exists because the v2.1.0 incident shipped enforce
# (graceMode=false) without delivering the out-of-band kyber-internal-signing-key
# Secret — so the control plane fail-closed the ENTIRE internal API (every
# /internal/... route 503'd) → fleet outage. This gate makes a keyless ENFORCE
# cutover ABORT before it reaches the cluster, so that class of outage can't
# recur from a missed delivery.
#
# CONSERVATIVE posture (Matt's Q1=NO): even a grace rollout fails closed on a
# missing key (it does not serve unauthenticated), so a keyless grace deploy is
# allowed through with a WARNING (it will fail-closed + alert, paged not silent),
# while a keyless ENFORCE deploy is a hard abort.
#
# Usage:
#   preflight-internal-auth-key.sh <namespace> <graceMode:true|false>
#       Pre-apply gate: assert the signing-key Secret exists when enforce.
#   preflight-internal-auth-key.sh <namespace> <graceMode:true|false> --post-apply <deployment>
#       Post-apply check: assert the control plane logged auth enabled.
#
# Exit codes: 0 = OK to proceed · 1 = ABORT (gate failed) · 2 = usage error.
#
# Env:
#   SIGNING_KEY_SECRET_NAME   override the Secret name (default kyber-internal-signing-key)
#   AUTH_ENABLED_LOG_MARKER   override the post-apply log marker
#
# Tested by preflight-internal-auth-key_test.sh (mocks `kubectl` via PATH).

set -uo pipefail

SECRET_NAME="${SIGNING_KEY_SECRET_NAME:-kyber-internal-signing-key}"
AUTH_ENABLED_MARKER="${AUTH_ENABLED_LOG_MARKER:-internal API per-agent auth enabled}"

if [ "$#" -lt 2 ]; then
    echo "usage: $0 <namespace> <graceMode:true|false> [--post-apply <deployment>]" >&2
    exit 2
fi

NAMESPACE="$1"
GRACE_MODE="$2"
shift 2

case "$GRACE_MODE" in
    true|false) ;;
    *) echo "usage: graceMode must be 'true' or 'false' (got '${GRACE_MODE}')" >&2; exit 2 ;;
esac

POST_APPLY_DEPLOY=""
if [ "$#" -ge 1 ]; then
    if [ "$1" = "--post-apply" ]; then
        if [ "$#" -lt 2 ]; then
            echo "usage: --post-apply requires a <deployment> name" >&2
            exit 2
        fi
        POST_APPLY_DEPLOY="$2"
    else
        echo "usage: unexpected argument '$1'" >&2
        exit 2
    fi
fi

# ---- Post-apply check: the control plane must have LOGGED auth enabled. -------
# A healthy enforce rollout logs the marker once at startup; its absence means
# the key was not actually picked up (still fail-closed) — don't declare healthy.
if [ -n "$POST_APPLY_DEPLOY" ]; then
    logs=$(kubectl -n "$NAMESPACE" logs "deploy/${POST_APPLY_DEPLOY}" --tail=2000 2>&1)
    rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "::error::preflight: could not read logs for deploy/${POST_APPLY_DEPLOY} in ${NAMESPACE}:" >&2
        echo "$logs" >&2
        exit 1
    fi
    if echo "$logs" | grep -qF "$AUTH_ENABLED_MARKER"; then
        echo "preflight: deploy/${POST_APPLY_DEPLOY} logged \"${AUTH_ENABLED_MARKER}\" — internal-auth rollout healthy"
        exit 0
    fi
    echo "::error::preflight: deploy/${POST_APPLY_DEPLOY} did NOT log \"${AUTH_ENABLED_MARKER}\"." >&2
    echo "::error::The internal API is likely FAILING CLOSED (signing key not picked up) — do not declare the rollout healthy." >&2
    echo "::error::Deliver/verify the ${SECRET_NAME} Secret and restart the control plane. See docs/operator/internal-api-auth-rollout.md." >&2
    exit 1
fi

# ---- Pre-apply gate: the signing-key Secret must exist when ENFORCE. ----------
if kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" >/dev/null 2>&1; then
    echo "preflight: ${SECRET_NAME} present in ${NAMESPACE} — OK to apply (graceMode=${GRACE_MODE})"
    exit 0
fi

# Secret is absent.
if [ "$GRACE_MODE" = "false" ]; then
    echo "::error::preflight: ENFORCE cutover (graceMode=false) but the ${SECRET_NAME} Secret is ABSENT in ${NAMESPACE}." >&2
    echo "::error::Applying this would fail-close the entire internal API (/internal/... 503s) — the v2.1.0 outage shape. ABORTING." >&2
    echo "::error::Deliver the signing key first (grace-first: roll out graceMode=true, deliver the key, verify, then flip to enforce)." >&2
    echo "::error::See docs/operator/internal-api-auth-rollout.md." >&2
    exit 1
fi

# Grace + no key: allowed through, but warn — it will fail-closed + alert (paged,
# not a silent outage) until the key is delivered. Deliver before flipping enforce.
echo "::warning::preflight: GRACE rollout (graceMode=true) but the ${SECRET_NAME} Secret is absent in ${NAMESPACE}." >&2
echo "::warning::The internal API will FAIL CLOSED and raise a startup alert until the key is delivered (no silent outage)." >&2
echo "::warning::Deliver the ${SECRET_NAME} Secret before flipping graceMode to enforce. See docs/operator/internal-api-auth-rollout.md." >&2
echo "preflight: grace rollout without key — proceeding (will fail-closed + alert until key delivered)"
exit 0
