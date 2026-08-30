// React Query wrappers for all Kyber API calls.

import { useMemo } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import { parseTranscript } from '../lib/transcript'
import type {
  AgentJob,
  CommsChannelId,
  CreateAgentRequest,
  CreateMachineRequest,
  InboundDebugRequest,
  PutDiscordCommsRequest,
  PutTelegramCommsRequest,
  SetResourcesRequest,
  UpdatePolicyPatch,
} from '../lib/types'
import type {
  CreateInboundBindingRequest,
  FleetDefaults,
  UpdateInboundBindingRequest,
} from '../lib/api'

// ---- Fleet ----

export function useFleetSummary() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'fleet'],
    queryFn: () => api.getFleetSummary(),
    refetchInterval: 30000,
  })
}

// ---- Agents ----

export function useAgents() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'agents'],
    queryFn: () => api.listAgents(),
    refetchInterval: 30000,
  })
}

export function useAgent(name: string) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'agents', name],
    queryFn: () => api.getAgent(name),
    enabled: Boolean(name),
    refetchInterval: 15000,
  })
}

export function useTokenUsage(name: string, enabled: boolean) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'tokenUsage', name],
    queryFn: () => api.getTokenUsage(name),
    enabled: enabled && Boolean(name),
    refetchInterval: 30000,
    staleTime: 20000,
    // 404 → null is a valid return value, not an error
    retry: false,
  })
}

// Structured session transcript for the last `windowDays` days, parsed into
// Session[] for the History view. A one-shot windowed read; the caller widens the
// window via "Load earlier" (see AgentHistory), which re-fetches because
// windowDays is part of the query key.
//
// The default is ONE day, not seven (kyber#669). A busy agent's 7-day transcript
// measured 84.7 MB in production — the fetch alone peaked ~330 MB of browser
// memory and the panel never rendered. The server caps any single response, but
// asking for a day up front is what keeps the common case small; widening is a
// deliberate click.
export function useAgentTranscript(name: string, windowDays = 1) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['agent-transcript', cluster.id, name, windowDays],
    queryFn: async () => {
      const until = new Date()
      const since = new Date(until.getTime() - windowDays * 24 * 60 * 60 * 1000)
      const { jsonl, truncated } = await api.fetchTranscript(name, since.toISOString(), until.toISOString())
      return { sessions: parseTranscript(jsonl), truncated }
    },
  })
}

export function useCreateAgent() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: CreateAgentRequest) => api.createAgent(req),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'fleet'] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Agent ${(v as CreateAgentRequest).name} created`,
      errorPrefix: 'Failed to create agent',
    },
  })
}

export function usePatchAgent() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, startupPrompt }: { name: string; startupPrompt: string }) =>
      api.patchAgent(name, { startupPrompt }),
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', variables.name] })
    },
    meta: { successMessage: 'Startup prompt saved', errorPrefix: 'Failed to save startup prompt' },
  })
}

// useSetSessionResume flips the kyber#118 per-agent session-resume toggle.
// Separate from usePatchAgent so each control keeps its own toast copy.
export function useSetSessionResume() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, sessionResume }: { name: string; sessionResume: boolean }) =>
      api.patchAgent(name, { sessionResume }),
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', variables.name] })
    },
    meta: { successMessage: 'Session resume setting saved', errorPrefix: 'Failed to save session resume setting' },
  })
}

export function useSetRequestReplyEnabled() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, requestReplyEnabled }: { name: string; requestReplyEnabled: boolean }) =>
      api.patchAgent(name, { requestReplyEnabled }),
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', variables.name] })
    },
    meta: { successMessage: 'Bounded request setting saved', errorPrefix: 'Failed to save bounded request setting' },
  })
}

// useRotateApiKey: rotates the control-plane API key. Cookie-authenticated
// browsers receive a replacement HttpOnly session in the same response.
export function useRotateApiKey() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useMutation({
    mutationFn: () => api.rotateApiKey(),
    meta: {
      // No success toast — the post-rotation modal is the user-facing
      // confirmation. A toast would be redundant + would obscure the modal.
      errorPrefix: 'Failed to rotate API key',
    },
  })
}

// useFleetDefaults: reads the cluster's defaultModel + defaultRuntimeVersion
// (kyber#376). Slow-changing — no auto-refresh. Pair with useUpdateFleetDefaults
// to write back from the Settings panel.
export function useFleetDefaults() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'fleetDefaults'],
    queryFn: () => api.getFleetDefaults(),
  })
}

export function useUpdateFleetDefaults() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (body: FleetDefaults & { force?: boolean }) => api.putFleetDefaults(body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'fleetDefaults'] })
    },
    meta: {
      successMessage: () => 'Fleet defaults saved',
      errorPrefix: 'Failed to save fleet defaults',
    },
  })
}

export function useStartAgent() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.startAgent(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Started ${String(name)}`,
      errorPrefix: 'Failed to start agent',
    },
  })
}

