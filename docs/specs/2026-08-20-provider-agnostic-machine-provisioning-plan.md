# Provider-agnostic Machine provisioning — implementation plan

**Status:** Draft plan
**Date:** 2026-08-20
**Design:**
[`docs/design/2026-08-20-provider-agnostic-machine-provisioning-design.md`](../design/2026-08-20-provider-agnostic-machine-provisioning-design.md)

## Implementation progress

- [x] Provider-neutral intent, observation, capabilities, profiles, and opaque
  reference types added alongside the legacy interface.
- [x] Legacy instance-observation compatibility mapping covered by table tests.
- [x] Fake provider implements declarative lifecycle and provider-owned
  interruption replacement.
- [x] Static/mock implements logical suspend and unregister-only semantics.
- [x] Machine reconciler bridge routes matching declarative providers through
  Online, Offline, and Deleted intent while preserving legacy direct-GCE and
  test-double behavior.
- [x] Direct GCE provider migration with bounded operations, stable opaque
  references, legacy numeric-ID adoption, and provider-owned spot replacement.
- [x] Neutral additive CRD/API fields, generated schema, legacy conflict
  validation, dual-read/write compatibility, and PWA contract types.
- [x] Provider capability/profile wiring, portable preflight, and opaque
  existing-capacity candidate discovery. Installer-curated catalog validation
  remains the transitional source for direct-GCE profile capacity.
- [x] Observation-only GKE provider, node-pool selector integration, chart
  configuration, and read-only live verification against the target `agents`
  pool.
- [x] Managed GKE ownership-gated create, scale-to-zero, restore, and delete
  lifecycle, including guarded live verification with an isolated disposable
  pool and independent cleanup verification.
- [ ] Target-cluster Machine migration to the GKE provider.
- [x] Unified PWA provider/profile flow and opinionated GKE Helm installation
  preset.

Managed GKE progress: installer-curated neutral-to-GKE profile mappings and the
ownership-gated create/resize/delete reconciler are implemented. Installation
and maintenance guides record the target topology, IAM boundary, ownership
rules, acceptance test, and recovery process. The adapter lifecycle test passed
against the target cluster on 2026-08-20. Installed-control-plane Agent drain,
rescheduling, and PVC continuity acceptance remains required before production
enablement.

## 1. Current baseline

The repository already has several prerequisites:

- provider registration and fail-closed construction in
  `pkg/adapters/compute_registry.go`;
- GCE, static/mock, and fake adapters;
- provider-owned translation to `InstanceObservation`;
- neutral REST aliases (`profile`, `location`, `interruptible`);
- neutral managed capabilities in `/api/v1/config`;
- fake-provider lifecycle and GCE REST emulator verification;
- `kyber.io/machine` node association;
- Machine and Agent preemption phases.

The remaining coupling is concentrated in:

- `pkg/adapters/compute.go`: VM verbs and instance-shaped observations;
- `pkg/controllers/machine/state_machine.go`: `CreateInstance` and
  Kyber-owned replacement actions;
- `pkg/controllers/machine/reconciler.go`: instance IDs and explicit
  start/stop/create/delete calls;
- `pkg/api/v1/machine_types.go`: GCE-shaped spec/status fields;
- `pkg/api/routes_config.go` and `routes_machines.go`: provider-name branches
  and a GCE catalog behind neutral aliases;
- `packages/pwa-views/src/pages/CreateMachine.tsx`: provider-name form switch;
- Helm values and installation docs: one provider configuration shape and a
  GCP guide focused on the self-managed k3s topology.

## 2. Delivery rules

- Keep each PR a coherent vertical or compatibility slice; do not combine the
  provider-contract migration, GKE mutation authority, and PWA redesign into
  one change.
- Every intermediate release must preserve local/static and direct-GCE
  behavior.
- Additive wire/CRD changes precede removal of old fields.
- Provider contract tests are shared; provider-native translation has focused
  adapter tests.
