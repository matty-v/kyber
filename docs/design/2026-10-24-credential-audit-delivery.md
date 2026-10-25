# Deliver and compact credential recovery references

> Historical implementation plan. The integrated operator-key feature shipped in
> [PR #396](https://github.com/matty-v/kyber/pull/396). Staged or proposed status
> statements below describe development at the time, not current availability.
> See the [current operator guide](../operator/operator-credentials.md) for supported
> setup, management, recovery and reporting limits.

Status: staged implementation for MAT-97 and MAT-134. No runtime worker or
credential authority activation is enabled by this slice.

Every committed registry mutation contains a bounded outbox reference to its
required journal admission and completion identities. The committed reference
proves the key effect; its `pending` state means audit delivery is pending, not
that another key should be issued. Audit completion and registry cleanup cannot
share a transaction across PostgreSQL and Kubernetes.

`api.OperatorCredentialDelivery.RunOnce` reads the selected, UID-pinned registry
and checks at most sixteen references under a ten-second deadline. Journal work
has a separate five-second budget, leaving time for the final registry CAS even
when a later lookup stalls. For each
supported registry operation it loads the original journal intent and verifies
all outbox proof fields against that intent and registry identity. A missing,
conflicting or unavailable journal record retains the reference. A committed
reference can finish only as success; a conflicting final failure is not
rewritten. No active administrator is required to finish an already-admitted
operation's audit record after its credential has been retired.

After an acknowledged durable completion, one registry CAS marks that reference
`delivered`. A later pass rechecks durable completion before removing it. This
preserves the persistence invariant that pending references cannot disappear.
Both writes preserve the complete credential document. A conflict or uncertain
acknowledgement never retries a cached candidate: the next pass loads current
state and relies on the journal's stable operation identity. Journal recovery
rows remain after outbox pruning, preventing replay of old issuance requests.

A rotating process-local cursor advances even past errors. A missing admission
cannot permanently starve unrelated operations. Restarting repeats idempotent
work; there is no lease or key-material cache. Multiple processes may race, but
only one matching UID/resource-version CAS can win. The helper never fences an
uncommitted key write, selects authority, issues a key or changes access.
Adoption's separate configuration-authority admission remains retained until
its executor supplies a corresponding durable contract.

Runtime scheduling and health/coverage reporting remain activation gates. This
primitive must not be described as a running collector merely because it exists.
