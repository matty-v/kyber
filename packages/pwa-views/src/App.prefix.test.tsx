import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

// Mock useWebSocket so the effect doesn't try to open a real WebSocket
// (jsdom has no WebSocket constructor; render would throw).
vi.mock('./hooks/useWebSocket', () => ({
  useWebSocket: () => undefined,
}))

// Mock the data-fetching hooks the views call on mount. Returning empty
// data + non-loading is the "everything renders, nothing is selected"
// default state. Match the existing CommandPalette.test.tsx pattern —
// hook-level mocks instead of fetch-level stubs (the latter is brittle
// because it depends on the request shape useAPI generates).
vi.mock('./hooks/useAPI', () => {
  const empty = { data: undefined, isLoading: false, error: null }
  const emptyList = { data: [], isLoading: false, error: null }
  const mutation = {
    mutate: vi.fn(),
    mutateAsync: vi.fn(),
    isPending: false,
    isError: false,
    error: null,
    reset: vi.fn(),
  }
  return {
    // Query hooks
    useFleetSummary: () => empty,
    useAgents: () => emptyList,
    useMachines: () => emptyList,
    useAgent: () => empty,
    useMachine: () => empty,
    useComputeConfig: () => empty,
    useTokenUsage: () => empty,
    useInboundBindings: () => emptyList,
    useAgentSecrets: () => emptyList,
    useGitHubRepos: () => emptyList,
    useGitHubRepoExists: () => empty,
    // Layout renders UpgradeBanner, which watches the in-flight upgrade run.
    useUpdates: () => ({ data: undefined, isError: false }),
    // Mutation hooks — pages call these on mount via useMutation; they must
    // exist in the mock even if the test doesn't exercise them.
    useCreateAgent: () => mutation,
    useRotateApiKey: () => mutation,
    useStartAgent: () => mutation,
    useStopAgent: () => mutation,
    useRestartAgent: () => mutation,
    useRestartAgentSession: () => mutation,
    useSetAgentModel: () => mutation,
    useSetAgentRuntimeVersion: () => mutation,
    useSetAgentResources: () => mutation,
    // kyber#378 PR-D: AgentDetail composes useEffectiveModelList which
    // calls useAvailable. Empty data lets the fallback (useComputeConfig)
    // path render the existing UI without surprises.
    useAvailable: () => empty,
    useAnthropicKeyStatus: () => empty,
    useSetAnthropicKey: () => mutation,
    useClearAnthropicKey: () => mutation,
    useFleetDefaults: () => empty,
    useUpdateFleetDefaults: () => mutation,
    useDeleteAgent: () => mutation,
    useForceNeedsAuthAgent: () => mutation,
    useRepairAgentRuntime: () => mutation,
    useCompactAgentSession: () => mutation,
    useReauthorizeAgent: () => mutation,
    usePatchAgentJobs: () => mutation,
    useRunAgentJob: () => mutation,
    useCreateInboundBinding: () => mutation,
    useDeleteInboundBinding: () => mutation,
    useUpdateInboundBinding: () => mutation,
    useRotateInboundSecret: () => mutation,
    useReplayInboundRun: () => mutation,
    useInboundDebug: () => mutation,
    usePutAgentSecretKV: () => mutation,
    usePutAgentSecretFile: () => mutation,
    useDeleteAgentSecret: () => mutation,
    useCreateMachine: () => mutation,
    useStartMachine: () => mutation,
    useStopMachine: () => mutation,
    useRebootMachine: () => mutation,
    useDeleteMachine: () => mutation,
    useRestartMachineAgents: () => mutation,
    useRetryCostOptimizedMachine: () => mutation,
  }
})

import { App } from './App'
import { ClusterProvider, type Cluster } from './lib/cluster-context'
import { RoutePrefixProvider, type BackTo } from './lib/route-prefix'
import { TooltipProvider } from './components/ui/tooltip'

const mockCluster: Cluster = {
  id: 'abc',
  name: 'kyber-test',
  baseURL: 'https://test.example/',
  apiKey: 'sk-test',
  version: '1.6.0',
  capabilities: ['agents', 'machines'],
}

