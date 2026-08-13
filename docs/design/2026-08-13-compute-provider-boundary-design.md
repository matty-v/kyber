# Compute provider boundary and local simulator design

**Status:** Accepted; first local-simulator vertical slice implemented 2026-08-13
**Scope:** Machine provisioning, observation, interruption handling, provider
configuration, and local lifecycle testing. Agent runtimes and log-archive
backends are out of scope.

The second vertical slice exposes managed compute through neutral capabilities:
profiles, opaque locations, disk choices, and interruptible-instance support.
The public API retains `gceVMTypes` and the `machineType`/`zone`/`spot` request
aliases temporarily for older clients, but the active PWA uses only the neutral
contract. The compute-layer `MachineSpec` likewise uses `Profile`, `Location`,
and `Interruptible`; only the GCE adapter translates those into Google API
fields.

## 1. Problem

Kyber has a `ComputeAdapter`, but the contract and its consumers still encode
GCE concepts. Provider selection is a hard-coded `gce`/`mock` branch, observed
states are GCE status strings, admission and the PWA understand GCE fields, and
the node agent polls the Google metadata service directly.

The current `mock` path is also not a cloud simulator. A `provider=mock`
Machine bypasses the Machine state machine and attaches directly to an existing
Ready Kubernetes node. That behavior is useful for standalone installations,
but it cannot prove that provisioning, stop/start, deletion, interruption, or
replacement work for a compute provider.

## 2. Goals

- Keep the Machine controller and state machine provider-neutral.
- Make provider instance IDs opaque outside provider implementations.
- Translate native provider states into Kyber-owned states at the boundary.
- Select providers through a registry rather than branches in `main`.
- Let providers own configuration validation, offerings, and capabilities.
- Separate existing-node operation from simulated-cloud operation.
- Run the simulated provider through the same Machine reconcile path as a real
  provider.
- Make a full local k3d Kyber stack able to test both runnable agents and cloud
  lifecycle behavior without cloud credentials.
- Preserve existing GCE and `provider=mock` behavior during migration.

## 3. Non-goals

- Implement AWS or Azure in this change set.
- Generalize GCS/S3 log storage or Secret Manager integrations.
- Replace k3s as the dynamically provisioned node bootstrap target.
- Make pods runnable on synthetic Kubernetes Node objects. Runnable-agent and
  simulated-cloud tests are separate modes.

## 4. Provider contract

The provider boundary owns cloud operations and translation into Kyber domain
types. Native SDK types and state strings must not cross it.

```go
type InstanceState string

const (
    InstanceStatePending InstanceState = "Pending"
    InstanceStateRunning InstanceState = "Running"
    InstanceStateStopped InstanceState = "Stopped"
    InstanceStateDeleted InstanceState = "Deleted"
    InstanceStateFailed  InstanceState = "Failed"
    InstanceStateUnknown InstanceState = "Unknown"
)

type Observation struct {
    State        InstanceState
    Interruption InterruptionState
    Location     string
    ExternalIP   string
    InternalIP   string
    CreatedAt    time.Time
}

type Provider interface {
    Type() string
    Capabilities() Capabilities
    Offerings(context.Context) ([]Offering, error)
    Validate(CreateRequest) error
    Create(context.Context, InstanceSpec) (ProviderID, error)
    Start(context.Context, ProviderID) error
    Stop(context.Context, ProviderID) error
    Delete(context.Context, ProviderID) error
    Observe(context.Context, ProviderID) (Observation, error)
}
```

`ProviderID` is serialized as a string but is otherwise opaque. `Observation`
is authoritative for both lifecycle state and provider-initiated interruption;
the controller must not infer either from native status strings.

The initial implementation may retain the smaller `ComputeAdapter` while its
methods are migrated. The invariants above still apply to that transitional
interface.

## 5. Provider kinds

### 5.1 `gce`

The production provider. It translates GCE statuses, scheduling, locations,
network configuration, metadata, and errors into the provider contract.

### 5.2 `static`

Represents Kubernetes nodes provisioned outside Kyber. It selects a Ready node
explicitly labelled `kyber.io/machine=<machine-name>`, with the existing
single-node fallback retained for standalone installations. It advertises no
create, start, stop, interruption, or replacement capability.

The current public `mock` value remains a compatibility alias for `static`
during migration.

### 5.3 `fake`

A deterministic simulated cloud provider. It uses opaque IDs and configurable
observations, and supports:

- pending-to-running creation;
- delayed creation and observation;
- create failure;
- stop and start;
- idempotent deletion and not-found;
- provider interruption;
- unexpected failure;
- failed and successful replacement.

