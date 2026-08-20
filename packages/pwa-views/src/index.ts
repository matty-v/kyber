// Public entry for @matty-v/kyber-pwa-views.
//
// Consumers (apps/embedded-pwa, holocron) import what they need from this
// barrel. The intent is to keep the public surface deliberate — re-export
// concrete things, not whole internal directories.
//
// Public surface: cluster context, page components, the cluster-aware API
// client factory, and the providers/utilities apps wrap their entry in.

// The full app, ready to mount inside the appropriate provider stack.
export { App } from './App'

// Direct page exports for consumers that want to render a single view in a
// custom layout (holocron will use these in Phase C; embedded-pwa renders
// <App> directly).
export { Dashboard } from './pages/Dashboard'
export { MachineList } from './pages/MachineList'
export { MachineDetail } from './pages/MachineDetail'
export { AgentList } from './pages/AgentList'
export { AgentDetail } from './pages/AgentDetail'
export { CreateMachine } from './pages/CreateMachine'
export { CreateAgent } from './pages/CreateAgent'
export { MetricsTab } from './pages/MetricsTab'
export { Settings } from './pages/Settings'

// Shared hook used by consumers that need to listen to cluster CRD updates.
export { useWebSocket } from './hooks/useWebSocket'
export { useUpgradeProgress, type UpgradeProgress } from './hooks/useUpgradeProgress'

// Cluster name + version display component. Renders "{name} {version}" with
// a refresh affordance when a new chart version is detected.
export { ClusterIdentifier } from './components/ClusterIdentifier'
export { UpgradeBanner } from './components/UpgradeBanner'
export { useLiveVersion, type LiveVersionState } from './hooks/useLiveVersion'

// Cluster context — Phase A introduces this; consumers must wrap <App /> in
// some <ClusterProvider> implementation that provides baseURL + apiKey +
// capability metadata.
export {
  ClusterContext,
  ClusterProvider,
  useCluster,
  useHasCapability,
  type Cluster,
  type ClusterCapability,
} from './lib/cluster-context'

// Route prefix — Phase A.5. Holocron (Phase C) wraps in
// <RoutePrefixProvider prefix={`/c/${clusterId}`} backTo={{href, label}}> so
// internal nav stays scoped AND the Layout renders an affordance back to the
// host. Embedded mode leaves the provider out (or omits backTo) and behaves
// like before A.5 — no back affordance shown.
export {
  RoutePrefixProvider,
  useRoutePrefix,
  useBackTo,
  usePrefixedPath,
  type BackTo,
} from './lib/route-prefix'

// Cluster-aware API client. Consumers obtain one via createApiClient(cluster)
// or via hooks (useFleet, useAgent, etc.) that read ClusterContext internally.
export { createApiClient, type ApiClient } from './lib/api'

// Embedded-mode browser-session helpers. Raw API keys are exchanged once and
// are never retained in browser-readable storage.
export {
  establishEmbeddedBrowserSession,
  takeLegacyEmbeddedApiKey,
  getLastApiCall,
  onSessionExpired,
  ERR_SESSION_EXPIRED,
} from './lib/api'

// Re-auth prompt for an expired browser session. Mount once near the app
// root in embedded mode; it opens itself when the control plane reports
// session_expired. Hub mode never triggers it (bearer auth per request).
export { SessionExpiredDialog } from './components/SessionExpiredDialog'

// Types consumers may want.
export type * from './lib/types'

// Providers + tooling consumers wrap App in (re-exported so apps/embedded-pwa
// has one import source).
export { TooltipProvider } from './components/ui/tooltip'
export { Toaster } from './components/ui/sonner'
export { enableMocks } from './mocks/init'

// Stylesheet: consumers import '@matty-v/kyber-pwa-views/styles.css' once
// at their entry point. The CSS is not auto-imported here to keep the JS
// barrel free of side-effect CSS imports that bundlers would hoist unexpectedly.
