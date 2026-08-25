// TypeScript types matching the Kyber API request/response shapes.
// Derived from pkg/api/routes_agents.go, routes_machines.go, routes_fleet.go, routes_events.go.

// ---- Agent types ----

export type AgentPhase =
  // Newly created Agents can be returned before the controller has written
  // status.phase. The Go response emits that zero value as an empty string.
  | ''
  | 'Creating'
  | 'Starting'
  | 'Running'
  | 'Stopping'
  | 'Stopped'
  | 'Restarting'
  | 'Failed'
  | 'Deleted'
  | 'NeedsAuth'
  | 'Draining'
  | 'WaitingForMachine'
  // MemoryExhausted is emitted by the backend (OOM-killed agent, #272). The PWA
  // models it so operator actions like force-needs-auth (#395) can be offered
  // from it.
  | 'MemoryExhausted'

export type AgentAuthType = 'oauth' | 'api-key'

export interface AgentResources {
  cpu: string
  memory: string
  disk: string
}

export type AgentIdentityRepoPhase = 'Pending' | 'Ready' | 'Failed'

// Mirrors agentIdentityRepoResponse from pkg/api/routes_agents.go.
// Populated on Agent when spec.identityRepo.repo is set.
export interface AgentIdentityRepoStatus {
  repo: string
  phase?: AgentIdentityRepoPhase
  message?: string
  tokenExpiresAt?: string
  lastMinted?: string
}

export interface AgentStatus {
  phase: AgentPhase
  podName?: string
  podIP?: string
  // kyber#355: pod-derived fields, populated by the reconciler when the
  // agent is in Running phase. Optional because Pending agents — and
  // any agent the controller hasn't reconciled since the populate-on-
  // diff patch landed — won't have them yet. The UI must tolerate any
  // subset being absent.
  nodeName?: string
  startTime?: string
  restartCount?: number
  message?: string
}

export interface Agent {
  id: string
  phase: AgentPhase
  machine: string
  runtime: string
  // Optional while older Kyber control planes roll forward.
  authType?: AgentAuthType
  model: string
  startupPrompt?: string
  // kyber#118: resume the previous harness session after an unexpected
  // session end (pod recreate, preemption, crash). Intentional session
  // restarts always start fresh.
  sessionResume?: boolean
  // Concrete model observed from the running agent. This is populated after
  // the first response when model is empty and the harness chose its default.
  currentModel?: string
  resources: AgentResources
  status: AgentStatus
  // Populated when spec.identityRepo.repo is configured on the agent.
  identityRepo?: AgentIdentityRepoStatus
  // Scheduled prompts declared on the agent (#135). Empty or absent when
  // no jobs are configured.
  jobs?: AgentJob[]
  // One entry per spec.jobs[].name — the most recent run of that job.
  // Absent until the first fire lands.
  lastJobRuns?: AgentJobRun[]
  // Most-recent context-budget snapshot from the agent's reporter. Optional —
  // omitted when the reporter hasn't written yet, the entry has expired, or
  // the control plane has no TokenStore configured. Surfaced in the agents
  // overview's Context column to avoid a per-row /token-usage fetch.
  tokenUsage?: TokenUsage
  // Runtime-harness version (e.g. "2.1.119") actually installed in the agent's
  // running pod. Populated by start-claude.sh on boot via the control-plane
  // internal API. Omitted until the first report lands or when the pod could
  // not determine the version.
  runtimeVersion?: AgentRuntimeVersion
  // PR-E (kyber#379) badges: True when the corresponding condition is
  // True on Agent.status.conditions. AgentDetail renders a visible badge
  // for each, with operator-facing remediation copy. Derived server-side
  // from AgentConditionRuntimeVersionMismatch / AgentConditionModelUnsupported.
  runtimeVersionMismatch?: boolean
  modelUnsupported?: boolean
  // kyber#674 — the two "the controller refused to build a pod" conditions.
  // Both leave the agent with NO pod, and until #674 neither reached the wire,
  // so the PWA showed an agent with a blank status and no way to reach the
  // cause (the reason existed only in the control-plane log). Remediation for
  // both is on the INSTALL or the agent spec, never a restart.
  runtimeImageMissing?: boolean
  modelUnresolved?: boolean
  // Remediation text from whichever blocked-before-pod condition is True,
  // computed by the controller and rendered verbatim. NOT status.message —
  // that is empty on these paths, which is the bug being fixed.
  blockedReason?: string
  // Inbound prompt bindings declared on the agent (#208). Spec-side list;
  // the runtime stats live behind the dedicated /inbound-bindings endpoint.
  // Empty/absent when no bindings are configured.
  inboundBindings?: AgentInboundBinding[]
  // Bounded ring buffer of recent inbound prompt deliveries — one entry per
  // dispatched-or-dropped POST. Absent until the first request lands.
  inboundRuns?: AgentInboundRun[]
  // Scheduling-failure status for stuck-Pending pods. Populated by the
  // controller after a 30s grace window when a Pod event matches one of
  // the known scheduling/kubelet failure reasons; cleared when the pod
  // reaches Running. The PWA banner (kyber#210 PR-B) reads this to show
  // category-specific remediation copy.
  scheduling?: AgentSchedulingStatus
  // Runtime-agnostic activity signals from the kyber-status-sidecar
  // pipeline (kyber#247 epic; kyber#248 foundation; kyber#249 adds the
  // working/idle state via in-pod kyber-token-reporter).
  activity?: AgentActivityStatus
  createdAt: string
}

