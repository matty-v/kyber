import { useEffect, useState, type PropsWithChildren } from 'react'
import {
  ClusterProvider,
  getEmbeddedApiKey,
  type Cluster,
} from '@matty-v/kyber-pwa-views'

type ClusterInfoResponse = {
  name: string
  version: string
  capabilities: string[]
}

type LoadState =
  | { status: 'loading' }
  | { status: 'ready'; cluster: Cluster }
  | { status: 'error'; message: string }

// Embedded mode does NOT wrap in <RoutePrefixProvider>. The package's
// useRoutePrefix() default is '' (so usePrefixedPath() is a no-op and
// Layout/CommandPalette/page navigates resolve to bare paths — same
// behavior as before A.5), and useBackTo() defaults to undefined (so the
// 0.3.0 back-to-host affordance is suppressed in Layout + CommandPalette
// — there's no host to go back to here). Hub mode (holocron, Phase C)
// supplies both: a `/c/<cluster-id>` prefix and a `backTo` pointing at
// `/clusters` so internal nav stays scoped AND the user can exit the
// cluster view.
export function EmbeddedClusterProvider({ children }: PropsWithChildren) {
  const [state, setState] = useState<LoadState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false
    void (async () => {
      try {
        // cluster-info is mounted on the *unprotected* mux (see Task A4) so a
        // fresh install with no API key still resolves and lands the user on
        // Settings to enter one. We still send the key if present so a bad key
        // can be detected here rather than per-request.
        const apiKey = getEmbeddedApiKey()
        const headers: Record<string, string> = {}
        if (apiKey) headers['Authorization'] = `Bearer ${apiKey}`
        const r = await fetch(`${window.location.origin}/api/v1/cluster-info`, { headers })
        if (!r.ok) {
          throw new Error(`status ${r.status}`)
        }
        const info = (await r.json()) as ClusterInfoResponse
        if (cancelled) return
        setState({
          status: 'ready',
          cluster: {
            id: 'local',
            name: info.name,
            baseURL: window.location.origin + '/',
            apiKey: getEmbeddedApiKey(),
            version: info.version,
            capabilities: info.capabilities,
          },
        })
      } catch (err) {
        if (cancelled) return
        const message = err instanceof Error ? err.message : 'unknown error'
        setState({ status: 'error', message })
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  // C1 fix: keep cluster.apiKey fresh after Settings → Save or useRotateApiKey.
  // Pre-Phase A every API call read localStorage fresh; post-Phase A the value
  // is captured in the Cluster object at mount. Dispatch 'kyber:apikey-changed'
  // from setEmbeddedApiKey (same-tab) + native 'storage' (other tabs) to bump it.
  useEffect(() => {
    const refresh = () => {
      setState(s =>
        s.status === 'ready'
          ? { ...s, cluster: { ...s.cluster, apiKey: getEmbeddedApiKey() } }
          : s,
      )
    }
    window.addEventListener('storage', refresh)
    window.addEventListener('kyber:apikey-changed', refresh)
    return () => {
      window.removeEventListener('storage', refresh)
      window.removeEventListener('kyber:apikey-changed', refresh)
    }
  }, [])

  if (state.status === 'loading') {
    return (
      <div style={{ padding: '2rem', fontFamily: 'system-ui' }}>
        Loading cluster...
      </div>
    )
  }
  if (state.status === 'error') {
    return (
      <div style={{ padding: '2rem', fontFamily: 'system-ui' }}>
        <h2>Could not reach the kyber control plane</h2>
        <p>{state.message}</p>
        <p>If this is an API key issue, set the key via the Settings page once the UI loads.</p>
      </div>
    )
  }
  return <ClusterProvider value={state.cluster}>{children}</ClusterProvider>
}