Unlike `static`, `fake` traverses the normal Machine state machine and
finalizer. It does not silently become Ready by selecting an existing node.

## 6. Node bootstrap

Bootstrap generation is a separate Kyber concern. A shared component produces
a provider-neutral bootstrap payload containing the k3s server URL, join token,
and `kyber.io/machine` label. Providers only deliver that payload using their
native transport:

- GCE: startup-script metadata;
- EC2: user data;
- Azure: custom data;
- fake: record it for assertions.

Provider code may select a provider-specific base image, but it must not own
the logical node registration contract.

## 7. Installation and Machine selection

Providers are registered by type. The control-plane binary enables concrete
providers explicitly and constructs them from provider-scoped configuration.
Initialization errors for an explicitly selected real provider fail closed;
Kyber must never silently fall back from GCE to fake/static behavior.

During the one-provider-per-install phase, API admission requires
`Machine.spec.provider` to match the configured provider or a documented
compatibility alias. A later provider-set configuration may allow multiple
providers in one installation; the reconciler must still resolve the provider
from the Machine rather than rely on one unlabelled global adapter.

## 8. API and PWA direction

The API exposes capabilities and offerings rather than GCE-specific catalogs:

```json
{
  "compute": {
    "provider": "gce",
    "capabilities": {
      "managedInstances": true,
      "interruptible": true,
      "startStop": true
    },
    "offerings": [{
      "id": "e2-standard-4",
      "displayName": "e2-standard-4",
      "capacity": {"cpu": "4", "memory": "16Gi"},
      "locations": ["us-central1-a"],
      "minimumDiskGiB": 10
    }]
  }
}
```

The PWA renders the advertised capabilities. Provider names must not select
hard-coded forms, zone lists, or capacity tables. This API-shape migration is a
later vertical slice and must update the Go handler, OpenAPI, PWA types, PWA
version, and changelog together.

## 9. Local verification modes

### Runnable stack

`scripts/devenv/up-full.sh` continues to use the compatibility `mock` alias for
the `static` provider. It attaches a Machine to the real k3d node, so agent pods
can run.

### Simulated cloud lifecycle

A new local profile starts the full control plane with `provider=fake`. Tests
create Machines and assert the same phases and status changes expected from a
real cloud:

1. create → Provisioning;
2. fake observation becomes Running;
3. an explicitly controlled Node-ready signal completes Ready;
4. stop/start traverse Stopping/Stopped/Provisioning;
5. interruption traverses Preempted/Replacing/Ready;
6. delete invokes the provider and clears the finalizer.

The fake control surface is test-only, disabled by default, and available only
inside the cluster. It must not be exposed through the public production API.

## 10. Test strategy

- Provider contract tests run against every managed provider implementation.
- GCE translation tests cover every native state and interruption combination.
- Fake-provider unit tests use opaque, nonnumeric IDs and scripted transitions.
- Machine controller changes include envtest coverage for complete fake
  lifecycle and finalizer behavior.
- Existing `up-full.sh` remains the runnable-agent smoke test.
- A local fake-profile smoke command creates, transitions, and deletes a
  Machine against k3d.
- Real-provider smoke tests remain small and provider-specific; the shared
  contract suite carries most lifecycle coverage.

## 11. Migration sequence

1. Add Kyber-owned instance observation types and translate GCE at the adapter.
2. Add provider construction/registration and fail-closed initialization.
3. Rename the existing-node implementation to `static`; retain `mock` as an
   alias.
4. Add `fake` and run it through the normal reconciliation path.
5. Add local fake-provider values and a smoke script.
6. Generalize capabilities/offerings in the API and PWA.
7. Extract node bootstrap generation and node-agent interruption detection.
8. Remove compatibility names and GCE-specific fallbacks after a deprecation
   window.

## 12. CRD migration

The provider enum is `gce;static;fake;mock`, with `mock` retained as a
compatibility alias. The operator approved this schema change on 2026-08-13;
the generated CRD is updated by `make generate` in the same change set.

## 13. Invariants

- A configured real provider never falls back to `static` or `fake`.
- All managed providers traverse the same Machine state machine.
- Only `static` may bypass external-instance lifecycle operations.
- Provider IDs remain opaque outside provider code.
- Native provider status strings never reach the controller.
- A fake behavior is deterministic and explicitly configured by the test.
- Runnable-agent testing never schedules workloads onto synthetic Nodes.
- Existing `provider=mock` installations remain functional during migration.