export function useAgentModels(name: string, enabled: boolean) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'agents', name, 'models'],
    queryFn: () => api.getAgentModels(name),
    enabled: enabled && name.length > 0,
    retry: 1,
    staleTime: 5 * 60 * 1000,
  })
}

export function useStopAgent() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.stopAgent(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Stopped ${String(name)}`,
      errorPrefix: 'Failed to stop agent',
    },
  })
}

export function useRestartAgent() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.restartAgent(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Restarted ${String(name)}`,
      errorPrefix: 'Failed to restart agent',
    },
  })
}

export function useRestartAgentSession() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.restartAgentSession(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Restarted session for ${String(name)}`,
      errorPrefix: 'Failed to restart session',
    },
  })
}

export function useCompactAgentSession() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.compactAgentSession(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      // "Requested", not "Compacted": the server confirms delivery of the
      // command, and the runtime compacts on its own clock afterwards.
      // Claiming completion here would be a toast the API can't back up.
      successMessage: (_d: unknown, name: unknown) => `Compaction requested for ${String(name)}`,
      errorPrefix: 'Failed to compact session',
    },
  })
}

// useForceNeedsAuthAgent drops a wedged agent to NeedsAuth (deleting any live
// pod) so it can be re-authorized from scratch (#395).
export function useForceNeedsAuthAgent() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.forceNeedsAuthAgent(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Forced ${String(name)} into re-authorization`,
      errorPrefix: 'Failed to force re-auth',
    },
  })
}

export function useRepairAgentRuntime() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.repairAgentRuntime(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Runtime repaired for ${String(name)}; restart requested`,
      errorPrefix: 'Failed to repair runtime',
    },
  })
}

export function useSetAgentModel() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, model }: { name: string; model: string }) =>
      api.setAgentModel(name, model),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) => {
        const { name, model } = v as { name: string; model: string }
        return `${name} set to ${model}`
      },
      errorPrefix: 'Failed to set model',
    },
  })
}

// useSetAgentRuntimeVersion calls POST /api/v1/agents/{name}/set-runtime-version
// (kyber#378 PR-D). Empty string clears spec.runtimeVersion (reverts to
// the fleet default per PR-B resolution). For Running agents the
// control-plane flips DesiredPhase to Restarting so the new value takes
// effect on next boot.
export function useSetAgentRuntimeVersion() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, runtimeVersion }: { name: string; runtimeVersion: string }) =>
      api.setAgentRuntimeVersion(name, runtimeVersion),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) => {
        const { name, runtimeVersion } = v as { name: string; runtimeVersion: string }
        return runtimeVersion === ''
          ? `${name} runtime version cleared (reverts to fleet default)`
          : `${name} runtime version set to ${runtimeVersion}`
      },
      errorPrefix: 'Failed to set runtime version',
    },
  })
}

export function useSetAgentResources() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, body }: { name: string; body: SetResourcesRequest }) =>
      api.setAgentResources(name, body),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Updated resources for ${(v as { name: string }).name}`,
      errorPrefix: 'Failed to update resources',
    },
  })
}

