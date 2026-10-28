import { useMemo } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useCluster } from '../lib/cluster-context'
import { createApiClient } from '../lib/api'

export function useEffortOptions(input: { agent?: string; runtime?: string; model?: string; customEndpoint?: boolean }) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  return useQuery({
    queryKey: ['cluster', cluster.id, 'effort-options', cluster.baseURL, input.agent, input.runtime, input.model, input.customEndpoint],
    queryFn: () => input.agent ? api.getAgentEffortOptions(input.agent) : api.getCreateEffortOptions(input.runtime ?? '', input.model ?? '', input.customEndpoint ?? false),
    enabled: Boolean(input.agent || input.runtime),
    staleTime: 0,
    retry: false,
  })
}

export function useSetAgentEffort() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const cache = useQueryClient()
  return useMutation({
    mutationFn: ({ name, effort }: { name: string; effort: string }) => api.setAgentEffort(name, effort),
    onSuccess: () => { void cache.invalidateQueries({ queryKey: ['cluster', cluster.id] }) },
  })
}
