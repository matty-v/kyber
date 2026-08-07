// Subscribes to the shared event bus and invalidates React Query caches on events.
// Attach at the app root level so all pages benefit from real-time updates.

import { useEffect, useMemo } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import { eventBus } from '../lib/websocket'
import type { KyberEvent } from '../lib/types'

export function useWebSocket() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const queryClient = useQueryClient()

  useEffect(() => {
    const unsubscribe = eventBus.subscribe((event: KyberEvent) => {
      const { type, resource } = event
      const kind = resource.kind?.toLowerCase()
      const name = resource.name

      // Invalidate fleet summary on any change.
      void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'fleet'] })

      if (kind === 'agent') {
        void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
        if (name) {
          void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents', name] })
        }
      }

      if (kind === 'machine') {
        void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines'] })
        if (name) {
          void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines', name] })
        }
      }

      // On deletion, also invalidate parent lists.
      if (type.endsWith('.deleted')) {
        void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'agents'] })
        void queryClient.invalidateQueries({ queryKey: ['cluster', cluster.id, 'machines'] })
      }
    }, () => api.eventStream())

    return unsubscribe
  }, [queryClient, cluster.id, api])
}