- Controller changes require envtest coverage.
- Every API shape change updates Go, OpenAPI, and hand-written PWA types.
- Every `pwa-views` source change includes its package version and changelog.
- CRD schema changes, RBAC changes, and any new dependency require explicit
  approval before implementation.
- Generated CRDs are updated only via `make generate`.

## 3. Phase 0 — decisions and characterization

### Deliverables

- Resolve the five open decisions in the design.
- Add characterization tests for current static, fake, and GCE lifecycle
  behavior before changing the interface.
- Record the current API compatibility matrix for legacy and neutral request
  fields.
- Define the portable provider contract and state vocabulary in a focused ADR
  or an accepted revision of the design.

### Key tests

- Static first-node fallback and additional labelled-node selection.
- GCE create, stop, start, delete, preemption replacement, and restart
  observation.
- Fake provider running-agent smoke.
- Machine finalizer behavior for managed versus external capacity.

### Exit criteria

- Existing behavior is locked by tests.
- The CRD and RBAC proposals are approved.
- The migration does not require a flag-day update across API, controller, and
  PWA.

## 4. Phase 1 — provider-neutral read model

Introduce additive domain types without changing provider behavior:

- `DesiredAvailability`;
- `AvailabilityState` and `AvailabilityReason`;
- opaque `ProviderRef`;
- lifecycle-mode `Capabilities` and `Profile`;
- provider-neutral `Observation` with a node selector.

Add a compatibility adapter that wraps the existing `ComputeAdapter` and maps
its VM operations/observations into the new vocabulary. The Machine controller
continues using existing actions in this phase.

### Exit criteria

- All existing adapters pass a shared observation/capabilities contract suite.
- No native provider string crosses the adapter boundary.
- No API or CRD breaking change.

## 5. Phase 2 — declarative provider reconciliation

Add the new consumer-owned `CapacityProvider` interface and port providers in
this order:

1. fake, because it provides deterministic lifecycle control;
2. static, to prove external-capacity semantics.

Also introduce a provider-neutral node-bootstrap input. Providers may deliver
that input through different native mechanisms, but credentials never become
part of the opaque provider reference or an observation.

Refactor the Machine reconciler to request `Online`, `Offline`, or `Deleted`
and act on provider-neutral observations. Remove replacement creation from
Kyber core; the provider reconciles Online after interruption.

Keep the pure Machine state machine. Rename instance-shaped events/actions to
capacity-shaped terms and update the authoritative transition documentation.

### High-risk points

- Preemption ordering with simultaneous node loss.
- Finalizer behavior during adapter errors.
- Preserving provider references across control-plane restarts.
- Ensuring static capacity never receives destructive intent.
- Agent `WaitingForMachine` behavior while a provider reports Recovering.

### Exit criteria

- Static and fake traverse the same reconciler contract.
- Fake local smoke still runs real Agents.
- Machine controller has envtest coverage for all desired availabilities.

## 6. Phase 3 — neutral CRD and API fields

Add neutral Machine fields while retaining legacy fields for reads and old
clients:

```text
spec.profile
spec.availabilityClass
spec.location
spec.managementMode
status.providerRef
status.availability
status.resolvedProfile
```

Admission resolves old and new fields into one internal request, rejects
conflicting values, and snapshots enough resolved profile data that an
installer mapping change cannot silently mutate an existing Machine.

Deprecate, but do not yet remove:

```text
spec.machineType
spec.spot
spec.zone
status.instanceId
```

This phase precedes direct-GCE migration. GCE operations are asynchronous and
the current adapter blocks through operation completion before it can persist
the numeric instance ID. A bounded declarative implementation must first be
able to persist an opaque provider reference (including operation identity)
between reconcile calls; implementing GCE earlier would preserve the blocking
behavior under a misleading interface.

After the additive fields and dual-read/write compatibility are present, port
GCE to `CapacityProvider` to prove managed individual-resource semantics.
Direct-GCE preemption must still replace capacity exactly once.