// Mirrors agentActivityStatusResponse from pkg/api/routes_agents.go.
// state is a small string enum the PWA branches on for the activity
// indicator; lastHeartbeatAt comes from the platform sidecar
// (kyber#248); state + lastActivityAt come from the per-runtime
// in-pod activity detector (kyber#249).
export type AgentActivityState = 'working' | 'idle' | 'unknown' | ''

export interface AgentActivityStatus {
  // RFC3339 timestamps.
  lastHeartbeatAt?: string
  state?: AgentActivityState
  lastActivityAt?: string
}

// Mirrors agentSchedulingStatusResponse from pkg/api/routes_agents.go.
// `category` is a small enum the PWA branches on for remediation copy;
// `lastError` is the verbatim scheduler/kubelet message rendered in mono.
export type AgentSchedulingCategory =
  | 'Capacity'
  | 'Placement'
  | 'Image'
  | 'Storage'
  | 'Other'

export interface AgentSchedulingStatus {
  category: AgentSchedulingCategory
  lastError?: string
  // RFC3339 timestamp.
  firstObservedAt?: string
}

// Mirrors agentRuntimeVersionResponse from pkg/api/routes_agents.go. Populated
// from Agent.status.runtime — represents what's installed in the pod, not
// what the image was built with (the two can differ once dynamic overrides
// ship in a later phase).
export interface AgentRuntimeVersion {
  installedVersion: string
  // RFC3339 timestamp; empty/absent when the controller hasn't recorded a
  // report yet.
  installedAt?: string
  // PR-E (kyber#379): what the controller asked the agent to run.
  // Absent on agents booted on the baked-in default.
  requestedVersion?: string
  // PR-E: boot-time install outcome. Absent when the report came from
  // a sidecar predating PR-E (staggered roll).
  requestedSatisfied?: boolean
  // PR-E: pre-flight model probe outcome. Absent when the probe didn't
  // run or the report came from an older sidecar.
  modelSupported?: boolean
  // The probe's diagnostic output when it did not succeed — the CLI's
  // actual rejection or error text. Absent on success or older images.
  modelProbeMessage?: string
}

export interface AgentJob {
  name: string
  schedule: string
  prompt: string
  exclusive?: boolean
  clearContextAfter?: boolean
}

export type AgentJobOutcome = 'success' | 'failed' | 'skipped'

export interface AgentJobRun {
  jobName: string
  startedAt: string
  finishedAt?: string
  outcome: AgentJobOutcome
  error?: string
}

// ---- Inbound prompt bindings (#208) ----
// Mirrors pkg/api/v1/agent_types.go — AgentInboundBinding,
// AgentInboundFilter, AgentInboundField, AgentInboundLimits, AgentInboundRun.

export interface AgentInboundFilter {
  jsonPath: string
  in?: string[]
  equals?: string
}

