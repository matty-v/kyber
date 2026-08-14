import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createElement, type ReactNode } from 'react'
import { useLiveVersion } from './useLiveVersion'
import { ClusterContext, type Cluster } from '../lib/cluster-context'
import type { VersionInfo } from '../lib/types'

vi.mock('../lib/api', () => ({
  createApiClient: vi.fn(),
}))

import { createApiClient } from '../lib/api'
const mockCreateApiClient = vi.mocked(createApiClient)

const baseCluster: Cluster = {
  id: 'test-cluster',
  name: 'kyber-falcon',
  baseURL: 'https://kyber-falcon.example.com/',
  apiKey: 'test-key',
  version: 'v1.1.1',
  capabilities: [],
}

const baseVersionInfo: VersionInfo = {
  sha: 'abc1234',
  buildDate: '2026-05-26T00:00:00Z',
  chartVersion: 'v1.1.1',
  substrate: 'k3s',
}

function makeWrapper(cluster: Cluster = baseCluster) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(
      QueryClientProvider,
      { client: qc },
      createElement(ClusterContext.Provider, { value: cluster }, children),
    )
  }
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('useLiveVersion', () => {
  it('returns the version the cluster is running', async () => {
    const getVersion = vi.fn<() => Promise<VersionInfo>>().mockResolvedValue(baseVersionInfo)
    mockCreateApiClient.mockReturnValue({ getVersion } as ReturnType<typeof createApiClient>)

    const { result } = renderHook(() => useLiveVersion(), { wrapper: makeWrapper() })

    await waitFor(() => expect(result.current.liveChartVersion).toBe('v1.1.1'))
    expect(result.current.unreachable).toBe(false)
    expect(result.current.versionInfo).toEqual(baseVersionInfo)
  })

  it('reports the cluster version even when this tab loaded with a different one', async () => {
    const newerVersion: VersionInfo = { ...baseVersionInfo, chartVersion: 'v1.2.0' }
    const getVersion = vi.fn<() => Promise<VersionInfo>>().mockResolvedValue(newerVersion)
    mockCreateApiClient.mockReturnValue({ getVersion } as ReturnType<typeof createApiClient>)

    const { result } = renderHook(() => useLiveVersion(), { wrapper: makeWrapper() })

    await waitFor(() => expect(result.current.liveChartVersion).toBe('v1.2.0'))
    expect(result.current.unreachable).toBe(false)
  })

  it('returns unreachable=true when getVersion throws', async () => {
    const getVersion = vi.fn<() => Promise<VersionInfo>>().mockRejectedValue(new Error('network error'))
    mockCreateApiClient.mockReturnValue({ getVersion } as ReturnType<typeof createApiClient>)

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const wrapper = ({ children }: { children: ReactNode }) =>
      createElement(
        QueryClientProvider,
        { client: qc },
        createElement(ClusterContext.Provider, { value: baseCluster }, children),
      )

    const { result } = renderHook(() => useLiveVersion(), { wrapper })

    await waitFor(() => expect(result.current.unreachable).toBe(true))
    expect(result.current.liveChartVersion).toBeNull()
    expect(result.current.versionInfo).toBeNull()
  })
})
