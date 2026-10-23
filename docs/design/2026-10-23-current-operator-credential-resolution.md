# Current operator credential resolution

This MAT-134/MAT-97 slice supplies a staged API integration seam. The
control-plane command does not configure it, no adoption endpoint is exposed,
and existing installations keep their current authentication behavior. Source
ownership, audit admission/outbox reconciliation, stream revocation and the
upgrade/rollback compatibility floor remain activation prerequisites.

## One selected authority

`Server.OperatorCredentialAuthority` accepts a bounded direct registry resolver.
Only an exact `operatorcredentials.ErrUninitialized` permits bootstrap
configuration. A joined error, outage, unsupported schema/grant, invalid document,
wrong namespace or empty storage version returns a safe 503
`authentication_unavailable`. It does not become a 401 or reveal backend errors.
Once the authenticator has observed adoption, even a later uninitialized result
cannot restore bootstrap keys. The durable marker provides the corresponding
cross-process guarantee; no last-good credential snapshot is cached.

Every authenticated request compiles current registry records. Revoked and
expired records cannot authenticate. Administrator authority comes from explicit
role, never a display name, the historical cookie's full-scope bit, or a union
with installation configuration. Principal, tenant, credential ID and generation
remain intact. Exact Agent/Machine grants and existing route policy still apply.
Unknown permission vocabulary fails closed rather than being interpreted by an
older binary. This is not a promise that every supported grant combination is
appropriate for issuance; the management permission editor must validate its
supported action/target combinations before writing them.

The administrator inventory projects only safe metadata from the selected
registry, including revoked/expired state and ownership. It returns no material,
signing seed, source Secret binding or outbox internals. Settings hides former
permissions for inactive keys. Usage remains explicitly unavailable in this
slice. The old configuration-key rotation route cannot write source Secrets
after adoption; individual audited management must own that operation.

## Browser sessions and task cursors

New registry cookies use `v2` with a distinct HKDF domain and the individual
credential's current material. Claims contain stable identity, generation and a
bounded lifetime; no grants or role are trusted from them. A bounded unverified
credential ID is a lookup hint only. HMAC and exact identity checks precede
acceptance. Grant/name edits preserve the session but resolve current grants;
material rotation, expiry and revocation reject it. An unrelated administrator's
rotation does not invalidate the session.

Adoption preserves the **derived V1 cookie signing key**, not a second accepted
bearer key, in the private registry codec. It is immutable after registry
creation, copied on access/clone, length-checked and excluded from public JSON
and diagnostic formatting. This permits V1 scoped cookies to survive installation
key rotation while still checking their material binding against the scoped
credential's current key. The installation cookie additionally checks its own
current generation, so its old cookie is rejected after its rotation. Historical
installation credential IDs are accepted only by the compatibility reader and
resolved to the current supported identity. Successful V1 browser requests renew
to V2. V1 compatibility is a reader, not a second management or bearer-key path.

Task event cursors select signing material by immutable credential identity,
including managed administrators. They cannot borrow the installation key merely
because the caller has full access. Rename and unrelated-key rotation preserve
cursors; the credential's own rotation/revocation invalidates them. Current task
ownership checks remain separate.

## Audit and verification boundaries

Registry-authenticated access carries the `registry` authority qualifier;
configuration snapshots retain `configuration`. Credential-target event/filter
validation accepts the same bounded opaque IDs as credential records, including
issuer-qualified IDs. This metadata work does not itself enable credential
mutation handlers or complete lifecycle/archive collection.

Tests must cover two replicas, current permission/role changes, revoked/expired
bearers and cookies, existing V1 cookies and task cursors, unrelated rotations,
wrong/unsupported authority, private-error redaction and 503 behavior, management
isolation, signing-seed persistence/immutability and safe inactive UI labels.
Production activation is a separate operator approval after all remaining gates
and GCP dev acceptance tests are complete.