export function useDeleteAgent() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.deleteAgent(name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'fleet'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Deleted ${String(name)}`,
      errorPrefix: 'Failed to delete agent',
    },
  })
}

export function useReauthorizeAgent() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, body }: { name: string; body: { oauthCode: string; pkceVerifier: string; state: string } }) =>
      api.reauthorizeAgent(name, body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Reauthorized ${(v as { name: string }).name}`,
      errorPrefix: 'Reauthorization failed',
    },
  })
}

export function useStartCodexDeviceAuth() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.startCodexDeviceAuth(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: 'Codex device login started',
      errorPrefix: 'Failed to start Codex device login',
    },
  })
}

/**
 * Poll what the in-pod Codex device login is showing.
 *
 * 2s, not the 15-30s the rest of this file uses: the operator is sitting in
 * front of the panel waiting for a code that is only good for 15 minutes, so
 * seconds of staleness are seconds of their time. Each poll is one exec into
 * the pod, so this must stay OFF for every agent that is not mid-login — the
 * caller gates it on runtime, auth type and phase.
 *
 * Pauses on its own while a code is showing and still good: the code does not
 * change while it is valid, and the panel's countdown runs client-side, so
 * continuing to poll would exec into the pod every 2s for fifteen minutes to be
 * told the same thing.
 *
 * `ready` is NOT terminal, though. Once that deadline passes, the panel offers
 * "Start again", and the restart it triggers is invisible unless polling
 * resumes — the mutation's invalidate fires one refetch milliseconds after the
 * POST, while the old pod is still showing the old expired prompt, so a query
 * that treated `ready` as final would answer `ready` once more and then never
 * poll again. The panel would sit on "that code expired" forever. Same reason a
 * `ready` we could not date keeps polling: without a deadline nothing else
 * would ever re-arm it.
 */
export function useCodexDeviceAuthStatus(name: string, enabled: boolean) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'agents', name, 'codex-device-auth'],
    queryFn: () => api.getCodexDeviceAuthStatus(name),
    enabled: Boolean(name) && enabled,
    refetchInterval: (query) => {
      const d = query.state.data
      const live = d?.state === 'ready' && d.expiresAt && Date.parse(d.expiresAt) > Date.now()
      return live ? false : 2000
    },
    // A pod restarting mid-poll answers `starting`; showing the last code we
    // saw would be a lie, so the panel follows the server.
    staleTime: 0,
  })
}

// ---- Agent jobs (#135) ----

export function usePatchAgentJobs() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, jobs }: { name: string; jobs: AgentJob[] }) =>
      api.patchAgentJobs(name, jobs),
    onSuccess: (_d, v) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', v.name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) => `Updated jobs for ${(v as { name: string }).name}`,
      errorPrefix: 'Failed to update jobs',
    },
  })
}

export function useRunAgentJob() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, jobName }: { name: string; jobName: string }) =>
      api.runAgentJob(name, jobName),
    onSuccess: (_d, v) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', v.name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) => {
        const { name, jobName } = v as { name: string; jobName: string }
        return `Ran ${jobName} on ${name}`
      },
      errorPrefix: 'Run failed',
    },
  })
}

// ---- Inbound prompt bindings (#208) ----

export function useInboundBindings(agentName: string) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'inboundBindings', agentName],
    queryFn: () => api.listInboundBindings(agentName),
    enabled: Boolean(agentName),
    refetchInterval: 30000,
  })
}

export function useCreateInboundBinding() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, body }: { name: string; body: CreateInboundBindingRequest }) =>
      api.createInboundBinding(name, body),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'inboundBindings', name] })
      // The agent CR's spec.inboundBindings has changed — refresh the agent
      // detail so any future surface that reads the spec list stays in sync.
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) => {
        const { name, body } = v as { name: string; body: CreateInboundBindingRequest }
        return `Webhook ${body.name} created on ${name}`
      },
      errorPrefix: 'Failed to create webhook',
    },
  })
}

