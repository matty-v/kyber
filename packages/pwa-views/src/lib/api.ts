// Typed fetch wrapper for the Kyber API. Cluster-bound: every method comes
// from createApiClient(cluster) and uses the cluster's baseURL + apiKey.
// No globals — multiple clusters can coexist in one React tree.

import type {
  Agent,
  AgentInboundBinding,
  AgentJob,
  AgentListResponse,
  AgentSecret,
  CodexDeviceAuthStatus,
  AgentSecretListResponse,
  CommsChannel,
  CommsChannelId,
  CommsListResponse,
  PutTelegramCommsRequest,
  PutDiscordCommsRequest,
  CreateAgentRequest,
  AgentModelsResponse,
  InboundBindingWithStats,
  InboundCreateResponse,
  InboundDebugRequest,
  InboundDebugResponse,
  InboundReplayResponse,
  InboundRotateResponse,
  SetModelRequest,
  SetResourcesRequest,
  Machine,
  MachineListResponse,
  CreateMachineRequest,
  RestartMachineAgentsResponse,
  FleetSummary,
  LogStreamOptions,
  LoggingSettings,
  LoggingTargetsResponse,
  LoggingReadOptions,
  LoggingStreamResult,
  LoggingExportOptions,
  LoggingExportResult,
  TranscriptResult,
  TokenUsage,
  AgentSkills,
  ComputeConfig,
  GitHubReposResponse,
  GitHubRepoExistsResponse,
  VersionInfo,
  MetricsFleetSummary,
  MetricsSeries,
  MetricsTokenUsage,
  MetricsLastActive,
  MetricsNodeResources,
  MetricsStateChange,
  AvailableResponse,
  UpdateStatus,
  UpdatePolicyPatch,
  UpdateRun,
} from './types'
import type { Cluster } from './cluster-context'

function loggingParams(opts: LoggingReadOptions): URLSearchParams {
  const params = new URLSearchParams({
    pod: opts.pod,
    podUid: opts.podUid,
    container: opts.container,
    component: opts.component,
    workload: opts.workload,
  })
  if (opts.source) params.set('source', opts.source)
  if (opts.follow) params.set('follow', 'true')
  if (opts.tail !== undefined) params.set('tail', String(opts.tail))
  if (opts.since) params.set('since', opts.since)
  if (opts.until) params.set('until', opts.until)
  return params
}

const LEGACY_STORAGE_KEY_API_KEY = 'kyber_api_key'
const STORAGE_KEY_SERVER_URL = 'kyber_server_url'
const STORAGE_KEY_LAST_API_CALL = 'kyber_last_api_call_ts'

// One-time cleanup (kyber#123): the Server URL field has been removed from
// the Settings UI. Any stored value is either redundant (pointed at the
// current origin, now identical to the same-origin fallthrough in baseUrl())
// or stale (pointed at a prior tunnel / preview URL that no longer works).
// Either way the right behavior post-PR is to drop it so baseUrl() uses
// same-origin unconditionally. Safe because there is no UI left that
// re-populates the key.
//
// Deviation from issue #123's D5: the original rule only cleaned when
// stored === window.location.origin, which misses the stale-cross-origin
// case (the "tunnel flip" example in the issue body). Unconditional wipe
// was approved over Telegram 2026-04-21.
if (typeof localStorage !== 'undefined' && localStorage.getItem(STORAGE_KEY_SERVER_URL) !== null) {
  localStorage.removeItem(STORAGE_KEY_SERVER_URL)
}

class KyberAPIError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message)
    this.name = 'KyberAPIError'
  }
}

// Error code the control plane returns when a browser-session cookie is no
// longer valid (pkg/api/auth.go ErrCodeSessionExpired). Distinct from the
// generic "unauthorized" precisely so this case can be recovered in place.
export const ERR_SESSION_EXPIRED = 'session_expired'

// Subscribers notified when the control plane reports an expired browser
// session. A module-level notifier rather than React state because the 401
// can surface from any query, mutation, or poll in the tree, and every one
// of them should produce the same single prompt.
const sessionExpiredListeners = new Set<() => void>()

/**
 * Subscribe to browser-session expiry. Returns an unsubscribe function.
 *
 * Only meaningful for the embedded app, which authenticates with the
 * HttpOnly cookie. Hub mode sends a bearer key on every request, so the
 * control plane never emits session_expired there.
 */
export function onSessionExpired(listener: () => void): () => void {
  sessionExpiredListeners.add(listener)
  return () => {
    sessionExpiredListeners.delete(listener)
  }
}

function notifySessionExpired(): void {
  for (const listener of sessionExpiredListeners) {
    try {
      listener()
    } catch {
      // A throwing subscriber must not stop the others from being told, and
      // must not turn an auth prompt into an unhandled rejection.
    }
  }
}

