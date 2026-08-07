import { describe, it, expect, beforeEach, vi } from 'vitest'
import { createApiClient } from './api'
import type { Cluster } from './cluster-context'

// Minimal localStorage polyfill (matches api.test.ts).
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

vi.stubGlobal('localStorage', makeLocalStorage())

const mockCluster: Cluster = {
  id: 'local',
  name: 'kyber-test',
  baseURL: 'http://localhost:8080',
  apiKey: 'test-key',
  version: '1.0.0',
  capabilities: [],
}

type FetchCall = { url: string; init: RequestInit | undefined }

function recordingFetch(response: Partial<Response> & { body?: unknown }): {
  fn: typeof fetch
  calls: FetchCall[]
} {
  const calls: FetchCall[] = []
  const fn = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(url), init })
    return response as unknown as Response
  }) as unknown as typeof fetch
  return { fn, calls }
}

function mockResponse(opts: {
  status?: number
  contentType?: string
  text?: string
  blob?: Blob
  json?: unknown
  disposition?: string
}): Partial<Response> {
  const headers = new Headers()
  if (opts.contentType) headers.set('Content-Type', opts.contentType)
  if (opts.disposition) headers.set('Content-Disposition', opts.disposition)
  const status = opts.status ?? 200
  return {
    ok: status < 400,
    status,
    headers,
    text: async () => opts.text ?? '',
    blob: async () => opts.blob ?? new Blob(),
    json: async () => opts.json ?? {},
  }
}

beforeEach(() => {
  localStorage.clear()
})

describe('listAgentSecrets', () => {
  it('returns items from the list response', async () => {
    const { fn } = recordingFetch(mockResponse({
      contentType: 'application/json',
      json: {
        items: [
          { key: 'FOO', kind: 'kv', size: 4, sha256Prefix: 'abc123def456', createdAt: '2026-04-18T00:00:00Z', updatedAt: '2026-04-18T00:00:00Z' },
        ],
      },
    }))
    vi.stubGlobal('fetch', fn)

    const api = createApiClient(mockCluster)
    const items = await api.listAgentSecrets('dave')
    expect(items).toHaveLength(1)
    expect(items[0].key).toBe('FOO')
    expect(items[0].kind).toBe('kv')
  })

  it('url-encodes the agent name', async () => {
    const { fn, calls } = recordingFetch(mockResponse({
      contentType: 'application/json', json: { items: [] },
    }))
    vi.stubGlobal('fetch', fn)
    const api = createApiClient(mockCluster)
    await api.listAgentSecrets('my agent')
    expect(calls[0].url).toBe('http://localhost:8080/api/v1/agents/my%20agent/secrets')
  })
})

describe('getAgentSecretValue', () => {
  it('returns { kind: "kv", value } for text/plain', async () => {
    const { fn } = recordingFetch(mockResponse({
      contentType: 'text/plain; charset=utf-8',
      text: 'sekret',
    }))
    vi.stubGlobal('fetch', fn)

    const api = createApiClient(mockCluster)
    const res = await api.getAgentSecretValue('dave', 'FOO')
    expect(res.kind).toBe('kv')
    if (res.kind === 'kv') expect(res.value).toBe('sekret')
  })

  it('returns { kind: "file", value: Blob, filename } for octet-stream', async () => {
    const { fn } = recordingFetch(mockResponse({
      contentType: 'application/octet-stream',
      disposition: 'attachment; filename="app.pem"',
      blob: new Blob([new Uint8Array([1, 2, 3])], { type: 'application/octet-stream' }),
    }))
    vi.stubGlobal('fetch', fn)

    const api = createApiClient(mockCluster)
    const res = await api.getAgentSecretValue('dave', 'APP_PEM')
    expect(res.kind).toBe('file')
    if (res.kind === 'file') {
      expect(res.filename).toBe('app.pem')
      expect(res.value).toBeInstanceOf(Blob)
    }
  })

  it('falls back to <key>.bin when Content-Disposition is absent', async () => {
    const { fn } = recordingFetch(mockResponse({
      contentType: 'application/octet-stream',
      blob: new Blob([new Uint8Array([1])]),
    }))
    vi.stubGlobal('fetch', fn)

    const api = createApiClient(mockCluster)
    const res = await api.getAgentSecretValue('dave', 'APP_PEM')
    if (res.kind === 'file') expect(res.filename).toBe('APP_PEM.bin')
  })

  it('throws KyberAPIError on non-2xx with structured error body', async () => {
    const { fn } = recordingFetch(mockResponse({
      status: 404,
      contentType: 'application/json',
      json: { error: { code: 'not_found', message: 'secret key not set' } },
    }))
    vi.stubGlobal('fetch', fn)

    const api = createApiClient(mockCluster)
    await expect(api.getAgentSecretValue('dave', 'FOO')).rejects.toMatchObject({
      status: 404,
      code: 'not_found',
    })
  })
})

describe('putAgentSecretKV', () => {
  it('PUTs JSON body with kind=kv', async () => {
    const { fn, calls } = recordingFetch(mockResponse({ status: 204, contentType: 'application/json' }))
    vi.stubGlobal('fetch', fn)

    const api = createApiClient(mockCluster)
    await api.putAgentSecretKV('dave', 'FOO', 'bar')
    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('http://localhost:8080/api/v1/agents/dave/secrets/FOO')
    expect(calls[0].init?.method).toBe('PUT')
    expect(calls[0].init?.body).toBe(JSON.stringify({ kind: 'kv', value: 'bar' }))
  })
})

describe('putAgentSecretFile', () => {
  it('PUTs multipart with "file" field and no explicit Content-Type', async () => {
    const { fn, calls } = recordingFetch(mockResponse({ status: 204 }))
    vi.stubGlobal('fetch', fn)

    const file = new File([new Uint8Array([0xab, 0xcd])], 'app.pem', { type: 'application/octet-stream' })
    const api = createApiClient(mockCluster)
    await api.putAgentSecretFile('dave', 'APP_PEM', file)

    expect(calls).toHaveLength(1)
    expect(calls[0].init?.method).toBe('PUT')
    expect(calls[0].init?.body).toBeInstanceOf(FormData)
    const form = calls[0].init?.body as FormData
    expect(form.get('file')).toBeInstanceOf(File)
    // Must NOT set Content-Type ourselves — fetch adds the multipart boundary.
    const headers = calls[0].init?.headers as Record<string, string> | undefined
    expect(headers && 'Content-Type' in headers).toBeFalsy()
  })
})

describe('deleteAgentSecret', () => {
  it('DELETEs the key', async () => {
    const { fn, calls } = recordingFetch(mockResponse({ status: 204, contentType: 'application/json' }))
    vi.stubGlobal('fetch', fn)

    const api = createApiClient(mockCluster)
    await api.deleteAgentSecret('dave', 'FOO')
    expect(calls[0].init?.method).toBe('DELETE')
    expect(calls[0].url).toBe('http://localhost:8080/api/v1/agents/dave/secrets/FOO')
  })
})