### Required updates

- Go CRD types and `make generate` output;
- REST request/response types;
- `test/contract/openapi.yaml`;
- PWA hand-written types;
- Machine table/detail compatibility rendering;
- architecture and migration documentation.

### Exit criteria

- Old Machine CRs reconcile unchanged.
- Old REST requests remain accepted.
- New responses use the neutral fields as authoritative.
- Generated-manifest diff is reviewed explicitly.

## 7. Phase 4 — provider-driven profiles and capabilities

Move the catalog source from `routes_config.go` into the provider contract.
Remove provider-name switches from API form selection and admission.

Add provider-neutral preflight:

```text
POST /api/v1/machines/preflight
```

The response reports resolved profile, location, lifecycle capabilities,
portable checks, and warnings. Provider-native remediation may appear only as
human-readable detail.

### Exit criteria

- The PWA does not branch on `gce`, `fake`, `static`, or future provider names.
- Static/local, fake, and GCE creation/registration are driven by capabilities.
- The server marks one recommended profile explicitly rather than relying on
  catalog order.

## 8. Phase 5 — existing-capacity discovery

Add:

```text
GET /api/v1/machine-candidates
```

Candidates use opaque IDs and portable summaries. Implement:

- static Ready-node candidates;
- GKE node-pool candidates after the GKE adapter exists;
- duplicate/claimed candidate prevention;
- protected-capacity filtering;
- `External` registration with no delete authority.

For local single-node presets, use this same machinery to auto-register the
first Machine instead of maintaining a separate product path.

### Exit criteria

- A local install starts with usable Machine capacity without operator cloud
  knowledge.
- Existing Kubernetes capacity can be registered without exposing native
  resource kinds.
- External resources cannot be deleted through Machine finalization.

## 9. Phase 6 — GKE provider, observation first

Implement a `gke` provider against the Google Kubernetes Engine API. Prefer the
already-present `google.golang.org/api` dependency unless an additional client
library is approved and materially improves correctness.

Initial scope:

- GKE Standard only;
- one configured cluster;
- one location per Machine;
- pool candidates and External observation;
- node resolution by `kyber.io/machine`, with the built-in GKE node-pool label
  as a compatibility discovery aid;
- protected pool names;
- operation polling;
- Workload Identity/ADC authentication;
- no create, resize, or delete authority yet.

### Target-cluster milestone

Discover `kyber-datawire`'s `agents` pool, register/migrate its Machine as
GKE-backed External capacity, and prove that the observed node, capacity,
Agents, and PVC behavior match the current static Machine. Do not alter the
pool during this milestone.

### Exit criteria

- The current target pool is observable without `mock` semantics.
- The `platform` pool is absent from candidates and cannot be adopted.
- No cloud resource mutation is possible in this phase.

## 10. Phase 7 — GKE managed lifecycle

Add GKE reconciliation for:

- Online: create or resize a Kyber-owned size-one node pool;
- Offline: drain through the existing lifecycle and resize to zero;
- Deleted: delete only a Kyber-owned pool;
- Recovering: observe GKE node repair or Spot replacement without creating a
  second pool.

Stamp ownership labels and `kyber.io/machine=<name>`. Keep autoscaling disabled
and auto-repair enabled. Validate that the selected location is compatible
with persistent-volume topology.

### Real-cluster verification

1. Preflight a disposable reliable Machine.
2. Create its node pool and wait for Ready.
3. Run a disposable Agent with a persistent volume.
4. Stop/start the Machine and verify volume reattachment.
5. Delete the Machine and verify the Agent PVC is retained.
6. Repeat with cost-optimized/Spot capacity.
7. Exercise provider repair/replacement and confirm only one backing pool
   exists.
8. Only then consider managed adoption of an existing pool.

### Exit criteria

- All cloud mutations have ownership and protected-resource guards.
- Stop/start preserves durable Agent state.
- Spot replacement converges without a duplicate resource.
- Real-cluster results are recorded in the design or operator verification doc.

