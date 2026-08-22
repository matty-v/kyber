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
Deployed via Helm + ArgoCD from a separate private deploy repo (referred to as
"the deploy repo" / `kyber-deploy` below: ArgoCD Applications + per-environment
values).

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

### 1.2 Pure state machine + thin reconciler (the agent lifecycle)
- `pkg/controllers/agent/state_machine.go` — pure function
  `(phase, event) → (action, nextPhase)`. Zero k8s imports. Unit-testable.
- `pkg/controllers/agent/reconciler.go` — watches pods/CRDs, classifies what it
  sees into an `Event` (`classifyEvent`), calls the state machine, executes the
  returned `Action` (create pod, delete pod, update status, …).
- Deletion is OUT-OF-BAND: `handleDeletion()` in the reconciler, driven by
  `DeletionTimestamp` — it does not go through the state machine.

WHY: lifecycle logic (14+ phases: Creating, Starting, Running, Suspended,
NeedsAuth, MemoryExhausted, Failed, Draining, WaitingForMachine, …) is the
highest-blast-radius code in the repo; the pure split makes every transition
testable without a cluster. Authoritative transition table:
`docs/architecture/agent-lifecycle.md`. Notable design points:
- OOM gets its own phase (`MemoryExhausted`) so undersized agents don't
  crash-loop; operator bumps memory first.
- `Suspended` unifies spot-preemption parking and idle parking; a wake event
  (e.g. Telegram message) or replacement machine resumes it.
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
runtime is a subpackage (`pkg/runtimes/claudecode/`, `pkg/runtimes/codex/`) that self-registers via
`init()`; binaries enable runtimes by blank-importing the subpackage.
`Adapter` answers pod-spec questions (image, env, secret mounts, probes);
`Probe` is the sidecar-side hook (mostly reserved — see status-pipeline doc).
Adding a runtime = new subpackage + blank import. See the package doc comment
in `runtime.go` for the exact file layout.

Codex subscription auth is performed in-pod with `codex login --device-auth`;
the PWA attaches read-only to tmux session `auth`. The exact `{}` payload in
`<agent>-codex-auth` is the device-login marker and pauses the normal Starting
timeout until the credential syncer replaces it. Codex API-key agents use
`<agent>-openai` / `OPENAI_API_KEY` and never enter the device flow.
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

### 1.4 Inbound dispatch gauntlet (how prompts reach agents)
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
token-usage events), `cmd/transcript-compact`, `cmd/fetch-provider-rates`
(generates `deploy/helm/kyber/files/provider-rates.generated.{yaml,meta}`).

Backing stores: k8s API (state), Redis (events/wake/token budgets), Postgres
(session briefs/fleet metadata). All have in-memory fallbacks (used by devenv).

---

## 2. Data flow

### REST request (operator/PWA → platform)
1. `pkg/api/server.go` `buildTopHandler()` → CORS, then Bearer API-key wall
   (`auth.go` `authMiddleware`, 401 on failure).
   The embedded PWA exchanges its bearer key once at
   `POST /api/v1/browser-session` for a bounded, process-local opaque session
   in an HttpOnly, SameSite=Strict cookie; raw keys are removed from legacy
   localStorage. CLI and external clients continue to use Bearer auth. Browser
   cookie mutations require a same-origin `Origin` header.
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
→ GHCR `:latest` + `:<sha>` → ArgoCD Image Updater rolls dev/canary clusters.
Releases: push tag `vX.Y.Z` → `release.yml` rebuilds all release images at the tag,
opens digest-pinned bump PRs on kyber-deploy (falcon + gcp matrix) + an
auto-merged `Chart.yaml` bump PR on main. CI never deploys; ArgoCD does.
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
  requires PRs; never bypass CI; deploy only via ArgoCD.

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
10. **GitGuardian flags `AuthType: "oauth"` struct literals** as a secret.
    Confirmed false positive (recurring); check the flagged line is an enum,
    then proceed.
11. **`fireAndForget` (node-agent dispatch) skips result POST and pushStatus**
    by design — terminal verbs only (delete, logout). Non-terminal verbs on it
    silently lose status.
12. Looks-wrong-but-intentional: hand-rolled HTTP routing (no mux lib); no
    testify; no ESLint; `make lint` = `go vet` only (documented placeholder —
    golangci-lint is NOT wired); runtime imports in `cmd/control-plane/main.go`
    (named Claude Code + blank Codex registration are deliberate);
    `helm template` requiring explicit image tags (kyber#358 — no AppVersion
    fallback, intentional fail-loud).
13. **User namespaces can break whole-disk package installs.** In local k3d,
    `hostUsers:false` prevents opening `/dev/fuse` (`EPERM`), forcing kernel
    overlayfs; dpkg directory replacement can then fail with `EXDEV` (verified
    with `figlet`). Keep `agent.security.userNamespaces` opt-in until the target
    validates FUSE plus representative apt installs. Do not make it default-on
    without changing the persistence/runtime boundary.
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

---

## 5. Do-not-touch zones

- **`deploy/helm/kyber/crds/`** and **`pkg/api/v1/zz_generated.deepcopy.go`** —
  generated. Edit `pkg/api/v1/*_types.go` + `make generate`.
- **`deploy/helm/kyber/templates/namespace.yaml` `fail` guard** — fences
  `preview.enabled` renders out of `kyber-system`. Weakening it re-enables the
  2026-04-19 ArgoCD prune cascade that DELETED PROD. The per-PR preview system
  is retired (kyber#531) but the fence stays.
- **`deploy/helm/kyber/files/provider-rates.generated.{yaml,meta}`** —
  generator output (`cmd/fetch-provider-rates`, weekly refresh bot). No manual
  cost data ever; code-owner review required; regeneration is provable
  byte-for-byte (docs/contributing/reviewing.md #14).
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

CI gates: `test.yml` (lint+test+pwa+pricing-feed preflight — a `changes` job
skips the heavy Go/PWA steps on docs-/`scripts/devenv/`-only PRs, but the jobs
still run green so the merge gate never wedges), `integration.yml`, `e2e.yml`,
`build.yml` (images), `design-lint.yml` (advisory, PWA paths only).
If docs/contributing/code-quality.md's table and `test.yml` disagree, the workflow wins.

---

## 7. Common-task recipes

### A. Add/modify a REST endpoint
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
2. Reaching Holocron requires the publish tag flow — `CONTRIBUTING.md`
   checklist (merge, then `git tag pwa-views/vX.Y.Z && git push origin <tag>`,
   then a Holocron dep-bump PR).
3. Verify in a browser (`make pwa-dev` or devenv), not just type-check.

### F. Add a new runtime type
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
