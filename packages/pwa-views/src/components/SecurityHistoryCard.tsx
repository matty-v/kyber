import { useEffect, useMemo, useState } from 'react'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import type { OperatorCredential, SecurityEvent, SecurityEventPage } from '../lib/types'
import { Button } from './Button'
import { Card } from './Card'

const actions: Record<string, string> = {
  'agents:exec': 'Use Agent terminal', 'machines:exec': 'Use Machine terminal',
  'secrets:list': 'List Agent credential names', 'secrets:read': 'Read Agent credential values',
  'secrets:write': 'Change Agent credentials', 'logs:read': 'Read logs',
  'transcripts:read': 'Read transcripts', 'machines:logs:read': 'Read Machine logs',
  'audit:read': 'View access history', 'operator:unclassified': 'Other protected action',
  'authenticate': 'Authenticate', 'credential:create': 'Create operator key',
  'credential:update': 'Change operator key access', 'credential:rotate': 'Rotate operator key',
  'credential:revoke': 'Revoke operator key', 'lifecycle:write': 'Change lifecycle',
  'lifecycle:delete': 'Delete resource', 'archives:export': 'Request disk export', 'archives:import': 'Request disk import',
  'archives:read': 'Read archive details', 'archives:download': 'Download disk archive', 'archives:link': 'Create archive download link',
  'archives:upload': 'Upload disk archive', 'archives:cancel': 'Cancel archive operation', 'archives:delete': 'Delete disk archive',
}

function resultLabel(event: SecurityEvent): string {
  if (event.phase === 'completion') return event.outcome === 'success' ? 'Request completed' : event.outcome === 'failure' ? 'Failed' : 'Completion unknown'
  return event.outcome === 'allow' ? 'Access allowed' : 'Access denied'
}

function targetLabel(event: SecurityEvent): string {
  switch (event.resourceKind) {
    case 'installation': return 'This installation'
    case 'agent': return `Agent ${event.resourceName}`
    case 'machine': return `Machine ${event.resourceName}`
    case 'credential': return `Operator key ${event.resourceName}`
    case 'log-container': return `Logs: ${event.container || event.resourceName}`
  }
}