export interface AgentInboundField {
  label: string
  jsonPath: string
  truncate?: number
  prefix?: string
}

export interface AgentInboundLimits {
  maxPerMinute?: number
}

export interface AgentInboundBinding {
  name: string
  existingSecret: string
  signatureHeader: string
  signaturePrefix?: string
  eventHeader?: string
  eventPath?: string
  matchEvents?: string[]
  filters?: AgentInboundFilter[]
  fields?: AgentInboundField[]
  action: string
  limits?: AgentInboundLimits
}

export type AgentInboundRunOutcome = 'dispatched' | 'dropped'

// Drop reasons emitted by the receiver — matches pkg/api/v1.AgentInboundRun.DropReason.
export type AgentInboundDropReason =
  | 'rate-limited'
  | 'queue-full'
  | 'sig-mismatch'
  | 'missing-secret'
  | 'unmatched-event'
  | 'filter-rejected'
  | 'dedup'

export interface AgentInboundRun {
  bindingName: string
  requestId: string
  startedAt: string
  finishedAt?: string
  outcome: AgentInboundRunOutcome
  dropReason?: AgentInboundDropReason | string
  error?: string
}

// Dropped counts indexed by drop reason, returned in InboundBindingWithStats.
export type InboundDropCounts = Record<AgentInboundDropReason, number>

export interface InboundBindingStats {
  lastReceivedAt?: string
  totalDispatched: number
  dropped: InboundDropCounts
}

// Wire shape returned by GET /api/v1/agents/{name}/inbound-bindings — the
// binding spec plus a derived stats block. `url` is omitted when the control
// plane has no PublicURL configured.
export interface InboundBindingWithStats extends AgentInboundBinding {
  url?: string
  stats: InboundBindingStats
}

// Response from POST /api/v1/agents/{name}/inbound-bindings. The `secret`
// field is shown to the operator EXACTLY ONCE — the server keeps a hash and
// never reveals it again.
export interface InboundCreateResponse {
  binding: InboundBindingWithStats
  secret: string
  url?: string
}

// Response from POST /api/v1/agents/{name}/inbound-bindings/{binding}/rotate-secret.
export interface InboundRotateResponse {
  secret: string
  rotatedAt: string
}

// ---- Inbound replay + debug (Phase 3) ----
// POST /api/v1/agents/{name}/inbound-bindings/{binding}/replay/{requestId}
// returns this on 202. 410 → envelope expired (cache TTL elapsed); 429 →
// queue full; 404 → unknown binding/request. The PWA surfaces each via toast.
export interface InboundReplayResponse {
  originalRequestId: string
  newRequestId: string
  status: 'queued'
}

// POST /api/v1/inbound-debug request body. The wire format mirrors the
// receiver's evaluation pipeline: `binding` is a full spec (so operators can
// test unsaved drafts), `payload` is the raw JSON body, optional `headers`
// drive event-header lookup, optional `agent` lets multi-agent debug pages
// scope which agent's secrets/limits get applied (server may default).
export interface InboundDebugRequest {
  binding: AgentInboundBinding
  payload: unknown
  headers?: Record<string, string>
  agent?: string
}

// One filter-evaluation result. `passed` says whether the filter accepted the
// payload; `reason` is populated when passed=false (e.g. "value not in list").
export interface InboundFilterResult {
  jsonPath: string
  passed: boolean
  extractedValue: string
  reason?: string
}

// One field-extraction result. `truncated` says whether the value was clipped
// to the binding's per-field truncate cap.
export interface InboundFieldResult {
  label: string
  jsonPath: string
  extractedValue: string
  truncated: boolean
}

// POST /api/v1/inbound-debug response — the same Decision shape the receiver
// uses internally, so operators can preview exactly what would happen if a
// real request landed. `envelope` is the rendered prompt and is only present
// when match=true.
export interface InboundDebugResponse {
  match: boolean
  dropReason?: string
  resolvedEvent: string
  filterResults: InboundFilterResult[]
  fieldResults: InboundFieldResult[]
  envelope?: string
}

export interface AgentListResponse {
  items: Agent[]
}

