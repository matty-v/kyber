# Provider-agnostic Machine provisioning

**Status:** Proposed
**Date:** 2026-08-20
**Scope:** Machine intent, compute-provider boundary, installation profiles,
operator UX, existing-capacity registration, and managed cloud capacity.

This design supersedes the future API/provider direction in
[`2026-08-13-compute-provider-boundary-design.md`](2026-08-13-compute-provider-boundary-design.md)
where that direction still models a provider resource as an individual VM. The
implemented provider registry, opaque observations, neutral create-request
aliases, `static` compatibility, fake provider, and emulator remain useful
foundations.

## 1. Decision

A Kyber `Machine` is one logical unit of assignable agent capacity. Kyber core
declares whether that capacity should be online, offline, or deleted. A compute
provider decides how to fulfill the intent.

The backing resource is deliberately opaque to the API, PWA, Machine state
machine, and controller. It may be a local Kubernetes Node, a GCE VM, a
size-one GKE node pool, an EC2 instance, an EKS node group, or a future
provider-specific construct.

```text
operator chooses profile + availability
                  |
                  v
            Kyber Machine
       logical capacity intent
                  |
                  v
          capacity provider
       provider-owned fulfillment
                  |
          +-------+--------+
          |       |        |
       static    GCE      GKE       future
        Node      VM    node pool   providers
```

Provider mechanics never become Machine kinds. In particular, Kyber core does
not model `Instance`, `ManagedGroup`, node-pool size, managed instance groups,
or a `ReplacementOwner` switch.

## 2. Goals

- Keep the operator's Machine list, detail, create flow, and lifecycle language
  consistent across local and cloud installations.
- Keep cloud-native resources and state inside provider implementations.
- Make providers reconcile desired capacity rather than expose VM verbs.
- Keep local and externally managed Kubernetes capacity first-class.
- Let installers choose an opinionated environment preset and override its
  ordinary Helm values.
- Let operators select a stable, friendly Machine profile instead of a native
  cloud SKU.
- Preserve existing `mock`/`static`, `fake`, and direct-GCE installations
  during migration.
- Allow a GKE installation to implement one Machine as one size-one node pool
  without making that mapping part of the public Kyber model.

## 3. Non-goals

- Multi-node Machines or autoscaled capacity pools.
- Multiple active compute providers in one installation.
- A portable least-common-denominator for every advanced cloud option.
- Provider-specific configuration in the operator UI.
- Cross-zone migration of zone-bound persistent volumes.
- Implementing GKE, EKS, and AKS in the same change set.
- Replacing Kubernetes scheduling or the existing Agent-to-Machine assignment.

## 4. Product model

### 4.1 Machine

A Machine is a durable Kyber identity with:

- a selected profile;
- an availability class such as `reliable` or `costOptimized`;
- desired phase (`Running` or `Stopped`);
- an optional provider-neutral location;
- a management mode (`Managed` or `External`);
- an opaque provider reference in status;
- zero or one current assignable Kubernetes Node.

The provider remains recorded internally so old CRs and a future provider-set
installation can be migrated safely, but the normal PWA does not ask the
operator to choose it. The configured installation supplies it.

### 4.2 Machine profile

A profile is an installer-curated promise presented to operators:

```yaml
id: standard
displayName: Standard
description: Good default for one or two typical agents
capacity:
  cpu: "4"
  memory: 16Gi
availabilityClasses:
  - reliable
  - costOptimized
recommended: true
```

The provider-specific realization stays in installer configuration:

```yaml
compute:
  provider: gke
  gke:
    profileMappings:
      standard:
        machineType: e2-standard-4
        nodeBootDisk:
          type: pd-balanced
          sizeGiB: 200
        imageType: UBUNTU_CONTAINERD
```

Profiles do not expose image type, service account, network, node-pool policy,
or native repair settings to the operator.

### 4.3 Storage vocabulary

Node boot/ephemeral storage and durable agent storage are separate concepts.
A cloud node's boot disk is provider and installer configuration. An Agent's
PVC is durable state and must be described separately in the UI and
installation preflight. A profile must not call the whole node boot disk
"agent disk."

### 4.4 Management mode

- `Managed`: the provider may create, suspend, resume, repair, and delete the
  backing capacity.
- `External`: the provider associates existing capacity and observes it, but
  Kyber cannot delete the backing resource.

The current static/local path is an `External` provider implementation, not a
deficient cloud mode. The product label is **Existing capacity**; `mock`
remains only a deprecated compatibility value.

## 5. Provider contract

The consumer-owned provider interface should converge on declarative
availability:

```go
type DesiredAvailability string

const (
    AvailabilityOnline  DesiredAvailability = "Online"
    AvailabilityOffline DesiredAvailability = "Offline"
    AvailabilityDeleted DesiredAvailability = "Deleted"
)

type CapacityProvider interface {
    Type() string
    Capabilities(context.Context) (Capabilities, error)
    Profiles(context.Context) ([]Profile, error)
    Validate(context.Context, DesiredMachine) error
    Reconcile(
        context.Context,
        MachineIdentity,
        DesiredMachine,
        ProviderRef,
    ) (Observation, error)
}
```

