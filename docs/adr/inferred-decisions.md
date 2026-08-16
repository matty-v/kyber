# Kyber — Architecture Decision Record (Inferred)

Generated from code + git history analysis (596 commits, scaffold → v1.14.0). Each entry is
an ADR-style reconstruction of a decision that is visible in the codebase, with an honest
assessment. Existing formal ADRs (`docs/adr/0001-memory-system.md`,
`docs/adr/0002-file-anatomy-index.md`) are referenced, not duplicated.

**Status vocabulary:** `sound` (keep), `questionable` (works, but the tradeoff is tilting),
`worth-revisiting` (the assumptions that justified it have eroded or the cost is being paid
continuously).

---

## ⚠️ Top 3 worth revisiting

**1. Agent sandbox isolation — resolved by
[ADR 0003](./0003-agent-sandbox-isolation.md) (kyber#78).**
The residual host-valid `SYS_ADMIN` this entry used to flag is gone: agent pods
run in a user namespace by default, and persistence moved off overlayfs onto a
durable root directory, so no host device and no host-valid capability is
needed. Kata/gVisor were evaluated and rejected with reasons recorded in the
ADR; the remaining boundary is the user namespace, not a guest kernel.

**2. ADR-004 — Prompt delivery via tmux send-keys into an interactive TUI.**
The wire protocol between the control plane and a running agent is keystroke injection into
a tmux session (`agent_types.go:275` — failure modes are literally "tmux session missing,
send-keys non-zero"). This couples the platform to Claude Code's terminal UI: no
delivery acknowledgment beyond "the keystrokes landed," no structured response channel
(transcript JSONL is tailed *separately* by a sidecar), and every CC TUI change is a
potential platform break. The dispatcher/queue/dedup machinery in `pkg/inbound` is solid
engineering wrapped around a fundamentally lossy last hop. A headless/SDK runtime mode
(or CC's `--input-format stream-json`) would give real request/response semantics. The
git history shows the cost being paid repeatedly: ack-skipping bugs, retry/backoff logic,
the entire transcript-tailer subsystem (#446-#475) existing because the platform can't
*read* what it can only *type into*.

**3. ADR-008 — Flat trust domain: one full-scope shared API key, authz off by default.**
Authentication is one shared Bearer key (`pkg/api/auth.go`); the legacy key is full-scope
and the caller-level authz added in kyber#474 ships in *permissive* mode by default
(`authz.go` — denied callers are logged-and-allowed). Compounding it: fleet agents push
to GitHub as a single shared PAT, and the worker→dispatcher ack HMAC is one shared secret
across all workers. One leaked credential from any agent pod (which, per ADR-003, is
privileged) is full control of the fleet, its repos, and its lifecycle. The #474 scope
model is the right shape — finish it: per-caller keys, enforce-by-default on new installs,
per-agent git credentials.

---

## ADR-001 — Kubernetes CRDs as the source of truth, controller-runtime reconciliation

- **Decision:** Platform state lives in two CRDs (`Agent`, `Machine` in `kyber.io/v1`,
  `pkg/api/v1/`); all lifecycle flows through declared spec + reconcilers built on
  controller-runtime/kubebuilder (`pkg/controllers/agent`, `pkg/controllers/machine`).
  Present from the very first commit (`9cd30e9 feat: scaffold Kyber monorepo with CRD types`).
- **Context:** Agents and VMs must survive crashes, preemption, and operator absence; the
  platform needs level-triggered convergence, not edge-triggered orchestration scripts.
- **Inferred rationale:** Reconciliation-toward-declared-intent is exactly the failure model
  needed for spot VMs and crash-prone agent pods. CRDs give free API/storage/watch/RBAC
  machinery and make `kubectl` a complete break-glass interface.
- **Alternatives + tradeoffs:** (a) Postgres-backed orchestrator with a worker loop —
  simpler mental model, but rebuilds watch/conflict/retry machinery k8s already has;
  (b) Nomad — lighter, but the team's ops knowledge and the k3s-on-WSL2 install path are
  k8s-shaped. Cost accepted: kubebuilder ceremony, envtest in CI, CRD schema migration pain.
- **Status:** **Sound.** The history validates it — desiredPhase kill switch (#468),
  drift-rolling (#523/#529), and preemption recovery all fell out of the reconcile loop
  rather than needing new machinery.
- **Blast radius if reversed:** Total. Both controllers, the API layer's CRD round-trips,
  the Helm CRD manifests, every integration/envtest, the ArgoCD deploy model.

## ADR-002 — Modular-monolith control plane (one Go binary)

- **Decision:** REST API, both controllers, telemetry, inbound dispatch, and background
  workers compile into a single `cmd/control-plane` binary, internally decomposed under
  `pkg/` by concern.
- **Context:** Solo-operator platform; the fleet is ~5-10 agents, not 10,000.
- **Inferred rationale:** One image to build/version/deploy, one process to debug,
  in-process interfaces instead of RPC contracts. The `pkg/` boundaries (briefstore,
  tokenstore, inbound, oauth as I/O-free primitives) keep a future split possible.
- **Alternatives + tradeoffs:** Microservices would buy independent scaling nobody needs
  and cost a distributed-systems tax the solo operator can't afford. Separate
  API-vs-controller binaries (the common kubebuilder split) was also rejected — fair, since
  they share CRD types and stores heavily.
- **Status:** **Sound.** The discipline is real: `pkg/inbound` is explicitly pure-logic
  with I/O wired at the edge. The one wobble: `pkg/api` has grown to ~58 `routes_*.go` files
  plus capacity/archive/compaction logic — it's becoming a junk drawer inside the monolith.
- **Blast radius if reversed:** High — deployment topology, Helm chart, health probes,
  the embedded PWA (ADR-010) all assume one binary.

## ADR-003 — Three-tier whole-disk persistence (kernel overlayfs → fuse-overlayfs → bind-mount HOME)

- **Decision:** Agent pods mount a PV at `/persist` and the entrypoint
  (`images/agent-base/entrypoint.sh`) dispatches through three strategies to overlay the
  *entire* container filesystem onto it, chrooting the runtime into `/merged`. Requires
  `CAP_SYS_ADMIN` + `/dev/fuse`.
- **Context:** Agents apt-install tools, write global config, and accumulate state outside
  HOME. Spot preemption and pod rolls must not lobotomize them.
- **Inferred rationale:** Tier 1 fails on k3s (containerd root is already overlayfs — kernel
  rejects nesting), hence tier 2 fuse-overlayfs as the prod default; tier 3 is the
  no-/dev/fuse fallback. The dispatcher with per-tier logging (`overlay-mount.log`) shows
  hard-won field debugging — the design is honest about its own fragility.
- **Alternatives + tradeoffs:** (a) HOME-only PV — simple, unprivileged, loses apt/system
  state; (b) periodic image commit/snapshot — heavyweight, racy; (c) dedicated VM per agent
  instead of pods — clean isolation but kills the k8s density/cost story.
- **Status:** **Worth revisiting** — not the persistence goal, the *privilege* cost (see Top 3 #1).
  Also note tier-3 silently degrades durability semantics; an agent can't tell which tier
  it booted under without reading the log.
- **Blast radius if reversed:** Agents lose system-level state on every roll; the
  `start-claude.sh` boot path, runtime-version install (#377), and chroot/nsenter operator
  procedures all assume the merged root.

## ADR-004 — Agent runtime = stock Claude Code in tmux; platform talks to it via exec + send-keys

- **Decision:** The runtime image launches Claude Code detached in tmux
  (`images/claude-code/start-claude.sh` — "Launch in tmux" section at :703, the actual
  `tmux new-session -d -s agent` at :787/:795); the platform delivers prompts by exec-ing
  into the pod and injecting keystrokes; output is recovered by tailing CC's session JSONL
  with a sidecar (#446).
- **Context:** Claude Code is the most capable agent runtime available and ships as an
  interactive CLI; Kyber wraps rather than reimplements it.
- **Inferred rationale:** Zero modification of the runtime, full feature parity with a human
  at the terminal (plugins, skills, OAuth), and tmux gives session survival across exec
  disconnects plus a human-attachable debug surface (the PWA shell tab nsenters into it).
- **Alternatives + tradeoffs:** (a) CC headless/print mode or stream-json — structured I/O
  but (at decision time) lost interactive-session continuity and channel plugins; (b) build
  on the Agent SDK — full control, but then Kyber owns the agent loop. The chosen path
  trades protocol robustness for runtime fidelity.
- **Status:** **Worth revisiting** (Top 3 #2). The runtime-adapter interfaces (`Runtime` /
  `Adapter` in `pkg/runtimes`, conceptually the "RuntimeAdapter") were designed for
  swappability — use them.
- **Blast radius if reversed:** The dispatcher's delivery path, wake handler, transcript
  tailer/compaction subsystem, status sidecar's activity heuristics, and the restart-session
  route all encode tmux semantics.

## ADR-005 — Cost-first infrastructure: spot VMs, scale-to-zero, k3s small profile

- **Decision:** Machines default to spot/preemptible GCE instances; agents support a
  Suspended phase with a Redis message buffer (5-min TTL) + wake handler so suspended
  agents cost nothing; the small install is a single k3s VM with Postgres/Redis as Helm
  sub-charts (commit `d0542cd` deliberately moved them out of Terraform).
- **Context:** Personal-scale platform where idle agents would otherwise burn VM hours 24/7.
- **Inferred rationale:** The README leads with "cost-optimized" — it's a primary product
  requirement, not an optimization. Preemption resilience (ADR-003 persistence + reconcile
  loops) is what makes spot viable.
- **Alternatives + tradeoffs:** On-demand VMs (2-3x cost), serverless/Cloud Run for agents
  (no persistent filesystem, no long sessions). The 5-minute message-buffer TTL is a real
  loss window: a message to a suspended agent that fails to wake within TTL is silently
  dropped.
- **Status:** **Sound** in intent; the TTL-based buffer is **questionable** — durability by
  timer is a race, and there's no dead-letter surface. A persisted queue (even a Postgres
  table) would close it cheaply.
- **Blast radius if reversed:** Machine controller's replacement logic, wake handler,
  messagebuffer package, cost model of the whole platform.

## ADR-006 — Polyglot persistence by concern: CRDs + Postgres + Redis, each behind a store interface with a memory fake

- **Decision:** Durable platform state → CRDs; session briefs → Postgres (`pkg/briefstore`);
  ephemeral/time-series → Redis (tokenstore, metricsstore, messagebuffer, statechangestore,
  envelope cache). Every store ships `store.go` (interface) + `redis.go`/`postgres.go` +
  `memory.go` (test fake). The pattern is rigidly consistent across 6+ packages.
- **Context:** Different data has different durability needs; tests must run without infra.
- **Inferred rationale:** Memory fakes keep the unit-test suite hermetic and fast; the
  interface seam allowed the Metrics tab to ship Redis-backed "without Prometheus" (#343)
  and the archive backend to go provider-agnostic S3/MinIO (#437).
- **Alternatives + tradeoffs:** Single Postgres for everything — fewer moving parts, but
  Redis's TTL/stream semantics fit the ephemeral data genuinely well. Cost: three stateful
  backends on a single-VM install, and Redis data is accepted-lossy (token windows,
  metrics) — which is fine *only* as long as nothing durable creeps in.
- **Status:** **Sound.** The discipline is one of the codebase's best features.
- **Blast radius if reversed:** Every store consumer; the test suite's hermeticity.

## ADR-007 — Inbound prompts as HMAC-gated webhooks over pure-logic primitives

- **Decision:** External events reach agents only through `routes_inbound*.go` →
  `pkg/inbound` (constant-time HMAC verify, body-hash dedup, JSONPath filter/template
  dispatch decisioning, per-binding rate limit, per-agent in-flight queue, replayable
  envelope cache). The package performs zero I/O by design (kyber#208).
- **Context:** GitHub webhooks and sibling agents need to trigger fleet agents without
  exposing the raw API key or allowing prompt floods.
- **Inferred rationale:** Per-binding HMAC secrets scope the blast of a leak to one binding;
  dedup + rate limit + queue protect both the control plane and the (slow, serial) tmux
  delivery path; the replay endpoint and debug ring buffer show operator-debuggability was
  a first-class requirement.
- **Alternatives + tradeoffs:** Direct API calls with the shared key (no scoping, see
  ADR-008), or a real broker (Kafka/NATS — operational overkill). Known sharp edge:
  DELETE-ing a binding rotates its auto-generated HMAC secret, so integrations must PATCH —
  a footgun that has already bitten in operation.
- **Status:** **Sound.** Best-engineered subsystem in the repo. The binding-delete secret
  rotation deserves an explicit guard or warning header.
- **Blast radius if reversed:** All agent-triggering integrations (sibling-agent dispatch,
  GitHub events), replay/debug tooling.

## ADR-008 — Single shared API key, with caller-level scopes bolted on later in permissive mode

- **Decision:** One Bearer key authenticates the whole REST surface (`pkg/api/auth.go`);
  kyber#474 added `lifecycle:write` / `lifecycle:admin` caller scopes enforced at the
  `setAgentDesiredPhase` chokepoint — but the legacy key is full-scope and enforcement
  defaults to permissive (log-would-deny, allow anyway).
- **Context:** V1 shipped fast with one trusted caller (the operator + PWA); then sibling agents
  (release automation, health monitoring, peer agents) became callers and verb-level risk diverged
  (suspend/force-needs-auth are not fail-safe).
- **Inferred rationale:** Permissive-first is a deliberate migration strategy — observe
  would-denies before breaking single-key installs. The admin⊃write nesting invariant is
  carefully reasoned in the code comments.
- **Alternatives + tradeoffs:** Per-caller API keys from day one (more setup friction),
  k8s RBAC impersonation (heavyweight for non-k8s callers). The current state is the worst
  *interim*: the scope machinery exists but protects nothing by default.
- **Status:** **Worth revisiting** (Top 3 #3). Flip enforcement on for fresh installs and
  mint per-caller keys; the code is already there.
- **Blast radius if reversed/completed:** Low to complete (flag flip + key issuance);
  reversing back to single-key costs nothing today, which is exactly the problem.

## ADR-009 — Anthropic auth via platform-managed PKCE OAuth, with NeedsAuth as a first-class phase

- **Decision:** Agents authenticate via OAuth tokens obtained through a PWA-driven PKCE
  flow (`pkg/oauth`); the control plane owns refresh + rotation push; expired/invalid auth
  drives the agent into a `NeedsAuth` phase with a PWA re-authorize button (and an operator
  force-needs-auth verb, #395).
- **Context:** Headless agents on a Claude Max subscription; API keys would bill per-token
  and (at the time) the keychain-based CC auth didn't survive pod recreation.
- **Inferred rationale:** Making auth-expiry a *lifecycle phase* rather than a runtime error
  was the key insight — the reconciler can hold the agent there, the operator gets a single
  affordance, and the history shows the alternative (silent token death) caused real outages
  (rotation-push-wrong-endpoint, skip-refresh-if-valid fixes).
- **Alternatives + tradeoffs:** `claude setup-token` long-lived tokens (simpler, 1-year
  expiry, but per-grant manual ceremony); raw API keys (cost + no Max features).
- **Status:** **Sound.** Hard-won — the git history shows ~6 fix iterations — but the end
  state is right.
- **Blast radius if reversed:** Agent boot path, phase state machine, PWA create/re-auth
  flows, secret storage.

## ADR-010 — PWA embedded in the control-plane binary; views later extracted to an npm package

- **Decision:** The React PWA builds into `pkg/api/pwa_dist` and ships inside the Go binary
  via `embed.FS` (commit `cfe9c3d`); later, views were extracted to `@matty-v/kyber-pwa-views`
  (#308, Holocron Phase A) so an external hub app can compose multiple installs, with the
  embedded app pinning published versions.
- **Context:** Zero-extra-deployment UI for a single-binary platform; then a multi-install
  "hub" requirement arrived.
- **Inferred rationale:** Embedding keeps deploy atomic (UI version === API version).
  Extraction-as-npm-package preserved that while letting Holocron reuse views — the
  publish-boundary doc and release-coupled auto-bump (#421) show the seam was formalized,
  not improvised.
- **Alternatives + tradeoffs:** Separate PWA deployment (version skew, CORS, another
  artifact); iframe embedding for Holocron (cheaper, uglier). Cost paid: a long tail of
  publish-pipeline CI bugs (#309-#341 — token minting, Tailwind entry, workspace layout)
  and a CHANGELOG/version-bump ceremony per view change.
- **Status:** **Sound**, but the npm publish pipeline is heavy for a single-consumer
  package. If Holocron stalls, fold it back in.
- **Blast radius if reversed:** Holocron breaks; control-plane Dockerfile and release
  workflow simplify.

## ADR-011 — GitOps deploys: ArgoCD + Image Updater, env config in a separate kyber-deploy repo, tag-driven semver releases

- **Decision:** CI builds images to GHCR; a tag-driven release workflow (replacing an
  earlier `:stable` re-tag system, retired in #334 after #333 showed re-tagged manifests
  were unsound) cuts `vX.Y.Z` full rebuilds; deploy-bump PRs into `matty-v/kyber-deploy`
  advance environments via a `[falcon, gcp]` matrix, auto-merged for auto-promote (#390).
- **Context:** Multiple clusters (canary and production) need declarative,
  auditable promotion; manual `helm upgrade` had already caused `--reuse-values` config
  archaeology.
- **Inferred rationale:** Separating "what the software is" (kyber) from "what each env
  runs" (kyber-deploy) is textbook GitOps; digest-pinned bumps + tag-immutability CI guard
  (#365) close the mutable-tag hole the `:stable` era had.
- **Alternatives + tradeoffs:** Push-based deploy from CI (simpler, no drift detection),
  env values in-repo (couples release cadence to env churn — explicitly rejected per the
  paired-PR requirement for new images).
- **Status:** **Sound.** The #333→#334 correction (full rebuild over manifest re-tag) was
  the right call at real CI cost.
- **Blast radius if reversed:** Every cluster's promotion path, ArgoCD apps, the release
  workflow chain (pwa-views publish → deploy bump → auto-merge).

## ADR-012 — Sidecar-per-concern on agent pods: status sidecar + transcript tailer + pruner

- **Decision:** Each agent pod carries a status sidecar (`cmd/status-sidecar`: activity
  metrics, cgroup-based OOM detection) and a transcript-tailer sidecar shipping CC session
  JSONL to the archive, with durable per-agent offset PVCs (#467) and an on-PVC pruner (#471).
- **Context:** The platform can't see inside the tmux session (ADR-004); observability has
  to be reconstructed from the filesystem and cgroups.
- **Inferred rationale:** Sidecars share the pod's volumes/cgroup namespace, so they read
  the truth directly; cgroup OOM detection (#285/#296) exists because k8s-level OOM signals
  proved insufficient (a real 3Gi agent-OOM incident).
- **Alternatives + tradeoffs:** Node-agent-level scraping (loses pod-fs access), in-runtime
  instrumentation (requires modifying CC — rejected per ADR-004). Cost: the sidecar fleet
  has its own convergence problem — image drift rolls (#299, #371, #523), admission fixes
  (#451), and an OOM crash-loop of the *tailer itself* (#466). Observability infrastructure
  now needs observing.
- **Status:** **Questionable** — each sidecar is individually justified, but the
  accumulating per-pod fleet (runtime + 2 sidecars + drift machinery to converge them) is
  the tax of ADR-004's opaque runtime. Fixing ADR-004 deflates most of this.
- **Blast radius if reversed:** Metrics tab, OOM surfacing, transcript log lane, archive
  pipeline.

## ADR-013 — Vendored, CI-refreshed LiteLLM pricing feed; never fetched at runtime

- **Decision:** Model pricing comes from LiteLLM's public JSON, projected and vendored into
  the chart by `cmd/fetch-provider-rates`, refreshed by a weekly bot PR behind code-owner
  review (#487, #495), rendered as a ConfigMap. Unpriced models fail loud rather than
  silently $0.
- **Context:** The Metrics tab prices token usage; hardcoded rates went stale with every
  model launch (the fable-5 refresh in `1c79a44` is this working as designed).
- **Inferred rationale:** A third-party feed in the runtime request path is a supply-chain
  and availability risk; vendoring + human-reviewed diff keeps the trust boundary at PR
  review. Fail-loud-unpriced prevents the worst failure (silently wrong cost data).
- **Alternatives + tradeoffs:** Runtime fetch with cache (fresher, riskier), manual rates
  (stale), provider billing APIs (no Anthropic per-token public API at the granularity
  needed).
- **Status:** **Sound.** A model decision — small, complete, correctly bounded.
- **Blast radius if reversed:** Metrics pricing accuracy and its trust story.

## ADR-014 — Data-driven runtime/model management: detection poller + spec.runtimeVersion + boot-time install

- **Decision:** Instead of baking model lists and CC versions into code, a detection poller
  probes what the runtime actually supports (`pkg/runtimedetect`, GET `/api/v1/available`),
  context windows auto-detect from `max_input_tokens` (#488) with a fleet-default resolution
  layer (#376), and `spec.runtimeVersion` drives a boot-time CC install with mismatch
  safety-net conditions (#377, #379).
- **Context:** New models (and CC versions) ship faster than platform releases; hardcoded
  maps caused stale-model UX and wrong token-budget windows.
- **Inferred rationale:** "Detect + apply without code changes" (the #373 design) is the
  correct response to a dependency that versions weekly. The mismatch conditions
  (RuntimeVersionMismatch/ModelUnsupported) turn drift into visible state instead of
  runtime surprise.
- **Alternatives + tradeoffs:** Pin everything (stale), trust CC defaults (no fleet
  consistency). Cost: boot-time npm installs make agent boot slower and flakier
  (stale-staging-dir fix #483, root-install fix #389) — the canary gate on drift rolls
  (#529/#530) exists because a bad runtime image rolling the whole fleet at once was a
  live risk.
- **Status:** **Sound**, with the canary gate as the load-bearing safety device.
- **Blast radius if reversed:** Model picker, token budgets, version pinning, drift rolls.

## ADR-015 — Per-agent identity as a GitHub repo; git auth via one generic PAT (App-token minting removed)

- **Decision:** Each agent's identity/memory/state is a private GitHub repo cloned at boot
  (`start-claude.sh` boot-sync, including default-branch guard fixes #542/#546/#548).
  Kyber#509 deliberately *removed* the in-platform GitHub-App per-agent token-minting loop
  in favor of a generic PAT.
- **Context:** Memory and continuity must survive total cluster loss (see ADR 0001); the
  App-token loop was platform complexity for a marginal scoping win.
- **Inferred rationale:** Git gives audit, backup, and offline access for free; #509's
  simplification trades per-agent credential scoping for a much smaller platform surface —
  defensible given everything already shares a trust domain (ADR-008), but it *deepens*
  that flaw.
- **Alternatives + tradeoffs:** Per-agent deploy keys (scoped, more ceremony), in-cluster
  storage (lost with the cluster — disqualifying given kyber#171's off-cluster backups are
  still unimplemented).
- **Update (kyber#508 Stage 3/4, 2026-07-06):** the shared-PAT scoping concern is now
  resolved *for the identity repo* — reads and writes. The decision: the identity repo is
  managed **exclusively by the Kyber Platform GitHub App** (a configured per-install plugin,
  nothing hardcoded — not a team's separate dev-access App, not a fine-grained PAT), which mints a
  short-lived token scoped to the calling agent's own repo, served on demand via
  `GET /internal/agents/{name}/identity-repo-token` (pod-token auth, act-on-self-only).
  **No PAT fallback** for the identity repo: if the App flow fails the git op fails LOUDLY
  rather than silently masking a broken path with the broad PAT. The generic PAT is retained
  only for an agent's *other* repos (e.g. maintainer cross-repo work). Keeping the Kyber App
  as the identity-repo minter cleanly separates the platform's identity lifecycle from any
  distinct GitHub App an operator's team uses for dev access.
- **Status:** **Sound** on identity-as-repo; the shared-PAT concern is **resolved for
  identity repos** (scoped App token) and still open for arbitrary work repos, which stay on
  the broad PAT pending the rest of #508.
- **Blast radius if reversed:** Agent boot, continuity/resume machinery, memory system.

## ADR-016 — Test pyramid with a real-cluster top: envtest → k3d e2e → prod-e2e, plus kyber-dev pre-merge deploys

- **Decision:** Unit tests on memory fakes (ADR-006), integration via envtest, e2e on
  ephemeral k3d, smoke on real clusters (`test/prod-e2e`), composite e2e composed of
  reusable Phase helpers — and as of #498/#544, each PR deploys to a kyber-dev substrate
  for pre-merge verification (shadow mode first, per-PR control-plane images).
- **Context:** This platform's failure modes (overlay mounts, OAuth, preemption, tmux) are
  exactly the things mocks can't catch; the e2e suite famously caught a latent health-probe
  bug unit tests missed.
- **Inferred rationale:** Pay for realism at the top of the pyramid because the bottom
  can't represent the risk. The per-PR-preview ApplicationSet machinery was built and then
  deliberately retired (#531/#537) in favor of the kyber-dev gate — evidence the team
  measures and prunes its own infra.
- **Alternatives + tradeoffs:** Mock-everything (fast, blind), staging-only (slow feedback).
  Cost: CI time and a dev-cluster to keep alive.
- **Status:** **Sound.** The willingness to retire the preview machinery is the healthiest
  signal in the repo.
- **Blast radius if reversed:** Regression-catch rate on the platform's riskiest layers.

## ADR-017 — Session continuity as a platform feature: brief builder + briefstore + restart semantics

- **Decision:** On every lifecycle transition the controller builds a "session brief"
  (deterministic `BuildBrief`, Postgres-persisted, drains the suspended-message buffer)
  delivered to the waking agent; paired with agent-side tail/summary conventions.
- **Context:** Pod recreation is *routine* (spot VMs, drift rolls); an agent that wakes
  amnesiac after every preemption is useless.
- **Inferred rationale:** Continuity is split correctly across layers — platform owns
  "why did you restart + what arrived while you slept" (the brief); the agent's identity
  repo owns long-term memory (ADR-015/ADR 0001). The B2-era commits show determinism and
  length-guarding were reviewed in from the start.
- **Alternatives + tradeoffs:** Agent-side-only continuity (misses buffered messages and
  restart cause), full transcript replay (token cost, stale context).
- **Status:** **Sound.**
- **Blast radius if reversed:** Wake handler, suspended-message delivery, every agent's
  resume experience.

## ADR-018 — Logs: live reads proxied through the API + node-level Vector shipping to provider-agnostic S3 archive

- **Decision:** Live logs proxy through the control plane (with concurrency caps #463 and
  memory-bounded reads #455 after real OOMs); durable logs ship via a node-level Vector
  DaemonSet to GCS/S3/MinIO behind a provider-agnostic backend (#431, #437), exposed as
  `?source=archive|transcript` lanes on one endpoint.
- **Context:** Pod logs die with pods; agents on spot VMs lose history exactly when you
  need it. Operators read from a PWA, not kubectl.
- **Inferred rationale:** One read endpoint with source lanes keeps the PWA simple;
  Vector-at-node-level avoids per-pod log sidecars (correctly resisting the ADR-012
  pattern where node-level suffices); MinIO support keeps the laptop/WSL2 install
  cloud-free.
- **Alternatives + tradeoffs:** Loki/ELK (heavy for the scale), CP-side log pump (the CP
  already OOM'd just *reading* logs — #455/#463 prove it shouldn't also ship them).
- **Status:** **Sound**, reached iteratively (the #437-#445 fix chain shows Vector's
  sharp edges were paid down one by one).
- **Blast radius if reversed:** Durable log/transcript history, PWA log surfaces.

---

## Honorable mentions (recorded elsewhere or too small for full entries)

- **Git-backed markdown memory over vector/memory platforms** — formal
  [ADR 0001](0001-memory-system.md); "do nothing" with reopen triggers. Sound.
- **No file-anatomy index** — formal [ADR 0002](0002-file-anatomy-index.md);
  rejected after an honest A/B spike that showed tokens going *up*. The spike-before-adopt
  method is worth more than the decision.
- **HOW/WHAT doc split** (`docs/architecture/` vs `docs/product/`, #397/#398) plus
  docs/contributing/reviewing.md gotcha accumulation — a deliberate institutional-memory system for
  agent-driven development. Sound, and unusual.
- **Off-cluster backups (kyber#171) remain unimplemented** — not a decision so much as a
  standing risk: k3s state.db on WSL2 plus no GCS backups means a laptop install's
  platform state has no DR story. The identity-repo design (ADR-015) covers agent *memory*
  but not platform state. This should be the next infrastructure investment after the
  Top 3.
