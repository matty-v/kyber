# Agent resource visibility and disk recovery — execution plan

**Status:** In progress
**Date:** 2026-08-27
**Tracker:** MAT-10

## Goal

Make agent CPU, memory, and persistent-disk pressure visible before failure;
preserve a working maintenance shell when disk use crosses the reserve; and
give operators a tested recovery path for a full or nearly-full volume.

## Current findings

- The status sidecar already samples the pod cgroup for OOM events and forwards
  authenticated observations to the control plane, but it does not mount the
  persistent volume or publish current resource use.
- `Agent.status.activity` is the existing sidecar-fed read model consumed by
  the PWA and is the appropriate home for the latest sample.
- Since kyber#575, supporting containers are native sidecars. When the regular
  agent container exits, Kubernetes tears the sidecars down and the pod becomes
  terminal. MAT-10's historical phantom-Running-pod failure therefore needs a
  regression test and old-pod compatibility handling only if characterization
  proves a remaining gap; it does not justify a parallel lifecycle mechanism.
- A reserve-triggered `DiskExhausted` state must keep PID 1 alive and stop only
  the interactive harness. A normal agent-container exit would remove Shell
  access and defeat the recovery window.

## Delivery rules

- Deliver three independently reviewable PRs in dependency order.
- Update Go types, generated CRDs, OpenAPI, and hand-written TypeScript types
  together for every wire-shape change.
- Include the required `pwa-views` version and changelog update with PWA source
  changes.
- Keep sampling and reporting best-effort: observability failures must never
  crash or block an agent pod.
- Keep lifecycle decisions in the pure state machine and I/O in the
  reconciler/runtime boundary.
- Validate controller behavior with envtest and validate the reserve-shell and
  PVC-growth paths on a disposable agent in `kyber-dev-gcp`.

## Slice 1 — resource telemetry and PWA visibility

**Branch:** `sol/mat-10-resource-telemetry`

- [x] Characterize the status-sidecar cgroup and volume mount contract.
- [x] Mount the agent persistence volume read-only in the status sidecar.
- [x] Sample `/persist` with `statfs`, pod-cgroup memory usage/limit, and CPU
  usage/quota on the heartbeat cadence using small testable readers.
- [x] Forward the latest sample through the internal status API and emit the
  corresponding OTel metrics.
- [x] Store the latest sample and timestamp on `Agent.status.activity`.
- [x] Update generated CRDs, OpenAPI, API tests, and PWA wire types.
- [x] Replace static resource rows on Agent Detail with used-of-allocated bars;
  warn at 80% and show danger at 90%.
- [x] Add a compact Agent List warning when disk usage is at least 80%.
- [x] Publish a reserve observation at 90% for slice 2 to consume, without
  changing lifecycle or stopping the harness in this slice.
- [x] Run focused Go and PWA tests, then the repository-required gates.

### Slice 1 acceptance evidence

- Unit tests cover statfs/cgroup parsing, unlimited cgroup values, absent files,
  and post failures.
- Contract tests prove the Go and PWA resource-sample shape agrees.
- Component tests prove detail bars and the list threshold states.
- A local/devenv pod reports a changing sample without affecting heartbeat or
  readiness when a sampler is unavailable.

## Slice 2 — `DiskExhausted` maintenance lifecycle

- [ ] Add and document `DiskExhausted` plus its state-machine events and
  transitions.
- [ ] Consume the reserve observation and stop only the harness/session while
  leaving the container and Shell path alive.
- [ ] Keep `DiskExhausted` stable while the maintenance pod remains present.
- [ ] Permit recovery after usage drops below the reserve or the PVC requested
  size grows; restart the harness cleanly and return through `Starting`.
- [ ] Classify a hard-full terminated agent from its last resource observation
  and a bounded, explicit entrypoint failure signal.
- [ ] Characterize modern native-sidecar pod termination and add a regression
  fixture for the historical terminated-agent/running-sidecar shape.
- [ ] Update lifecycle documentation, API/PWA phase rendering, and recovery
  actions.
- [ ] Cover transitions with table tests and reconciler behavior with envtest.

### Slice 2 acceptance evidence

- A disposable real agent crossing 90% enters `DiskExhausted` while its Shell
  tab remains usable.
- Removing files below the recovery threshold or increasing the PVC request
  allows a deliberate restart and returns the agent to `Running`.
- A hard-full agent is never mislabeled `NeedsAuth` solely because boot or a
  harness command failed after ENOSPC.

## Slice 3 — recovery and prevention

- [ ] Enable expansion on the GCE PD StorageClass and verify the Helm replacement
  behavior used by the chart.
- [ ] Extend `docs/operator/wedged-agent-recovery.md` with diagnosis, cleanup-pod,
  and volume-growth procedures.
- [ ] Cross-link the recovery procedure from the agent manual.
- [ ] Clear `/tmp` during rootfs-mode boot without following unsafe paths or
  touching non-rootfs persistence modes.
- [ ] Test that a file written under `/tmp` does not survive pod recreation.
- [ ] Verify PVC expansion on the GKE development installation.

## Progress log

- 2026-08-27: Matt approved the three-slice plan. Reordered telemetry ahead of
  lifecycle because the reserve transition depends on a trustworthy in-pod
  sample. Confirmed kyber#575 superseded the historical non-terminal pod shape.
- 2026-08-27: Slice 1 implementation checkpoint complete through generated
  contracts and PWA surfaces. Focused Go tests, OpenAPI contract tests,
  TypeScript compile, and all 744 PWA tests pass. After installing the required
  envtest assets, the full controller package progressed through its envtests
  but hit the repository's 10-minute package timeout in the unrelated
  `TestTranscriptTailerScript_BoundedOverManyFiles`; focused pod-injection tests
  pass.
- 2026-08-27: PR CI is green. The first GKE dev deployment proved image
  convergence but exposed the cluster's cgroup-namespace shape (`0::/`): the
  mounted cgroup root is already the pod cgroup. Updated path resolution and
  regression coverage so resource and OOM sampling use
  `/sys/fs/cgroup/{memory.current,memory.max,cpu.stat,cpu.max,memory.events}` in
  that environment. Focused sidecar tests and vet pass; live redeployment is in
  progress.
- 2026-08-27: Redeployed commit `1b922f8` to GKE dev as immutable worktree tag
  `worktree-20260827205401-1b922f8`. Normal reconciliation rolled the existing
  test agent to the new status-sidecar. Its CR status and public API now carry a
  fresh sample with CPU usage/limit, memory used/limit, disk used/total, sample
  time, and `diskReserveReached=false`; heartbeat/readiness remained healthy.