export type ApiClient = ReturnType<typeof createApiClient>

export function createApiClient(cluster: Cluster) {
  const baseURL = cluster.baseURL.replace(/\/$/, '')

  async function request<T>(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<T> {
    const url = `${baseURL}${path}`
    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
    }
    if (cluster.apiKey) {
      headers['Authorization'] = `Bearer ${cluster.apiKey}`
    }
    const res = await fetch(url, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })

    if (!res.ok) {
      let code = 'UNKNOWN'
      let message = `HTTP ${res.status}`
      try {
        const err = (await res.json()) as { error?: { code?: string; message?: string } }
        code = err.error?.code ?? code
        message = err.error?.message ?? message
      } catch {
        // ignore parse failure
      }
      // Announce a dead or missing browser session before throwing, so the
      // embedded app can put up a re-auth prompt instead of leaving the
      // caller's error to render as a dead end. A stale cookie produces the
      // dedicated session_expired code, but once a cookie reaches its Max-Age
      // the browser removes it and the server can only report a generic 401.
      // An empty cluster API key identifies embedded cookie-auth mode; bearer
      // clients must not turn a genuinely invalid key into a session prompt.
      // Still throws so callers keep their existing error handling.
      if (res.status === 401 && (code === ERR_SESSION_EXPIRED || !cluster.apiKey)) {
        notifySessionExpired()
      }
      throw new KyberAPIError(res.status, code, message)
    }

    bumpLastApiCall()

    // 204 No Content
    if (res.status === 204) {
      return undefined as T
    }

    return res.json() as Promise<T>
  }

  return {
    // ---- Agents ----

    listAgents: async (): Promise<Agent[]> => {
      const res = await request<AgentListResponse>('GET', '/api/v1/agents')
      return res.items
    },

    getAgent: (name: string): Promise<Agent> =>
      request<Agent>('GET', `/api/v1/agents/${name}`),

    createAgent: (req: CreateAgentRequest): Promise<Agent> =>
      request<Agent>('POST', '/api/v1/agents', req),

    patchAgent: (name: string, req: import('./types').PatchAgentRequest): Promise<Agent> =>
      request<Agent>('PATCH', `/api/v1/agents/${encodeURIComponent(name)}`, req),

    getAgentModels: (name: string): Promise<AgentModelsResponse> =>
      request<AgentModelsResponse>('GET', `/api/v1/agents/${encodeURIComponent(name)}/models`),

    // Rotate the control-plane API key (#143). Returns the new key, which the
    // caller is responsible for storing in localStorage and showing to the
    // operator once. Other browser sessions hit 401 on next request.
    rotateApiKey: (): Promise<{ apiKey: string }> =>
      request<{ apiKey: string }>('POST', '/api/v1/rotate-api-key'),

    // Anthropic API key — operator-supplied credential the detection
    // poller uses for the Anthropic Models API (kyber#375 PR-A). The
    // server stores the key in a control-plane-namespace Secret and
    // NEVER returns it via API. getAnthropicKey only reveals whether
    // a key is configured.
    // `supported` says whether this control plane can store a key at all
    // (false when no anthropic-key Secret is configured, i.e. model discovery
    // is off); `configured` says whether one is currently set. Optional so a
    // newer PWA against an older control plane, which omits it, keeps today's
    // behaviour instead of hiding the field.
    getAnthropicKey: (): Promise<{ supported?: boolean; configured: boolean }> =>
      request<{ supported?: boolean; configured: boolean }>('GET', '/api/v1/settings/anthropic-key'),
    setAnthropicKey: (key: string): Promise<void> =>
      request<void>('PUT', '/api/v1/settings/anthropic-key', { key }),
    clearAnthropicKey: (): Promise<void> =>
      request<void>('DELETE', '/api/v1/settings/anthropic-key'),

    // Updates. getUpdates carries everything the card renders — status, policy,
    // ownership and the last run — so an upgrade in flight is followed by
    // polling one endpoint rather than three.
    getUpdates: (): Promise<UpdateStatus> =>
      request<UpdateStatus>('GET', '/api/v1/updates'),
    setUpdatePolicy: (patch: UpdatePolicyPatch): Promise<UpdateStatus> =>
      request<UpdateStatus>('PUT', '/api/v1/updates/policy', patch),
    // Synchronous on the server: an operator pressing "Check now" wants the
    // answer, not an acknowledgement to poll behind.
    checkUpdates: (): Promise<UpdateStatus> =>
      request<UpdateStatus>('POST', '/api/v1/updates/check'),
    // 202 — the Job has been created, nothing is installed yet. Progress comes
    // from getUpdates().lastRun.
    applyUpdate: (version?: string): Promise<UpdateRun> =>
      request<UpdateRun>('POST', '/api/v1/updates/apply', version ? { version } : {}),

    // Detection poller output (kyber#375 PR-A). PR-D layers the
    // operator override map onto this.
    getAvailable: (): Promise<AvailableResponse> =>
      request<AvailableResponse>('GET', '/api/v1/available'),

    startAgent: (name: string): Promise<void> =>
      request<void>('POST', `/api/v1/agents/${name}/start`),

    stopAgent: (name: string): Promise<void> =>
      request<void>('POST', `/api/v1/agents/${name}/stop`),

    restartAgent: (name: string): Promise<void> =>
      request<void>('POST', `/api/v1/agents/${name}/restart`),

    // restartAgentSession fires POST /restart-session (#128). Server-side 429
    // cooldown surfaces as a KyberAPIError with status=429 — callers translate
    // to a friendlier toast in the mutation hook's meta.errorPrefix.
    restartAgentSession: (name: string): Promise<RestartSessionResponse> =>
      request<RestartSessionResponse>(
        'POST',
        `/api/v1/agents/${encodeURIComponent(name)}/restart-session`,
      ),

    // compactAgentSession fires POST /compact-session. Same error shape as
    // restartAgentSession, plus a 501 when the agent's runtime has no
    // compaction command — callers should treat that as "not available on
    // this runtime", not as a failure to report loudly.
    compactAgentSession: (name: string): Promise<CompactSessionResponse> =>
      request<CompactSessionResponse>(
        'POST',
        `/api/v1/agents/${encodeURIComponent(name)}/compact-session`,
      ),

    // forceNeedsAuthAgent drops a wedged agent to NeedsAuth (deleting any live
    // pod) so it can be re-authorized from scratch (#395).
    forceNeedsAuthAgent: (name: string): Promise<void> =>
      request<void>('POST', `/api/v1/agents/${name}/force-needs-auth`),

    repairAgentRuntime: (name: string): Promise<RuntimeRepairResponse> =>
      request<RuntimeRepairResponse>('POST', `/api/v1/agents/${encodeURIComponent(name)}/repair-runtime`),

    setAgentModel: (name: string, model: string): Promise<Agent> => {
      const req: SetModelRequest = { model }
      return request<Agent>('POST', `/api/v1/agents/${name}/set-model`, req)
    },

    // setAgentRuntimeVersion sets spec.runtimeVersion + triggers a pod
    // roll for Running agents (kyber#378 PR-D wires the PWA call site;
    // the endpoint validates the same charset pattern as the CRD).
    setAgentRuntimeVersion: (name: string, runtimeVersion: string): Promise<Agent> =>
      request<Agent>(
        'POST',
        `/api/v1/agents/${encodeURIComponent(name)}/set-runtime-version`,
        { runtimeVersion },
      ),

    setAgentResources: (name: string, body: SetResourcesRequest): Promise<Agent> =>
      request<Agent>('POST', `/api/v1/agents/${name}/set-resources`, body),

    // kyber#565: DELETE requires the ?confirm=<name> interlock. The human
    // confirmation happens in the UI dialog before this is called; here we pass
    // the matching token so the request reaches the (gated) delete path.
    deleteAgent: (name: string): Promise<void> =>
      request<void>('DELETE', `/api/v1/agents/${name}?confirm=${encodeURIComponent(name)}`),

    // ---- Agent jobs (#135) ----

    // patchAgentJobs replaces spec.jobs wholesale. Pass [] to clear all jobs.
    // Validation (name regex, 5-field schedule, duplicates, non-empty prompt)
    // is server-side; errors surface as 400 via KyberAPIError.
    patchAgentJobs: (name: string, jobs: AgentJob[]): Promise<Agent> =>
      request<Agent>('PATCH', `/api/v1/agents/${encodeURIComponent(name)}`, { jobs }),

    // runAgentJob invokes the named job out-of-band via kubectl exec → dispatcher.
    // Server responds with 202 + captured stdout/stderr. The canonical outcome
    // lands in status.jobs[] shortly after the dispatch completes.
    runAgentJob: (name: string, jobName: string): Promise<RunAgentJobResponse> =>
      request<RunAgentJobResponse>(
        'POST',
        `/api/v1/agents/${encodeURIComponent(name)}/jobs/${encodeURIComponent(jobName)}/run`,
      ),

    reauthorizeAgent: (
      name: string,
      body: { oauthCode: string; pkceVerifier: string; state: string },
    ): Promise<void> =>
      request<void>('POST', `/api/v1/agents/${encodeURIComponent(name)}/oauth`, body),

    startCodexDeviceAuth: (name: string): Promise<void> =>
      request<void>('POST', `/api/v1/agents/${encodeURIComponent(name)}/codex-device-auth`),

    getCodexDeviceAuthStatus: (name: string): Promise<CodexDeviceAuthStatus> =>
      request<CodexDeviceAuthStatus>(
        'GET',
        `/api/v1/agents/${encodeURIComponent(name)}/codex-device-auth`,
      ),

    getTokenUsage: async (name: string): Promise<TokenUsage | null> => {
      const headers: Record<string, string> = {}
      if (cluster.apiKey) headers['Authorization'] = `Bearer ${cluster.apiKey}`
      const res = await fetch(
        `${baseURL}/api/v1/agents/${encodeURIComponent(name)}/token-usage`,
        { headers },
      )
      if (res.status === 404) return null
      if (!res.ok) throw new Error(`token-usage: ${res.status}`)
      return res.json() as Promise<TokenUsage>
    },

    // ---- Agent skills (read-only) ----
    //
    // 404 means the agent has never reported, which is different from "has no
    // skills" — a pod that has not booted since this shipped, or an install
    // with no identity repo. Returning null keeps those two distinguishable in
    // the UI instead of both rendering as an empty list.
    getAgentSkills: async (name: string): Promise<AgentSkills | null> => {
      const headers: Record<string, string> = {}
      if (cluster.apiKey) headers['Authorization'] = `Bearer ${cluster.apiKey}`
      const res = await fetch(
        `${baseURL}/api/v1/agents/${encodeURIComponent(name)}/skills`,
        { headers },
      )
      if (res.status === 404) return null
      if (!res.ok) throw new Error(`skills: ${res.status}`)
      return res.json() as Promise<AgentSkills>
    },

    // ---- Inbound prompt bindings (#208) ----
    // Operator UI for declaring HMAC-authenticated webhooks. The receiver
    // at /webhooks/inbound/{agent}/{binding} is governed by these specs.

    listInboundBindings: async (agentName: string): Promise<InboundBindingWithStats[]> => {
      const res = await request<{ bindings: InboundBindingWithStats[] }>(
        'GET',
        `/api/v1/agents/${encodeURIComponent(agentName)}/inbound-bindings`,
      )
      return res.bindings
    },

    createInboundBinding: (
      agentName: string,
      body: CreateInboundBindingRequest,
    ): Promise<InboundCreateResponse> =>
      request<InboundCreateResponse>(
        'POST',
        `/api/v1/agents/${encodeURIComponent(agentName)}/inbound-bindings`,
        body,
      ),

    deleteInboundBinding: (agentName: string, bindingName: string): Promise<void> =>
      request<void>(
        'DELETE',
        `/api/v1/agents/${encodeURIComponent(agentName)}/inbound-bindings/${encodeURIComponent(bindingName)}`,
      ),

    // PATCH the editable subset of a binding's spec. Returns the full
    // updated binding (same shape as listInboundBindings entries) so the
    // caller can refresh without a follow-up GET.
    updateInboundBinding: (
      agentName: string,
      bindingName: string,
      body: UpdateInboundBindingRequest,
    ): Promise<InboundBindingWithStats> =>
      request<InboundBindingWithStats>(
        'PATCH',
        `/api/v1/agents/${encodeURIComponent(agentName)}/inbound-bindings/${encodeURIComponent(bindingName)}`,
        body,
      ),

    rotateInboundSecret: (
      agentName: string,
      bindingName: string,
    ): Promise<InboundRotateResponse> =>
      request<InboundRotateResponse>(
        'POST',
        `/api/v1/agents/${encodeURIComponent(agentName)}/inbound-bindings/${encodeURIComponent(bindingName)}/rotate-secret`,
      ),

    // Replay a previously-logged inbound run. Backend looks up the cached
    // envelope by requestId, bypasses dedup, and re-dispatches. Status codes the
    // PWA cares about:
    //   202 → queued (returns InboundReplayResponse)
    //   410 → envelope expired (PWA toasts "older than 7 days can't be replayed")
    //   429 → queue full
    //   404 → unknown binding or requestId
    // Non-202 errors surface as KyberAPIError; the caller's mutation hook
    // translates the status into operator-friendly toasts.
    replayInboundRun: (
      agentName: string,
      bindingName: string,
      requestId: string,
    ): Promise<InboundReplayResponse> =>
      request<InboundReplayResponse>(
        'POST',
        `/api/v1/agents/${encodeURIComponent(agentName)}/inbound-bindings/${encodeURIComponent(bindingName)}/replay/${encodeURIComponent(requestId)}`,
      ),

    // Run a binding spec against an arbitrary payload without dispatching. Used
    // by the Webhooks tab debugger and the AddWebhookWizard "Test payload"
    // affordance to preview filter/field/event evaluation.
    inboundDebug: (req: InboundDebugRequest): Promise<InboundDebugResponse> =>
      request<InboundDebugResponse>('POST', '/api/v1/inbound-debug', req),

    // ---- Per-agent comms channels (#664) ----
    // Configure Telegram / two-way Discord on an existing agent. PUT is
    // idempotent — a channel is a singleton per agent, so configure and update
    // are the same call. Tokens go in and never come back out.

    listAgentComms: async (agentName: string): Promise<CommsChannel[]> => {
      const res = await request<CommsListResponse>(
        'GET',
        `/api/v1/agents/${encodeURIComponent(agentName)}/comms`,
      )
      return res.channels
    },

    putTelegramComms: (
      agentName: string,
      body: PutTelegramCommsRequest,
    ): Promise<CommsChannel> =>
      request<CommsChannel>(
        'PUT',
        `/api/v1/agents/${encodeURIComponent(agentName)}/comms/telegram`,
        body,
      ),

    putDiscordComms: (
      agentName: string,
      body: PutDiscordCommsRequest,
    ): Promise<CommsChannel> =>
      request<CommsChannel>(
        'PUT',
        `/api/v1/agents/${encodeURIComponent(agentName)}/comms/discord`,
        body,
      ),

    deleteAgentComms: (agentName: string, channel: CommsChannelId): Promise<void> =>
      request<void>(
        'DELETE',
        `/api/v1/agents/${encodeURIComponent(agentName)}/comms/${channel}`,
      ),

    // ---- User-defined per-agent secrets (#75) ----

    listAgentSecrets: async (name: string): Promise<AgentSecret[]> => {
      const res = await request<AgentSecretListResponse>(
        'GET',
        `/api/v1/agents/${encodeURIComponent(name)}/secrets`,
      )
      return res.items
    },

    // Readback returns text/plain for kv and application/octet-stream for file.
    // For files we surface a Blob so the caller can offer a download.
    getAgentSecretValue: async (
      name: string,
      key: string,
    ): Promise<{ kind: 'kv'; value: string } | { kind: 'file'; value: Blob; filename: string }> => {
      const url = `${baseURL}/api/v1/agents/${encodeURIComponent(name)}/secrets/${encodeURIComponent(key)}`
      const headers: Record<string, string> = {}
      if (cluster.apiKey) headers['Authorization'] = `Bearer ${cluster.apiKey}`
      const res = await fetch(url, { headers })
      if (!res.ok) {
        let code = 'UNKNOWN'
        let message = `HTTP ${res.status}`
        try {
          const err = (await res.json()) as { error?: { code?: string; message?: string } }
          code = err.error?.code ?? code
          message = err.error?.message ?? message
        } catch {
          // ignore
        }
        throw new KyberAPIError(res.status, code, message)
      }
      const ct = res.headers.get('Content-Type') ?? ''
      if (ct.startsWith('text/plain')) {
        return { kind: 'kv', value: await res.text() }
      }
      // Derive filename from Content-Disposition if present; fall back to `<key>.bin`.
      const disp = res.headers.get('Content-Disposition') ?? ''
      const match = /filename="?([^";]+)"?/i.exec(disp)
      const filename = match ? match[1] : `${key}.bin`
      return { kind: 'file', value: await res.blob(), filename }
    },

    putAgentSecretKV: (name: string, key: string, value: string): Promise<void> =>
      request<void>(
        'PUT',
        `/api/v1/agents/${encodeURIComponent(name)}/secrets/${encodeURIComponent(key)}`,
        { kind: 'kv', value },
      ),

    putAgentSecretFile: async (name: string, key: string, file: File): Promise<void> => {
      const url = `${baseURL}/api/v1/agents/${encodeURIComponent(name)}/secrets/${encodeURIComponent(key)}`
      const form = new FormData()
      form.append('file', file, file.name)
      const headers: Record<string, string> = {}
      if (cluster.apiKey) headers['Authorization'] = `Bearer ${cluster.apiKey}`
      // Don't set Content-Type — fetch fills in the multipart boundary.
      const res = await fetch(url, {
        method: 'PUT',
        headers,
        body: form,
      })
      if (!res.ok) {
        let code = 'UNKNOWN'
        let message = `HTTP ${res.status}`
        try {
          const err = (await res.json()) as { error?: { code?: string; message?: string } }
          code = err.error?.code ?? code
          message = err.error?.message ?? message
        } catch {
          // ignore
        }
        throw new KyberAPIError(res.status, code, message)
      }
    },

    deleteAgentSecret: (name: string, key: string): Promise<void> =>
      request<void>(
        'DELETE',
        `/api/v1/agents/${encodeURIComponent(name)}/secrets/${encodeURIComponent(key)}`,
      ),

    // ---- Machines ----

    listMachines: async (): Promise<Machine[]> => {
      const res = await request<MachineListResponse>('GET', '/api/v1/machines')
      return res.items
    },

    getMachine: (name: string): Promise<Machine> =>
      request<Machine>('GET', `/api/v1/machines/${name}`),

    createMachine: (req: CreateMachineRequest): Promise<Machine> =>
      request<Machine>('POST', '/api/v1/machines', req),

    startMachine: (name: string): Promise<void> =>
      request<void>('POST', `/api/v1/machines/${name}/start`),

    stopMachine: (name: string): Promise<void> =>
      request<void>('POST', `/api/v1/machines/${name}/stop`),

    rebootMachine: (name: string): Promise<void> =>
      request<void>('POST', `/api/v1/machines/${name}/reboot`),

    // kyber#565: machine DELETE requires the same ?confirm=<name> interlock.
    deleteMachine: (name: string): Promise<void> =>
      request<void>('DELETE', `/api/v1/machines/${name}?confirm=${encodeURIComponent(name)}`),

    restartMachineAgents: (name: string): Promise<RestartMachineAgentsResponse> =>
      request<RestartMachineAgentsResponse>(
        'POST',
        `/api/v1/machines/${encodeURIComponent(name)}/restart-agents`,
      ),

    retryCostOptimizedMachine: (name: string, requestId: string): Promise<Machine> =>
      request<Machine>(
        'POST',
        `/api/v1/machines/${encodeURIComponent(name)}/retry-cost-optimized`,
        { requestId },
      ),

    // ---- Fleet ----

    getFleetSummary: (): Promise<FleetSummary> =>
      request<FleetSummary>('GET', '/api/v1/fleet'),

    // ---- Config ----

    getComputeConfig: (): Promise<ComputeConfig> =>
      request<ComputeConfig>('GET', '/api/v1/config'),

    getLoggingSettings: (): Promise<LoggingSettings> =>
      request<LoggingSettings>('GET', '/api/v1/logging/settings'),

    getLoggingTargets: (): Promise<LoggingTargetsResponse> =>
      request<LoggingTargetsResponse>('GET', '/api/v1/logging/targets'),

    loggingStream: async (opts: LoggingReadOptions): Promise<LoggingStreamResult> => {
      const params = loggingParams(opts)
      const url = `${baseURL}/api/v1/logging/logs?${params}`
      const headers: HeadersInit = cluster.apiKey
        ? { Authorization: `Bearer ${cluster.apiKey}` }
        : {}
      const controller = new AbortController()
      const res = await fetch(url, { headers, signal: controller.signal })
      if (!res.ok || !res.body) throw new Error(`log read failed: HTTP ${res.status}`)
	  const body = res.body
      return { truncated: res.headers.get('X-Kyber-Log-Truncated') === 'true', stream: new ReadableStream<string>({
        async start(streamController) {
          try {
            const reader = body.getReader()
            const decoder = new TextDecoder()
            while (true) {
              const { done, value } = await reader.read()
              if (done) break
              streamController.enqueue(decoder.decode(value, { stream: true }))
            }
            streamController.close()
          } catch (err) {
            streamController.error(err)
          }
        },
        cancel() { controller.abort() },
      }) }
    },

    exportLogging: async (opts: LoggingExportOptions): Promise<LoggingExportResult> => {
      const params = loggingParams(opts)
      params.set('format', opts.format)
      const headers: HeadersInit = cluster.apiKey
        ? { Authorization: `Bearer ${cluster.apiKey}` }
        : {}
      const res = await fetch(`${baseURL}/api/v1/logging/export?${params}`, { headers })
      if (!res.ok) throw new Error(`log export failed: HTTP ${res.status}`)
      const disposition = res.headers.get('Content-Disposition') ?? ''
      const filename = /filename="?([^";]+)"?/.exec(disposition)?.[1]
        ?? `kyber-logs.${opts.format === 'text' ? 'log' : 'ndjson'}`
      return {
        blob: await res.blob(),
        filename,
        truncated: res.headers.get('X-Kyber-Log-Truncated') === 'true',
      }
    },

    // ---- GitHub (#134) ----

    listGitHubRepos: (): Promise<GitHubReposResponse> =>
      request<GitHubReposResponse>('GET', '/api/v1/github/repos'),

    checkGitHubRepoExists: (owner: string, name: string): Promise<GitHubRepoExistsResponse> =>
      request<GitHubRepoExistsResponse>(
        'GET',
        `/api/v1/github/repos/${encodeURIComponent(owner)}/${encodeURIComponent(name)}/exists`,
      ),

    // ---- Version + health (Diagnostics card) ----

    getVersion: (): Promise<VersionInfo> =>
      request<VersionInfo>('GET', '/api/v1/version'),

    // Unauthenticated ping to /healthz. Returns true on 200, false on anything
    // else (network error, non-200, etc.). Deliberately does NOT bump
    // the last-API-call timestamp: the Diagnostics card polls /healthz every
    // 30s on its own, which would peg "last successful API call" at a
    // constant "just now" and rob it of its signal value.
    checkHealthz: async (): Promise<boolean> => {
      try {
        const res = await fetch(`${baseURL}/healthz`, { method: 'GET' })
        return res.ok
      } catch {
        return false
      }
    },

    // ---- Streaming ----

    // Returns a ReadableStream of text chunks from the log endpoint.
    // The backend uses chunked HTTP with Content-Type: text/plain (not SSE).
    logStream: (
      kind: 'agent' | 'machine',
      name: string,
      opts: LogStreamOptions = {},
    ): ReadableStream<string> => {
      const params = new URLSearchParams()
      if (opts.source === 'archive') {
        // Durable absolute-window read (kyber#431): source + RFC3339 window.
        // follow/tail do not apply to the archive surface.
        params.set('source', 'archive')
        if (opts.since !== undefined) params.set('since', opts.since)
        if (opts.until !== undefined) params.set('until', opts.until)
      } else {
        if (opts.follow) params.set('follow', 'true')
        if (opts.tail !== undefined) params.set('tail', String(opts.tail))
      }
      const qs = params.toString()
      const url = `${baseURL}/api/v1/${kind}s/${name}/logs${qs ? `?${qs}` : ''}`

      const headers: HeadersInit = {}
      if (cluster.apiKey) {
        (headers as Record<string, string>)['Authorization'] = `Bearer ${cluster.apiKey}`
      }

      let controller: AbortController | null = null

      return new ReadableStream<string>({
        async start(streamController) {
          controller = new AbortController()
          let res: Response
          try {
            res = await fetch(url, { headers, signal: controller.signal })
          } catch (err) {
            streamController.error(err)
            return
          }
          if (!res.ok || !res.body) {
            streamController.error(new Error(`log stream failed: HTTP ${res.status}`))
            return
          }
          const reader = res.body.getReader()
          const decoder = new TextDecoder()
          try {
            while (true) {
              const { done, value } = await reader.read()
              if (done) break
              streamController.enqueue(decoder.decode(value, { stream: true }))
            }
            streamController.close()
          } catch (err) {
            streamController.error(err)
          }
        },
        cancel() {
          controller?.abort()
        },
      })
    },

    // One-shot windowed read of the agent's structured session transcript
    // (source=transcript, kyber#446). Unlike logStream this uses a plain fetch
    // so we can read the X-Kyber-Log-Truncated response header.
    fetchTranscript: async (
      name: string,
      since: string,
      until: string,
    ): Promise<TranscriptResult> => {
      const params = new URLSearchParams({ source: 'transcript', since, until })
      const url = `${baseURL}/api/v1/agents/${name}/logs?${params.toString()}`
      const headers: HeadersInit = {}
      if (cluster.apiKey) {
        (headers as Record<string, string>)['Authorization'] = `Bearer ${cluster.apiKey}`
      }
      const res = await fetch(url, { headers })
      if (!res.ok) throw new Error(`transcript read failed: HTTP ${res.status}`)
      const jsonl = await res.text()
      return { jsonl, truncated: res.headers.get('X-Kyber-Log-Truncated') === 'true' }
    },

    // Returns a WebSocket connected to the events endpoint.
    // Auth is passed as ?token= because browsers cannot set Authorization headers
    // on WebSocket upgrades.
    eventStream: (): WebSocket => {
      const wsBase = baseURL.replace(/^http/i, 'ws')
      const qs = cluster.apiKey ? `?token=${encodeURIComponent(cluster.apiKey)}` : ''
      return new WebSocket(`${wsBase}/api/v1/events${qs}`)
    },

    // Returns a WebSocket for an exec session into an agent or machine.
    execWebSocket: (
      kind: 'agent' | 'machine',
      name: string,
      mode?: 'attach' | 'shell' | 'history' | 'device-auth',
    ): WebSocket => {
      const wsBase = baseURL.replace(/^http/i, 'ws')
      const params = new URLSearchParams()
      if (cluster.apiKey) params.set('token', cluster.apiKey)
      if (mode) params.set('mode', mode)
      const qs = params.toString()
      return new WebSocket(`${wsBase}/api/v1/${kind}s/${name}/exec${qs ? '?' + qs : ''}`)
    },

    // ---- Metrics (#328) ----

    getMetricsSummary: (): Promise<MetricsFleetSummary> =>
      request<MetricsFleetSummary>('GET', '/api/v1/metrics/summary'),

    getMetricsActivity: (start: number, end: number): Promise<MetricsSeries[]> =>
      request<MetricsSeries[]>('GET', `/api/v1/metrics/activity?start=${start}&end=${end}`),

    getMetricsWorkingTime: (start: number, end: number): Promise<MetricsSeries[]> =>
      request<MetricsSeries[]>('GET', `/api/v1/metrics/working-time?start=${start}&end=${end}`),

    getMetricsTokens: (start: number, end: number): Promise<MetricsTokenUsage[]> =>
      request<MetricsTokenUsage[]>('GET', `/api/v1/metrics/tokens?start=${start}&end=${end}`),

    getMetricsLastActive: (): Promise<MetricsLastActive[]> =>
      request<MetricsLastActive[]>('GET', '/api/v1/metrics/last-active'),

    getMetricsNodes: (): Promise<MetricsNodeResources[]> =>
      request<MetricsNodeResources[]>('GET', '/api/v1/metrics/nodes'),

    getMetricsStateChanges: (start: number, end: number): Promise<MetricsStateChange[]> =>
      request<MetricsStateChange[]>('GET', `/api/v1/metrics/state-changes?start=${start}&end=${end}`),

    // Fleet defaults (kyber#376 / PR-B). GET returns the current
    // kyber-fleet-defaults ConfigMap data; PUT replaces both keys.
    // Empty strings are valid ("not configured" — the reconciler then
    // requires every agent to set spec.model explicitly).
    getFleetDefaults: (): Promise<FleetDefaults> =>
      request<FleetDefaults>('GET', '/api/v1/fleet-defaults'),

    putFleetDefaults: (body: FleetDefaults & { force?: boolean }): Promise<FleetDefaults> =>
      request<FleetDefaults>('PUT', '/api/v1/fleet-defaults', body),
  }
}