export interface CreateAgentRequest {
  name: string
  machine: string
  runtime: string
  model?: string
  startupPrompt?: string
  sessionResume?: boolean
  resources?: Partial<AgentResources>
  identity?: {
    soulDescription?: string
  }
  // GitHub identity repo config. Mutually exclusive modes (server picks up
  // whichever field is set):
  //   template set, repo empty → controller scaffolds a new repo from the template
  //   repo set                 → controller links the existing repo
  //   neither set              → agent runs with soulDescription only
  identityRepo?: {
    repo?: string
    template?: string
  }
  secrets?: {
    authType?: AgentAuthType
    telegramEnabled?: boolean
    oauthCode?: string
    pkceVerifier?: string
    pkceState?: string
    anthropicApiKey?: string
    openaiApiKey?: string
    /** Legacy import path; new subscription agents use in-pod device auth. */
    codexAuthJson?: string
    telegramBotToken?: string
    telegramAllowedUserIds?: string[]
  }
}

export interface PatchAgentRequest {
  startupPrompt?: string
  sessionResume?: boolean
}

export interface SetModelRequest {
  model: string
}

export interface SetResourcesRequest {
  cpu?: string
  memory?: string
}

// ---- Machine types ----

export type MachinePhase =
  | 'Provisioning'
  | 'Ready'
  | 'Running'
  | 'Stopping'
  | 'Stopped'
  | 'Preempted'
  | 'Replacing'
  | 'Failed'
  | 'Deleted'

export type MachineProvider = 'gce' | 'gke' | 'static' | 'fake' | 'mock'
export type MachineAvailabilityClass = 'reliable' | 'costOptimized'
export type MachineManagementMode = 'Managed' | 'External'
export type MachineAvailability = 'Pending' | 'Available' | 'Recovering' | 'Offline' | 'Absent' | 'Failed' | 'Unknown'

export interface MachineCapacitySpec {
  cpu: string
  memory: string
  // Optional since #129 PR-C — the operator-typed ephemeral-storage budget on
  // mock machines, server-stamped on GCE. Older Machine CRs (pre-PR-C) omit it.
  ephemeralStorage?: string
}

export interface MachineSpec {
  provider: MachineProvider
  // Capacity is populated for all Phase B machines (stamped by the server on
  // gce, supplied by the user on mock). May be undefined on pre-Phase-B
  // Machine CRs that haven't been reconciled yet.
  capacity?: MachineCapacitySpec
  // Managed-provider fields. Present for gce/fake; absent for static/mock.
  machineType?: string
  diskSizeGb?: number
  spot?: boolean
  zone?: string
  profile?: string
  availabilityClass?: MachineAvailabilityClass
  location?: string
  managementMode?: MachineManagementMode
}

export interface ResolvedMachineProfile {
  id: string
  displayName?: string
  description?: string
  capacity: MachineCapacitySpec
}

/**
 * Shape used for any "amount of resources" field — Spec.Capacity, Status.Allocatable,
 * Status.ObservedCapacity, Status.AssignableCapacity, Status.AvailableCapacity.
 * Backend uses kyberv1.MachineCapacity for the same role.
 *
 * `ephemeralStorage` is a resource.Quantity string (e.g. "190Gi") and was
 * added to the wire by #129 PR-C; older machines / pre-PR-C clusters omit it.
 */
export interface MachineCapacityFields {
  cpu?: string
  memory?: string
  ephemeralStorage?: string
}

/** @deprecated Use MachineCapacityFields */
export type MachineAllocatable = MachineCapacityFields

export interface MachineStatus {
  phase: MachinePhase
  message?: string
  instanceId?: string
  providerRef?: string
  availability?: MachineAvailability
  resolvedProfile?: ResolvedMachineProfile
  externalIP?: string
  internalIP?: string
  nodeName?: string
  agentCount?: number
  // Wire-named for backward compat; mirrors node.status.allocatable.
  // The server still emits JSON key "allocatable" — renaming the wire contract
  // is a follow-up after #140.
  allocatable?: MachineCapacityFields
  // Mirror the Go-side Machine.Status.{Observed,Assignable,Available}Capacity fields.
  // Emitted on the wire by machineToResponse since #142.
  observedCapacity?: MachineCapacityFields
  assignableCapacity?: MachineCapacityFields
  availableCapacity?: MachineCapacityFields
  availableCPU?: string
  availableMemory?: string
}

