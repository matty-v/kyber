import { describe, it, expect } from 'vitest'
import { renderHook } from '@testing-library/react'
import type { PropsWithChildren } from 'react'
import { ClusterProvider, useCluster, useHasCapability, type Cluster } from './cluster-context'

const mockCluster: Cluster = {
  id: 'local',
  name: 'kyber-test',
  baseURL: 'https://test.example/',
  apiKey: 'sk-test',
  version: '1.6.0',
  capabilities: ['agents', 'machines'],
}

function wrapWith(cluster: Cluster | null) {
  return function Wrapper({ children }: PropsWithChildren) {
    return cluster ? <ClusterProvider value={cluster}>{children}</ClusterProvider> : <>{children}</>
  }
}

describe('useCluster', () => {
  it('returns the cluster value when wrapped in <ClusterProvider>', () => {
    const { result } = renderHook(() => useCluster(), { wrapper: wrapWith(mockCluster) })
    expect(result.current.id).toBe('local')
    expect(result.current.baseURL).toBe('https://test.example/')
  })

  it('throws when used outside a provider', () => {
    expect(() => renderHook(() => useCluster())).toThrowError(/No active cluster/)
  })
})

describe('useHasCapability', () => {
  it('returns true when the capability is present', () => {
    const { result } = renderHook(() => useHasCapability('agents'), { wrapper: wrapWith(mockCluster) })
    expect(result.current).toBe(true)
  })

  it('returns false when the capability is absent', () => {
    const { result } = renderHook(() => useHasCapability('inbound'), { wrapper: wrapWith(mockCluster) })
    expect(result.current).toBe(false)
  })
})
