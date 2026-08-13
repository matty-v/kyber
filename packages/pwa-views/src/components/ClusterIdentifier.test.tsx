import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { ClusterIdentifier } from './ClusterIdentifier'

vi.mock('../lib/cluster-context', () => ({
  useCluster: vi.fn(),
}))
vi.mock('../hooks/useLiveVersion', () => ({
  useLiveVersion: vi.fn(),
}))

import { useCluster } from '../lib/cluster-context'
import { useLiveVersion } from '../hooks/useLiveVersion'
const mockUseCluster = vi.mocked(useCluster)
const mockUseLiveVersion = vi.mocked(useLiveVersion)

function setup(
  clusterOverrides: Partial<ReturnType<typeof useCluster>> = {},
  liveOverrides: Partial<ReturnType<typeof useLiveVersion>> = {},
) {
  mockUseCluster.mockReturnValue({
    id: 'test',
    name: 'kyber-falcon',
    baseURL: 'https://kyber-falcon.example.com/',
    apiKey: 'key',
    version: 'v1.1.1',
    capabilities: [],
    ...clusterOverrides,
  })
  mockUseLiveVersion.mockReturnValue({
    versionInfo: null,
    liveChartVersion: 'v1.1.1',
    isStale: false,
    unreachable: false,
    ...liveOverrides,
  })
}

describe('ClusterIdentifier', () => {
  it('renders cluster name and version in normal state', () => {
    setup()
    render(<ClusterIdentifier />)
    const el = screen.getByTestId('cluster-identifier')
    expect(el).toHaveTextContent('kyber-falcon')
    expect(el).toHaveTextContent('v1.1.1')
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('renders degraded form when unreachable', () => {
    setup({}, { unreachable: true, liveChartVersion: null })
    render(<ClusterIdentifier />)
    const el = screen.getByTestId('cluster-identifier')
    expect(el).toHaveTextContent('kyber-falcon · version unavailable')
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('shows the live cluster version, not the one this tab loaded with', () => {
    setup({}, { isStale: true, liveChartVersion: 'v1.2.0' })
    render(<ClusterIdentifier />)
    const el = screen.getByTestId('cluster-identifier')
    expect(el).toHaveTextContent('kyber-falcon')
    // The cluster is on v1.2.0; this tab was served by v1.1.1. The header
    // reports the CLUSTER, so it must show v1.2.0 — showing the load-time
    // version would tell an operator their upgrade hadn't landed when it had.
    expect(el).toHaveTextContent('v1.2.0')
    expect(el).not.toHaveTextContent('v1.1.1')
    // The refresh affordance remains: the page's own code is still the old
    // build, and only a reload fixes that.
    const btn = screen.getByRole('button', { name: /refresh to load the new interface/i })
    expect(btn).toBeInTheDocument()
  })

  it('renders pre-release version string without breaking layout', () => {
    setup({ version: 'v1.3.0-rc.1' }, { liveChartVersion: 'v1.3.0-rc.1' })
    render(<ClusterIdentifier />)
    expect(screen.getByTestId('cluster-identifier')).toHaveTextContent('v1.3.0-rc.1')
  })

  it('renders dirty build version without breaking layout', () => {
    setup({ version: 'v1.2.0-dirty' }, { liveChartVersion: 'v1.2.0-dirty' })
    render(<ClusterIdentifier />)
    expect(screen.getByTestId('cluster-identifier')).toHaveTextContent('v1.2.0-dirty')
  })

  it('falls back to — when cluster.name is empty', () => {
    setup({ name: '' }, {})
    render(<ClusterIdentifier />)
    expect(screen.getByTestId('cluster-identifier')).toHaveTextContent('—')
  })
})