## 11. Phase 8 — unified Machine UX

Replace raw provider forms with a capability-driven flow:

- name;
- profile cards;
- availability class;
- location only when selectable;
- existing-capacity candidate when provisioning is unavailable or explicitly
  requested;
- preflight review;
- operation progress.

Machine detail shows profile, assignable/available capacity, hosted Agents,
availability class, location, management mode, and supported actions. Provider
diagnostics are secondary.

### Exit criteria

- Local and cloud screenshots share the same information architecture.
- Unsupported actions are absent or clearly disabled based on capabilities.
- No primary UI string says `mock`.
- PWA package publish requirements are satisfied.

## 12. Phase 9 — installation presets and preflight

Define values overlays and focused guides for:

- `local`;
- `existing-kubernetes`;
- `gce-self-managed`;
- `gke-standard`.

Each preset configures provider, platform placement, storage mode, profile
mappings, and safe lifecycle defaults. Add installation preflight with portable
check groups and provider-specific implementations.

### Documentation restructuring

- Keep quickstart focused on local evaluation.
- Split the current GCP installation guide into self-managed GCE/k3s and GKE
  Standard paths.
- Add a target-environment decision page.
- Document expected topology, storage guarantees, minimum permissions,
  verification, and uninstall behavior for each preset.

### Exit criteria

- A new installer can select an environment without understanding controller
  internals.
- Every preset has a render test and a smoke/preflight path.
- Local installation remains cloud-credential-free.
- New chart defaults are checked against `--reuse-values` upgrade behavior.

## 13. Phase 10 — compatibility removal

Only after telemetry, release notes, and deployed-cluster inspection show that
old clients and CRs have migrated:

- remove `mock` from new configuration examples, retaining read compatibility
  for an agreed window;
- remove GCE-specific API aliases;
- remove `InstanceId` and VM-shaped CRD fields in a separately approved schema
  migration;
- rename remaining instance-oriented internal symbols;
- update `AGENTS.md`, architecture, product, and reviewing guidance with the
  final contracts and new gotchas.

## 14. Suggested PR sequence

1. Contract types plus compatibility adapter and tests.
2. Fake provider on declarative reconciliation.
3. Static provider plus local compatibility tests.
4. GCE provider plus preemption regression tests.
5. Machine reconciler/state-machine migration.
6. Neutral additive CRD/API fields.
7. Provider-driven capabilities/profiles and preflight.
8. Candidate discovery and local auto-registration.
9. GKE observation-only provider.
10. Target-cluster External migration verification.
11. GKE managed lifecycle.
12. Capability-driven PWA flow.
13. Installation presets, preflight, and guide split.
14. Later compatibility cleanup.

PR boundaries may move after Phase 0 characterization, but provider migration
and real-cloud mutation must remain independently reviewable.

## 15. Global verification matrix

| Surface | Required verification |
|---|---|
| Go baseline | `make build`, `make lint`, `make test` |
| CRD | `make generate`, reviewed generated diff, envtest |
| API | unit tests, OpenAPI contract, hand-checked TS parity |
| PWA | both workspace builds, lints, and tests; browser verification |
| Chart/presets | `make helm-lint`, `make helm-template`, preset render tests |
| Local static | single-node auto-registration and Agent smoke |
| Local managed | fake-provider full lifecycle smoke |
| Direct GCE | emulator contract and preemption smoke |
| GKE observe | candidate/adoption read-only target-cluster smoke |
| GKE managed | disposable pool create, stop/start, replace, delete, PVC proof |

## 16. Immediate next work

1. Review and decide the five design questions.
2. Inventory every `ComputeAdapter`, `InstanceObservation`, `InstanceId`, and
   VM-shaped state-machine dependency.
3. Draft the exact `CapacityProvider` Go interface and compatibility mapping.
4. Add characterization tests before production refactoring.
5. Request approval for the eventual CRD/RBAC changes and any dependency
   decision before beginning those slices.
