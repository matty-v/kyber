import { describe, it, expect, beforeEach, vi } from 'vitest'
import { createApiClient } from './api'
import type { Cluster } from './cluster-context'

// Minimal localStorage polyfill for the node test environment.
// jsdom / happy-dom are not installed; we stub the global directly.
function makeLocalStorage(): Storage {
  const store: Record<string, string> = {}
  return {
    getItem: (k: string) => store[k] ?? null,
    setItem: (k: string, v: string) => { store[k] = v },
    removeItem: (k: string) => { delete store[k] },
    clear: () => { Object.keys(store).forEach(k => delete store[k]) },
    key: (i: number) => Object.keys(store)[i] ?? null,
    get length() { return Object.keys(store).length },
  } as Storage
}

const localStorageMock = makeLocalStorage()
vi.stubGlobal('localStorage', localStorageMock)

const mockCluster: Cluster = {
  id: 'local',
  name: 'kyber-test',
  baseURL: 'http://localhost:8080',
  apiKey: 'test-key',
  version: '1.0.0',
  capabilities: [],
}

function mockFetch(body: unknown, status = 200): void {
  const fetchMock = vi.fn().mockResolvedValue({
    ok: status < 400,
    status,
    json: async () => body,
  }) as unknown as typeof fetch
  vi.stubGlobal('fetch', fetchMock)
}

describe('getComputeConfig', () => {
  beforeEach(() => {
    // reset localStorage so API key doesn't leak across tests
    localStorage.clear()
  })

  it('returns the parsed response', async () => {
    mockFetch({
      compute: { provider: 'mock' },
      models: [{ id: 'claude-opus-4-7', contextWindow: 1_000_000 }],
    })
    const api = createApiClient(mockCluster)
    const cfg = await api.getComputeConfig()
    expect(cfg.compute.provider).toBe('mock')
    expect(cfg.models[0].id).toBe('claude-opus-4-7')
    expect(cfg.models[0].contextWindow).toBe(1_000_000)
  })

  it('works for provider=gce', async () => {
    mockFetch({
      compute: { provider: 'gce' },
      models: [{ id: 'claude-sonnet-4-6', contextWindow: 1_000_000 }],
    })
    const api = createApiClient(mockCluster)
    const cfg = await api.getComputeConfig()
    expect(cfg.compute.provider).toBe('gce')
    expect(cfg.models.length).toBe(1)
  })

  it('surfaces empty provider when server has none configured', async () => {
    mockFetch({ compute: { provider: '' }, models: [] })
    const api = createApiClient(mockCluster)
    const cfg = await api.getComputeConfig()
    expect(cfg.compute.provider).toBe('')
    expect(cfg.models).toEqual([])
  })
})

describe('logging discovery', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('loads logging settings', async () => {
    mockFetch({
      globalLevel: 'info',
      componentOverrides: { 'status-sidecar': 'debug' },
      archiveRetentionDays: 30,
      managedBy: 'helm',
    })
    const settings = await createApiClient(mockCluster).getLoggingSettings()
    expect(settings.componentOverrides['status-sidecar']).toBe('debug')
    expect(fetch).toHaveBeenCalledWith(
      'http://localhost:8080/api/v1/logging/settings',
      expect.any(Object),
    )
  })

  it('loads logging targets', async () => {
    mockFetch({ targets: [], selector: 'app.kubernetes.io/part-of=kyber' })
    const response = await createApiClient(mockCluster).getLoggingTargets()
    expect(response.targets).toEqual([])
    expect(response.selector).toBe('app.kubernetes.io/part-of=kyber')
  })
})

describe('kyber_server_url migration (kyber#123)', () => {
  // The migration runs once at module-import time. vitest caches modules
  // across test files, so we re-import with a fresh localStorage to observe
  // the behavior deterministically.
  beforeEach(() => {
    localStorage.clear()
    vi.resetModules()
  })

  it('evicts a stored server URL equal to current origin', async () => {
    localStorage.setItem('kyber_server_url', 'http://localhost:3000')
    // Stub window.location.origin to match what's stored — the original D5
    // rule would fire here; the unconditional rule fires here too.
    vi.stubGlobal('window', { location: { origin: 'http://localhost:3000' } })
    await import('./api')
    expect(localStorage.getItem('kyber_server_url')).toBeNull()
  })

  it('evicts a stored server URL that differs from current origin (the tunnel-flip case)', async () => {
    localStorage.setItem('kyber_server_url', 'https://old-tunnel.example.com')
    vi.stubGlobal('window', { location: { origin: 'https://new-tunnel.example.com' } })
    await import('./api')
    expect(localStorage.getItem('kyber_server_url')).toBeNull()
  })

  it('is a no-op when the key is absent', async () => {
    // Neither get nor set should throw, and no spurious key should appear.
    await import('./api')
    expect(localStorage.getItem('kyber_server_url')).toBeNull()
  })
})

describe('getVersion', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('returns the parsed VersionInfo', async () => {
    mockFetch({
      sha: 'abc1234',
      buildDate: '2026-04-21T17:30:00Z',
      chartVersion: '0.1.0',
      substrate: 'kyber-razer',
    })
    const api = createApiClient(mockCluster)
    const v = await api.getVersion()
    expect(v.sha).toBe('abc1234')
    expect(v.buildDate).toBe('2026-04-21T17:30:00Z')
    expect(v.chartVersion).toBe('0.1.0')
    expect(v.substrate).toBe('kyber-razer')
  })

  it('handles an all-empty response (pre-auth or dev build)', async () => {
    mockFetch({ sha: '', buildDate: '', chartVersion: '', substrate: '' })
    const api = createApiClient(mockCluster)
    const v = await api.getVersion()
    expect(v.sha).toBe('')
    expect(v.substrate).toBe('')
  })
})

