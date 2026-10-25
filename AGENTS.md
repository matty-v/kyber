# AGENTS.md — Kyber (self-hosted AI-agent fleet platform)

AI-reader orientation file. Dense and explicit by design. Deeper truth lives in
`docs/architecture/` (HOW), `docs/product/` (WHAT), `docs/contributing/code-quality.md`, and
`docs/contributing/reviewing.md` — read the relevant one BEFORE touching a subsystem. When this
file and code/CI disagree, code/CI wins.

This is the canonical repository instruction file. `CLAUDE.md` is only a
compatibility pointer back here; never duplicate guidance in it. Every PR that
changes architecture, repository layout, conventions, generated/do-not-touch
paths, verification commands, or a documented gotcha must update `AGENTS.md` in
the same change. Review concrete claims against current code and CI before
editing them.

Kyber runs long-lived Claude Code and Codex agents as Kubernetes pods with
whole-disk persistence, declared via CRDs and reconciled by a Go control plane.
Installed from a versioned Helm chart. Standalone installations update through
Kyber’s supervised Helm workflow; GitOps installations use their own deployment
repository and controller. Never let both mechanisms own the same release.

---

## 1. Architecture — the 5 core abstractions

### 1.1 CRDs are the source of truth
Two CRDs in group `kyber.io/v1` (Go types: `pkg/api/v1/agent_types.go`,
`pkg/api/v1/machine_types.go`; generated manifests: `deploy/helm/kyber/crds/`):
- **`Agent`** — one AI agent instance: target machine, runtime type, resources,
  identity repo, secrets, model, `spec.desiredPhase` (operator intent).
- **`Machine`** — one cloud VM: provider, machine type, disk, spot, zone.

WHY: operators (PWA, humans, other agents) declare intent on `spec`;
controllers reconcile reality toward it. Nothing mutates cluster state
imperatively except controllers executing reconcile actions. Regenerate CRD
manifests with `make generate` — NEVER hand-edit `deploy/helm/kyber/crds/`.

`Agent.status.observedPodConfigHash` records the restart-relevant spec last
built into a pod. The reconciler excludes hot API/controller fields from that
fingerprint and records `jobsProjectedGeneration` only after the per-agent jobs
ConfigMap revision appears in the running pod's mount. Runtime capability
availability uses both markers; API `dirty` uses only the pod fingerprint and
sidecar condition so ConfigMap propagation does not imply a restart.
Existing pods at a matching observed generation seed both baselines once;
later jobs edits require an in-pod revision probe.
`observedGeneration` remains the Failed restart claim and legacy fallback.
The agent pod also receives `KYBER_USAGE_ROUTE_FINGERPRINT`, derived from its
runtime, authentication mode, and configured inference endpoint. Usage
reporters use this non-secret stamp to detect route changes across pod
restarts; the control plane independently derives the same trusted revision.
The internal `/usage-event` and `/usage-gap` sidecar paths forward to
`/internal/agents/{name}/...`, where same-Agent pod authentication, current
Agent UID and route revision checks precede one Postgres transaction for the
evidence and source cursor. A successful ACK means durable ingest; without
Postgres these paths return 503. The Claude reporter scans complete finalized
JSONL lines every five seconds alongside its existing context snapshot loop.
The Agent UID check uses the uncached Kubernetes API reader, so an informer
delay after Agent name reuse cannot attribute an old pod's event to the new UID.
The Hermes reporter applies the same durable ACK rule to complete per-call log
lines, interpreting its timezone-less timestamps with the pod's configured
`TZ`. Rotation, truncation and route changes create explicit coverage gaps.
The Codex reporter derives accepted call deltas from monotonic rollout totals,
so repeated `token_count` observations are never billed twice. It prefers an
observed `turn_context` model, tracks the configured model as a fallback, and
records a coverage gap on counter reset or an unidentifiable model.
The public `GET /api/v1/usage/events` and `/api/v1/usage/aggregates` endpoints
require `usage:read` plus current Agent resource access. Event cursors pin
committed ingestion sequence horizons; daily aggregate cursors page frozen
one-hour PostgreSQL read snapshots. Repeat the same filters on every page.
Coverage reports detected gaps but never promises every upstream call was
captured. Agent deletion invalidates all usage read snapshots.

An existing Agent changes harness through the lifecycle-write
`POST /api/v1/agents/{name}/switch-runtime` action, not a bare `spec.runtime`
edit. It checks the target image, authentication and channels, prepares the
target on the same PVC when the adapter has a repair contract, clears the old
model/version overrides, and requests NeedsAuth. See
`docs/design/2026-09-22-agent-runtime-switch.md`.

An agent's disk is exported as a portable ZIP through async jobs at
`POST /api/v1/agents/{name}/exports` (`archives:admin` plus the agent in the
caller's `agentResources`). The job row (Postgres `agent_archive_jobs`,
`pkg/archivejob`) is the only job state; a leader-gated worker
(`pkg/api/archives.go`) drives it idempotently. The export stops the agent
through `desiredPhase` and sets the `kyber.io/archive-hold` annotation; while
that annotation is present the reconciler never builds an agent pod. A
same-node pod (control-plane image, `kyber-disk-archive`, read-only PVC, no
service-account token, per-job token) streams the ZIP to
`/internal/archive-jobs/{id}/upload`; the control plane seals it (per-archive
AES-GCM key wrapped under the internal signing key) into `pkg/archivestore`:
the chart's built-in `kyber-archive-store` by default on every target, or
S3/GCS. The archive format and its validation are `pkg/diskarchive`; never
extract an archive except through `diskarchive.Extract`. Downloads use
short-lived path-token links under `/api/v1/archive-downloads/`, redacted from
request logs. Verification runs in an unprivileged pod that reads the archive
back through `/internal/archive-jobs/{id}/archive` and posts a summary — never
parse a whole archive in the control plane. Release restores the agent's prior
intent only while the `kyber.io/archive-paused` mark stands; lifecycle verbs
clear it. Create-from-archive (`POST /api/v1/agent-imports`, uploads at
`/api/v1/archive-uploads`) runs the ordinary `createAgent` handler with the
hold set from the request context, pre-creates the PVC, and releases the hold
only after the restore pod has re-scanned the volume against the manifest. See `docs/design/2026-09-22-agent-disk-export-import.md`.

Agent disk increases use `POST /api/v1/agents/{name}/increase-disk` after
`GET /api/v1/agents/{name}/disk-options` checks the bound PVC, PV, CSI driver,
StorageClass, and quota. Generic Agent PATCH must reject disk changes. The
controller in `pkg/controllers/agent/disk_expansion.go` grows the existing PVC
only after checking its UID and bound PV UID; never replace or delete either
volume to change size. It waits for provisioned and mounted filesystem
capacity, then intentionally restarts a Running Agent once so its disk limit
and status match. Stopped or failed Agents use a short-lived, unprivileged
capacity helper pod with the existing PVC and no Agent credentials. Physical
shrinking is unsupported; a blocked, unfulfilled PVC request may be reduced
only while the new target exceeds already allocated capacity. See
`docs/operator/agent-disk-expansion.md`.

### 1.2 Pure state machine + thin reconciler (the agent lifecycle)
- `pkg/controllers/agent/state_machine.go` — pure function
  `(phase, event) → (action, nextPhase)`. Zero k8s imports. Unit-testable.
- `pkg/controllers/agent/reconciler.go` — watches pods/CRDs, classifies what it
  sees into an `Event` (`classifyEvent`), calls the state machine, executes the
  returned `Action` (create pod, delete pod, update status, …).
- Deletion is OUT-OF-BAND: `handleDeletion()` in the reconciler, driven by
  `DeletionTimestamp` — it does not go through the state machine.

WHY: lifecycle logic (14 phases: Creating, Starting, Running,
NeedsAuth, MemoryExhausted, Failed, Draining, WaitingForMachine, …) is the
highest-blast-radius code in the repo; the pure split makes every transition
testable without a cluster. Authoritative transition table:
`docs/architecture/agent-lifecycle.md`. Notable design points:
- OOM gets its own phase (`MemoryExhausted`) so undersized agents don't
  crash-loop; operator bumps memory first.
- Spot-preemption parking is `Draining` → `WaitingForMachine`; a replacement
  machine resumes the agent automatically.
- A `Failed` Agent's Pending pod pinned to a replaced node is removed only
  when its required hostname differs from the Machine's current ready node;
  unrelated Machine outages do not revive exhausted crash retries.