export function useDeleteInboundBinding() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, bindingName }: { name: string; bindingName: string }) =>
      api.deleteInboundBinding(name, bindingName),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'inboundBindings', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Deleted webhook ${(v as { bindingName: string }).bindingName}`,
      errorPrefix: 'Failed to delete webhook',
    },
  })
}

export function useUpdateInboundBinding() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({
      name,
      bindingName,
      body,
    }: {
      name: string
      bindingName: string
      body: UpdateInboundBindingRequest
    }) => api.updateInboundBinding(name, bindingName, body),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'inboundBindings', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Updated webhook ${(v as { bindingName: string }).bindingName}`,
      errorPrefix: 'Failed to update webhook',
    },
  })
}

export function useRotateInboundSecret() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, bindingName }: { name: string; bindingName: string }) =>
      api.rotateInboundSecret(name, bindingName),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'inboundBindings', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Rotated secret for ${(v as { bindingName: string }).bindingName}`,
      errorPrefix: 'Failed to rotate secret',
    },
  })
}

// Phase 3: replay a previously-logged run. Invalidates the agent query so the
// inboundRuns ring buffer refreshes after the new request lands.
//
// Note: errorPrefix is INTENTIONALLY OMITTED. Replay's failure modes are
// status-coded (410 envelope expired, 429 queue full, 404 binding deleted)
// and each warrants a specific operator message. The caller's onError owns
// the full toast lifecycle for errors; the global mutationCache toast would
// otherwise stack on top of the situational one. successMessage stays so
// the green "Replayed (new request id: …)" toast still fires automatically.
export function useReplayInboundRun() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({
      name,
      bindingName,
      requestId,
    }: {
      name: string
      bindingName: string
      requestId: string
    }) => api.replayInboundRun(name, bindingName, requestId),
    onSuccess: (_data, { name }) => {
      // The new run will land in agent.status.inboundRuns shortly. Invalidate
      // both the per-agent query and the bindings list so the recent-runs
      // panel and aggregate stats refresh together.
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'inboundBindings', name] })
    },
    meta: {
      successMessage: (data: unknown) => {
        const d = data as { newRequestId: string }
        return `Replayed (new request id: ${d.newRequestId})`
      },
      // No errorPrefix: caller's onError handles every status code with a
      // situational message. See comment above.
    },
  })
}

// Phase 3: operator-triggered binding debug. No invalidation — this is a
// pure preview, server-side state is not mutated. Errors surface via the
// existing toast pipeline; success is surfaced inline by the caller.
export function useInboundDebug() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useMutation({
    mutationFn: (req: InboundDebugRequest) => api.inboundDebug(req),
    meta: {
      errorPrefix: 'Debug failed',
    },
  })
}

// ---- Agent skills (read-only) ----

// Skills change rarely — an agent reports at boot and on every identity sync,
// which can be days apart — so this polls slowly. There is nothing to refetch
// aggressively for; a stale-looking tab is answered by the "as of" timestamp,
// not by hammering the endpoint.
export function useAgentSkills(name: string, enabled: boolean) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'agentSkills', name],
    queryFn: () => api.getAgentSkills(name),
    enabled: enabled && Boolean(name),
    staleTime: 60000,
    // 404 → null is a valid answer ("never reported"), not an error.
    retry: false,
  })
}

// ---- Per-agent comms channels (#664) ----

export function useAgentComms(name: string) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'agentComms', name],
    queryFn: () => api.listAgentComms(name),
    enabled: Boolean(name),
  })
}

export function usePutTelegramComms() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, body }: { name: string; body: PutTelegramCommsRequest }) =>
      api.putTelegramComms(name, body),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agentComms', name] })
    },
    meta: {
      successMessage: () => 'Telegram saved — restart the pod to apply',
      errorPrefix: 'Failed to save Telegram',
    },
  })
}

export function usePutDiscordComms() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, body }: { name: string; body: PutDiscordCommsRequest }) =>
      api.putDiscordComms(name, body),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agentComms', name] })
    },
    meta: {
      successMessage: () => 'Discord saved — restart the pod to apply',
      errorPrefix: 'Failed to save Discord',
    },
  })
}

export function useDeleteAgentComms() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, channel }: { name: string; channel: CommsChannelId }) =>
      api.deleteAgentComms(name, channel),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agentComms', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Disabled ${(v as { channel: string }).channel}`,
      errorPrefix: 'Failed to disable channel',
    },
  })
}