export interface Machine {
  id: string
  phase: MachinePhase
  spec: MachineSpec
  status: MachineStatus
  createdAt: string
}

export interface MachineListResponse {
  items: Machine[]
}

export interface RestartMachineAgentsResponse {
  restarted: string[]
  skipped?: { name: string; reason: string }[]
  count: number
}

// One of two shapes depending on provider. The server validates cross-field
// compatibility (e.g., rejects capacity for gce/fake). Existing-node machines
// can include `ephemeralStorage` in capacity since #129 PR-C; older clients
// that omit it still work — the field is optional on the wire. The whole
// `capacity` object is optional too since #240 — when omitted, the server
// auto-fills from node.status.allocatable.
export type CreateMachineRequest =
  | { name: string; provider: 'gce' | 'gke' | 'fake'; profile: string; diskSizeGb?: number; interruptible?: boolean; availabilityClass?: MachineAvailabilityClass; location: string; managementMode?: 'Managed' }
  | { name: string; provider: 'static' | 'mock'; capacity?: { cpu: string; memory: string; ephemeralStorage?: string }; managementMode?: 'External' }

export interface ComputeCapabilities {
  canProvision: boolean
  canDiscoverExisting: boolean
  suspendMode: 'Capacity' | 'LogicalOnly' | 'Unsupported'
  deletionMode: 'DeleteCapacity' | 'UnregisterOnly'
  supportsReliable: boolean
  supportsInterruptible: boolean
  supportsLocations: boolean
  requiresSchedulerDemand: boolean
}

export interface MachinePreflightResponse {
  valid: boolean
  resolved: {
    provider: MachineProvider
    profile?: string
    availabilityClass?: MachineAvailabilityClass
    location?: string
    managementMode: MachineManagementMode
    diskSizeGb?: number
    capacity?: MachineCapacitySpec
  }
  warnings: string[]
}

export interface MachineCandidate {
  id: string
  displayName: string
  location?: string
  capacity?: MachineCapacitySpec
}

export interface MachineCandidatesResponse {
  items: MachineCandidate[]
}

// ---- Fleet types ----

export interface FleetSummary {
  machineCount: number
  agentCount: number
  agentsByPhase: Record<string, number>
  machinesByPhase: Record<string, number>
}

// ---- Event types ----

export interface KyberEvent {
  type: string
  timestamp: string
  resource: {
    kind: string
    name: string
    phase?: string
    machine?: string
  }
}

// ---- Error types ----

export interface APIError {
  error: {
    code: string
    message: string
    field?: string
  }
}

// ---- Log stream options ----

export interface LogStreamOptions {
  follow?: boolean
  tail?: number
  /**
   * Log source. 'kubelet' (default) is the live pod-stdout tail; 'archive' is
   * the durable, off-cluster surface read for an absolute RFC3339 window
   * (kyber#431). When 'archive', `since` and `until` are required RFC3339
   * timestamps and `follow`/`tail` do not apply.
   */
  source?: 'kubelet' | 'archive'
  /** Absolute window start (RFC3339) — required when source==='archive'. */
  since?: string
  /** Absolute window end (RFC3339) — required when source==='archive'. */
  until?: string
}

/** Result of a windowed transcript read (source=transcript). */
export interface TranscriptResult {
  /** Raw newline-delimited Claude Code session JSONL for the window. */
  jsonl: string
  /** True when the control plane capped the read (X-Kyber-Log-Truncated). */
  truncated: boolean
}

export type LoggingLevel = 'debug' | 'info' | 'warn' | 'error'

export interface LoggingSettings {
  globalLevel: LoggingLevel
  componentOverrides: Record<string, LoggingLevel>
  archiveRetentionDays: number
  managedBy: 'helm'
}

export interface LoggingContainer {
  name: string
  component: string
  effectiveLevel: LoggingLevel | 'unmanaged'
  managedLevel: boolean
  init: boolean
}

export interface LoggingTarget {
  namespace: string
  pod: string
  podUid: string
  component: string
  workload: string
  agent?: string
  machine?: string
  phase: string
  sources: Array<'kubelet' | 'archive'>
  liveAvailable: boolean
  archiveAvailable: boolean
  containers: LoggingContainer[]
}