- `pkg/controllers/machine/` mirrors the same pattern for VMs. Compute
  providers sit behind `pkg/adapters/compute.go`: GCE is the production
  adapter, fake exercises managed lifecycle locally, and static attaches an
  existing node (`mock` remains a compatibility alias).
  Managed-provider UI/API discovery uses neutral capabilities (`profiles`,
  `locations`, disk choices, interruptible support); GCE status strings,
  metadata, networking, and Google API fields stay in `compute_gce.go`.
  `--compute-provider gce-emulator` runs that real adapter against the local
  REST subset in `pkg/gceemulator`; `scripts/devenv/compute-scenario.sh`
  controls neutral scenarios in explicitly enabled devenv modes only. That
  profile alone enables `KYBER_ALLOW_SIMULATED_NODES`, allowing Nodes labelled
  `kyber.io/simulated=true` to stand in for kubelet heartbeats.
  Compute adapters expose Kyber-owned `InstanceObservation` state rather than
  provider-native status strings, and are constructed through the registry in
  `pkg/adapters/compute_registry.go`. Explicit provider initialization fails
  closed — never fall back from a broken real provider to mock behavior. The
  A declarative direct-GCE `CapacityProvider` implementation exists for
  migration testing, but the Machine reconciler deliberately keeps production
  GCE Machines on the legacy action-oriented path. That path preserves the
  distinction between operator stop/start and Spot preemption plus the stale
  k3s Node/password cleanup required before replacement. Do not route GCE
  through `CapacityProvider` until observation is non-mutating and those
  invariants have equivalent regression coverage.
  Operator discovery is capability-driven through `/api/v1/config`,
  `POST /api/v1/machines/preflight`, and `GET /api/v1/machine-candidates`;
  candidate IDs are opaque, and platform, unready, labelled, or already
  claimed Nodes are never offered as external capacity.
  `provider=gke` supports External observation: one Machine maps by name (or
  opaque `gke://<project>/<location>/<cluster>/nodePools/<pool>` reference) to
  one installer-managed node pool. It selects Nodes through
  `cloud.google.com/gke-nodepool`, reports pool health, does not resize or
  delete cloud resources, rejects Offline, and treats Machine deletion as
  unregister-only. Configure it with `compute.gke.{project,location,cluster}`.
  Installer-curated `compute.gke.profiles` enable explicit Managed Machines.
  Managed pools are created at size one with Kyber ownership labels, resized
  to zero for Offline, and deleted only when both ownership labels match; an
  unowned pool is a hard failure, never an adoption opportunity.
  `provider=eks` is managed-only: installer-curated profiles map one logical
  Machine to zonal EKS managed node-group capacity, and `allowedZones` is the
  exact operator-facing location catalog. Profile instance alternatives must
  have an equivalent CPU/memory shape. Disk capacity is installer-owned and,
  when configured, fixed by the profile's encrypted launch template; the PWA
  exposes that approved size and sends it with the Machine request.
  Regional GKE installations separate the cluster resource location from
  `compute.gke.nodeLocations`. Two or more eligible zones use GKE total-count
  autoscaling (`totalMinNodeCount=totalMaxNodeCount=1`, location policy `ANY`)
  so one logical Machine still has at most one Node while GKE selects available
  capacity across the regional-PD-compatible zones. The chart fails rendering
  when a managed node location is absent from `storage.gcePD.allowedZones`.
  provider-neutral capacity migration is additive: `pkg/adapters/compute.go`
  also defines declarative `CapacityProvider` intent/observation types. `fake`
  and `static`/`mock` implement both contracts. The Machine reconciler routes a
  matching declarative provider through `CapacityProvider` but retains the
  legacy path for direct GCE and test doubles; do not expose provider resource
  kinds through the new contract or remove the compatibility interface before
  all providers migrate. The target design
  is `docs/design/2026-08-20-provider-agnostic-machine-provisioning-design.md`.
  The `provider=static` means existing-node/standalone attachment;
  `provider=fake` uses a deterministic in-memory instance but traverses the
  normal managed Machine state machine and finalizer against one local Ready
  node. `provider=mock` remains a compatibility alias for `static`. Verify the
  static provider supports multiple existing nodes: the first Machine may use
  the single-Ready-node fallback, while each additional Machine requires a
  distinct Ready node labelled `kyber.io/machine=<machine-name>`. Verify the
  fake lifecycle locally with `scripts/devenv/up.sh --compute-provider fake`
  followed by `scripts/devenv/smoke-fake-provider.sh`. The contract is in
  `docs/design/2026-08-13-compute-provider-boundary-design.md`.
  The production Agent reconciler must be wired with a
  `KubernetesMachineGetter`: active agents consume the provider-neutral
  Machine readiness contract, park in `WaitingForMachine`, and remove their
  stale pod while capacity recovers. Pods on healthy Nodes retain termination
  grace; only Pending pods or pods on missing/NotReady Nodes are force-deleted.
  They resume without spending their retry budget when the Machine becomes Ready. See
  `docs/design/2026-08-21-machine-capacity-recovery.md`. Scheduler-driven
  regional providers keep autoscaling demand alive with one Machine-owned,
  credential-free capacity-request Pod while active Agents are parked; Agent
  changes must continue to enqueue their assigned Machine immediately. The
  matching provider capability is exposed as
  `compute.managed.capabilities.requiresSchedulerDemand`; PWA status must use
  that capability rather than infer behavior from provider location strings.

### 1.3 Runtime registry (pluggable agent runtimes)
`pkg/runtimes/runtime.go` defines `Runtime { Type, Adapter, Probe }`. Each
runtime is a subpackage (`pkg/runtimes/claudecode/`, `pkg/runtimes/codex/`,
`pkg/runtimes/hermes/`) that self-registers via
`init()`; binaries enable runtimes by blank-importing the subpackage.
`Adapter` answers pod-spec questions (image, env, secret mounts, probes) and
provides the fixed runtime-repair package/binary/path contract used by the
same-node maintenance pod; request input never supplies repair commands or paths.
`Probe` is the sidecar-side hook (mostly reserved — see status-pipeline doc).
A new runtime requires the package/registration skeleton plus the harness
contract onboarding checklist; registration alone is not a complete integration.
See `runtime.go` for the file layout and the conformance guide for other wiring.

Codex subscription auth is performed in-pod with `codex login --device-auth`;
the PWA attaches read-only to tmux session `auth`. The exact `{}` payload in
`<agent>-codex-auth` is the device-login marker and pauses the normal Starting
timeout until the credential syncer replaces it. Codex API-key agents use
`<agent>-openai` / `OPENAI_API_KEY` and never enter the device flow.
Codex agents using `spec.inference` instead read an endpoint Secret. A
structured HTTP 401 at boot or on a live turn enters NeedsAuth; the live
report is scoped to the current pod UID and recovery waits for that endpoint
Secret to rotate. The pod stamps the Secret resource version and a newer
version rolls a Running Codex endpoint pod to refresh its environment. The
controller polls these pods every 30 seconds because operator-supplied Secrets
have no Agent owner watch; a reported working turn defers the rotation.
Free-form errors and 403 are not credential proof.
Codex's direct OpenRouter route has no built-in authenticated model catalog.
The PWA accepts an explicit author/model ID on create and model change;
`RuntimeDescriptor.routeModelEvidence` carries bounded, versioned dev
observations for known IDs. Unlisted IDs remain unverified, and the route
does not reveal the selected upstream provider. Never treat a runtime
capability signal as model/provider task-contract qualification.
The MAT-115 conformance evaluator in `scripts/devenv/` checks redacted live
task evidence for exact IDs, completion markers, and cancellation schema;
its fixture tests run in the required `test.yml` job.
Codex readiness enters PID 1's chroot from a kubelet exec probe; it must use
the absolute `/home/kyber/.codex/auth.json` path because probe processes do not
inherit the start script's `CODEX_HOME` export. Local Vite development must
proxy WebSocket upgrades on `/api` as well as HTTP requests and rewrite both
forwarded Origins to the backend origin, or auth and Shell terminals close.
Authenticated model catalogs are stored per agent, separately from the public
npm-backed harness snapshot. Claude catalog entries require Anthropic's
authoritative context window; Codex `model/list` lacks that field, so Codex
picker entries remain explicitly unknown and the active rollout's
`token_count.context_window` is authoritative. Never add a guessed fallback.

