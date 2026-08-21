# Machine capacity recovery

## Incident and expectation

On 2026-08-21, multiple cost-optimized Machine pools in a production GKE
installation lost their Nodes while Google Cloud reported zonal Spot capacity
exhaustion. The provider correctly kept each pool at a desired size of one,
but Agents cycled through `Starting` and `Failed` instead of parking in
`WaitingForMachine` and resuming after replacement capacity arrived.

An infrastructure interruption is not an Agent failure. The operator contract
is therefore:

- a Machine with desired availability Online keeps reconciling until capacity
  returns;
- an Agent assigned to unavailable capacity enters `WaitingForMachine`, does
  not spend restart retries, and has no stale pod pinned to the old Node;
- when the Machine becomes Ready, the Agent creates a fresh pod against the
  replacement Node and resumes automatically;
- the UI explains recovery in provider-neutral terms and keeps raw scheduler
  and provider identifiers behind copyable technical details.

## Confirmed causes

1. Production did not initialize `AgentReconciler.MachineGetter`, although
   unit tests injected it. Machine-aware classification therefore returned
   false in the deployed binary.
2. `Starting` consulted Machine state only after the pod disappeared or began
   terminating. A Pending pod on lost capacity instead reached the startup
   timeout and became `Failed`.
3. `ActionTransitionToWaiting` did not delete the old pod. On recovery,
   `createPod` correctly refused to overwrite that non-terminal object.
4. Pool-oriented providers may not expose a per-instance preemption signal.
   GKE left the node pool present and repairing after its Spot Node vanished,
   so the Machine controller classified the disappearance as an ordinary
   failure.

## Design

Agent lifecycle consumes only Machine readiness. `MachineUnavailable` is an
internal, provider-neutral event emitted for active or retrying Agents when
their assigned Machine is not `Ready` or `Running`. It transitions `Creating`,
`Starting`, `Running`, and `Restarting` to `WaitingForMachine` using the
existing transition action. A pre-existing `Failed` Agent retains its failure
provenance instead of being revived by an unrelated Machine recovery. The
action force-deletes a Pending pod or a pod on a missing/NotReady Node, but
retains normal termination grace while the Node is healthy. It preserves PVCs
and never increments `restartCount`. Operator Stop remains available from
`WaitingForMachine` and converges directly to `Stopped`.

The production control plane supplies a cache-backed `KubernetesMachineGetter`.
`WaitingForMachine` polls on the existing 15-second cadence. A Ready/Running
Machine emits `MachineReady`, and the existing `WriteBriefAndCreatePod` action
rebuilds the pod with the Machine's current `status.nodeName`.

For interruptible capacity providers, disappearance of the selected Ready Node
is sufficient evidence to enter replacement recovery even when the provider
cannot report a native interruption reason. Provider-specific observation and
repair remain inside the adapter. Reliable-capacity failures retain the
ordinary Failed/reconcile path, while their Agents still wait because Machine
readiness is the only Agent-side contract.

The GKE NodePool API does not expose a reliable current target size through
`initialNodeCount`, so Kyber retains the existing idempotent SetSize calls while
capacity is absent. Avoiding redundant provider operations requires a separate
trustworthy target-size observation; recovery correctness does not depend on
that optimization.

No CRD or REST shape changes are required. Existing Machine `status.message`
stores a provider-neutral recovery explanation. PWA banners derive recovery
copy from existing Agent/Machine phases and availability. Verbatim scheduler
messages and provider references render only inside an expandable panel with a
copy action.

## Verification

- Pure state-machine coverage for every new `MachineUnavailable` transition.
- Agent envtest: Pending pod on an unavailable Machine is deleted, the Agent
  parks without a retry increment, stale scheduling status clears, and a Ready
  replacement Machine recreates the pod with its new hostname affinity.
- Machine/controller test: missing interruptible provider capacity enters the
  replacement path even without a native interruption reason.
- GKE adapter test: a pool with zero attached Nodes reports Recovering and
  continues asserting the Online size target.
- PWA tests: friendly recovery copy is visible and raw details are collapsed.