function LocationProbe() {
  const location = useLocation()
  return <span data-testid="loc">{location.pathname}</span>
}

// Polyfill ResizeObserver for jsdom — data-table / cmdk reads it.
if (typeof globalThis.ResizeObserver === 'undefined') {
  ;(globalThis as unknown as { ResizeObserver: unknown }).ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
}

// Polyfill scrollIntoView for jsdom.
if (typeof Element !== 'undefined' && typeof Element.prototype.scrollIntoView !== 'function') {
  Element.prototype.scrollIntoView = function () {}
}

function renderUnderPrefix(initialPath: string, backTo?: BackTo) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <QueryClientProvider client={qc}>
        <TooltipProvider>
            <Routes>
              <Route
                path="/c/:clusterId/*"
                element={
                  // Provider order mirrors Phase C's HubClusterProvider:
                  // ClusterProvider wraps RoutePrefixProvider inside the
                  // route element. LocationProbe is at the same level so
                  // it observes the location after navigation.
                  <ClusterProvider value={mockCluster}>
                    <RoutePrefixProvider prefix="/c/abc" backTo={backTo}>
                      <LocationProbe />
                      <App />
                    </RoutePrefixProvider>
                  </ClusterProvider>
                }
              />
              {/* Stub route that exists outside the cluster prefix so the
                  back affordance can navigate to it without a 404. */}
              <Route path="/clusters" element={<LocationProbe />} />
            </Routes>
        </TooltipProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  )
}

describe('App under /c/:clusterId/* prefix', () => {
  it('renders at the prefixed root', () => {
    renderUnderPrefix('/c/abc/')
    expect(screen.getByTestId('loc').textContent).toBe('/c/abc/')
  })

  it('renders the agents page at /c/abc/agents', () => {
    renderUnderPrefix('/c/abc/agents')
    expect(screen.getByTestId('loc').textContent).toBe('/c/abc/agents')
  })

  it('keeps cluster context when the user clicks the Machines nav link', async () => {
    renderUnderPrefix('/c/abc/agents')
    const user = userEvent.setup()

    // The nav renders the same items in both desktop sidebar (hidden via CSS
    // on mobile viewports) and the bottom tab bar. findAllByRole returns all
    // matching links; take the first (sidebar) to click.
    const machinesLinks = await screen.findAllByRole('link', { name: /machines/i })
    await user.click(machinesLinks[0])

    // The critical assertion: location is /c/abc/machines, not /machines.
    expect(screen.getByTestId('loc').textContent).toBe('/c/abc/machines')
  })

  it('keeps cluster context when the user clicks the Dashboard nav link', async () => {
    renderUnderPrefix('/c/abc/agents')
    const user = userEvent.setup()

    const fleetLinks = await screen.findAllByRole('link', { name: /dashboard/i })
    await user.click(fleetLinks[0])

    expect(screen.getByTestId('loc').textContent).toBe('/c/abc/')
  })

  it('does not render a back-to-host affordance in embedded mode', () => {
    renderUnderPrefix('/c/abc/agents')
    expect(screen.queryByTestId('back-to-host')).toBeNull()
  })

  it('renders a back-to-host affordance when the host supplies backTo', () => {
    renderUnderPrefix('/c/abc/agents', { href: '/clusters', label: 'Clusters' })
    // Both desktop sidebar and mobile header render one — at least one exists.
    const backLinks = screen.getAllByTestId('back-to-host')
    expect(backLinks.length).toBeGreaterThan(0)
  })

  it('navigates out of the cluster prefix when the back link is clicked', async () => {
    renderUnderPrefix('/c/abc/agents', { href: '/clusters', label: 'Clusters' })
    const user = userEvent.setup()
    const backLinks = screen.getAllByTestId('back-to-host')
    await user.click(backLinks[0])

    // After clicking the back link the outer Routes match /clusters and the
    // cluster-prefix route unmounts. The standalone LocationProbe at /clusters
    // is what's rendered now.
    expect(screen.getByTestId('loc').textContent).toBe('/clusters')
  })
})