export interface LoggingTargetsResponse {
  targets: LoggingTarget[]
  selector: string
}

export interface LoggingReadOptions {
  pod: string
  podUid: string
  container: string
  component: string
  workload: string
  source?: 'kubelet' | 'archive'
  follow?: boolean
  tail?: number
  since?: string
  until?: string
}

export interface LoggingStreamResult {
  stream: ReadableStream<string>
  truncated: boolean
}

export interface LoggingExportOptions extends LoggingReadOptions {
  format: 'ndjson' | 'text'
}

export interface LoggingExportResult {
  blob: Blob
  filename: string
  truncated: boolean
}

// ---- Config types ----

// Read-only public configuration exposed by GET /api/v1/config.
// Used by the PWA to render provider-conditional forms (e.g., Create Machine
// shows different fields on mock vs gce) and to drive the Create-Agent model
// dropdown from the server's known-models list.
export type ComputeProvider = 'gce' | 'gke' | 'static' | 'fake' | 'mock' | ''

export type ModelInfo = {
  id: string
  contextWindow: number
}

export type ComputeConfig = {
  compute: {
    provider: ComputeProvider
    managed?: {
      profiles: VMType[]
      locations: string[]
      diskSizesGb: number[]
      supportsInterruptible: boolean
      capabilities?: ComputeCapabilities
    }
    // Catalog of GCE machine types accepted by the create-machine API.
    // Sourced from compute.gce.vmTypeCatalog in the chart. Server omits
    // this when provider !== 'gce', so the field is optional. Mirrors
    // ConfigVMType in pkg/api/routes_config.go.
    gceVMTypes?: VMType[]
  }
  models: ModelInfo[]
  // Public URL the control plane exposes externally (no trailing slash).
  // Used by the PWA to render full webhook URLs for inbound bindings (#208).
  // Omitted by the server when not configured — the PWA degrades gracefully.
  publicUrl?: string
  // Identity-repo settings used by the Create Agent wizard's identity
  // dropdown + collision check. RepoOwner is empty when the control
  // plane was started without KYBER_IDENTITY_REPO_OWNER.
  identity: {
    repoOwner: string
  }
}

// One installed-repo entry returned by GET /api/v1/github/repos.
// Mirrors pkg/githubapp.Repository on the wire.
export type GitHubRepo = {
  fullName: string
  description: string
  private: boolean
  defaultBranch: string
  isTemplate: boolean
}

export type GitHubReposResponse = {
  repos: GitHubRepo[]
  templates: GitHubRepo[]
}

export type GitHubRepoExistsResponse = {
  exists: boolean
}

export type VMType = {
  type: string
  // vCPU count as a string (e.g. "2"). Server-side comes from a
  // resource.Quantity so the wire format is always a string for stability.
  cpu: string
  // Kubernetes resource.Quantity for memory (e.g. "4Gi", "3750Mi").
  memory: string
}

// User-defined per-agent secrets — matches pkg/api/routes_user_secrets.go.
// See docs/design/2026-04-18-user-secrets-design.md (#75).
export type AgentSecretKind = 'kv' | 'file'

export interface AgentSecret {
  key: string
  kind: AgentSecretKind
  size: number
  sha256Prefix: string
  createdAt: string
  updatedAt: string
}

export interface AgentSecretListResponse {
  items: AgentSecret[]
}

// Token budget — matches pkg/tokenreport.Snapshot on the backend.
export type TokenUsage = {
  model: string
  tokens: {
    used: number
    limit: number
    input: number
    cacheCreation: number
    cacheRead: number
    // Cumulative output-token spend since the in-pod reporter started
    // (Claude Code) or since the rollout session started (Codex). NOT part
    // of the context window (`used` stays input + cacheCreation +
    // cacheRead). Optional for resilience against older control-planes /
    // cached payloads that don't emit it yet (same idiom as
    // contextWindowKnown).
    output?: number
  }
  percentage: number
  effortLevel: string
  speed: string
  updatedAt: string
  // #396: false when the model's context window wasn't found in the operator
  // ConfigMap (200K floor used) — the card renders the % as an estimate.
  // Optional for resilience against older control-planes / cached payloads;
  // treat only an explicit `false` as "unknown".
  contextWindowKnown?: boolean
}