// ---- User-defined per-agent secrets (#75) ----

export function useAgentSecrets(name: string) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'agentSecrets', name],
    queryFn: () => api.listAgentSecrets(name),
    enabled: Boolean(name),
    refetchInterval: 30000,
  })
}

export function usePutAgentSecretKV() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, key, value }: { name: string; key: string; value: string }) =>
      api.putAgentSecretKV(name, key, value),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agentSecrets', name] })
      // Replacing an existing entry can roll the pod; refresh agent status.
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Secret ${(v as { key: string }).key} saved`,
      errorPrefix: 'Failed to save secret',
    },
  })
}

export function useImportAgentSecretsKV() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      name,
      entries,
    }: {
      name: string
      entries: { key: string; value: string }[]
    }) => {
      for (const [index, entry] of entries.entries()) {
        try {
          // Keep these sequential. Concurrent merge-patches against the same
          // Kubernetes Secret can conflict or overwrite another imported row.
          await api.putAgentSecretKV(name, entry.key, entry.value)
        } catch (err) {
          throw new Error(
            `Imported ${index} of ${entries.length}; ${entry.key} failed: ${err instanceof Error ? err.message : 'Unknown error'}`,
          )
        }
      }
      return entries.length
    },
    // Refresh even after failure because earlier entries in the sequential
    // import may already have landed.
    onSettled: (_count, _error, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agentSecrets', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (count: unknown) => `Imported ${count as number} secrets`,
      errorPrefix: 'Failed to import secrets',
    },
  })
}

export function usePutAgentSecretFile() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, key, file }: { name: string; key: string; file: File }) =>
      api.putAgentSecretFile(name, key, file),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agentSecrets', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Secret file ${(v as { key: string }).key} uploaded`,
      errorPrefix: 'Failed to upload secret file',
    },
  })
}

export function useDeleteAgentSecret() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, key }: { name: string; key: string }) =>
      api.deleteAgentSecret(name, key),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agentSecrets', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Deleted secret ${(v as { key: string }).key}`,
      errorPrefix: 'Failed to delete secret',
    },
  })
}

// ---- Machines ----

export function useMachines() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'machines'],
    queryFn: () => api.listMachines(),
    refetchInterval: 30000,
  })
}

export function useMachine(name: string) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'machines', name],
    queryFn: () => api.getMachine(name),
    enabled: Boolean(name),
    refetchInterval: 15000,
  })
}

export function useCreateMachine() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: CreateMachineRequest) => api.createMachine(req),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'fleet'] })
    },
    meta: {
      successMessage: (_d: unknown, v: unknown) =>
        `Machine ${(v as CreateMachineRequest).name} created`,
      errorPrefix: 'Failed to create machine',
    },
  })
}

export function useStartMachine() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.startMachine(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Started ${String(name)}`,
      errorPrefix: 'Failed to start machine',
    },
  })
}

export function useStopMachine() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.stopMachine(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Stopped ${String(name)}`,
      errorPrefix: 'Failed to stop machine',
    },
  })
}

