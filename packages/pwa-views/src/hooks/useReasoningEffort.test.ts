import { createElement, type ReactNode } from 'react'
import { act, renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ClusterContext, type Cluster } from '../lib/cluster-context'
import { useEffortOptions } from './useReasoningEffort'
import { createApiClient } from '../lib/api'

vi.mock('../lib/api', () => ({ createApiClient: vi.fn() }))
const cluster: Cluster = { id: 'test', name: 'Test', version: 'fixture', baseURL: 'https://example.test', apiKey: 'fixture', capabilities: [] }
afterEach(() => { vi.useRealTimers(); vi.clearAllMocks() })

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  return ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client },
    createElement(ClusterContext.Provider, { value: cluster }, children))
}

describe('effort catalog refresh', () => {
  it('picks up native choices published after the agent view mounts', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const getAgentEffortOptions = vi.fn()
      .mockResolvedValueOnce({ state: 'unknown', levels: [], runtime: 'codex', runtimeVersion: 'latest', model: 'gpt-6.1-terra' })
      .mockResolvedValue({ state: 'supported', levels: ['high', 'ultra'], runtime: 'codex', runtimeVersion: '0.166.0', model: 'gpt-6.1-terra' })
    vi.mocked(createApiClient).mockReturnValue({ getAgentEffortOptions } as ReturnType<typeof createApiClient>)
    const { result, unmount } = renderHook(() => useEffortOptions({ agent: 'glyph' }), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data?.state).toBe('unknown'))
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000) })
    await waitFor(() => expect(result.current.data?.levels).toEqual(['high', 'ultra']))
    expect(getAgentEffortOptions).toHaveBeenCalledWith('glyph')
    unmount()
  })
})
