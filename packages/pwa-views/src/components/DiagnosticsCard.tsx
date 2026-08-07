import { useCallback, useEffect, useMemo, useState } from 'react'
import { CheckCircle2, XCircle, Loader2 } from 'lucide-react'
import { Card } from './Card'
import { createApiClient, getLastApiCall } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import { useLiveVersion } from '../hooks/useLiveVersion'

const REFRESH_MS = 30_000

function shortSha(sha: string): string {
  if (!sha) return '—'
  return sha.slice(0, 7)
}

function formatBuildDate(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

function formatLastCall(iso: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const deltaSec = Math.max(0, Math.round((Date.now() - d.getTime()) / 1000))
  if (deltaSec < 60) return `${deltaSec}s ago`
  if (deltaSec < 3600) return `${Math.round(deltaSec / 60)}m ago`
  return d.toLocaleString()
}

export function DiagnosticsCard() {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const { versionInfo } = useLiveVersion()
  const [healthy, setHealthy] = useState<boolean | null>(null)
  const [lastCall, setLastCall] = useState<string | null>(getLastApiCall())

  const refresh = useCallback(async () => {
    const h = await api.checkHealthz()
    setHealthy(h)
    setLastCall(getLastApiCall())
  }, [api])

  useEffect(() => {
    void refresh()
    const id = window.setInterval(() => {
      if (document.visibilityState === 'visible') void refresh()
    }, REFRESH_MS)
    const onVis = () => { if (document.visibilityState === 'visible') void refresh() }
    document.addEventListener('visibilitychange', onVis)
    return () => {
      window.clearInterval(id)
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [refresh])

  const substrate = versionInfo?.substrate || '—'
  const sha = shortSha(versionInfo?.sha ?? '')
  const build = formatBuildDate(versionInfo?.buildDate ?? '')

  return (
    <Card className="max-w-lg mt-4">
      <h2 className="text-sm font-medium text-text-muted mb-3">Version</h2>
      <dl className="space-y-2 text-sm">
        <div className="flex justify-between">
          <dt className="text-text-muted">Cluster</dt>
          <dd className="text-text-secondary">{cluster.name || '—'}</dd>
        </div>
        <div className="flex justify-between">
          <dt className="text-text-muted">Environment</dt>
          <dd className="text-text-secondary">{substrate}</dd>
        </div>
        <div className="flex justify-between">
          <dt className="text-text-muted">Control-plane</dt>
          <dd className="text-text-secondary">
            {sha === '—' && build === '—' ? '—' : `${sha} · ${build}`}
            {versionInfo?.chartVersion && <span className="text-text-disabled"> · chart {versionInfo.chartVersion}</span>}
          </dd>
        </div>
        <div className="flex justify-between">
          <dt className="text-text-muted">Connected</dt>
          <dd className={`flex items-center gap-1.5 ${healthy === null ? 'text-text-disabled' : healthy ? 'text-success' : 'text-danger'}`}>
            {healthy === null ? (
              <><Loader2 className="h-3.5 w-3.5 animate-spin" /> checking…</>
            ) : healthy ? (
              <><CheckCircle2 className="h-3.5 w-3.5" /> healthy</>
            ) : (
              <><XCircle className="h-3.5 w-3.5" /> unreachable</>
            )}
          </dd>
        </div>
        <div className="flex justify-between">
          <dt className="text-text-muted">Last API call</dt>
          <dd className="text-text-secondary">{formatLastCall(lastCall)}</dd>
        </div>
      </dl>
    </Card>
  )
}
