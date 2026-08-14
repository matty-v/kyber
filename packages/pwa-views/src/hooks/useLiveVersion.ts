import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import type { UpdateStatus, VersionInfo } from '../lib/types'

export interface LiveVersionState {
  versionInfo: VersionInfo | null
  liveChartVersion: string | null
  unreachable: boolean
}

/**
 * useLiveVersion reports what version the cluster is ACTUALLY running, as
 * opposed to the version that was current when this tab loaded.
 *
 * The two diverge every time the cluster upgrades, because the PWA is served by
 * the control plane being replaced: the bundle in this browser is a snapshot of
 * the old version and cannot update itself in place. The header reports the
 * CLUSTER, so it has to ask rather than trust what it loaded with.
 *
 * It used to also report whether this tab's own bundle was behind, which drove
 * a reload affordance in the header. That affordance was removed when the
 * header's icon became the update indicator (Matt, 2026-08-14), and the flag
 * went with it rather than being left as a computed value nobody reads.
 */
export function useLiveVersion(): LiveVersionState {
  const cluster = useCluster()
  const api = useMemo(
    () => createApiClient(cluster),
    [cluster.id, cluster.baseURL, cluster.apiKey],
  )

  // Poll faster while an upgrade is landing: this number is precisely what the
  // operator is watching the header for, and 30s of staleness after pressing
  // Install reads as "did it work?".
  //
  // This OBSERVES the updates query rather than owning it — `enabled: false`
  // subscribes to whatever `useUpdates` has already put in the cache without
  // issuing a request of its own. The header has no business driving update
  // polling, and a second owner here would mean two components deciding how
  // often to hit the API. When nothing is watching updates, this resolves to
  // undefined and we simply fall back to the idle interval.
  const { data: updateStatus } = useQuery<UpdateStatus>({
    queryKey: ['cluster', cluster.id, 'updates'],
    // Same queryFn as the owner, deliberately. An observer's options are copied
    // wholesale onto the shared Query on every render, so omitting it would set
    // queryFn: undefined on the query useUpdates owns — which only keeps working
    // because Query.fetch has an internal rescue path that borrows a queryFn
    // from some other observer. Supplying it costs nothing and leans on nothing.
    queryFn: () => api.getUpdates(),
    enabled: false,
  })
  const upgrading =
    updateStatus?.lastRun?.phase === 'pending' || updateStatus?.lastRun?.phase === 'running'

  const { data, isError } = useQuery({
    queryKey: ['cluster', cluster.id, 'version'],
    queryFn: () => api.getVersion(),
    refetchInterval: upgrading ? 3_000 : 30_000,
    // During an upgrade the control plane is mid-restart and these requests
    // will fail for a stretch. That is expected; keep polling rather than
    // giving up and leaving the header frozen on the old number.
    refetchIntervalInBackground: upgrading,
    // No retry override: during an upgrade the 3s refetch interval IS the
    // retry, and adding another layer only delays the honest "unreachable"
    // state the header uses to stop claiming a version it can no longer see.
  })

  const liveChartVersion = data?.chartVersion ?? null

  return {
    versionInfo: data ?? null,
    liveChartVersion,
    unreachable: isError,
  }
}
