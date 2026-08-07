import { describe, it, expect } from 'vitest'
import { renderHook } from '@testing-library/react'
import type { PropsWithChildren } from 'react'
import {
  RoutePrefixProvider,
  useBackTo,
  usePrefixedPath,
  useRoutePrefix,
  type BackTo,
} from './route-prefix'

function wrap(prefix?: string, backTo?: BackTo) {
  return function Wrapper({ children }: PropsWithChildren) {
    if (prefix === undefined) return <>{children}</>
    return (
      <RoutePrefixProvider prefix={prefix} backTo={backTo}>
        {children}
      </RoutePrefixProvider>
    )
  }
}

describe('useRoutePrefix', () => {
  it('returns "" when no provider is mounted', () => {
    const { result } = renderHook(() => useRoutePrefix())
    expect(result.current).toBe('')
  })

  it('returns the provided prefix', () => {
    const { result } = renderHook(() => useRoutePrefix(), { wrapper: wrap('/c/abc') })
    expect(result.current).toBe('/c/abc')
  })
})

describe('usePrefixedPath', () => {
  it('returns the input unchanged when prefix is empty', () => {
    const { result } = renderHook(() => usePrefixedPath(), { wrapper: wrap() })
    expect(result.current('/agents')).toBe('/agents')
    expect(result.current('/agents/dave')).toBe('/agents/dave')
    expect(result.current('/')).toBe('/')
  })

  it('prepends the prefix to a path that starts with /', () => {
    const { result } = renderHook(() => usePrefixedPath(), { wrapper: wrap('/c/abc') })
    expect(result.current('/agents')).toBe('/c/abc/agents')
    expect(result.current('/agents/dave')).toBe('/c/abc/agents/dave')
  })

  it('prepends the prefix to a path that does NOT start with /', () => {
    const { result } = renderHook(() => usePrefixedPath(), { wrapper: wrap('/c/abc') })
    expect(result.current('agents')).toBe('/c/abc/agents')
  })

  it('handles the root path correctly', () => {
    const { result } = renderHook(() => usePrefixedPath(), { wrapper: wrap('/c/abc') })
    expect(result.current('/')).toBe('/c/abc/')
  })

  it('is idempotent — does not double-prepend', () => {
    const { result } = renderHook(() => usePrefixedPath(), { wrapper: wrap('/c/abc') })
    expect(result.current('/c/abc/agents')).toBe('/c/abc/agents')
    expect(result.current('/c/abc/')).toBe('/c/abc/')
  })

  it('does not confuse "/c/abc-other" with "/c/abc" prefix', () => {
    // Prefix matching is segment-aware (prefix + '/'), not raw startsWith.
    const { result } = renderHook(() => usePrefixedPath(), { wrapper: wrap('/c/abc') })
    expect(result.current('/c/abc-other/agents')).toBe('/c/abc/c/abc-other/agents')
  })
})

describe('useBackTo', () => {
  it('returns undefined when no provider is mounted', () => {
    const { result } = renderHook(() => useBackTo())
    expect(result.current).toBeUndefined()
  })

  it('returns undefined when provider has no backTo (embedded mode)', () => {
    const { result } = renderHook(() => useBackTo(), { wrapper: wrap('/c/abc') })
    expect(result.current).toBeUndefined()
  })

  it('returns the host-supplied backTo (hub mode)', () => {
    const { result } = renderHook(() => useBackTo(), {
      wrapper: wrap('/c/abc', { href: '/clusters', label: 'Clusters' }),
    })
    expect(result.current).toEqual({ href: '/clusters', label: 'Clusters' })
  })
})