// Mirrors VersionResponse from pkg/api/routes_version.go. Any of the fields
// may be empty string — the PWA Diagnostics card renders "—" for empties
// rather than hiding the row.
export interface VersionInfo {
  sha: string
  buildDate: string
  chartVersion: string
  substrate: string
}

// ---- Metrics tab types (kyber#328) ----

export interface MetricsFleetSummary {
  total: number
  working: number
  idle: number
  offline: number
}

export interface MetricsSeriesPoint {
  ts: number
  v: number
}

export interface MetricsSeries {
  labels: Record<string, string>
  points: MetricsSeriesPoint[]
}

export interface MetricsTokenUsage {
  agent: string
  model: string
  tokens: Record<string, number>
  costUsd: number
  /**
   * Fail-loud sentinel (kyber#487). `false` means the model has no entry in
   * provider-rates, so `costUsd` is a placeholder 0 the panel renders as
   * "—"/unpriced rather than a believable $0.0000. Omitted by older payloads —
   * only an explicit `false` is treated as unpriced (mirrors
   * `contextWindowKnown`).
   */
  priced?: boolean
}

export interface MetricsLastActive {
  name: string
  state: string
  lastHeartbeatAt?: string
  secondsSinceHeartbeat?: number
  stale: boolean
}

export interface MetricsNodeResources {
  node: string
  cpuPercent: number
  memUsedBytes: number
  memTotalBytes: number
  diskUsedBytes: number
  diskTotalBytes: number
}

export interface MetricsStateChange {
  agent: string
  toState: string
  count: number
}

// ---- Runtime detection (kyber#375 PR-A) ----
//
// The control-plane detection poller surfaces newly-released Claude Code
// versions + Claude models via GET /api/v1/available so the PWA picker
// stays current without a Kyber release.

export interface AvailableModel {
  id: string
  displayName: string
  contextWindow: number
  contextWindowKnown: boolean
}

export interface AvailableResponse {
  claudeCodeVersions: string[]
  codexVersions?: string[]
  models: AvailableModel[]
  codexModels?: AvailableModel[]
}

export interface AgentModelsResponse {
  models: AvailableModel[]
}

// ---- Per-agent comms channels (kyber#664) ----
//
// Mirrors commsChannelResponse in pkg/api/routes_agent_comms.go. Credentials
// are write-only on the backend: `botTokenSet` reports presence, and no
// endpoint ever returns a token.

export type CommsChannelId = 'telegram' | 'discord'

export interface CommsChannel {
  channel: CommsChannelId
  configured: boolean
  /**
   * True when the stored config differs from what the running pod has. Neither
   * channel takes effect on a live pod, and Kyber deliberately does not roll it
   * for you — that would destroy the agent's session. The operator restarts.
   */
  podRestartRequired: boolean
  botTokenSet: boolean

  // Discord only. Empty guild/channel lists mean "any"; the user allowlist is
  // never empty because the API rejects that (it would be fail-closed).
  guildIds?: string[]
  channelIds?: string[]
  allowedUserIds?: string[]
  mentionOnly?: boolean
  discordConnection?: {
    status: 'not-configured' | 'restart-required' | 'not-running' | 'starting' | 'connected' | 'degraded'
    ready: boolean
    restartCount: number
    detail?: string
  }
}

export interface CommsListResponse {
  channels: CommsChannel[]
}

export interface PutTelegramCommsRequest {
  /** Omit to re-enable a channel whose token is already stored. */
  botToken?: string
  allowedUserIds: string[]
}

export interface PutDiscordCommsRequest {
  botToken?: string
  guildIds?: string[]
  channelIds?: string[]
  allowedUserIds: string[]
  mentionOnly?: boolean
  /** Overrides the inbound binding's instruction text. Omit to keep whatever
   *  is already there, or to accept Kyber's working default on first setup. */
  action?: string
}

// ---------------------------------------------------------------------------
// Updates (GET /api/v1/updates)
// ---------------------------------------------------------------------------