`ProviderRef` is a serialized opaque string. Only the provider that created or
discovered it may parse it. `Reconcile` is an idempotent, bounded reconcile
step: it may start or observe one provider operation, but it must not block for
an entire cloud operation. The controller requeues until the observation
converges.

Node bootstrap is a separate provider-neutral input containing the cluster
endpoint and join credential. A direct VM provider may deliver it as instance
metadata, while a managed-cluster provider may rely on its native node-pool
bootstrap. Bootstrap secrets must never be embedded in `ProviderRef`, returned
in observations, or exposed through the operator API.

Providers translate intent as follows:

| Provider | Online | Offline | Deleted |
|---|---|---|---|
| static | verify an eligible Node exists | make the Machine logically unavailable without touching the host | unregister only |
| fake | ensure simulated capacity | simulate absent capacity | remove simulated capacity |
| GCE | ensure a VM is running | stop VM | delete VM |
| GKE | ensure backing pool desires one node | resize to zero | delete backing pool |

The first GKE delivery is deliberately observation-only: it discovers the
configured cluster's existing node pool, reports health, and selects its Nodes,
while advertising no provisioning, unsupported suspend, and unregister-only
deletion. Managed resize/delete semantics are enabled only after this identity
mapping is verified in the target environment.

Replacement is always provider-owned. Direct GCE recreates a reclaimed VM;
GKE waits for or initiates repair through its native resource; static waits for
the installer. The Machine controller only reacts to provider-neutral
availability.

## 6. Capabilities and observations

Capabilities describe operator actions, not provider resource types:

```go
type Capabilities struct {
    CanProvision          bool
    CanDiscoverExisting   bool
    SuspendMode           SuspendMode
    DeletionMode          DeletionMode
    SupportsReliable      bool
    SupportsInterruptible bool
    SupportsLocations     bool
}
```

`SuspendMode` is `Capacity`, `LogicalOnly`, or `Unsupported`.
`DeletionMode` is `DeleteCapacity` or `UnregisterOnly`. These modes let the UI
describe consequences accurately while keeping the provider resource opaque.

The provider reports a Kyber-owned observation:

```go
type AvailabilityState string

const (
    StatePending    AvailabilityState = "Pending"
    StateAvailable  AvailabilityState = "Available"
    StateRecovering AvailabilityState = "Recovering"
    StateOffline    AvailabilityState = "Offline"
    StateAbsent     AvailabilityState = "Absent"
    StateFailed     AvailabilityState = "Failed"
    StateUnknown    AvailabilityState = "Unknown"
)

type Observation struct {
    State        AvailabilityState
    Reason       AvailabilityReason
    Message      string
    ProviderRef  ProviderRef
    Location     string
    NodeSelector map[string]string
    CreatedAt    time.Time
}
```

Portable reasons include `Provisioning`, `NodeJoining`, `Interrupted`,
`Repairing`, `Stopped`, `ExternalWait`, and `ProviderError`. Native status
strings stay inside the adapter.

The controller resolves the current Ready Node from `NodeSelector`. The
canonical managed selector remains `kyber.io/machine=<machine-name>`;
providers may supply a compatible stable selector. `status.nodeName` records
the current resolution and may change without changing Machine identity.

Provider node counts, managed-group types, operation resources, and native IDs
are diagnostics rather than portable Machine status.

## 7. Controller lifecycle

The Machine state machine operates on desired availability and observations:

```text
Running requested + Pending     -> Provisioning
Running requested + Available   -> Ready/Running
Running requested + Recovering  -> Replacing/WaitingForMachine
Stopped requested + Offline     -> Stopped
Deleted requested + Absent      -> remove finalizer
Failed observation              -> Failed
```

The controller never issues a provider-specific replacement action. On a node
loss it asks the same provider to reconcile `Online`; the provider restores
its capacity and reports progress. This removes the current assumption that a
preempted resource must be replaced by calling `CreateInstance` again.

Agent lifecycle behavior remains driven by Machine and Node availability. A
replacement hostname updates `status.nodeName`; the Agent is recreated with
affinity to the newly resolved node.

## 8. Portable API

The API exposes an installation-selected environment:

```json
{
  "compute": {
    "capabilities": {
      "canProvision": true,
      "canDiscoverExisting": true,
      "suspendMode": "Capacity",
      "deletionMode": "DeleteCapacity",
      "supportsReliable": true,
      "supportsInterruptible": true,
      "supportsLocations": true
    },
    "profiles": [{
      "id": "standard",
      "displayName": "Standard",
      "description": "Good default for one or two typical agents",
      "capacity": {"cpu": "4", "memory": "16Gi"},
      "availabilityClasses": ["reliable", "costOptimized"],
      "recommended": true
    }],
    "locations": [{"id": "us-central1-a", "displayName": "us-central1-a"}]
  }
}
```

Machine creation is provider-neutral:

```json
{
  "name": "worker-2",
  "profile": "standard",
  "availabilityClass": "costOptimized",
  "location": "us-central1-a"
}
```