describe('forceNeedsAuthAgent (#395)', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('POSTs to the force-needs-auth sub-action for the named agent', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({}),
    }) as unknown as typeof fetch
    vi.stubGlobal('fetch', fetchMock)

    const api = createApiClient(mockCluster)
    await api.forceNeedsAuthAgent('dave')

    const calls = (fetchMock as unknown as { mock: { calls: unknown[][] } }).mock.calls
    expect(calls.length).toBe(1)
    const [url, init] = calls[0] as [string, RequestInit]
    expect(url).toBe('http://localhost:8080/api/v1/agents/dave/force-needs-auth')
    expect(init.method).toBe('POST')
  })
})

describe('repairAgentRuntime', () => {
  it('POSTs to the encoded repair-runtime sub-action', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({ agent: 'han solo', runtime: 'codex', message: 'ok', output: 'verified' }),
    }) as unknown as typeof fetch
    vi.stubGlobal('fetch', fetchMock)

    await createApiClient(mockCluster).repairAgentRuntime('han solo')

    const [[url, init]] = (fetchMock as unknown as { mock: { calls: [string, RequestInit][][] } }).mock.calls
    expect(url).toBe('http://localhost:8080/api/v1/agents/han%20solo/repair-runtime')
    expect(init.method).toBe('POST')
  })
})

describe('logStream — source/window query building (kyber#431)', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  // Stubs fetch to capture the request URL and return a tiny readable body so
  // logStream's ReadableStream resolves. Returns a fn to read the captured URL.
  function mockLogFetch(text: string): () => string {
    let captured = ''
    const fetchMock = vi.fn().mockImplementation((url: string) => {
      captured = url
      const body = new ReadableStream<Uint8Array>({
        start(c) {
          c.enqueue(new TextEncoder().encode(text))
          c.close()
        },
      })
      return Promise.resolve({ ok: true, status: 200, body, headers: new Headers() })
    }) as unknown as typeof fetch
    vi.stubGlobal('fetch', fetchMock)
    return () => captured
  }

  async function drain(stream: ReadableStream<string>): Promise<string> {
    const reader = stream.getReader()
    let out = ''
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      out += value
    }
    return out
  }

  it('builds source=archive with RFC3339 since/until and omits follow/tail', async () => {
    const getURL = mockLogFetch('archived line\n')
    const api = createApiClient(mockCluster)
    const text = await drain(
      api.logStream('agent', 'dave', {
        source: 'archive',
        since: '2026-06-03T10:00:00Z',
        until: '2026-06-03T11:00:00Z',
      }),
    )
    const url = getURL()
    expect(url).toContain('/api/v1/agents/dave/logs?')
    expect(url).toContain('source=archive')
    expect(url).toContain('since=2026-06-03T10%3A00%3A00Z')
    expect(url).toContain('until=2026-06-03T11%3A00%3A00Z')
    expect(url).not.toContain('follow=')
    expect(url).not.toContain('tail=')
    expect(text).toBe('archived line\n')
  })

  it('defaults to the live path (follow/tail) with no source param', async () => {
    const getURL = mockLogFetch('live line\n')
    const api = createApiClient(mockCluster)
    await drain(api.logStream('agent', 'dave', { follow: true, tail: 200 }))
    const url = getURL()
    expect(url).toContain('follow=true')
    expect(url).toContain('tail=200')
    expect(url).not.toContain('source=')
    expect(url).not.toContain('until=')
  })
})

describe('generic logging client (kyber#105)', () => {
  it('streams an identity-bound live target', async () => {
    let captured = ''
    vi.stubGlobal('fetch', vi.fn().mockImplementation((url: string) => {
      captured = url
      const body = new ReadableStream<Uint8Array>({
        start(c) { c.enqueue(new TextEncoder().encode('hello\n')); c.close() },
      })
      return Promise.resolve({ ok: true, status: 200, body, headers: new Headers() })
    }))
    const result = await createApiClient(mockCluster).loggingStream({
      pod: 'agent-sol-0', podUid: 'uid-1', container: 'agent', component: 'agent', workload: 'sol', follow: true, tail: 500,
    })
    const reader = result.stream.getReader()
    expect((await reader.read()).value).toBe('hello\n')
    expect(captured).toContain('/api/v1/logging/logs?')
    expect(captured).toContain('podUid=uid-1')
    expect(captured).toContain('container=agent')
    expect(captured).toContain('follow=true')
  })

  it('downloads a bounded text export and reports truncation', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('hello\n', {
      status: 200,
      headers: {
        'Content-Disposition': 'attachment; filename="kyber-agent.log"',
        'X-Kyber-Log-Truncated': 'true',
      },
    })))
    const result = await createApiClient(mockCluster).exportLogging({
      pod: 'agent-sol-0', podUid: 'uid-1', container: 'agent', component: 'agent', workload: 'sol', format: 'text',
      since: '2026-06-03T10:00:00Z', until: '2026-06-03T11:00:00Z',
    })
    expect(result.filename).toBe('kyber-agent.log')
    expect(result.truncated).toBe(true)
    expect(await result.blob.text()).toBe('hello\n')
    expect(fetch).toHaveBeenCalledWith(expect.stringContaining('format=text'), expect.any(Object))
  })

  it('propagates archive truncation headers with the stream', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('partial\n', {
      status: 200,
      headers: { 'X-Kyber-Log-Truncated': 'true' },
    })))
    const result = await createApiClient(mockCluster).loggingStream({
      pod: 'archived-uid-old', podUid: 'uid-old', container: 'control-plane',
      component: 'control-plane', workload: 'control-plane', source: 'archive',
    })
    expect(result.truncated).toBe(true)
    expect((await result.stream.getReader().read()).value).toBe('partial\n')
  })
})
