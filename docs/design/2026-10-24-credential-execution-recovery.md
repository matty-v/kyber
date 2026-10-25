# Identify maintenance writers before recovery

Draft implementation slice for the approved MAT-134 recovery flow. Runtime
protocol remains zero. No recovery endpoint, release shortcut or activation is
introduced by recording a writer.

A recovery cannot infer termination from reservation age, an HTTP failure, a
missing Pod, or a Job label alone. Kubernetes can mark Pods Failed after losing
contact with their node; older Job terminal conditions can also precede full
Pod termination. See the Kubernetes [Pod lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)
and [Job lifecycle](https://kubernetes.io/docs/concepts/workloads/controllers/job/).

Bind new supervised executions to their actual namespace, Pod UID and controller
Job UID before acquisition. Use a single non-retrying Job Pod with restart policy
Never, one completion and zero retries. Read these objects directly within the
existing bound, not from informer cache or operator-supplied metadata. Persist
only canonical whitelisted metadata in the immutable reservation. Old reservations
without a writer remain inspectable but cannot gain automatic recovery rights.

This also favors executing the eventual adoption handoff in a supervised Job,
rather than an HTTP handler inside a restarting control-plane container. The
adoption admission must be durable before credential effects, and no key values
belong in Job args, environment, logs or the journal. Complete that separate
transaction/UI design before connecting an adoption executor.

Recovery evidence must bind the exact inspected reservation and original writer.
A known terminal original Pod, actual container termination information, a
terminal non-retrying Job and no other active owned Pods can establish shutdown.
Missing/replaced objects, node-loss/unknown container state, pending termination,
retries, ambiguous ownership or read failures remain unverified. Retain the block;
never turn unavailable evidence into permission to unlock.

Shutdown evidence is necessary, not sufficient: required audit, current admin
access, authority/release/registry reconciliation and UID/RV-protected release
remain mandatory. Recording an execution identity does not implement those gates.
Do not offer a general force-unlock or revive configuration authority.

Before enabling protocol one, ensure evidence is retained while a reservation is
unresolved. Existing upgrade Jobs have seven-day TTL; pending maintenance cannot
rely indefinitely on objects that can disappear. Arbitrary privileged deletion
or restoration is outside the supervised-operation guarantee and may require
operator/cloud action. Keep that limitation explicit in the UI.

## Upgrade write admission

Before the first CRD/Helm effect, the supervisor records a one-time metadata CAS
checkpoint with release, starting revision, target version, Deployment and the
fresh authority mode/registry UID. No values or rendered manifests are stored.
The acknowledged write returns a new private ownership handle; the old opaque
resource version stays invalid. Lost acknowledgements reconcile only within the
original call using its fresh nonce. Failed or unconfirmed checkpoint admission
never starts CRD/Helm effects.

A checkpoint is permission to start effects, not evidence they ran or succeeded.
Recovery must inspect the current release/authority before release. Conversely,
a confirmed absence of this checkpoint can support the preflight-only case only
for these bound upgrade writers, after the original writer has stopped and old
metadata writes are fenced. It does not describe adoption effects, which need
their own durable handoff journal contract.