export function SecurityHistoryCard({ credentials }: { credentials: OperatorCredential[] }) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  const [credential, setCredential] = useState('')
  const [includeHistoryViews, setIncludeHistoryViews] = useState(false)
  const [cursor, setCursor] = useState<string>()
  const [refresh, setRefresh] = useState(0)
  // Administrator metadata stays out of shared caches. Match connection AND
  // selection before painting, and ignore responses from abandoned requests.
  const [result, setResult] = useState<{
    client: typeof api; credential: string; cursor?: string; includeHistoryViews: boolean
    data?: SecurityEventPage; error?: number; loading: boolean
  }>()
  useEffect(() => {
    let cancelled = false
    const controller = new AbortController()
    const timeout = setTimeout(() => controller.abort(), 10_000)
    setResult({ client: api, credential, cursor, includeHistoryViews, loading: true })
    void api.getSecurityEvents({ limit: 25, credentialId: credential || undefined, cursor, includeHistoryViews }, controller.signal).then(
      (data) => { if (!cancelled) setResult({ client: api, credential, cursor, includeHistoryViews, data, loading: false }) },
      (error: unknown) => {
        const status = typeof error === 'object' && error !== null && 'status' in error ? Number(error.status) : 0
        if (!cancelled) setResult({ client: api, credential, cursor, includeHistoryViews, error: status, loading: false })
      },
    ).finally(() => clearTimeout(timeout))
    return () => { cancelled = true; clearTimeout(timeout); controller.abort() }
  }, [api, credential, cursor, includeHistoryViews, refresh])

  const names = new Map<string, string>()
  const duplicates = new Set<string>()
  for (const item of credentials.filter((c) => c.state === 'accepted' && c.id)) {
    if (names.has(item.id)) duplicates.add(item.id)
    names.set(item.id, item.displayName || item.id)
  }
  for (const id of duplicates) names.set(id, id)
  const current = result?.client === api && result.credential === credential && result.cursor === cursor && result.includeHistoryViews === includeHistoryViews ? result : undefined
  const data = current?.data
  const loading = !current || current.loading

  return <Card>
    <div className="flex flex-wrap items-center justify-between gap-3">
      <h2 className="text-sm font-medium text-text-muted">Recent access</h2>
      <Button size="sm" disabled={loading} onClick={() => { setCursor(undefined); setRefresh((n) => n + 1) }}>Refresh activity</Button>
    </div>
    <p className="mt-3 text-sm text-text-muted">Access decisions and request results appear separately.</p>
    <label className="mt-4 block text-sm text-text-muted">
      Operator key
      <select value={credential} onChange={(e) => { setCredential(e.target.value); setCursor(undefined) }} className="mt-1 block w-full min-w-0 rounded border border-border-subtle bg-surface-subtle p-2 text-text-primary">
        <option value="">All keys</option>
        {[...names].map(([id, name]) => <option key={id} value={id}>{name}</option>)}
        {credential && !names.has(credential) && <option value={credential}>Previously selected key</option>}
      </select>
    </label>
    <label className="mt-3 flex items-center gap-2 text-sm text-text-muted">
      <input type="checkbox" checked={includeHistoryViews} onChange={(e) => { setIncludeHistoryViews(e.target.checked); setCursor(undefined) }} />
      Include history views
    </label>
    <p className="mt-1 text-xs text-text-muted">Recorded denials, failures and uncertain outcomes remain visible.</p>
    {loading && <p className="mt-4 text-sm text-text-muted" role="status">Loading activity…</p>}
    {!loading && !data && <p className="mt-4 text-sm text-warning" role="alert">
      {current?.error === 403 ? 'Administrator access is required to view activity.'
        : current?.error === 404 ? 'Access history is not available on this installation. Upgrade Kyber to view it here.'
          : current?.error === 400 ? 'Could not apply these history filters. Refresh activity or upgrade Kyber.'
          : 'Access history is unavailable. Try refreshing.'}
    </p>}
    {data && <>
      {data.items.length === 0 ? <p className="mt-4 text-sm text-text-muted">No recorded activity for this selection.</p> : <ul aria-label="Access activity" className="mt-4 space-y-3">
        {data.items.map((event) => <li key={event.id} className="min-w-0 rounded-lg border border-border-subtle p-3">
          <div className="flex flex-wrap justify-between gap-2 text-sm">
            <span className="font-medium text-text-primary">{actions[event.action] || 'Protected action'}</span>
            <span className={event.outcome === 'deny' || event.outcome === 'failure' ? 'text-warning' : 'text-text-muted'}>{resultLabel(event)}</span>
          </div>
          <p className="mt-1 break-words text-sm text-text-primary">{targetLabel(event)}</p>
          <p className="mt-1 break-all text-xs text-text-muted">{names.get(event.credentialId) || event.credentialId || 'Unidentified credential'}</p>
          <time dateTime={event.observedAt} className="mt-1 block text-xs text-text-muted">{new Date(event.observedAt).toLocaleString()}</time>
          <details className="mt-2 text-xs text-text-muted">
            <summary className="cursor-pointer">Record details</summary>
            <p className="mt-1 break-all">Request: {event.requestId}</p>
            <p className="mt-1 break-all">Operation: {event.operationId}</p>
          </details>
        </li>)}
      </ul>}
      {data.nextCursor && <Button className="mt-4" size="sm" onClick={() => setCursor(data.nextCursor)}>Older activity</Button>}
      <p className="mt-4 text-xs text-text-muted">Loaded {new Date(data.observedAt).toLocaleString()}.</p>
      <p className="mt-2 text-xs text-text-muted">No recorded activity does not mean a key has never been used. Shared keys cannot identify individual people or applications.</p>
      <details className="mt-2 text-xs text-text-muted">
        <summary className="cursor-pointer">History coverage</summary>
        {data.warnings.map((warning) => <p key={warning} className="mt-2">{warning}</p>)}
      </details>
    </>}
  </Card>
}
