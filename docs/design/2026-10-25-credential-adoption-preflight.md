# Verify currently accepted access before adoption

> Historical implementation plan. The integrated operator-key feature shipped in
> [PR #396](https://github.com/matty-v/kyber/pull/396). Staged or proposed status
> statements below describe development at the time, not current availability.
> See the [current operator guide](../operator/operator-credentials.md) for supported
> setup, management, recovery and reporting limits.

This is the next bounded MAT-134/MAT-97 slice. It does not adopt keys, create
Jobs, activate registry authority or introduce another authentication method.
The existing authenticator remains the only source of accepted identity/access.

Before a supervised adoption imports configured values, it must prove that each
value authenticates as the expected identity and grants on every supported
control-plane replica. A successful version request proves only acceptance;
it cannot detect two values exchanged between identities with different grants.
Names or configuration alone do not prove current access either.

Add authenticated, read-only `GET /api/v1/operator-credentials/current`. Any
accepted key may inspect only its own current metadata, including stable identity
when known, material generation as a decimal string, authority, role, exact scopes
and Agent/Machine targets. Unknown identity stays unknown. Include a valid
Downward-API control-plane Pod UID when available; absence is explicit, not an
invented process identity. Never expose another key, material, key fingerprints,
private candidates, source Secret data or browser signers.

Use the existing protected mux and explicitly admit only GET for restricted
callers. Re-authenticate immediately before creating the response, without a
last-good fallback. Revocation is unauthorized and authority failure is 503.
Responses are no-store and stay outside shared caches. This read has no database
or audit-admission dependency and cannot grant management access.

The future worker will compare direct per-Pod observations against its private
fresh source snapshot, verify the Pod UID/serving artifact before and after,
reject missing/extra/changed accepted identities, and pin all source UID/resource
versions through handoff. Comparison and ownership transfer are separate work;
this metadata endpoint alone does not satisfy adoption acceptance or raise the
runtime protocol from zero.

Review: swapped values must be distinguishable by current identity/access;
restricted reads cannot select another credential or expose installation-wide
inventory. The response must reflect current state after narrowing and reject
retirement/outage rather than reuse middleware's earlier snapshot. No new UI
flow is introduced; hand-written SDK/types and OpenAPI remain in parity.