Discord-enabled agents register the sidecar's loopback Streamable HTTP MCP
endpoint (`kyber-discord`, port 14007) in both runtimes. Its `reply` tool is the
primary outbound path; port 14005 `/send` remains a compatibility fallback.
The runtime never receives the Discord bot token.
The sidecar owns Discord working-state UX: 👀 plus refreshed typing after
dispatch, then ✅ on reply or ❌ on inbound delivery failure. Indicator errors
never block the message path.
Discord outbound text is split against the 2,000 UTF-16-unit limit; code fences
are balanced per chunk, only the first chunk carries the reply reference, and
MCP reports all created IDs plus partial-delivery failures.
Discord attachment capabilities are bounded like Telegram's: only the newest
256 accepted inbound attachment IDs may be downloaded, CDN hosts are pinned,
files land under `/persist/discord-attachments`, uploads cannot escape
`/persist`, and each direction has a 10 MiB per-file limit.
Discord MCP `edit_message` and `react` are channel-scoped; edits keep the
single-message limit and reactions can add or remove only the bot's own emoji.
Allowlisted parent channels authorize their observed Discord threads; inbound
envelopes preserve the thread reply target and include parent/thread metadata.
They also include referenced-message metadata and at most five preceding
messages, each capped at 500 UTF-16 units.
Discord Comms writes stamp `kyber.io/discord-config-revision`; agent pods copy
it, and the reconciler rolls a mismatched pod only when the runtime is Idle,
under the shared one-delete concurrency budget.
Durable Discord delivery is a non-goal: the Gateway sidecar serves running
agent pods only. Users who need acceptance or durable queuing while a pod is
absent must provide an external relay into Kyber's signed inbound webhook.

### 1.4 A2A conformance contract
The A2A 1.0 HTTP+JSON claim is generated from `conformance/a2a/1.0/` and
validated by `scripts/a2a_conformance.py`; never hand-edit the generated
support matrix. The complete normative inventory comes from the exact pinned
official TCK commit. Applicable and deferred-optional rows require exact test
references, while non-applicable rows require reviewed reasons. Adapter-path
changes must run the pinned official TCK through the loopback Bearer injection
shim in `test/a2aconformance`; do not patch upstream TCK source or weaken a
failing MUST into a skip. Evidence bundles are immutable directories with an
exact SHA256SUMS file and must use the self-tested—not certified—claim.

Outbound A2A is exposed to both runtimes as the status sidecar's distinct
loopback `kyber-a2a` MCP endpoint (`/a2a/mcp`). Agents select only names from
`Agent.spec.a2aPeers`; HTTPS endpoints and Kubernetes Secret-backed bearer
credentials are projected into the sidecar and never the runtime container.
Private address space requires an explicit per-peer opt-in, while loopback,
link-local, metadata, userinfo URLs, and redirects remain prohibited. Keep
tool schemas closed and bounded; never replace this surface with generic HTTP.

### 1.5 Inbound dispatch gauntlet (how prompts reach agents)
External senders POST signed envelopes to
`/webhooks/inbound/<agent>/<binding>`. The pipeline, in fixed order:
HMAC verify → dedup (body hash) → binding match + filters → per-binding rate
limit → per-agent bounded queue (depth 5) → hold until the Agent CR reports
Running (bounded wait; a message must not be answered by a terminating pod's
dying session mid-roll) → deliver into the agent pod's tmux.
- Pure primitives (no HTTP/k8s/subprocess): `pkg/inbound/` (`verifier.go`,
  `dedup.go`, `dispatcher.go`, `queue.go`, `ratelimit.go`, `envelopecache.go`).
- HTTP wiring + outcome recording on `Agent.status.inboundRuns[]`:
  `pkg/api/routes_inbound*.go`.

The same per-agent queue also carries authenticated internal request/reply
jobs. Those jobs are explicitly typed, carry an immutable delivery deadline,
and report `dispatched`, `agent_unavailable`, or `delivery_failed` through an
in-memory outcome callback. They do not loop through the public webhook route,
binding matcher, HMAC verifier, or deduper. Unlike legacy webhook jobs, they do
not attempt delivery after the Running-phase wait reaches their expiry.

Current one-line agent goals live in `Agent.status.goal`. Managed
`UserPromptSubmit` hooks signal prompt acceptance without forwarding prompt
text; the trusted status sidecar owns the revision timestamp and writes the
privacy-safe fallback through the internal API. Agents refine only their own
current revision through `get_goal` / `set_goal` on the existing
`kyber-request-reply` MCP endpoint. Keep this separate from activity state and
never derive a fleet-visible fallback by truncating raw prompt text. The design
is `docs/design/2026-09-16-agent-goals.md`.

Durable agent tasks are a separate PostgreSQL-backed contract in
`pkg/taskstore/` and `pkg/taskdispatch/`, exposed at
`/api/v1/agents/{agent}/tasks`. They are opt-in via
`api.durableTasks.enabled`, require PostgreSQL, and never fall back to memory
in production. Creation atomically stores the task plus dispatch intent;
workers claim with cross-replica leases and reuse the per-agent inbound queue.
The managed `UserPromptSubmit` hook must persist an exact task/attempt receipt
through the status sidecar before public state becomes `dispatched`; ambiguous
attempts fail as `delivery_unknown` and are never automatically replayed.
The same task contract accepts cooperative progress and immutable typed results
through the status sidecar's loopback `kyber-task` MCP tools. Text and canonical
JSON remain inline; files must originate below `/persist/task-results`, pass the
openat/no-follow safety checks, and stream into private GCS/S3-compatible object
storage. PostgreSQL is the visibility authority: uploads are recorded pending
before bytes leave the pod, downloads reauthorize every request, and leased
cleanup retries provider deletion before removing metadata. See
`docs/architecture/durable-tasks.md` before changing this cross-component path.
Every accepted public task mutation also appends a normalized per-task event in
that same PostgreSQL transaction. Task Get exposes the signed high-water cursor;
the `task-events:read` SSE route replays and polls PostgreSQL by monotonic
sequence, never a harness transcript or process-local event bus. Preserve cursor
principal binding, replay-to-live catch-up, terminal close, and periodic auth
checks when changing task mutations or middleware streaming interfaces.

Operator-curated discovery lives in `pkg/capabilities/`: the declaration is
`Agent.spec.publicCapabilities`, bounded reconciliation output is
`Agent.status.publicCapabilities`, and the privacy-safe authenticated
projection is `/api/v1/agents/{agent}/capabilities`. Skill reports are private
evidence only and may narrow availability; they must never create or broaden a
declaration. The public response is an explicit allowlist and must never gain
evidence IDs, paths, prompts, tool schemas, runtime/model, pod/node, or internal
endpoint fields. A skill report touches the agent's private evidence-refresh
annotation so the controller re-evaluates drift, and the controller schedules
the staleness boundary so old evidence cannot remain healthy indefinitely.

WHY the trust boundary matters: `verifier.go` does constant-time compare and
collapses ALL failures to a single `ErrSignatureMismatch` — never differentiate
causes externally. The queue is bounded so a flooded agent sheds load
(`queue-full`) instead of buffering unboundedly.

### 1.5 Agent pod anatomy + whole-disk persistence
Each `Agent` gets one pod containing: the runtime container (`Containers[0]`,
always), `kyber-status-sidecar`, `transcript-tailer`, `transcript-pruner`, and
a session-brief boot container. The sidecar/tailer/pruner are appended AFTER
`BuildPodSpec` (`pod_builder.go` → `status_sidecar.go`, `transcript_tailer.go`,
`transcript_pruner.go`) so runtime stays index 0.
The status sidecar also owns the loopback-only `kyber-request-reply` MCP
surface on its existing `127.0.0.1:8091` listener. Both runtimes register the
same `respond(request_id, response)` tool; the sidecar adds the pod token and
agent identity when forwarding to the internal API, so the runtime receives no
control-plane credential or callback destination.
The status sidecar mounts the persist PVC read-only for disk telemetry. Disk
total is always the requested Agent allocation. Used bytes come from `statfs`
only when mountinfo says `/persist` is a whole-filesystem mount; bind-mounted
directories use a throttled asynchronous tree walk and cached result, with the
method/state/sample time exposed in Agent status. Permission-denied paths are
skipped and logged as a `partial` sample; partial used bytes remain a useful
lower bound and participates in the same reserve hysteresis as a complete
sample. The sidecar combines that topology with `statfs` backing-store capacity:
a whole-filesystem mount is
reported as enforced only when its size is within tolerance of the allocation;
directory-backed storage such as k3s local-path is explicitly a soft
reservation and reports shared backing total/available bytes.
The control plane combines that disk sample with the metrics-server's agent-container CPU/memory sample
and requested Agent limits; the sidecar does not enter the runtime container's
chroot or inspect agent file contents.
Persistence: the agent's root filesystem is a real directory on its PVC
(`/persist/agentroot`), seeded from the base image by
`images/agent-base/scripts/kyber-rootfs` and entered with chroot. There is no
overlayfs and no `/dev/fuse` — neither works inside a user namespace, and the
pod runs in one by default (`hostUsers: false`), which is what makes its
`CAP_SYS_ADMIN` namespaced rather than host-valid. A new base image reaches an
existing durable root through a three-way merge that never overwrites a file
the agent has touched. The pod is de-privileged, gets no host devices or
hostPath volumes, disables ServiceAccount token automount, and default-denies
ingress plus infrastructure-range egress via NetworkPolicy. Break-glass
privilege, the seccomp fallback, the persistence-mode rollback, and the egress
settings are under `agent.security.*`.
See `docs/design/agent-pod-isolation.md` and
`docs/adr/0003-agent-sandbox-isolation.md`. This is why agents survive
restarts with full filesystem continuity — though on `local-path` volumes the
root does NOT survive node replacement. `kubectl exec` does NOT land inside the
agent's chroot — use nsenter.

