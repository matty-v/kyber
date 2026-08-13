import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

// Layout pulls in CommandPalette (useAgents/useMachines) and ClusterIdentifier
// (useLiveVersion). Mock the data layer so the test renders without a server —
// same hook-level-mock pattern as CommandPalette.test.tsx / App.prefix.test.tsx.
vi.mock('../hooks/useAPI', () => {
  const emptyList = { data: [], isLoading: false, error: null }
  return {
    useAgents: () => emptyList,
    useMachines: () => emptyList,
    // Layout renders UpgradeBanner, which watches the in-flight upgrade run.
    useUpdates: () => ({ data: undefined, isError: false }),
  }
})

vi.mock('../hooks/useLiveVersion', () => ({
  useLiveVersion: () => ({ isStale: false, unreachable: false }),
}))

import { Layout } from './Layout'
import { ClusterProvider, type Cluster } from '../lib/cluster-context'
import { RoutePrefixProvider, type BackTo } from '../lib/route-prefix'
import { DensityProvider } from '../contexts/DensityContext'
import { TooltipProvider } from './ui/tooltip'

const mockCluster: Cluster = {
  id: 'abc',
  name: 'kyber-test',
  baseURL: 'https://test.example/',
  apiKey: 'sk-test',
  version: '1.6.0',
  capabilities: ['agents', 'machines'],
}

// jsdom polyfills the Layout subtree relies on (mirror App.prefix.test.tsx).
if (typeof window !== 'undefined' && typeof window.matchMedia !== 'function') {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
      onchange: null,
    })),
  })
}
if (typeof globalThis.ResizeObserver === 'undefined') {
  ;(globalThis as unknown as { ResizeObserver: unknown }).ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
}
if (
  typeof Element !== 'undefined' &&
  typeof Element.prototype.scrollIntoView !== 'function'
) {
  Element.prototype.scrollIntoView = function () {}
}

function renderLayout(initialPath: string, backTo?: BackTo) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <QueryClientProvider client={qc}>
        <DensityProvider>
          <TooltipProvider>
            <ClusterProvider value={mockCluster}>
              <RoutePrefixProvider prefix="" backTo={backTo}>
                <Layout>
                  <div>page content</div>
                </Layout>
              </RoutePrefixProvider>
            </ClusterProvider>
          </TooltipProvider>
        </DensityProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  )
}

// These lock the react-router-dom NavLink/Link surface the Layout depends on —
// the `isActive` render-prop and `end`-prop semantics — which is the bit most
// exposed to a react-router major bump (#413: v6 → v7 peer widen). NavLink sets
// aria-current="page" on the active link; we assert via aria-current (router-
// provided, robust) plus the active class token the render-prop applies.
describe('Layout — NavLink/Link under react-router-dom', () => {
  it('renders all nav destinations as links with correct hrefs', () => {
    renderLayout('/')
    // Each nav item renders in both the desktop sidebar and the mobile bottom
    // bar, so destinations appear twice. Assert the href on the first match.
    for (const [name, href] of [
      [/dashboard/i, '/'],
      [/machines/i, '/machines'],
      [/agents/i, '/agents'],
      [/metrics/i, '/metrics'],
      [/settings/i, '/settings'],
    ] as const) {
      const link = screen.getAllByRole('link', { name })[0]
      expect(link).toHaveAttribute('href', href)
    }
  })

  it('marks the current route active via the isActive render-prop', () => {
    renderLayout('/machines')
    const machines = screen.getAllByRole('link', { name: /machines/i })[0]
    // react-router sets aria-current="page" on the active NavLink, and the
    // render-prop applies the accent class. Both must hold under v7.
    expect(machines).toHaveAttribute('aria-current', 'page')
    expect(machines.className).toContain('text-accent')
  })

  it('honors the `end` prop so Dashboard ("/") is not active on a sub-route', () => {
    // The Dashboard link uses end={true}. On /machines it must NOT be active —
    // this is exactly the NavLink `end` semantics that must survive the bump.
    renderLayout('/machines')
    const fleet = screen.getAllByRole('link', { name: /dashboard/i })[0]
    expect(fleet).not.toHaveAttribute('aria-current', 'page')
    expect(fleet.className).toContain('text-text-muted')
  })

  it('marks Dashboard active at the index route', () => {
    renderLayout('/')
    const fleet = screen.getAllByRole('link', { name: /dashboard/i })[0]
    expect(fleet).toHaveAttribute('aria-current', 'page')
    expect(fleet.className).toContain('text-accent')
  })

  it('renders a back-to-host Link with the host-supplied href when backTo is set', () => {
    renderLayout('/agents', { href: '/clusters', label: 'Clusters' })
    const back = screen.getAllByTestId('back-to-host')[0]
    expect(back.tagName).toBe('A')
    expect(back).toHaveAttribute('href', '/clusters')
  })

  it('renders no back-to-host affordance in embedded mode (no backTo)', () => {
    renderLayout('/agents')
    expect(screen.queryByTestId('back-to-host')).toBeNull()
  })
})