export interface FleetDefaults {
  defaultModel: string
  defaultRuntimeVersion: string
  codexDefaultModel?: string
  codexDefaultRuntimeVersion?: string
}

// Exchange a bearer API key for the embedded app's opaque HttpOnly session.
// The raw key is deliberately not retained in browser-readable storage.
export async function establishEmbeddedBrowserSession(key: string): Promise<void> {
  const res = await fetch('/api/v1/browser-session', {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}` },
  })
  if (!res.ok) throw new Error(`browser session failed: HTTP ${res.status}`)
}

// One-time migration for browsers that predate HttpOnly sessions. Read the
// legacy value once, remove it before the network call, and never write it
// back even if exchange fails.
export function takeLegacyEmbeddedApiKey(): string {
  const key = localStorage.getItem(LEGACY_STORAGE_KEY_API_KEY) ?? ''
  localStorage.removeItem(LEGACY_STORAGE_KEY_API_KEY)
  return key
}

export function getLastApiCall(): string | null {
  return localStorage.getItem(STORAGE_KEY_LAST_API_CALL)
}

function bumpLastApiCall(): void {
  try {
    localStorage.setItem(STORAGE_KEY_LAST_API_CALL, new Date().toISOString())
  } catch {
    // localStorage quota exceeded or unavailable — silent no-op. The
    // Diagnostics card handles a missing timestamp gracefully.
  }
}

// Body type for createInboundBinding — the binding spec without the existingSecret
// reference (the server creates the Kubernetes Secret on the operator's behalf
// and returns the secret value once in InboundCreateResponse).
export type CreateInboundBindingRequest = Omit<AgentInboundBinding, 'existingSecret'>

// Body type for updateInboundBinding (#222). All fields are optional —
// only fields the caller sends are applied. `name` and `existingSecret`
// are intentionally absent from the type because they're read-only on
// PATCH; renaming requires DELETE + POST and rotation has its own
// endpoint.
export type UpdateInboundBindingRequest = Partial<
  Omit<AgentInboundBinding, 'name' | 'existingSecret'>
>

// restartAgentSession response type (#128).
export interface RestartSessionResponse {
  agent: string
  runtime: string
  stdout: string
  stderr: string
}

// compactAgentSession response type. `delivered` is deliberately not the
// same claim as "compacted": the server pastes the slash command into the
// running runtime and returns — compaction itself finishes later.
export interface CompactSessionResponse {
  agent: string
  runtime: string
  stdout: string
  stderr: string
  delivered: boolean
  detail: string
}

export interface RuntimeRepairResponse {
  agent: string
  runtime: string
  message: string
  output: string
}

// runAgentJob response type.
export interface RunAgentJobResponse {
  agent: string
  job: string
  stdout: string
  stderr: string
}