Identity repos are optional (MAT-53). `Server.identityRepoCapability`
(`pkg/api/identity_repo_capability.go`) is the one source for both
`GET /api/v1/config` `identity.supportedModes` and create-time validation;
both GitHub modes need the App client and `identityRepo.defaultOwner`, and
the PWA must follow `supportedModes`, never infer from `repoOwner`. A
repo-less agent has `KYBER_IDENTITY_REPO` unset: no clone, credential helper,
or App token, and it launches in `$HOME`. A template-backed Agent gets no pod
until its repo exists: the reconciler holds it in `Creating` with the
`AwaitingIdentityRepo` condition, and `EventIdentityRepoReady` builds the
first pod once `.repo` is patched. `createPod` refuses a first pod while that
wait is on; don't bypass it.

Identity repos are dual-runtime. The template's canonical contract is
`AGENTS.md`; `CLAUDE.md` is only a Claude Code compatibility entrypoint. Shared
`skills/<name>/SKILL.md` packages must be linked into both
`~/.claude/skills/` and `~/.codex/skills/` by
`images/shared/kyber-identity-repo.sh`. Claude-only hooks belong in the
template's project-local `.claude/settings.json`; cross-runtime behavior
belongs in `AGENTS.md`, scripts, or skills.
Platform capability cookbooks live under `images/shared/skills/` and are
packaged into both runtime images. Startup scripts install
`discord-messaging` only when `KYBER_DISCORD_MCP_URL` is present; an
identity-repo skill with the same name wins, so operators can customize the
behavior without forking an image.

Harness-version discovery is public and npm-backed. Model discovery is
per-agent and begins only after authentication: Claude Code uses that agent's
Anthropic credential and Codex uses app-server `model/list`, then each reports
non-secret metadata through the status sidecar. `GET
/api/v1/agents/{name}/models` never substitutes another agent's catalog.
For an empty `spec.model`, the first transcript-backed token snapshot records
the concrete harness choice in `status.currentModel`; do not copy that value
into spec, because doing so would turn a dynamic default into a persistent pin.

Model ids are validated at write time (fleet-defaults PUT, set-model, create)
against the catalogs the cluster can see; `force: true` bypasses when a model
is newer than the last detection poll. The boot-time pre-flight probe reports
its RAW exit+output and `pkg/modelprobe` classifies it server-side — never
re-add classification heuristics to `start-claude.sh`, and never let an
inconclusive probe collapse to silence: it must surface as the
`ModelUnsupported` condition status `Unknown` (an invalid fleet-default model
once failed every agent turn while the platform showed green).

Control plane = ONE binary (`cmd/control-plane/main.go`) — a modular monolith:
REST API (`pkg/api`), both controllers, inbound, telemetry, background workers.
Other binaries: `cmd/node-agent` (DaemonSet, node metrics + machine actions),
`cmd/status-sidecar`, `cmd/token-reporter` (in-pod transcript tailer →
token-usage events), `cmd/transcript-compact`.

Backing stores: k8s API (resource state), Redis (events/token budgets/metrics),
Postgres (session briefs, skills, durable tasks). Briefs and telemetry have
in-memory fallbacks; production durable tasks require PostgreSQL and fail closed.

Operator security events use `pkg/securityaudit` and the existing PostgreSQL
connection, without an in-memory runtime fallback. Sensitive access requires a
durable admission before backend effects. Administrator-only
`GET /api/v1/security-events` exposes bounded, filter-bound snapshot pagination;
every history read is itself audited. This is partial sensitive-access history,
not complete authentication usage or credential lifecycle coverage. See
`docs/architecture/api-authorization.md` before extending collection or readers.
The shared Settings API-access view includes a private, connection-bound recent
access card. It distinguishes authorization from completion and reports partial
coverage. Inventory checks run every five seconds with a four-second abort;
failure or denied access unmounts protected history and management controls.
History itself loads on demand, so opening the page does not continuously
generate audited history reads. UI-owned requests abort on selection/unmount.
Routine history reads are hidden by default in the UI but still recorded;
denied/failed/unknown history attempts remain visible. The API defaults to
including all records, with `includeHistoryViews=false` as an optional filter.

`pkg/operatorcredentials` is a candidate registry persistence foundation, not an
active authentication source. Its public factory creates a direct Kubernetes
client with a static Secret mapper and bounded requests. Documents use opaque
resource-version CAS, immutable identity/provenance, separate material/policy
revisions, retained revocation tombstones and a bounded pending-operation outbox.
Material is private; public JSON/diagnostic formatting is blocked/redacted.
The inert `AuthorityStore` binds a separate immutable adoption marker to the
registry Secret UID. Direct reads reject missing/replaced adopted registries;
only a verified missing marker before adoption signals bootstrap. This does
not protect downgrades to old binaries: the upgrade compatibility floor remains
an activation prerequisite. Neither Secret belongs in Helm templates.
Read `docs/design/2026-10-23-operator-credential-store.md` before integration.
Runtime adoption, grant compilation, cookie/stream resolution and upgrade/
rollback compatibility gates must be complete before management activation.
The opt-in real-API test in this package uses an inert Secret only in the
verified empty GCP dev cluster; it must never target production credentials.
`pkg/usagestore` owns the new Postgres schema for local usage evidence and
detected gaps. Its event identity includes the immutable Agent UID; token
classes use null for unknown and zero for observed zero. It is not a source of
dollar costs. Accepted events update exact daily aggregates in the same
transaction; corrections subtract the superseded event before adding their
replacement. Detailed evidence defaults to 90 days, daily aggregates to 13
months. The public `usage:read` API exposes this evidence without dollar costs.
Usage route revisions live in `agent_usage_routes`: a random UUID is exposed,
while only an internal fingerprint of the configured endpoint is retained.
Ingest rejects a route ID belonging to another Agent UID. A configured route
or Hermes provider label never proves the upstream inference provider.
`pkg/tokenreport/claude_usage_collector.go` scans complete finalized Claude
records with a cursor saved on the Agent PVC. It skips pre-cutover transcript
history and records a gap when a route changes or a file truncates. Hermes
and Codex have their own local collectors and durable cursors.

---

## 2. Data flow

### REST request (operator/PWA → platform)
1. `pkg/api/server.go` `buildTopHandler()` → CORS, then Bearer API-key wall
   (`auth.go` `authMiddleware`, 401 on failure).
   The embedded PWA exchanges its bearer key once at
   `POST /api/v1/browser-session` for an HttpOnly, SameSite=Strict cookie; raw
   keys are removed from legacy localStorage. The cookie is a SIGNED token, not
   a session record — HMAC over principal/credential identity+expiry, keyed by HKDF from the live
   API key (`browser_session_token.go`), so sessions survive a restart, carry no
   server state, and are all invalidated by an API-key rotation. The token
   carries NO scopes: authority is re-resolved from live config every request
   (`callerForSession`), so narrowing or removing a scoped caller takes effect
   immediately instead of at cookie expiry. It is renewed
   on use once past half its TTL (`RenewBrowserSession`, called from
   `authMiddleware`); WebSocket upgrades are skipped. CLI and external clients
   continue to use Bearer auth. Browser cookie mutations require a same-origin
   `Origin` header.
   Durable-task routes additionally enforce narrow task-action scopes plus a
   stable tenant/principal owner and exact `namespace/name` agent-resource
   allowlist on every read and mutation. Repository queries apply that envelope
   before pagination or loading results; object-addressed denials are
   non-enumerating 404s. The legacy shared key maps to the explicit installation
   tenant/admin principal for migration.
2. Routing is hand-rolled per route group: `pkg/api/routes_<group>.go`, one
   file per group, helpers in `routing.go` (`splitAction`). No router lib.
3. Lifecycle mutations funnel through `setAgentDesiredPhase` and pass the
   caller-scope authorization gate (`authz.go`; scopes `lifecycle:write` <
   `lifecycle:admin`; enforcement off by default — see
   `docs/architecture/api-authorization.md`).
