// packages/pwa-views/src/hooks/useAgentTranscript.test.ts
import { describe, it, expect } from 'vitest'
import { createApiClient } from '../lib/api'
import type { Cluster } from '../lib/cluster-context'

// Minimal cluster fixture — mirrors the mockCluster convention in lib/api.test.ts.
const mockCluster: Cluster = {
  id: 'c',
  name: 'test-cluster',
  baseURL: 'http://x',
  apiKey: 'k',
  version: '1.0.0',
  capabilities: [],
}

// fetchTranscript is exercised directly against a stubbed fetch — no network.
describe('fetchTranscript', () => {
  it('requests source=transcript with the window and reads the truncation header', async () => {
    const calls: string[] = []
    const orig = globalThis.fetch
    globalThis.fetch = (async (url: string) => {
      calls.push(String(url))
      return new Response('line1\nline2\n', {
        status: 200,
        headers: { 'X-Kyber-Log-Truncated': 'true', 'Content-Type': 'text/plain' },
      })
    }) as unknown as typeof fetch

    const api = createApiClient(mockCluster)
    const res = await api.fetchTranscript('echo', '2026-07-06T00:00:00Z', '2026-07-13T00:00:00Z')

    globalThis.fetch = orig
    expect(calls[0]).toContain('/api/v1/agents/echo/logs')
    expect(calls[0]).toContain('source=transcript')
    expect(calls[0]).toContain('since=2026-07-06T00%3A00%3A00Z')
    expect(res.jsonl).toBe('line1\nline2\n')
    expect(res.truncated).toBe(true)
  })
})