/** Which stream of builds a cluster watches. */
export type UpdateChannel = 'stable' | 'main'

/** What happens when an update is found. */
export type UpdateMode = 'manual' | 'notify' | 'auto'

export interface UpdatePolicy {
  channel: UpdateChannel
  mode: UpdateMode
  /** Holds the cluster at an exact version; overrides channel and mode. */
  pinnedVersion?: string
  window?: string
  timeZone?: string
}

/** Who owns this cluster's resources, which decides whether it may self-upgrade. */
export type UpdateManagedBy = 'helm' | 'argocd' | 'unknown'

export type UpdateRunPhase = 'pending' | 'running' | 'succeeded' | 'failed'

/** One upgrade attempt. */
export interface UpdateRun {
  jobName: string
  targetVersion: string
  phase: UpdateRunPhase
  startedAt?: string
  finishedAt?: string
  message?: string
}

/**
 * Fields a policy PUT may carry. `null` clears a field — that is how the pin
 * is removed — which `Partial<UpdatePolicy>` cannot express, hence a separate
 * type rather than a cast at the call site.
 */
export interface UpdatePolicyPatch {
  channel?: UpdateChannel
  mode?: UpdateMode
  pinnedVersion?: string | null
  window?: string | null
  timeZone?: string | null
}

export interface UpdateStatus {
  currentVersion: string
  latestVersion?: string
  latestUrl?: string
  /**
   * When the latest version was published. Absent when the source does not
   * publish one — the chart registry behind the `main` channel carries no
   * date, so an unreleased build legitimately has none.
   */
  latestPublishedAt?: string
  /** True only when a newer version was positively identified. */
  updateAvailable: boolean
  policy: UpdatePolicy
  managedBy: UpdateManagedBy
  /** Whether THIS cluster may install an update (false under ArgoCD). */
  canSelfUpgrade: boolean
  /** Why it may not, in the operator's terms. Empty when it may. */
  reason?: string
  lastChecked?: string
  /** Most recent check failure, cleared on success. */
  lastError?: string
  /** Whether the control plane has an apply path wired up at all. */
  applySupported: boolean
  lastRun?: UpdateRun
}

// ---- Agent skills (read-only) ----
//
// What the agent found on its OWN filesystem, reported by kyber-skills at boot
// and on every identity sync. Read-only by design: skills are added, changed,
// and removed by talking to the agent, which writes them into its identity repo
// and pushes. There is no write path from the UI or the API.

/** Where a skill came from. */
export type AgentSkillSource = 'identity' | 'vendor' | 'platform'

/**
 * How much a finding matters. 'error' means the skill does not work; 'warning'
 * means it works but something will bite later.
 */
export type AgentSkillSeverity = 'error' | 'warning'

/** One concrete problem found with a skill, or with a runtime skills home. */
export interface AgentSkillIssue {
  /** Stable machine code, e.g. 'not_linked', 'unmanaged', 'shadowed'. */
  code: string
  severity: AgentSkillSeverity
  /** Human-readable explanation, safe to render directly. */
  detail: string
}

export interface AgentSkill {
  /** Directory name — this is what the runtime actually invokes. */
  name: string
  /** From the SKILL.md frontmatter; empty when there is none. */
  description: string
  source: AgentSkillSource
  /** Vendor package name; only set when source is 'vendor'. */
  sourcePackage?: string
  /** Repo-relative for identity/vendor skills, absolute for platform ones. */
  path: string
  /** Runtimes this skill is genuinely loadable in. Empty means it is dead. */
  linked: string[]
  issues?: AgentSkillIssue[]
}

export interface AgentSkillsSummary {
  total: number
  /** At least one error-severity issue: exists, does not work. */
  broken: number
  /** Works, but has something worth fixing. */
  warnings: number
  /** Nothing reported against it at all. */
  healthy: number
  otherIssues: number
}

export interface AgentSkills {
  agent: string
  /** When the control plane accepted the report (RFC3339). */
  reportedAt: string
  skills: AgentSkill[]
  /** Problems belonging to no single skill — stray or dangling runtime state. */
  issues: AgentSkillIssue[]
  summary: AgentSkillsSummary
}
