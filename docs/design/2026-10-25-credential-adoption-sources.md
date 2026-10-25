# Freeze credential source versions for adoption

> Historical implementation plan. The integrated operator-key feature shipped in
> [PR #396](https://github.com/matty-v/kyber/pull/396). Staged or proposed status
> statements below describe development at the time, not current availability.
> See the [current operator guide](../operator/operator-credentials.md) for supported
> setup, management, recovery and reporting limits.

Staged MAT-134/MAT-97 source-reader slice. No activation, Job creation, HTTP
endpoint or source/registry write. The future supervised worker must combine
this private input with current per-replica identity/access observations.

Read the installation credential Secret and resolve its caller references using
the same parser/trimming rules as control-plane startup. Pin every source to its
namespace, name, UID and opaque resource version. Repeated references must agree;
a changed source during collection, a missing/unreadable key, rejected caller,
unsupported HTTP material or malformed configuration prevents the whole snapshot.
Never silently adopt a partial set of configured credentials.

The runtime constructor uses a direct bounded Secret-only Kubernetes client,
with no informer cache or discovery. Every read has a two-second limit inside a
bounded overall snapshot operation. Recheck all recorded source versions before
returning; the worker must recheck again before authoritative effects. Compare
resource versions only for equality. A reused name with a different UID is a
changed source, even if its version text happens to match.

Values live only in the private source object. Formatting and JSON reject or
redact it. Errors are static and never contain parser input, material, reference
values or underlying API diagnostics. There are no material hashes/fingerprints,
new persistence tables, extra model/provider dependencies or last-good fallback.

Review: this is source freshness, not proof that pods loaded these values.
The later worker must match each value to the currently accepted identity and
exact grants on every supported replica, reject missing/extra accepted values,
serialize supported rotation/upgrade paths and require explicit ownership
confirmation before selecting the registry. External source changes must not be
claimed as an atomic transaction with the handoff. Protocol remains zero.
