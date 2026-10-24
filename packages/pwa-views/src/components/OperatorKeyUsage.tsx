import { useEffect, useState, type ReactNode } from 'react'
import type { ApiClient } from '../lib/api'
import type { OperatorCredential, OperatorCredentialUsage } from '../lib/types'
import { validOperatorCredentialUsage } from '../lib/operator-usage'
import { OperatorKeyUsageCoverage, OperatorKeyUsageRow } from './OperatorKeyUsageRow'

// Admin metadata stays in connection-bound local state, outside shared caches.
export function OperatorKeyUsage({ api, enabled, revision = 0, children }: { api: ApiClient; enabled: boolean; revision?: number; children: (usage: (credential: OperatorCredential) => ReactNode) => ReactNode }) {
  const [result, setResult] = useState<{ client: ApiClient; data?: OperatorCredentialUsage; error?: number; loading: boolean }>()
  useEffect(() => {
    if (!enabled) { setResult(undefined); return }
    let cancelled = false, inFlight = false
    let controller: AbortController | undefined
    let timeout: ReturnType<typeof setTimeout> | undefined
    async function load() {
      if (inFlight) return
      inFlight = true; controller = new AbortController()
      timeout = setTimeout(() => controller?.abort(), 4_000)
      setResult((previous) => previous?.client === api ? { ...previous, loading: true } : { client: api, loading: true })
      try {
        const data = await api.getOperatorCredentialUsage(controller.signal)
        if (!validOperatorCredentialUsage(data)) throw new Error('Unsupported activity response')
        if (!cancelled) setResult({ client: api, data, loading: false })
      } catch (failure) {
        const status = typeof failure === 'object' && failure !== null && 'status' in failure ? Number(failure.status) : 0
        if (!cancelled) setResult({ client: api, error: status, loading: false })
      } finally { clearTimeout(timeout); inFlight = false }
    }
    void load()
    const timer = setInterval(() => { void load() }, 5_000)
    return () => { cancelled = true; clearInterval(timer); clearTimeout(timeout); controller?.abort() }
  }, [api, enabled, revision])
  const current = result?.client === api && enabled ? result : undefined
  return <>
    {enabled && current?.data && <OperatorKeyUsageCoverage data={current.data} />}
    {enabled && !current?.data && (current?.loading || !current) && <p role="status" className="mb-3 text-xs text-text-muted">Loading key activity…</p>}
    {enabled && current && !current.loading && !current.data && <p role="alert" className="mb-3 text-xs text-warning">{current.error === 403 ? 'Administrator access is required to view key activity.' : current.error === 404 ? 'Key activity requires a newer Kyber version.' : 'Key activity is unavailable. No activity counts are being shown.'}</p>}
    {children((credential) => <OperatorKeyUsageRow credential={credential} data={current?.data} enabled={enabled} />)}
  </>
}
