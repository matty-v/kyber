# Preemption Resilience Design

**Date:** 2026-04-13
**Status:** Approved — core design shipped 2026-04-13; issue #3 follow-up landed 2026-04-14 (see below)
**Author:** Dave (with Matt's input)

## Follow-ups that landed after initial ship

**Issue #3 (2026-04-14):** Machine reconciler 409 conflict handling — when a machine CRD write returns 409, the reconciler now adopts the existing object if it is in RUNNING phase, or deletes it and retries if it is in TERMINATED phase. This closes the race where a preempted VM's replacement comes back with the same node name and the reconciler couldn't adopt it cleanly.

## Problem

Kyber agents run on spot/preemptible GCE VMs for cost efficiency. When GCE preempts a VM, the agent pod dies ungracefully, the local-path PVC is lost (it lives on the node's local disk), and the agent's retry counter burns through attempts before a replacement node is available. The agent loses installed packages, keychain credentials, conversation history, and any local files.

## Goals

1. Agent disk state (packages, credentials, files) survives node preemption
2. Agents drain gracefully when preemption is detected (~30s warning)
3. Retry counter is not burned on infrastructure failures
4. Session brief captures preemption context for agent restart
5. Agents notify their users about infrastructure disruptions

## Non-Goals

- Zero-downtime preemption (there will be a brief outage while the replacement VM boots)
- Cross-region migration (regional PD covers all zones within a region, but not across regions)
- Non-GCE cloud providers (GCE-specific metadata API and PD CSI)

## Design

### 1. GCE Persistent Disk Storage

Replace `local-path` StorageClass with a GCE Regional Persistent Disk-backed StorageClass. Regional PDs replicate across two zones in the same region, so they remain accessible even if the replacement VM lands in a different zone.

**New StorageClass:**
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: kyber-pd
provisioner: pd.csi.storage.gke.io
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
parameters:
  type: pd-standard
  replication-type: regional-pd
```

**Why regional PD over zonal PD:**
- Eliminates zone-pinning constraint — machine controller can freely pick any zone in the region for replacement VMs, improving spot availability
- Survives full zone outages, not just node preemption
- Cost: 2x storage (replicated), but disks are small (20Gi per agent) so the difference is negligible
- IO: ~2-4ms write latency vs ~1-2ms for zonal PD. Acceptable for agent workloads which are not IO-bound.

**Changes:**
- Install GCE PD CSI driver in the Helm chart (k3s does not bundle it, unlike GKE)
- `pod_builder.go` `BuildPVC()` uses `kyber-pd` StorageClass
- Existing PVCs on `local-path` must be migrated (copy data to new PD-backed PVC)
- Machine controller no longer needs to pin replacement VMs to the same zone (regional PD is accessible from any zone in the region)

### 2. Node Agent Preemption Detector

The node agent (`cmd/node-agent/`) gains a preemption watcher goroutine that polls the GCE instance metadata endpoint.

**Endpoint:** `http://metadata.google.internal/computeMetadata/v1/instance/preempted`
- Header: `Metadata-Flavor: Google`
- Returns `TRUE` when preemption is imminent (~30s before termination)
- Poll interval: 5 seconds

**Signal path:**
1. Node agent detects `TRUE` from metadata endpoint
2. Node agent calls control plane: `POST /internal/machines/{name}/preemption-notice`
3. Control plane receives notice with ~25s remaining to act

**Why the node agent:** The metadata endpoint is link-local, accessible only from the VM itself. The machine controller runs on the control plane node and cannot reach it. The node agent already runs as a DaemonSet on every worker.

**New code in `cmd/node-agent/`:**
- `preemption.go` — goroutine that polls metadata endpoint, calls control plane on detection
- Machine name resolved from node labels (`kyber.dev/machine`)

### 3. Graceful Agent Drain

When the control plane receives a preemption notice, it drains all agent pods on the doomed node within the ~25s window.

**Flow:**
1. `POST /internal/machines/{name}/preemption-notice` received
2. Control plane looks up all agents with `status.machineName == machine.Name`
3. For each agent: fire `EventPreemptionNotice` on the agent state machine
4. Agent transitions: `Running` -> `Draining` -> (write brief + delete pod with 20s grace) -> `WaitingForMachine`
5. Pod deletion uses explicit `GracePeriodSeconds: 20` (leaving 5s margin from the ~25s window)

**If the node dies before drain completes:** The agent pod is evicted by k8s. The agent reconciler detects this and checks if the machine is in `Preempted`/`Replacing` phase. If so, it routes to `WaitingForMachine` instead of the crash path. The brief may be incomplete (best-effort).

**New internal API route:**
- `POST /internal/machines/{name}/preemption-notice` — accepts `{ "timestamp": "...", "instanceId": "..." }`
- Handler fires `EventPreemptionNoticeReceived` on the machine, which triggers agent drain

### 4. Preemption-Aware Retry Logic

Changes to the agent state machine (`pkg/controllers/agent/state_machine.go`):

**New event:** `EventMachinePreempted`
- Fired when agent pod dies AND the underlying machine is in `Preempted` or `Replacing` phase

**New phase:** `WaitingForMachine`
- Agent is healthy but has no node to run on
- Not counted as a failure, does not increment `restartCount`
- Agent stays in this phase until a suitable machine becomes Ready

**Transitions:**
| From | Event | Action | To |
|------|-------|--------|-----|
| Running | EventPreemptionNotice | ActionDrainAgent | Draining |
| Draining | EventPodDeleted | ActionTransitionToWaiting | WaitingForMachine |
| Running | EventPodDied + machine preempted | ActionTransitionToWaiting | WaitingForMachine |
| WaitingForMachine | EventMachineReady | ActionWriteBriefAndCreatePod | Starting |

**How `EventMachineReady` fires:** The agent reconciler already watches machine CRDs. When a machine transitions to `Ready` (replacement VM provisioned and joined k3s), the reconciler re-evaluates all agents assigned to that machine. Agents in `WaitingForMachine` whose machine is now `Ready` receive `EventMachineReady`.

**Classification logic change in `reconciler.go`:**
When `EventPodDied` fires, before incrementing `restartCount`:
1. Look up the agent's machine
2. If machine phase is `Preempted`, `Replacing`, or `Provisioning` → fire `EventMachinePreempted` instead
3. Route to `WaitingForMachine` (no retry increment)

**Existing crash path unchanged:** Pod dies without machine preemption → `restartCount++` → 3-retry limit with 10s/30s/90s backoff → `Failed`.

### 5. Session Brief Enrichment

Extend the `Brief` struct in `pkg/briefstore/store.go`:

**New `ShutdownType` value:** `"preemption"`
**New `RestartReason` value:** `"preemption"`

**New field in `BriefMetadata`:**
```go
type PreemptionContext struct {
    InstanceId    string `json:"instanceId"`
    Zone          string `json:"zone"`
    Timestamp     string `json:"timestamp"`
    GracefulDrain bool   `json:"gracefulDrain"`
}
```

**Write paths:**
- Graceful drain: `writeBrief()` called during `ActionDrainAgent` with full `PreemptionContext` and `GracefulDrain: true`
- Ungraceful (node died first): `writeBrief()` called during `ActionTransitionToWaiting` with `GracefulDrain: false`, context filled from machine status

**Read path:** Unchanged — init container fetches via `GET /internal/agents/{name}/session-brief`, writes to `/persist/session-brief.json`.

### 6. Agent Resume Notification

The session brief flows to the agent runtime via the init container. The agent decides how to handle a preemption restart.

**For the Claude Code runtime:**
- `start-claude.sh` reads `/persist/session-brief.json` at boot
- If `shutdownType == "preemption"`: sets `KYBER_PREEMPTION_RESTART=true` env var
- The agent's CLAUDE.md or system prompt references this to send a Telegram notification: "Moved to new infrastructure — picking up where we left off."

**Design principle:** The notification is in the agent's personality layer, not hardcoded in the platform. Each runtime handles preemption context in its own way. The platform provides the signal (`session-brief.json`), the agent decides the response.

## Component Summary

| Component | Files Changed | What Changes |
|-----------|--------------|--------------|
| Helm chart | `charts/kyber/` | GCE PD CSI driver dependency, new StorageClass |
| Pod builder | `pkg/controllers/agent/pod_builder.go` | StorageClass → `kyber-pd` |
| Node agent | `cmd/node-agent/` | New `preemption.go` watcher goroutine |
| Control plane API | `pkg/api/routes_internal.go` | New `POST /internal/machines/{name}/preemption-notice` |
| Machine controller | `pkg/controllers/machine/` | Handle preemption notice event, trigger agent drain |
| Agent controller | `pkg/controllers/agent/` | New `WaitingForMachine` phase, `Draining` phase, preemption-aware retry logic |
| Brief store | `pkg/briefstore/store.go` | New `PreemptionContext` field, new shutdown/restart types |
| Claude Code runtime | `images/claude-code/start-claude.sh` | Read preemption flag from brief, set env var |

## Testing

- **Unit tests:** State machine transitions for new events/phases, brief serialization with preemption context
- **Integration tests:** Preemption notice API endpoint, agent drain flow, retry counter behavior
- **E2E test:** Create agent on spot VM → simulate preemption (delete VM) → verify agent restarts on replacement with PVC intact and brief shows preemption context

## Migration

Existing agents on `local-path` PVCs need migration to `kyber-pd`:
1. Stop agent (graceful)
2. Create new PD-backed PVC
3. Copy data from local-path PV to PD PV (via a temporary pod mounting both)
4. Update agent to use new PVC
5. Start agent

This is a one-time operation per existing agent.
