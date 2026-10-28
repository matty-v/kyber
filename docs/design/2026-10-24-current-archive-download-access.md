# Current access for disk archive downloads

An archive can contain the whole Agent disk, including credentials. Its short-lived
browser download URL must stop working when the issuing key loses archive access.
Currently the public download handler verifies only a job ID, expiry and an
installation signature, so a previously issued URL survives key revocation.

Replace that capability with a versioned signed claim bound to the exact job,
Agent, namespace, issuing credential identity/generation and current key material.
The material binding is a domain-separated HMAC under the private archive signing
key, never a raw key or a publicly testable hash of a potentially weak key. Verify
current authority before opening archive storage and every three seconds during
transfer, reusing the live-access transport cancellation. Require archives:admin
and the exact Agent target each time; unrelated key changes and credential renames
must preserve access. An unavailable authority returns 503 before streaming and
closes an existing transfer without pretending the credential was revoked.

The URL remains a bearer capability for one archive, so opening it does not grant
access to other API routes. Ordinary browser navigation and download resumption by
requesting a fresh link remain available. Link expiry applies to admission;
a healthy transfer already in progress may outlive that short expiry. Existing
version-one links and links issued before authority adoption must be regenerated;
archive bytes, jobs, Agent disks and operator key values are unchanged.

Configuration credentials must resolve using the same duplicate-key precedence as
normal bearer authentication. Do not select a privileged record merely because a
restricted caller supplied the same identity fields. Registry-issued links must
never fall back to configuration. Private token/material data must remain absent
from logs, audit records and public errors. URL logging remains redacted.

Review before implementation: keep the handler's exact completed-job and namespace
checks; do not require an Agent to still exist to retrieve its retained archive.
Exercise root/configured/registry credentials, rotation, revocation, expiry,
permission and target narrowing, cross-job tampering, authority outage, browser
cookies issuing links, healthy transfers beyond link expiry, backpressure and
unrelated-key changes. Full archive mutation admission/completion coverage remains
a separate required part of the credential scope.
