import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import type { VersionInfo } from '../lib/types'

export interface LiveVersionState {
  versionInfo: VersionInfo | null
  liveChartVersion: string | null
  isStale: boolean
  unreachable: boolean
}

export function useLiveVersion(): LiveVersionState {
  const cluster = useCluster()
  const api = useMemo(
    () => createApiClient(cluster),
    [cluster.id, cluster.baseURL, cluster.apiKey],
  )

  const { data, isError } = useQuery({
    queryKey: ['cluster', cluster.id, 'version'],
    queryFn: () => api.getVersion(),
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
  })

  const liveChartVersion = data?.chartVersion ?? null

  const isStale =
    !isError &&
    liveChartVersion !== null &&
    cluster.version !== '' &&
    liveChartVersion !== cluster.version

  return {
    versionInfo: data ?? null,
    liveChartVersion,
    isStale,
    unreachable: isError,
  }
}
