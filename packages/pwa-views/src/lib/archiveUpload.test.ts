import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { createApiClient, UPLOAD_PART_RETRY_DELAYS_MS } from './api'
import type { Cluster } from './cluster-context'

const cluster: Cluster = {
  id: 'local',
  name: 'kyber-test',
  baseURL: 'http://localhost:8080',
  apiKey: 'test-key',
  version: '1.0.0',
  capabilities: [],
}

type Reply = { status: number; body?: unknown } | 'network-error'

// A part PUT the fake XHR saw, with the bytes it carried.
interface Sent {
  method: string
  url: string
  body: string
}

let sent: Sent[] = []
let replies: Reply[] = []

// FakeXHR answers each send with the next queued reply, after reporting
// upload progress of half the body and then all of it.
class FakeXHR {
  status = 0
  responseText = ''
  withCredentials = false
  upload: { onprogress: ((e: { loaded: number; total: number }) => void) | null } = { onprogress: null }
  onload: (() => void) | null = null
  onerror: (() => void) | null = null
  private method = ''
  private url = ''
  open(method: string, url: string) {
    this.method = method
    this.url = url
  }
  setRequestHeader() {}
  send(body: Blob) {
    void body.text().then((text) => {
      sent.push({ method: this.method, url: this.url, body: text })
      const reply = replies.shift() ?? { status: 200, body: {} }
      this.upload.onprogress?.({ loaded: Math.floor(body.size / 2), total: body.size })
      if (reply === 'network-error') {
        this.onerror?.()
        return
      }
      this.upload.onprogress?.({ loaded: body.size, total: body.size })
      this.status = reply.status
      this.responseText = JSON.stringify(reply.body ?? {})
      this.onload?.()
    })
  }
}

let fetchCalls: { method: string; url: string; body?: string }[] = []

beforeEach(() => {
  sent = []
  replies = []
  fetchCalls = []
  vi.stubGlobal('localStorage', { getItem: () => null, setItem: () => {}, removeItem: () => {} })
  vi.stubGlobal('XMLHttpRequest', FakeXHR)
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
    fetchCalls.push({ method: init.method ?? 'GET', url, body: init.body as string | undefined })
    const job = url.endsWith('/complete')
      ? { id: 'up-1', kind: 'upload', state: 'running' }
      : { id: 'up-1', kind: 'upload', state: 'running', partSize: 4, partCount: 3 }
    return { ok: true, status: url.endsWith('/complete') ? 202 : 201, json: async () => job }
  }))
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const file = new Blob(['0123456789']) // 3 parts of 4 bytes, the last one short

describe('uploadArchive', () => {
  it('declares the size, sends each part in order and completes', async () => {
    const job = await createApiClient(cluster).uploadArchive(file)

    expect(fetchCalls[0]).toEqual({ method: 'POST', url: 'http://localhost:8080/api/v1/archive-uploads', body: '{"size":10}' })
    expect(sent).toEqual([
      { method: 'PUT', url: 'http://localhost:8080/api/v1/archive-uploads/up-1/parts/0', body: '0123' },
      { method: 'PUT', url: 'http://localhost:8080/api/v1/archive-uploads/up-1/parts/1', body: '4567' },
      { method: 'PUT', url: 'http://localhost:8080/api/v1/archive-uploads/up-1/parts/2', body: '89' },
    ])
    expect(fetchCalls[1]).toMatchObject({ method: 'POST', url: 'http://localhost:8080/api/v1/archive-uploads/up-1/complete' })
    expect(job.id).toBe('up-1')
  })

  it('reports progress across the whole file', async () => {
    const progress: [number, number][] = []
    await createApiClient(cluster).uploadArchive(file, (loaded, total) => progress.push([loaded, total]))

    expect(progress.every(([, total]) => total === 10)).toBe(true)
    const loaded = progress.map(([l]) => l)
    expect(loaded).toEqual([...loaded].sort((a, b) => a - b))
    expect(loaded).toContain(6) // halfway through part 1 counts part 0 too
    expect(loaded[loaded.length - 1]).toBe(10)
  })

  it('retries a part that failed with a transient error, then carries on', async () => {
    vi.useFakeTimers()
    replies = [{ status: 200 }, 'network-error', { status: 503 }, { status: 200 }, { status: 200 }]
    const done = createApiClient(cluster).uploadArchive(file)
    await vi.advanceTimersByTimeAsync(UPLOAD_PART_RETRY_DELAYS_MS[0] + UPLOAD_PART_RETRY_DELAYS_MS[1])
    await done

    expect(sent.map((s) => s.url.split('/').pop())).toEqual(['0', '1', '1', '1', '2'])
    expect(sent.filter((s) => s.url.endsWith('/1')).every((s) => s.body === '4567')).toBe(true)
    expect(fetchCalls[1].url).toMatch(/\/complete$/)
  })

  it('gives up on a part after the last retry', async () => {
    vi.useFakeTimers()
    replies = Array(UPLOAD_PART_RETRY_DELAYS_MS.length + 1).fill({ status: 502 })
    const done = createApiClient(cluster).uploadArchive(file)
    const failed = expect(done).rejects.toMatchObject({ status: 502 })
    await vi.advanceTimersByTimeAsync(UPLOAD_PART_RETRY_DELAYS_MS.reduce((a, b) => a + b, 0))
    await failed

    expect(sent).toHaveLength(UPLOAD_PART_RETRY_DELAYS_MS.length + 1)
    expect(fetchCalls.some((c) => c.url.endsWith('/complete'))).toBe(false)
  })

  it('does not retry a part the server rejected', async () => {
    replies = [{ status: 409, body: { error: { code: 'conflict', message: 'upload failed' } } }]
    await expect(createApiClient(cluster).uploadArchive(file)).rejects.toMatchObject({ status: 409, message: 'upload failed' })
    expect(sent).toHaveLength(1)
  })
})
