import { MaintenanceRecoveryControls } from './MaintenanceRecoveryControls'
import { useEffect, useState } from 'react'
import type { ApiClient } from '../lib/api'
import type { OperatorCredentialMaintenanceStatus } from '../lib/types'

function validStatus(value: OperatorCredentialMaintenanceStatus): boolean {
  if (value?.recoveryAvailable !== undefined && typeof value.recoveryAvailable !== 'boolean') return false
  if (!value || !Number.isFinite(Date.parse(value.observedAt)) || typeof value.blocked !== 'boolean') return false
  if (!value.blocked) return value.operation === undefined && value.version === undefined
  return typeof value.version === 'string' && /^"[A-Za-z0-9_-]{43}"$/.test(value.version)
    && !!value.operation && ['adoption', 'upgrade'].includes(value.operation.kind)
    && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(value.operation.executionId)
    && Number.isFinite(Date.parse(value.operation.startedAt))
}

// Connection-bound protected metadata; no acquisition or recovery side effects.
export function OperatorCredentialMaintenance({ api, enabled, revision = 0 }: { api: ApiClient; enabled: boolean; revision?: number }) {
  const [result, setResult] = useState<{ client: ApiClient; data?: OperatorCredentialMaintenanceStatus; failed?: boolean }>()
  useEffect(() => {
    if (!enabled) { setResult(undefined); return }
    let cancelled = false, inFlight = false
    let controller: AbortController | undefined
    let timeout: ReturnType<typeof setTimeout> | undefined
    async function load() {
      if (inFlight) return
      inFlight = true; controller = new AbortController()
      timeout = setTimeout(() => controller?.abort(), 4_000)
      try {
        const data = await api.getOperatorCredentialMaintenance(controller.signal)
        if (!validStatus(data)) throw new Error('Unsupported maintenance status')
        if (!cancelled) setResult({ client: api, data })
      } catch {
        if (!cancelled) setResult({ client: api, failed: true })
      } finally { clearTimeout(timeout); inFlight = false }
    }
    void load()
    const timer = setInterval(() => { void load() }, 5_000)
    return () => { cancelled = true; clearInterval(timer); clearTimeout(timeout); controller?.abort() }
  }, [api, enabled, revision])
  if (!enabled) return null
  const current = result?.client === api ? result : undefined
  if (!current) return <p role="status" className="mt-3 text-xs text-text-muted">Checking credential maintenance…</p>
  if (current.failed || !current.data) return <p role="alert" className="mt-3 text-xs text-warning">Credential maintenance status is unavailable.</p>
  const data = current.data
  if (!data.blocked) return <><p className="mt-3 text-xs text-text-muted">No credential maintenance block.</p>{data.recoveryAvailable && <MaintenanceRecoveryControls api={api} />}</>
  return <div role="status" className="mt-3 space-y-2 rounded-lg border border-warning p-3 text-sm text-warning">
    <p>{data.operation!.kind === 'upgrade' ? 'An upgrade' : 'A key adoption'} operation holds credential maintenance.</p>
    <p className="text-xs">Started {new Date(data.operation!.startedAt).toLocaleString()}. This block does not expire automatically.</p>
    {data.recoveryAvailable && <MaintenanceRecoveryControls key={data.version} api={api} version={data.version} />}
    <details className="text-xs"><summary className="cursor-pointer">Operation details</summary>
      <p className="mt-2 break-all">Execution {data.operation!.executionId}</p>
      <p className="mt-2">Recovery must confirm the operation has stopped and check its changes.</p>
      <p className="mt-2">Checked {new Date(data.observedAt).toLocaleString()}.</p>
    </details>
  </div>
}
