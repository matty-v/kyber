import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { ClusterIdentifier } from './ClusterIdentifier'
import type { UpdateStatus } from '../lib/types'

vi.mock('../lib/cluster-context', () => ({
  useCluster: vi.fn(),
}))
vi.mock('../hooks/useLiveVersion', () => ({
  useLiveVersion: vi.fn(),
}))
vi.mock('../hooks/useAPI', () => ({
  useUpdates: vi.fn(),
  // Pulled in by the UpdateDialog the indicator opens.
  useAgents: vi.fn(() => ({ data: [], isLoading: false, isError: false })),
  useApplyUpdate: vi.fn(() => ({ mutate: vi.fn(), isPending: false })),
}))

import { useCluster } from '../lib/cluster-context'
import { useLiveVersion } from '../hooks/useLiveVersion'
import { useUpdates, useAgents } from '../hooks/useAPI'
const mockUseCluster = vi.mocked(useCluster)
const mockUseLiveVersion = vi.mocked(useLiveVersion)
const mockUseUpdates = vi.mocked(useUpdates)

function status(over: Partial<UpdateStatus> = {}): UpdateStatus {
  return {
    currentVersion: '1.1.1',
    latestVersion: '1.2.0',
    latestUrl: 'https://example.invalid/releases/v1.2.0',
    latestPublishedAt: '2026-08-12T09:30:00Z',
    updateAvailable: true,
    policy: { channel: 'stable', mode: 'notify' },
    managedBy: 'helm',
    canSelfUpgrade: true,
    applySupported: true,
    ...over,
  }
}

function setup(
  clusterOverrides: Partial<ReturnType<typeof useCluster>> = {},
  liveOverrides: Partial<ReturnType<typeof useLiveVersion>> = {},
  updateStatus: UpdateStatus | undefined = undefined,
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
    unreachable: false,
    ...liveOverrides,
  })
  mockUseUpdates.mockReturnValue({
    data: updateStatus,
  } as ReturnType<typeof useUpdates>)
}

describe('ClusterIdentifier', () => {
  beforeEach(() => vi.clearAllMocks())

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
    setup({}, { liveChartVersion: 'v1.2.0' })
    render(<ClusterIdentifier />)
    const el = screen.getByTestId('cluster-identifier')
    expect(el).toHaveTextContent('kyber-falcon')
    // The cluster is on v1.2.0; this tab was served by v1.1.1. The header
    // reports the CLUSTER, so it must show v1.2.0 — showing the load-time
    // version would tell an operator their upgrade hadn't landed when it had.
    expect(el).toHaveTextContent('v1.2.0')
    expect(el).not.toHaveTextContent('v1.1.1')
    // ...and it offers nothing. The icon now means exactly one thing — an
    // update is available — and a tab whose own bundle is behind the cluster
    // is not that (Matt, 2026-08-14).
    expect(screen.queryByRole('button')).toBeNull()
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

  // The indicator is the whole point of the feature: an operator who never
  // opens Settings still finds out a release is waiting.
  it('offers the update when one is available', () => {
    setup({}, {}, status())
    render(<ClusterIdentifier />)

    const btn = screen.getByRole('button', { name: /1\.2\.0 is available/i })
    fireEvent.click(btn)

    expect(screen.getByRole('dialog')).toHaveTextContent('Install 1.2.0?')
    // The three facts the old confirmation never gave an operator.
    expect(screen.getByRole('dialog')).toHaveTextContent('1.1.1 → 1.2.0')
    // Loosely matched on purpose: the exact date string is the runner's locale,
    // and pinning it would fail in CI rather than catch a real regression.
    expect(screen.getByRole('dialog')).toHaveTextContent(/Released.*2026/)
    expect(screen.getByRole('link', { name: /What changed in 1\.2\.0/ })).toHaveAttribute(
      'href',
      'https://example.invalid/releases/v1.2.0',
    )
  })

  it('says nothing when the cluster is already on the latest version', () => {
    setup({}, {}, status({ updateAvailable: false, latestVersion: 'v1.1.1' }))
    render(<ClusterIdentifier />)
    expect(screen.queryByRole('button')).toBeNull()
  })

  // The dialog is mounted alongside the icon, not alongside the status. It
  // calls useAgents() at the top level, so mounting it on any status at all
  // would run a 30s agent-list poll from the header, on every page, forever,
  // for a dialog that can never open here. Caught in review (Chewie, kyber#71).
  it('does not poll the agent list when there is no update to offer', () => {
    setup({}, {}, status({ updateAvailable: false, latestVersion: 'v1.1.1' }))
    render(<ClusterIdentifier />)
    expect(vi.mocked(useAgents)).not.toHaveBeenCalled()
  })

  it('does poll the agent list while an update is on offer', () => {
    setup({}, {}, status())
    render(<ClusterIdentifier />)
    // The dialog names the agents that will lose their sessions, so the list
    // has to be warm by the time it opens.
    expect(vi.mocked(useAgents)).toHaveBeenCalled()
  })

  // The app-wide banner already narrates a running upgrade. A second control
  // offering the same install is noise at best and a double-apply at worst.
  it('stands down while an upgrade is already running', () => {
    setup({}, {}, status({ lastRun: { jobName: 'j', targetVersion: '1.2.0', phase: 'running' } }))
    render(<ClusterIdentifier />)
    expect(screen.queryByRole('button')).toBeNull()
  })

  // A cluster that cannot install its own updates still has to say one is out
  // — otherwise it silently never mentions being months behind (Matt,
  // 2026-08-14). What it must not do is offer a button that does nothing.
  it('announces the update without an install button on an ArgoCD-managed cluster', () => {
    setup(
      {},
      {},
      status({
        canSelfUpgrade: false,
        managedBy: 'argocd',
        reason: 'ArgoCD manages this cluster, so it will not install its own updates.',
      }),
    )
    render(<ClusterIdentifier />)

    fireEvent.click(screen.getByTestId('cluster-identifier-action'))
    const dialog = screen.getByRole('dialog')
    expect(dialog).toHaveTextContent('1.2.0 is available')
    expect(dialog).toHaveTextContent(/ArgoCD manages this cluster/)
    expect(screen.queryByRole('button', { name: /^Install/ })).toBeNull()
  })
})