export function useRebootMachine() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.rebootMachine(name),
    onSuccess: (_data, name) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Rebooted ${String(name)}`,
      errorPrefix: 'Failed to reboot machine',
    },
  })
}

export function useDeleteMachine() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.deleteMachine(name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'fleet'] })
    },
    meta: {
      successMessage: (_d: unknown, name: unknown) => `Deleted ${String(name)}`,
      errorPrefix: 'Failed to delete machine',
    },
  })
}

export function useRestartMachineAgents() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.restartMachineAgents(name),
    onSuccess: () => {
      // Every agent on the machine just had its desiredPhase patched — refresh
      // agent lists and detail views so phase flips from Running → Restarting
      // become visible without waiting on the 30s refetchInterval.
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'fleet'] })
    },
    meta: {
      successMessage: (data: unknown, name: unknown) => {
        const d = data as { count: number; skipped?: unknown[] }
        const skipped = d.skipped?.length ?? 0
        const base = `Restarting ${d.count} agent${d.count !== 1 ? 's' : ''} on ${String(name)}`
        return skipped > 0 ? `${base} (${skipped} skipped)` : base
      },
      errorPrefix: 'Failed to restart agents',
    },
  })
}

export function useRetryCostOptimizedMachine() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ name, requestId }: { name: string; requestId: string }) =>
      api.retryCostOptimizedMachine(name, requestId),
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines', name] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines'] })
    },
    meta: {
      successMessage: 'Cost-optimized capacity retry started',
      errorPrefix: 'Failed to retry cost-optimized capacity',
    },
  })
}

// ---- Config ----

export function useComputeConfig() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'config'],
    queryFn: () => api.getComputeConfig(),
    // Provider doesn't change at runtime — cache for 5 min.
    staleTime: 5 * 60 * 1000,
    refetchInterval: false as const,
  })
}

// ---- GitHub (#134) ----

export function useGitHubRepos(enabled: boolean = true) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'github', 'repos'],
    queryFn: () => api.listGitHubRepos(),
    enabled,
    // Server caches for 60s; the client mirrors that so a wizard re-open
    // doesn't refetch.
    staleTime: 60 * 1000,
    refetchInterval: false as const,
    retry: false,
  })
}

// useGitHubRepoExists drives the live availability badge while the user
// types a new identity-repo name. Only fires when both owner and name
// are non-empty so we don't spam the server with bad requests.
export function useGitHubRepoExists(owner: string, name: string) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'github', 'exists', owner, name],
    queryFn: () => api.checkGitHubRepoExists(owner, name),
    enabled: owner !== '' && name !== '',
    // Treat the result as fresh for 5s — fast enough that "I just typed
    // it" feels live, slow enough to absorb keystroke debouncing.
    staleTime: 5 * 1000,
    refetchInterval: false as const,
    retry: false,
  })
}

// ---- Metrics (#328) ----

export function useMetricsSummary() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'metrics', 'summary'],
    queryFn: () => api.getMetricsSummary(),
    refetchInterval: 15000,
    staleTime: 10000,
    retry: false,
  })
}

export function useMetricsActivity(start: number, end: number) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'metrics', 'activity', start, end],
    queryFn: () => api.getMetricsActivity(start, end),
    enabled: start > 0 && end > start,
    retry: false,
  })
}

export function useMetricsWorkingTime(start: number, end: number) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'metrics', 'working-time', start, end],
    queryFn: () => api.getMetricsWorkingTime(start, end),
    enabled: start > 0 && end > start,
    retry: false,
  })
}

export function useMetricsTokens(start: number, end: number) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'metrics', 'tokens', start, end],
    queryFn: () => api.getMetricsTokens(start, end),
    enabled: start > 0 && end > start,
    retry: false,
  })
}

export function useMetricsLastActive() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'metrics', 'last-active'],
    queryFn: () => api.getMetricsLastActive(),
    refetchInterval: 15000,
    staleTime: 10000,
    retry: false,
  })
}

export function useMetricsNodes() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'metrics', 'nodes'],
    queryFn: () => api.getMetricsNodes(),
    refetchInterval: 30000,
    staleTime: 20000,
    retry: false,
  })
}

export function useMetricsStateChanges(start: number, end: number) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'metrics', 'state-changes', start, end],
    queryFn: () => api.getMetricsStateChanges(start, end),
    enabled: start > 0 && end > start,
    retry: false,
  })
}

// ---- Runtime detection (kyber#375 PR-A) ----
//
// Anthropic API key status: GET reveals only whether a key is configured,
// never the value. PUT/DELETE rotate the underlying Secret server-side.

export function useAnthropicKeyStatus() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'settings', 'anthropic-key'],
    queryFn: () => api.getAnthropicKey(),
    staleTime: 60000,
  })
}

export function useSetAnthropicKey() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (key: string) => api.setAnthropicKey(key),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'settings', 'anthropic-key'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'available'] })
    },
    meta: {
      successMessage: 'Anthropic API key updated',
      errorPrefix: 'Failed to update Anthropic API key',
    },
  })
}

export function useClearAnthropicKey() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => api.clearAnthropicKey(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'settings', 'anthropic-key'] })
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'available'] })
    },
    meta: {
      successMessage: 'Anthropic API key cleared',
      errorPrefix: 'Failed to clear Anthropic API key',
    },
  })
}

export function useAvailable() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'available'],
    queryFn: () => api.getAvailable(),
    refetchInterval: 60000,
    staleTime: 30000,
  })
}

// ---- Updates ----
//
// One query backs the whole card: status, policy, ownership and the last run
// arrive together, so an upgrade in flight is followed by polling this rather
// than stitching three endpoints.

export function useUpdates() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'updates'],
    queryFn: () => api.getUpdates(),
    staleTime: 30000,
    // Poll while an upgrade is running. The control plane is being replaced
    // mid-upgrade, so requests will fail for a stretch — that is expected, and
    // the polling has to survive it rather than give up and leave the operator
    // staring at a stale "running".
    refetchInterval: (query) => {
      const phase = query.state.data?.lastRun?.phase
      if (phase === 'pending' || phase === 'running') return 5000
      // Keep polling through an error too. The control plane goes down DURING
      // an upgrade, so the refetch that would have shown us the new run is
      // exactly the one likely to fail — and stopping on that error leaves the
      // cache showing the PREVIOUS run, which is terminal, which stops polling
      // for good. Nothing recovers that but a refocus or a reload.
      if (query.state.status === 'error') return 5000
      // Idle: keep a slow poll going so the header's update indicator appears
      // on its own, without the operator reloading or visiting Settings. Five
      // minutes because this only re-reads a value the control plane already
      // has cached — its own feed poll is hourly, so asking faster cannot
      // learn anything sooner, it just costs a request.
      return 300_000
    },
  })
}

export function useSetUpdatePolicy() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (patch: UpdatePolicyPatch) => api.setUpdatePolicy(patch),
    // The PUT returns the full status, so seed the cache with it rather than
    // refetching: the control plane reads policy through a cache that can lag
    // its own write, and a refetch here can echo back the PRE-write value.
    onSuccess: (status) => {
      queryClient.setQueryData(['cluster', cluster.id, 'updates'], status)
    },
    meta: {
      successMessage: 'Update settings saved',
      errorPrefix: 'Failed to save update settings',
    },
  })
}

export function useCheckUpdates() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => api.checkUpdates(),
    onSuccess: (status) => {
      queryClient.setQueryData(['cluster', cluster.id, 'updates'], status)
    },
    meta: { errorPrefix: 'Update check failed' },
  })
}

export function useApplyUpdate() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (version?: string) => api.applyUpdate(version),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'updates'] })
    },
    meta: {
      successMessage: 'Update started — watch the progress below',
      errorPrefix: 'Could not start the update',
    },
  })
}
