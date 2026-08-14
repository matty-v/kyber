import { useEffect, useState, type PropsWithChildren } from 'react'
import {
  ClusterProvider,
  establishEmbeddedBrowserSession,
  takeLegacyEmbeddedApiKey,
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
        // Migrate the pre-session localStorage key once, then discard it. New
        // installs establish the same HttpOnly session from Settings.
        const legacyKey = takeLegacyEmbeddedApiKey()
        if (legacyKey) await establishEmbeddedBrowserSession(legacyKey)
        const r = await fetch(`${window.location.origin}/api/v1/cluster-info`)
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
            apiKey: '',
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