4. Handler writes `Agent.spec.desiredPhase` (intent), NOT pod state.
5. Reconciler observes the CR change → `classifyEvent` → state machine →
   action → pod create/delete → status patch → PWA polls/WebSockets it back.

### Status/telemetry (pod → platform, upward)
In-pod runtime binary (e.g. `kyber-token-reporter`) → POST to localhost:8091 on
`kyber-status-sidecar` → sidecar authenticates + forwards →
`POST /internal/agents/{name}/...` (`pkg/api/internal*.go`) → control plane
patches `Agent.status.activity` / token stores. RULE: a runtime must NEVER
POST directly to the control plane — always through the sidecar's localhost
forwarder (`docs/architecture/status-pipeline.md`).

### Inbound webhook (sender → agent prompt)
See § 1.4. Outcomes (`dispatched` | `dropped` + reason) land on the Agent CR;
debug/replay/rotation surfaces are `pkg/api/routes_inbound_{debug,replay,...}.go`.

### Build → deploy (code → cluster)
Push to `main` → `.github/workflows/build.yml` (path-filtered, per-image jobs)
→ GHCR `:latest` + `:<sha>` for explicitly configured development consumers.
Releases: dispatch `prepare-release.yml` with the approved version. It merges
chart/install-doc version updates before tagging. `release.yml` builds nine
images, gates the GitHub Release on A2A conformance, publishes the OCI chart,
and optionally tags the independently versioned pwa-views package. Chart
packaging must stamp every built image, including `slackSidecar`. There are no
release-time deploy-repo bump jobs; installation updates require an operator
apply through the updates API. See `docs/operator/release-runbook.md`.
Displayed version is BUILD-INJECTED via `-ldflags "-X main.Version=…"`
(`cmd/control-plane/version.go`), not chart-rendered (kyber#482).

---

## 3. Conventions (cite docs/contributing/code-quality.md § in reviews)

Go:
- Wrap errors with `%w`, lowercase verb-phrase prefix, no trailing period:
  `fmt.Errorf("fetching agent: %w", err)`. Sentinels for branchable conditions
  (`ErrQueueFull` in `pkg/inbound/queue.go`); compare with `errors.Is`. Use
  apimachinery `errors.IsNotFound` for k8s errors.
- Tests: table-driven, STDLIB ONLY — no testify anywhere; do not introduce it.
  Canonical shape: `pkg/usersecrets/usersecrets_test.go`.
- Receivers single-letter matching type (`r *AgentReconciler`). Small
  interfaces defined at the consumer (`Runtime`, `Adapter`, `Probe`).
- Env vars read by control plane / node-agent: `KYBER_*` prefix.
- API handler files: `pkg/api/routes_<group>.go` + `routes_<group>_test.go`.

TypeScript/PWA (npm workspaces monorepo, root package `kyber-monorepo`):
- `packages/pwa-views/` — shared React view library, PUBLISHED to GitHub
  Packages as `@matty-v/kyber-pwa-views`; consumed by `apps/embedded-pwa`
  (workspace link) AND the external Holocron host (published version only).
- Function components, PascalCase; hooks `use*` in
  `packages/pwa-views/src/hooks/`. `strict` TS; NO ESLint — `tsc --noEmit` IS
  the lint. PWA types are HAND-WRITTEN (no OpenAPI→TS codegen).

Cross-cutting:
- Any API request/response shape change updates THREE things in one PR: the Go
  handler, `test/contract/openapi.yaml`, and the hand-written PWA type.
- Per-agent Secrets: `<agent>-<type>` (`alice-oauth`); platform:
  `kyber-<role>` (`kyber-api-credentials`).
- Helm namespace: always `{{ include "kyber.namespace" . }}`, never
  `{{ .Values.namespace.name }}` (past prod incident).
- One consolidated PR per logical change set (CI is expensive). `main`
  requires PRs; never bypass CI; use the target installation’s declared update
  mechanism (GitOps or supervised Helm), never a competing manual deploy.

---

## 4. Gotchas (the ones that have already burned this repo)

Full living list: `docs/contributing/reviewing.md` (append-on-discovery). Highest-value subset:

1. **Image equality is a STRING comparison, not digest.** kubelet's
   `status.containerStatuses[].imageID` is the per-arch manifest digest; the
   configured image is a multi-arch index digest — they NEVER match. Drift
   checks compare `Spec.Containers[i].Image` strings (`isSidecarDrifted`,
   `reconciler.go`). Any new code reaching for `imageID` reopens a fleet-wide
   roll loop (kyber#371).
2. **`classifyEvent` ordering is load-bearing.** Centralized desired-phase
   kill switches (NeedsAuth, Stop) sit BEFORE two sequential per-phase
   switches; an early `return` in the first switch pre-empts pod-death
   detection in the second. New kill switches go at the top with an explicit
   allowlist; new early returns in `case Running/Starting` must consider a
   simultaneously-dead pod (docs/contributing/reviewing.md #10, #11).
3. **Reconciler steps 5c→5d share a concurrency budget of 1** via
   `countAgentPodsBeingDeleted`; the call order in `Reconcile()` is what
   enforces the cap. Reordering doubles rollout rate (docs/contributing/reviewing.md #7).
4. **`ensure*` for any pod-spec-referenced resource belongs INSIDE
   `createPod`** (idempotent Get-then-Create), not only in birth-time actions —
   recreation paths skip birth ensures and pre-existing agents wedge Pending
   (docs/contributing/reviewing.md #10/kyber#467).
5. **Both runtime start scripts run `set -euo pipefail`.** A bare
   `VAR=$(cmd)` that fails kills boot, and a `${VAR:-default}` on the next line
   is dead code; guard inside the substitution (`$(cmd || true)`). Also:
   `git config --global` on multi-valued keys needs `--replace-all` — stale
   PVC gitconfig otherwise crash-loops boot with no auto-recovery.
6. **`helm upgrade --reuse-values` silently ignores new chart defaults.**
   A chart PR adding/changing a default can be inert on live releases. Audit
   with `helm get values <release>`.
7. **`pwa-views` source change without a `package.json` version bump never
   reaches Holocron** (CI guard in `test.yml` enforces the bump on PRs;
   publish itself happens only on a `pwa-views/vX.Y.Z` tag — `CONTRIBUTING.md`
   checklist).
8. **`build.yml` on `pull_request`:** `github.sha` is the synthetic merge
   commit; use `github.event.pull_request.head.sha` for tags/checkouts.
   Push-only context vars (`github.event.head_commit.*`) are empty strings.
9. **Tier-1 metrics-store reads must be authoritative on empty windows** —
   return `(emptySlice, true)`, not `(nil, false)`, or callers fall through to
   Prometheus and resurrect all-time data (`pkg/api/routes_metrics.go`;
   `activityFromMetricsStore` predates this rule — do NOT copy it).
10. **A key in the control-plane ConfigMap is NOT an env var.** The container
    names its env explicitly; nothing does `envFrom` on that ConfigMap. Adding
    a setting to `control-plane/configmap.yaml` without a matching
    `configMapKeyRef` in `control-plane/deployment.yaml` renders a value the
    process can never read — `helm get values` shows it set, the operator sees
    it applied, and `os.Getenv` returns "". This shipped: every
    `api.durableTasks.*` value was inert on every cluster, invisible because
    each chart default equalled the compiled default in
    `taskstore.DefaultLimits()`. `test/chart/task_env_wiring_test.go` guards
    the task keys; extend it when a new settings family lands.
11. **GitGuardian flags `AuthType: "oauth"` struct literals** as a secret.
    Confirmed false positive (recurring); check the flagged line is an enum,
    then proceed.
12. **`fireAndForget` (node-agent dispatch) skips result POST and pushStatus**
    by design — terminal verbs only (delete, logout). Non-terminal verbs on it
    silently lose status.
13. Looks-wrong-but-intentional: hand-rolled HTTP routing (no mux lib); no
    testify; no ESLint; `make lint` = `go vet` only (documented placeholder —
    golangci-lint is NOT wired); runtime imports in `cmd/control-plane/main.go`
    (named Claude Code + blank Codex registration are deliberate);
    `helm template` requiring explicit image tags (kyber#358 — no AppVersion
    fallback, intentional fail-loud).
13. **Whole-disk persistence uses chroot, not overlayfs.** User namespaces
    are enabled by default. Keep Kubernetes/runtime compatibility requirements
    and the `agent.security.*` break-glass controls aligned with the chart.
    The old overlay mode exists only as an explicit rollback option.
14. **Empty fleet model defaults are intentional.** They mean "let the runtime
    choose its default model," while the fresh harness-version default is the
    literal `latest`. The live ConfigMap wins for all four runtime-scoped keys
    so an operator's concrete pin survives Helm upgrades.
15. **Machine-aware Agent recovery depends on production wiring.** Keep
    `AgentReconciler.MachineGetter` initialized in `cmd/control-plane/main.go`.
    `SetupWithManager` fails closed when it is nil; weakening that gate can
    silently disable `WaitingForMachine` recovery and turn provider capacity
    loss into Agent startup failures.
16. **Automatic convergence must request Restarting before pod deletion.** A
    direct `r.Delete` while status remains Running races pod watches into
    `PodDied` → `Failed`, spends retry budget, and adds crash backoff to a
    healthy rollout. Only a Running Agent may persist
    `desiredPhase=Restarting`; let the state machine own deletion, count that
    request in the shared rollout budget, and arm any image canary only after
    the request succeeds. See `docs/contributing/reviewing.md` #17.
17. **Background relaunch helpers must release request streams and locks.**
    Claude's generated skill-repair loop must close fd 200 and redirect all
    standard streams before backgrounding. Otherwise native restart succeeds
    while the API times out and task dispatch remains locked. The generated
    relaunch regression fixture exercises this with A2A enabled.


18. **API-key injection must prepare the native interactive login.** Codex
    needs `login --with-api-key` with the key on stdin; an env var alone leaves
    its TUI at the login menu. Claude records approval of the selected key in
    its private state and must keep the full interactive profile: `--bare`
    skips discovery of hooks/skills/MCP and is not a replacement for approval.

19. **Runtime evidence has its own expiry deadline.** Public task declarations
    must requeue when runtime capability evidence expires, even without a skill
    report. Advanced job controls require an executable native probe; a sentinel
    alone is insufficient. Receipt probes verify the full runtime-specific argv.
20. **Provider exit codes and transcript paths are runtime-scoped.** Classify
    auth failures using the pod's runtime label (legacy fallback: Agent spec),
    never every registered provider's codes. Do not inject a transcript pruner
    when the runtime has no declared transcript root; empty env paths fall back
    to the script's historical Claude defaults.

---

## 5. Do-not-touch zones

- **`deploy/helm/kyber/crds/`** and **`pkg/api/v1/zz_generated.deepcopy.go`** —
  generated. Edit `pkg/api/v1/*_types.go` + `make generate`.
- **`deploy/helm/kyber/templates/namespace.yaml` `fail` guard** — fences
  `preview.enabled` renders out of `kyber-system`. Weakening it re-enables the
  2026-04-19 ArgoCD prune cascade that DELETED PROD. The per-PR preview system
  is retired (kyber#531) but the fence stays.
- **`pkg/api/pwa_dist/`** — build artifact of `make pwa-build`.
- **Controller cache scope** (`cmd/control-plane/main.go`
  `cache.Options{DefaultNamespaces}`) — namespace-scoped on purpose (PR #118).
  Don't broaden to cluster-wide without a verified need.
- **Scaffolder retry/backoff in `pkg/githubapp/`** (PR #91) and the
  sweep-before-create in pod creation (PR #73) — both are incident fixes;
  don't "simplify" them away.
- **Ask first**: new dependencies (go/npm/helm), CRD schema changes, RBAC,
  branch protection/CI permissions, secret rotation.

---

## 6. How to verify changes (in order)

```bash
make build                      # go build ./...
make lint                       # go vet ./... (this IS the Go lint gate)
make test                       # go test ./... — REQUIRED gate; includes
                                # envtest suites (need KUBEBUILDER_ASSETS:
                                #   go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19
                                #   export KUBEBUILDER_ASSETS=$(setup-envtest use 1.31.0 -p path)
make generate                   # if you touched pkg/api/v1/ — commit the diff

# PWA (only if you touched packages/ or apps/):
npm ci
npm run build --workspace=packages/pwa-views
npm run build --workspace=apps/embedded-pwa
npm run lint  --workspace=packages/pwa-views    # tsc --noEmit
npm run lint  --workspace=apps/embedded-pwa
npm run test  --workspace=packages/pwa-views    # vitest run
npm run test  --workspace=apps/embedded-pwa

# Heavier suites (CI runs these in integration.yml / e2e.yml):
go test -tags integration -timeout 15m ./test/integration/... ./test/contract/...
    # needs docker (postgres/redis services) — see test/integration/docker-compose.yml
go test -tags e2e -timeout 25m ./test/e2e/... -cluster-name kyber-e2e   # needs k3d+docker

# Chart:
make helm-lint
make helm-template              # pins placeholder image tags for you

# Local control-plane/API instance (no cloud creds; API on localhost:18080):
scripts/devenv/up.sh            # control-plane/API only; see scripts/devenv/README.md
scripts/devenv/down.sh

# Full local stack — control-plane AND live agent pods (real runtime in a
# pod, fake managed compute; the closest runnable local mirror of prod).
scripts/devenv/up-full.sh --compute-provider fake
                                # one command; managed fake compute + live agent
                                # pods; then create an agent via the PWA
                                # wizard (OAuth). See scripts/devenv/full-local.md
# Local GitHub App identity-repo testing: save credentials outside git with
# scripts/devenv/setup-github-app.sh, then rerun up-full.sh --skip-build.
# Hot-reload UI against that backend: bring up.sh/up-full.sh on --api-port 8080,
# then `make pwa-dev` (embedded PWA at :5173, proxies /api → :8080). For the
# holocron hub against local pwa-views, see holocron's README "Local development".

# Managed compute lifecycle without cloud credentials:
scripts/devenv/up.sh --compute-provider fake
scripts/devenv/smoke-fake-provider.sh  # create → stop → start → delete
scripts/devenv/down.sh

# GCE adapter/API fidelity (synthetic Nodes; agent pods cannot schedule here):
scripts/devenv/up-full.sh --compute-provider gce-emulator
scripts/devenv/compute-scenario.sh attach-node <machine>
scripts/devenv/compute-scenario.sh apply <machine> preempted
scripts/devenv/compute-scenario.sh attach-node <machine>  # replacement join
```

CI gates: `test.yml` (lint+test+pwa — a `changes` job
skips the heavy Go/PWA steps on docs-/`scripts/devenv/`-only PRs, but the jobs
still run green so the merge gate never wedges), `integration.yml`, `e2e.yml`,
`build.yml` (images), `design-lint.yml` (advisory, PWA paths only).
If docs/contributing/code-quality.md's table and `test.yml` disagree, the workflow wins.

---

## 7. Common-task recipes

### A. Add/modify a REST endpoint
The operator boundary defaults new public routes to administrator-only.
Restricted access requires an explicit classification in `operator_boundary.go`
plus the handler's action/resource checks. Do not admit a route merely because
it is authenticated, and do not let `AuthzEnforce=false` bypass that boundary.

Installation credential management must use the always-enforced
`requireInstallationAdmin` gate before backend access. `lifecycle:admin` is an
Agent lifecycle grant, not installation administrator access; never let the
observe-only lifecycle flag permit API-key rotation or management.
The administrator-only `GET /api/v1/operator-credentials` projects whitelisted
metadata from the live authenticator and configured caller records. Never
serialize `ScopedCaller` or Kubernetes Secret data. System identities use
`installation-admin`; old signed task-event cursor IDs have a narrowly scoped
administrator compatibility reader. See `docs/operator/operator-credentials.md`.

1. Handler in `pkg/api/routes_<group>.go` (new group → new file); register in
   `server.go` `registerProtectedRoutes` (or `registerWebhookRoutes` for
   unauthenticated webhook paths).
2. Lifecycle verb? Route through `setAgentDesiredPhase` + add scope in
   `authz.go` (read `docs/architecture/api-authorization.md`).
3. Update `test/contract/openapi.yaml` + hand-written PWA type in
   `packages/pwa-views/src/` (3-way rule, § 3) — pwa-views change ⇒ version
   bump in `packages/pwa-views/package.json` + CHANGELOG entry.
4. Table test beside the handler; run § 6.

### B. Change agent lifecycle (phase/event/transition)
1. READ `docs/architecture/agent-lifecycle.md` first. Check gotchas #2/#3.
2. Phase constants: `pkg/api/v1/agent_types.go`; transitions:
   `state_machine.go`; event derivation: `reconciler.go` `classifyEvent`
   (kill switches at top, mind the two-switch ordering).
3. Unit tests in `state_machine_test.go` + an ENVTEST integration test
   (controller changes always require one — `reconciler_*_test.go` is the bar).
4. Update the transition table in `agent-lifecycle.md` — it must stay in sync.

### C. Change the agent pod spec (volume/container/env)
1. `pod_builder.go` (`BuildPodSpec`) or the relevant `Append*` (sidecar/
   tailer/pruner — runtime must stay `Containers[0]`).
2. New referenced resource (PVC/Secret/ConfigMap)? Idempotent `ensure*` INSIDE
   `createPod` (gotcha #4).
3. Runtime-specific env/mounts go in the relevant `pkg/runtimes/<runtime>/` Adapter, not
   hardcoded in the builder.
4. Envtest + `scripts/devenv/up.sh` smoke. Note: `images/` changes hit every
   agent on next deploy — verify tmux + runtime process + identity-repo clone.

### D. Chart change (`deploy/helm/kyber/`)
1. Default in `values.yaml`; templates use the `kyber.namespace` helper.
2. New image ⇒ paired PR in the deploy repo (per-env values + Image
   Updater annotations) or pods ImagePullBackOff.
3. Remember `--reuse-values` drift (gotcha #6) and the namespace fence (§ 5).
4. `make helm-lint && make helm-template`; chart-only PRs skip the dev-deploy
   substrate — verification is render-level.

### E. PWA change
1. Shared views/hooks/types: `packages/pwa-views/src/`; embedded-app-only glue:
   `apps/embedded-pwa/`. pwa-views diff ⇒ version bump (CI-enforced).
2. Reaching Holocron requires successful package publication and a host
   dependency bump. Main-branch version changes trigger auto-publish; inspect
   both auto-publish and tag-publish runs before using the recovery dispatch.
   See `CONTRIBUTING.md` and the publish-boundary architecture document.
3. Verify in a browser (`make pwa-dev` or devenv), not just type-check.

### F. Add a new runtime type
Read `docs/architecture/agent-harness-contract.md` and its conformance guide first.
The v1 contract separates normative requirements from versioned evidence and
explicit conformance exceptions. Reuse `pkg/runtimes/contracttest` for adapter checks and run the
receipt/behavior suites listed in the guide; source inspection is not live proof.
Follow `pkg/runtimes/runtime.go` package doc: subpackage with `init()`
registration + Adapter + Probe + paths, blank import in `cmd/control-plane`
(and status-sidecar if needed). Read `docs/architecture/status-pipeline.md`
before wiring any in-pod signal — signals go through the sidecar forwarder.

---

Ambiguities known at time of writing: `make lint` is intentionally only
`go vet` (golangci-lint "configured in A2" never landed); `pwa/` paths in some
older docs refer to the pre-workspace layout (now `packages/pwa-views` +
`apps/embedded-pwa`); `test/prod-e2e/` largely moved to kyber-deploy but a stub
dir remains here. Trust current code over any doc, including this one.

Operator terminal delegation uses `agents:exec` + exact `agentResources`, or
`machines:exec` + separate exact `machineResources`. Both require stable caller
identity in configuration and are checked before lookup/upgrade; lifecycle
scopes do not imply exec. Machine terminals control the host. The credential
inventory reports Machine targets separately; never conflate them with Agents.

`pkg/api/exec_transport.go` isolates cancellable Kubernetes exec upgrade headers
while retaining client-go TLS/proxy/authentication and SPDY streams. Do not
replace it with the standard executor without preserving stalled-handshake
cancellation, redirect rejection and upstream error-body redaction tests.

Sensitive data uses independent `secrets:list`, `secrets:read`, `secrets:write`,
`logs:read`, `transcripts:read` grants on exact Agents, and `machines:logs:read`
on exact Machines. `sensitive_routes.go` enumerates method/action/source pairs;
new log sources/subpaths do not inherit grants. Guards precede backend reads,
ignore observe-only authorization, and set no-store. Metadata-only secret lists
redact value fingerprints. Generic logging/export stays administrator-only.

The staged `Server.OperatorCredentialAuthority` seam resolves one current
registry authority per request; only an exact uninitialized result before
adoption permits bootstrap configuration. The control-plane command does not
wire it yet. Registry cookies use per-credential V2 signing; private immutable
V1 compatibility signing state preserves existing sessions without retaining a
second accepted bearer key. Read
`docs/design/2026-10-23-current-operator-credential-resolution.md` before enabling
or changing this seam. Do not activate it before ownership, audited management,
stream revocation and upgrade/rollback compatibility gates are complete.
`AuthorityStore.Bind` additionally requires a live adoption reservation from
`ReserveAdoption`, held before preflight and through uncertain-write recovery.
The immutable maintenance Secret has no automatic expiry; never release it from
an unconditional defer or steal it based on age. Upgrade integration and
compatibility enforcement remain activation gates. Read
`docs/design/2026-10-24-credential-maintenance.md` before using the reservation.

Protected requests install `liveOperatorRequests` inside authentication via the
sensitive-audit wrapper. It rechecks identity/current route policy every three
seconds with a two-second authority deadline. Sensitive handlers add exact
resource/action requirements; task event streams add their read requirement.
Do not replace these checks with the original request's grants or the task
heartbeat. Cursor replay captures signing material once while the independent
watcher enforces revocation. Read
`docs/design/2026-10-24-operator-stream-access.md` before changing cancellation,
write deadlines, WebSocket close behavior or completion outcome handling.

Public archive download capabilities use signed V2 claims bound to one job,
Agent/namespace, credential identity and a private-keyed material proof. Resolve
the current authority at admission and during the transfer; never use the link
as a general API key. URL expiry limits admission, not healthy transfer duration.
Configuration resolution keeps ordinary bearer duplicate-key precedence. Old V1
or pre-adoption links require regeneration; retained archives are unchanged. Read
`docs/design/2026-10-24-current-archive-download-access.md` before changing this path.

Management mutations use `requireManagementAudit`/explicit current permission
predicates before effects, `beginManagementMutation` before remote writes and
`completeManagementMutation` only after confirmation. Imports reuse their private,
same-actor/same-target import admission during nested Agent creation. Public
archive downloads collect outcomes inside their narrow capability guard. A
verified old link may supply denial attribution only, never caller permissions.
Read `docs/design/2026-10-24-required-management-audit.md` before changing these
boundaries. Handler deadline changes pass through the live writer's serialized
setters; never let a later timeout undo an access-cancellation deadline.

Credential adoption candidate conversion is private and inert. Keep actual
root/final duplicate-material precedence, stable identities/grants and unknown
issuance times. Never borrow another principal's tenant while filling missing
identity. The V1 compatibility signer has immutable issuance/expiry cutoffs and
bounded lifetime; retiring the installation signer ends scoped V1 trust while
V2 stays per-key. Unknown credential writes are fenced at their original opaque
resource version, then resolved through current authority/outbox; never replay
issuance. Read `docs/design/2026-10-24-credential-adoption.md` before connecting
adoption or mutation endpoints. Admission, root/source mutation coordination,
replica checks, reconciliation and recovery UI remain activation gates.

Supervised upgrades require `UpgradeCredentialCoordinator`. Once runtime protocol
one is enabled, reserve before any
release/authority decision, retain through writes/verification/rollback, and
retain uncertain effects for recovery. Dry runs reserve nothing. Selected
registry authority requires compatible effective target, initial rollback and
live workloads; recheck the recorded revision before rollback. Published chart
protocol/image metadata is independent of image-value overrides. The current
runtime/chart advertise protocol one with explicit adoption and recovery.
Older protocol-zero releases perform fail-closed authority reads but create no
persistent maintenance lock; never infer protocol support from schema alone.
Read `docs/design/2026-10-24-credential-upgrade-compatibility.md` before changing
these gates. The Go upgrade client maps Deployments and CRDs statically.

Self-upgrade dry-run rendering must use captured output, never a streamed Helm
render. Hide Secret objects and discard all render stdout and diagnostics:
ConfigMaps, environment values and chart notes may also contain credentials.
Keep safe cancellation causes without echoing Helm's rendered/error text.

The required `test` job also runs credential-store/upgrade tests and focused
live-operator access tests with Go's race detector when authentication, stream
authorization, upgrade/credential-store code, authority artifact declarations,
Go module inputs or publishing/test workflows change. The change detector keeps this
on the security-sensitive surface and forces Go setup for that path. Ordinary
local API tests do not substitute for the race check on concurrent guard changes.

Credential mutation recovery uses `pkg/securityaudit.CredentialIntent` for registry
writes: admission and the original registry UID/opaque CAS version commit in one
Postgres transaction before effects. This is private metadata, never candidate
or key storage. Unknown writes stay pending until authority/outbox/fencing proves
the outcome; completion ID/outcome/timestamp are immutable on replay. Pending
work survives ordinary event retention. Recovery scans page by operation ID and
restart periodically; they are not a frozen snapshot. The journal alone does not
authorize callers, perform credential writes or activate authority.

Staged managed-key mutation candidates preserve principal/tenant/provenance and
separate material generation from permission revision. Client input cannot assign
those identities or source ownership. Registry writes pin both the selected
Secret UID and original resource version; missing authority is unavailable.
The staged management endpoints expose explicit changes, original operation
lookup and audited recovery. Inventory versions are opaque If-Match values bound to the selected registry UID
and resource version; management forms must remain bound to their connection.
Pending is unconfirmed, never a successful/failed key change; GET never fences.
Explicit recovery pins UID/version and outbox identity, fences late CAS writes
before final failure, and never issues/re-reveals material. `credential:recover`
audits the current administrator separately from the original key change.
The typed client never automatically retries mutation POSTs. Issuance time is
reported only when known. Runtime protocol remains zero and cmd/control-plane
registry activation, adoption/recovery UI and final acceptance remain outstanding.

Operator key management forms use server-provided permission target choices from
`/api/v1/operator-credentials/options`. Prepared key values stay in component
memory, never React Query variables/cache or browser storage. Forms retain the
reviewed inventory version through polling. GET operation status/pending scans
never fence writes; only explicit POST reconciliation can do that. Cookie self
rotation refreshes the same principal/tenant session; bearer hosts update their
connection separately. Runtime authority activation remains staged.

Credential audit delivery uses `api.OperatorCredentialDelivery.RunOnce` as a
staged bounded helper. It confirms the immutable journal/outbox proof, marks a
reference delivered only after durable success, then rechecks before pruning on
a later pass. It never reissues/fences keys or changes access, and never retries
a stale CAS candidate. Its rotating cursor prevents missing journal records from
starving unrelated work. Runtime scheduling is not enabled by the helper alone.

Operator key activity uses separate `securityaudit` usage aggregates and observer
batch cursors. Accepted request metadata never includes credential material,
content, raw paths or IP addresses. Collection is non-blocking; bounded queue
loss, pending acknowledgement and partial coverage must stay visible. Aggregate
counters and cursor advance commit together, with exact replay checks; cursor
rows cannot be deleted and recreated without risking duplicate old batches.
Counters cross the API as decimal strings. With PostgreSQL configured, main
starts a reporter for its installation.
Authentication never waits for reporter writes. Admin activity reads recheck
current access before output; the UI presents windowed counts and explicit gaps.

Credential maintenance visibility is read-only: direct bounded inspection of
validated installation reservation metadata, admin authorization rechecked after
the read, namespace binding and no-store. Only confirmed NotFound means idle;
age, malformed state and outages never permit takeover. The opaque inspected
version is not a release capability. Keep metadata connection-local in the UI
and clear it on denial/failure. Protocol-zero upgrades may reserve nothing, so
idle is not an assertion that no upgrade runs. Read
`docs/design/2026-10-24-credential-maintenance-visibility.md` before recovery work.


Staged maintenance recovery binds supervised non-retrying Pod/Job UIDs and
requires known termination, release/authority reconciliation, current admin
access and durable journal admission before metadata fencing. The journal stores
an immutable UID/resource-version/proof-ID delete intent before release. A
pending recovery cannot be displaced; GET readiness/outcome/pending requests
never fence or delete. Only explicit audited resolution retries the same intent.
The new recovery API/UI remains disabled while the runtime protocol is zero;
writer-evidence retention and complete adoption acceptance are activation gates.
Read `docs/design/2026-10-24-credential-execution-recovery.md` before changing it.

The authenticated `GET /api/v1/operator-credentials/current` exposes only the
presented credential's freshly resolved identity and grants, with no-store and
no database dependency. Restricted callers may GET only; it does not permit
inventory or management. Missing identity/Pod UID is not invented. This supports
adoption replica checks without adding an authentication method or activating
protocol one. See `docs/design/2026-10-25-credential-adoption-preflight.md`.

Protocol-one supervised Jobs start without TTL, and bound acquisition rejects
expiring evidence. A fresh Job metadata CAS binds one execution before acquiring;
the same Job cannot acquire twice, so cleanup cannot race its reuse. Private handles schedule seven-day Job cleanup only after
confirmed release, with exact Job UID/version and no current reservation using
that writer. Uncertain cleanup retains evidence without rewriting the operation
outcome. Protocol-zero Job cleanup stays unchanged. See
`docs/design/2026-10-25-credential-writer-retention.md`.

The protocol-one credential-maintenance Role is namespace-scoped: Jobs
get/list/watch/create/update and ReplicaSets get, with no Job delete grant.
Protocol zero renders neither Role nor binding. The existing self-upgrade
ClusterRole rule and dedicated upgrade account remain independently gated.

Adoption input collection is a staged private read: use the uncached bounded
Secret client and startup parser/resolution rules, pin every namespace/name/UID/
opaque resource version, reject any partial set, and recheck the full source
graph. Format/JSON and errors must not expose values or parser diagnostics.
This does not prove replica acceptance or activate authority. See
`docs/design/2026-10-25-credential-adoption-sources.md`.

The private adoption replica probe compares every distinct source value against
its own current-credential response and compares the accepted/shadowed inventory.
Use verified Pod IP/UID inputs, never a Service. It disables proxies/redirects
and rechecks frozen sources, but does not discover all replicas, establish image
ownership, quiesce old writers or authorize an adoption effect by itself.

Adoption candidate repair IDs are operation-scoped, purpose-separated UUID
labels derived from the original admission UUID and ordered source index. They
are not authentication values or material hashes. Recovery must retain the
original admission/time and independently verify unchanged source definitions;
rebuilding a candidate alone never authorizes an effect or refreshes cookie
compatibility. Existing principal/tenant/credential IDs remain unchanged.

Credential adoption journal rows use their own credential-adoption kind and a
sorted complete source graph and deployment/release metadata BEFORE native
Job creation, never invented registry/maintenance UIDs. ExpectedVersion pins
the root source. Effect checkpoints bind actual maintenance UID/original version
then advance reserved -> candidate -> registry -> selection -> selected. Terminal
failure can fence a late launch only before candidate admission; after that,
absence cannot finalize failure. Success requires the exact selected checkpoint. Generic key/maintenance
finalizers and pending scans cannot consume adoption rows. The native worker
enforces ownership and external-effect evidence before completing this journal.

Adoption candidate repair IDs are operation-scoped, purpose-separated UUID
labels derived from the original admission UUID and ordered source index. They
are not authentication values or material hashes. Recovery must retain the
original admission/time and independently verify unchanged source definitions;
rebuilding a candidate alone never authorizes an effect or refreshes cookie
compatibility. Existing principal/tenant/credential IDs remain unchanged.

The private adoption replica probe compares every distinct source value against
its own current-credential response and compares the accepted/shadowed inventory.
Use verified Pod IP/UID inputs, never a Service. It disables proxies/redirects
and rechecks frozen sources, but does not discover all replicas, establish image
ownership, quiesce old writers or authorize an adoption effect by itself.

Native adoption recovery uses an exact UID/resource-version annotation CAS only
after the old single-execution Job/Pod is proven stopped. The active writer
projection follows the replacement receipt, and the original admission version
survives every replacement. Original handles fail after fencing; do not use age
or missing objects as shutdown evidence. Maintenance Secret data stays immutable.
Completed handoff must seal the selected marker UID in registry metadata before
final audit/outbox completion. A sealed registry prevents configuration fallback
across a fresh process if its marker is missing; an unsealed candidate remains
recoverable through the still-configured administrator. Normal mutations preserve
this receipt. These are integration gates, not runtime activation by themselves.

The native credential handoff entrypoint is cmd/kyber-credential-adopt in the
control-plane image. The API durably admits the immutable source/deployment
intent before creating one fixed-command Job. Job environment contains only
operation/namespace/deployment/Pod identity metadata; the worker resolves its
private database profile from the verified Deployment's ordered environment.
It never migrates the audit schema. Recovery requires a separately audited,
currently authorized administrator and a proven stopped writer. Completed
selection is verified through the durable registry seal before final delivery.
The dedicated worker ServiceAccount has namespace-only read/source access and
fixed registry/maintenance mutations, never workload or Agent storage changes.
All runtime wiring and Helm grants remain gated by the complete protocol
constant and publisher chart assertion. Protocol one exposes explicit setup;
installation configuration remains authoritative until the administrator confirms
the handoff. An environment flag cannot override the compiled protocol.
Recovery eligibility must check every known native launch for the operation,
including a replacement Job that has not claimed its maintenance receipt yet.
An already-stopped original does not justify another launch while a known
replacement is pending/running. The receipt CAS still fences concurrent requests.