Portable supporting routes are:

- `POST /api/v1/machines/preflight` — validate resolved intent before create.
- `GET /api/v1/machine-candidates` — list eligible existing capacity using
  opaque candidate IDs.
- `POST /api/v1/machines` — create managed capacity or register a candidate.

There is no generic `/compute/resources` API. Provider-native diagnostics may
exist behind an explicitly diagnostic surface, but the ordinary PWA does not
consume them.

## 9. Operator experience

All installations use **Machines -> New Machine** and the same Machine list and
detail views. The form renders only advertised capabilities.

Managed environments show name, profile, availability, and location when
selectable. Existing-capacity environments show name and an eligible worker.
Single-node local presets should auto-register their Machine, avoiding a form
entirely.

After creation, every Machine shows:

- phase and progress;
- profile and assignable/available capacity;
- hosted Agents;
- availability class;
- location when meaningful;
- `Kyber managed` or `Externally managed`;
- only the lifecycle actions supported by capabilities.

Provider names, node pools, instance groups, and opaque references stay out of
the primary view.

## 10. Installation model

Installation presets provide strong defaults while rendering to normal Helm
values:

- `local` — shared single node, static provider, auto-registration, local
  persistence warning.
- `existing-kubernetes` — external worker discovery/registration, installer
  chooses platform placement and storage.
- `gce-self-managed` — direct VM provider for the documented k3s topology.
- `gke-standard` — dedicated platform placement, managed Machine capacity,
  network PVC storage, Workload Identity.

Future presets may add EKS, AKS, and bare metal without changing the operator
contract.

The installer chooses a preset, runs preflight, reviews the topology, and may
override individual settings. Guides should be organized per target
environment rather than one document with interleaved conditionals.

Installation preflight reports portable requirement groups:

- cluster access;
- platform placement;
- worker provisioning;
- persistent storage and topology;
- network access;
- provider identity and permissions;
- supported lifecycle actions.

Providers supply environment-specific checks and remediation text.

## 11. GKE realization

The GKE adapter may privately implement one Machine as one node pool with
desired size zero or one:

- Online ensures the pool exists and desires one node.
- Offline resizes the pool to zero.
- Deleted removes a Kyber-owned pool.
- A missing node while the pool desires one reports `Recovering`; Kyber does
  not create a second pool.
- The pool stamps `kyber.io/machine=<name>` on its nodes.
- Existing pools are returned as opaque candidates and begin in `External`
  mode unless explicitly adopted for management.
- Protected installer pools, including the platform pool, can never be
  adopted or deleted.

The first implementation targets GKE Standard, one location per Machine,
autoscaling disabled, and durable network PVC storage. GKE Autopilot,
multi-node pools, and multi-zone placement are out of scope.

For the observed `kyber-datawire` installation, the existing `agents` pool is
first migrated from `mock` to GKE-backed `External` observation. Managed create,
suspend, and delete are tested with a disposable pool before the existing pool
is eligible for managed adoption.

## 12. Local compatibility invariants

- `scripts/devenv/up-full.sh --compute-provider fake` remains the runnable
  managed lifecycle test.
- `mock` remains a compatibility alias for `static` during migration.
- A local/static provider never gains authority to stop or delete a host.
- The first Ready-node fallback remains available until the `local` preset
  auto-registers it equivalently.
- No cloud credentials or cloud SDK calls occur in local/static modes.
- Operator Machine list/detail behavior remains shared across provider modes.
- Provider initialization continues to fail closed; no real provider falls
  back to fake or static.

## 13. Safety invariants

- Only the configured provider parses its opaque references and candidates.
- A provider deletes only resources carrying an expected Kyber ownership
  marker.
- External capacity is never deleted by Kyber.
- Protected capacity cannot be adopted.
- A Machine finalizer is removed only after the provider confirms the managed
  backing capacity is absent or confirms that external capacity requires no
  deletion.
- Location selection must be compatible with assigned persistent-volume
  topology.
- Profiles are stable IDs; changing their provider mapping does not silently
  mutate existing Machines.
- API capability rendering never branches on a provider name.
- Native provider statuses and resource kinds never enter the Machine state
  machine.

## 14. Decisions and remaining questions

1. Resolved: the CRD stores the neutral `profile`, `availabilityClass`,
   `location`, `managementMode`, and `providerRef` additively while retaining
   legacy aliases during migration. The remaining question is when to remove
   those aliases rather than whether to stage the neutral fields
   behind compatibility fields first.
2. Resolved: `Offline` for static Machines is logical only: no Agents are
   scheduled and the host is left untouched.
3. Resolved: `status.resolvedProfile` snapshots the stable profile ID,
   presentation metadata, and declared capacity used at creation. Existing
   Machines do not silently adopt changed mappings.
4. The future managed-adoption policy. The first implementation permits
   `Managed` only for capacity created by Kyber; discovered resources remain
   `External`.
5. The stable installer preset schema and whether preflight first ships as a
   control-plane route, a CLI command, or a repository script.
