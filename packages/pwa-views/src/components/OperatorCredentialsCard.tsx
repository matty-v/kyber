import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { createApiClient } from '../lib/api'
import { useCluster } from '../lib/cluster-context'
import type { OperatorCredentialInventory } from '../lib/types'
import { Button } from './Button'
import { Card } from './Card'
import { SecurityHistoryCard } from './SecurityHistoryCard'

export function OperatorCredentialsCard({ children }: { children?: ReactNode }) {
  const cluster = useCluster()
  const api = useMemo(() => createApiClient(cluster), [cluster.id, cluster.baseURL, cluster.apiKey])
  // Keep administrator-only metadata out of the shared query cache. A changed
  // connection must never paint the previous credential's inventory.
  const [result, setResult] = useState<{
    client: typeof api
    data?: OperatorCredentialInventory
    error?: number
    loading: boolean
  }>()
  const [refresh, setRefresh] = useState(0)

  useEffect(() => {
    let cancelled = false
    let inFlight = false
    let controller: AbortController | undefined
    let timeout: ReturnType<typeof setTimeout> | undefined
    async function load() {
      if (inFlight) return
      inFlight = true
      controller = new AbortController()
      timeout = setTimeout(() => controller?.abort(), 4_000)
      // Keep an open rotation dialog mounted while a same-connection
      // refresh runs. A denied response still clears all protected content.
      setResult((previous) => previous?.client === api
        ? { ...previous, loading: true }
        : { client: api, loading: true })
      try {
        const data = await api.getOperatorCredentials(controller.signal)
        if (!cancelled) setResult({ client: api, data, loading: false })
      } catch (error) {
        const status = typeof error === 'object' && error !== null && 'status' in error ? Number(error.status) : 0
        if (!cancelled) setResult({ client: api, error: status, loading: false })
      } finally {
        clearTimeout(timeout)
        inFlight = false
      }
    }
    void load()
    const timer = setInterval(() => { void load() }, 5_000)
    return () => { cancelled = true; clearInterval(timer); clearTimeout(timeout); controller?.abort() }
  }, [api, refresh])

  const current = result?.client === api ? result : undefined
  const loading = !current || current.loading
  const data = current?.data
  const error = current?.error

  return <><Card>
    <div className="flex items-center justify-between gap-3">
      <h2 className="text-sm font-medium text-text-muted">Operator credentials</h2>
      <Button size="sm" onClick={() => setRefresh((n) => n + 1)} disabled={loading}>Refresh</Button>
    </div>
    {loading && <p className="mt-4 text-sm text-text-muted" role="status">Loading credentials…</p>}
    {!loading && !data && <p className="mt-4 text-sm text-text-muted" role="alert">
      {error === 403 ? 'Administrator access is required to view operator credentials.'
        : error === 404 ? 'Credential inventory is not available on this installation. Upgrade Kyber to view it here.'
          : 'Could not load operator credentials. Try refreshing.'}
    </p>}
    {data && <>
      <p className="mt-3 text-sm text-text-muted">Keys active on this Kyber server, plus additional configured keys. Secret values are never shown.</p>
      {data.warnings.map((warning) => <p key={warning} role="alert" className="mt-3 text-sm text-warning">{warning}</p>)}
      {data.credentials.length === 0 ? <p className="mt-4 text-sm text-text-muted">No accepted credentials were reported.</p> : <ul className="mt-4 space-y-3">
        {data.credentials.map((credential, index) => <li key={`${credential.source}:${credential.id}:${index}`} className="min-w-0 rounded-lg border border-border-subtle p-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h3 className="break-words text-sm font-medium text-text-primary">{credential.displayName || 'Unnamed credential'}</h3>
            <span className="text-xs text-text-muted">{credential.state === 'accepted' ? 'Active' : credential.state === 'shared-key' ? 'Shared key' : 'Configured, not active'}</span>
          </div>
          <p className="mt-1 text-xs text-text-muted">{credential.accessLevel === 'administrator' ? 'Administrator' : 'Restricted integration'} · {credential.source === 'installation' ? 'Provided by the installation' : 'Managed outside Kyber'}</p>
          {credential.authenticatesAs && <p className="mt-2 break-words text-sm text-warning">This key authenticates as {credential.authenticatesAs}. It does not have separate access.</p>}
          {credential.state !== 'configured-not-accepted' ? <>
            {credential.permissions.length > 0 ? <ul className="mt-3 space-y-1 text-sm text-text-primary">
              {credential.permissions.map((permission, i) => <li key={i}>{permission}</li>)}
            </ul> : <p className="mt-3 text-sm text-text-muted">No additional operator permissions.</p>}
            <p className="mt-2 break-words text-xs text-text-muted">Agent access: {credential.agentResources.includes('*') ? 'All Agents' : credential.agentResources.length > 0 ? credential.agentResources.join(', ') : 'No Agents specified'}</p>
            {credential.accessLevel === 'restricted' && credential.agentResources.length === 0 && credential.scopes.some((scope) => scope !== 'metrics:read' && scope !== 'machines:exec' && scope !== 'machines:logs:read') && <p className="mt-2 text-xs text-text-muted">Agent-specific permissions have no allowed target.</p>}
            {(!!credential.machineResources?.length || credential.scopes.includes('machines:exec')) && <p className="mt-2 break-words text-xs text-text-muted">Machine access: {credential.machineResources?.includes('*') ? 'All Machines' : credential.machineResources?.length ? credential.machineResources.join(', ') : 'No Machines specified'}</p>}
            {data.configurationAvailable && !credential.configured && <p className="mt-2 text-sm text-warning">Still active, but no longer present in configuration.</p>}
          </> : <p className="mt-3 text-sm text-text-muted">This credential is configured but is not accepted by the current control plane. Configuration may need correction or a restart.</p>}
          {credential.configurationDiffers && <p className="mt-2 text-sm text-warning">Configured permissions differ from active access. Active access is shown above.</p>}
          <p className="mt-3 text-xs text-text-muted">Creation time unknown · Last use unavailable</p>
          {credential.scopes.length > 0 && <details className="mt-2 text-xs text-text-muted">
            <summary className="cursor-pointer">Permission identifiers</summary>
            <p className="mt-1 break-words">{credential.scopes.join(', ')}</p>
          </details>}
        </li>)}
      </ul>}
      <p className="mt-4 text-xs text-text-muted">Inventory checked {new Date(data.observedAt).toLocaleString()}. Complete usage totals are not available.</p>
      <details className="mt-2 text-xs text-text-muted">
        <summary className="cursor-pointer">About this inventory</summary>
        <p className="mt-2">Keys shared by several people or tools cannot identify individual users. Changes made outside Kyber may require a control-plane restart. Pending key replacements are not compared here.</p>
      </details>
    </>}
  </Card>{data && <SecurityHistoryCard credentials={data.credentials} />}{data && children}</>
}
