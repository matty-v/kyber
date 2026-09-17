# Harness lifecycle staging qualification

Use this runbook for the final MAT-76 through MAT-79 qualification before a
Kyber release. It complements deterministic CI; it does not replace it. The
machine-readable map from contract requirements to CI and staging evidence is
[`test/contract/harness_lifecycle_evidence.json`](../../test/contract/harness_lifecycle_evidence.json).

## Safety and entry criteria

Run only in the dedicated development cluster with disposable agents and
operator-approved, scoped test credentials. Never paste credential values into
the evidence record, issues, logs, screenshots, or task prompts. Do not delete
a Secret, persistent volume, or storage backend merely to manufacture a loss
case. The required storage test is recoverable reattachment; destructive loss
is optional and needs separate operator authorization.

Before starting, require:

- MAT-76 through MAT-79 implementation PRs merged and their required CI green;
- an otherwise clean staging namespace with enough capacity for two disposable
  agents and a replacement node;
- immutable control-plane, sidecar, agent-base, Claude Code, and Codex image
  digests recorded, plus the installed upstream CLI versions;
- one disposable Claude Code credential and one disposable Codex credential;
- access to Kubernetes events, Agent status, task status/events, and pod logs;
- a cleanup owner and an evidence-record location.

Record UTC start time, cluster/context, namespace, Kyber commit/release
candidate, image digests, runtime versions, storage class, and node provider.
Abort if an image is mutable or if a test agent points at non-disposable state.

## Baseline matrix

Create fresh Claude Code and Codex agents through the supported public flow.
For each runtime, record only identifiers and redacted status:

1. Confirm the requested and installed runtime versions are distinct fields.
2. Wait for `Running` and current-pod capability observations. Record the pod
   UID, observation timestamp, contract version, and boolean feature names.
3. Send a unique startup marker and one durable task. Require a correlated
   receipt and explicit completion; tmux delivery alone is not acceptance.
4. Restart the native session. Require a new native session identity and a
   second explicitly completed task.
5. Confirm the platform recall file is present and bounded. Do not equate it
   with native conversation resume.

If capability evidence is missing, stale, for another pod UID, or negative,
capture the actionable reason and verify the dependent operation stays gated.
Restore the integration and require new positive evidence before continuing.

## Native compaction

Run for both Claude Code and Codex:

1. Establish a conversation long enough that native compaction has observable
   state to summarize; record only a benign marker and timestamps.
2. Invoke `POST /api/v1/agents/{name}/compact-session` once. Record the HTTP
   response and delivery time. A success response proves delivery only.
3. Observe the native TUI/transcript until the upstream runtime reports that
   compaction completed. Record the runtime-owned completion signal and elapsed
   time without copying conversation content.
4. Send a follow-up marker and verify the session remains usable.
5. While a session restart is in progress, verify compaction is refused. Also
   verify an unsupported fixture/runtime returns a visible unsupported result.

If the upstream runtime offers no stable completion signal, record the case as
unverified rather than inferring completion from HTTP 200 or prompt return.

## In-place runtime repair

Run for both production runtimes with a disposable agent:

1. Record hashes and modes—not contents—of unrelated identity, session recall,
   and runtime credential files on the persistent volume.
2. Produce `BrokenRuntime` using the approved non-credential harness fault for
   that runtime. Do not revoke or delete its credential.
3. Invoke `POST /api/v1/agents/{name}/repair-runtime`. Verify exactly one
   short-lived, same-node maintenance pod mounts the agent PVC, receives no
   service-account token, and uses the pinned runtime image.
4. Require package installation and executable/version verification before the
   Agent requests restart. Confirm the maintenance pod is removed.
5. Recompute unrelated-file hashes and modes; they must be unchanged. Confirm
   the agent returns to `Running`, publishes fresh capability evidence, and
   completes a new durable task.
6. Repeat with an intentionally invalid disposable package version or isolated
   registry failure. The agent must remain `BrokenRuntime`, unrelated state
   must remain unchanged, and the maintenance pod must be cleaned up.

Record timeout/failure classification separately from authorization. A repair
cannot prove that provider credentials are valid and must not change `NeedsAuth`
into an installation failure.

## Node, pod, and storage recovery

Run for both production runtimes. Capture the initial pod UID, PVC UID/name,
credential Secret UID/resource version, native session identity, recall-file
hash, restart count, and current capability observation without recording any
Secret data.

### Pod replacement

Delete only the disposable agent pod and let its controller recreate it.
Require the same PVC and credential Secret, a new pod UID, invalidation of old
capability evidence, fresh evidence from the new pod, available platform
recall, and an explicitly completed post-replacement task. Record whether the
native runtime resumed or began a new conversation; never infer one from the
other.

### Node replacement and recoverable volume reattachment

Use the provider's normal managed-node replacement path. Require the Agent to
enter `WaitingForMachine` without spending a runtime retry, remove the stranded
pod, and create a replacement pod only after the Machine becomes ready. Verify:

- replacement-node affinity and a new pod UID;
- the same persistent claim and provider volume identity reattached;
- the per-agent credential Secret remained unchanged;
- old-pod capabilities did not authorize task delivery;
- platform recall is honest and bounded after reattachment;
- a task uncertain at the death boundary is completed only with a matching
  receipt, or terminates `delivery_unknown` without automatic replay;
- a new post-recovery task can be accepted and explicitly completed.

The local-path development storage class does not survive physical node loss
and cannot certify this case. Use a staging storage class that explicitly
supports the tested reattachment behavior, and record that class/provider.

### Honest loss boundary

Loss of the persistent volume is outside workspace durability. Kyber may still
retain the Kubernetes credential Secret after volume loss, but a provider
rotation that existed only on the lost volume can require reauthorization.
In-flight external side effects can remain ambiguous. The required result is a
visible loss/recovery boundary, never an invented transcript, credential, task
completion, or exactly-once claim.

## Final evidence record

For every row, record `pass`, `fail`, `blocked`, or `unverified` plus:

| Field | Required evidence |
|---|---|
| Build | Kyber commit/release candidate and immutable image digests |
| Runtime | runtime ID and requested/installed upstream versions |
| Scope | agent, pod UID, Machine/node, PVC/storage class; no credential values |
| Action | API operation or provider replacement action and UTC timestamps |
| Observation | status reason, capability booleans, receipt/session/task IDs, bounded hashes |
| Result | the contract guarantee actually established and any residual boundary |
| Cleanup | agent, maintenance pod, tasks, test Secrets, PVCs, and temporary provider resources removed |

Link logs by access-controlled location and redact headers, bodies, environment
values, transcript text, and Secret data. A screenshot is supporting context,
not primary evidence when structured status or task events exist.

MAT-78 remains in testing, and no Kyber release is cut, until the combined
matrix passes or every exception is explicitly accepted and recorded. After
the run, delete the disposable agents through the normal lifecycle, verify no
repair/test pods remain, remove their test Secrets and PVCs, remove replacement
nodes or temporary capacity, and record UTC cleanup completion.
