import { useEffect, useRef, useState } from 'react'
import type { ApiClient } from '../lib/api'
import type { MaintenanceRecoveryOperation, MaintenanceRecoveryReadiness } from '../lib/types'
import { Button } from './Button'

const reasons: Record<string, string> = {
  ready: 'Ready to recover.', idle: 'No maintenance block.',
  'writer-running': 'The operation is still running.',
  'writer-unverified': 'Kyber cannot confirm that the operation has stopped.',
  'release-unverified': 'The release needs further inspection before recovery.',
  'metadata-changed': 'Maintenance changed. Check again.',
  'recovery-pending': 'A recovery is already pending. Check its outcome below.',
  'recovery-unverified': 'The previous recovery needs further inspection.',
}
function validOperation(value: MaintenanceRecoveryOperation): boolean {
  return !!value && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(value.operationId)
    && ['pending', 'success', 'failure'].includes(value.status) && Number.isFinite(Date.parse(value.observedAt))
}

// Mounted only for the currently verified connection/status. Reads never resolve
// an operation; uncertain writes retain their ID and require an explicit action.
export function MaintenanceRecoveryControls({ api, version }: { api: ApiClient; version?: string }) {
  const [readiness, setReadiness] = useState<MaintenanceRecoveryReadiness>()
  const [operations, setOperations] = useState<MaintenanceRecoveryOperation[]>([])
  const [next, setNext] = useState<string>()
  const [uncertain, setUncertain] = useState<string>()
  const [message, setMessage] = useState('')
  const [busy, setBusy] = useState(false)
  const [denied, setDenied] = useState(false)
  const active = useRef(true)
  const inFlight = useRef(false)
  const controllers = useRef(new Set<AbortController>())
  useEffect(() => {
    active.current = true
    return () => { active.current = false; for (const controller of controllers.current) controller.abort() }
  }, [])
  async function run(action: (signal: AbortSignal) => Promise<void>) {
    if (inFlight.current) return
    inFlight.current = true; setBusy(true); setMessage('')
    const controller = new AbortController()
    controllers.current.add(controller)
    const timeout = setTimeout(() => controller.abort(), 12_000)
    try { await action(controller.signal) } catch (error) {
      if (!active.current) return
      setReadiness(undefined); setOperations([]); setNext(undefined)
      const status = typeof error === 'object' && error !== null && 'status' in error ? Number(error.status) : 0
      if (status === 401 || status === 403) {
        setDenied(true); setOperations([]); setNext(undefined); setUncertain(undefined)
        setMessage('Administrator access is no longer available.')
      } else setMessage('The outcome could not be confirmed. Check pending recoveries before trying again.')
    } finally {
      clearTimeout(timeout); controllers.current.delete(controller); inFlight.current = false
      if (active.current) setBusy(false)
    }
  }
  function accept(operation: MaintenanceRecoveryOperation) {
    if (!validOperation(operation)) throw new Error('Unsupported recovery outcome')
    if (!active.current) return
    setOperations(current => [operation, ...current.filter(item => item.operationId !== operation.operationId)])
    setUncertain(undefined); setReadiness(undefined)
  }
  return <div className="space-y-2 text-sm text-text-primary">
    <div className="flex flex-wrap gap-2">
      {version && <Button variant="secondary" size="sm" disabled={busy || denied} onClick={() => { void run(async signal => {
        const result = await api.checkMaintenanceRecovery(signal)
        if (!result || typeof result.ready !== 'boolean' || !(result.reason in reasons)
          || result.ready !== (result.reason === 'ready') || (result.ready && result.version !== version)) throw new Error('Stale recovery check')
        if (active.current) setReadiness(result)
      }) }}>Check recovery</Button>}
      {readiness?.ready && readiness.version === version && <Button variant="secondary" size="sm" disabled={busy || denied || !!uncertain} onClick={() => {
        void run(async signal => {
          const id = crypto.randomUUID()
          setUncertain(id); setReadiness(undefined)
          accept(await api.recoverCredentialMaintenance(id, version!, signal))
        })
      }}>Recover maintenance</Button>}
      <Button variant="secondary" size="sm" disabled={busy || denied} onClick={() => { void run(async signal => {
        const page = await api.getPendingMaintenanceRecoveryOperations(undefined, signal)
        if (!page || !Array.isArray(page.items) || !page.items.every(validOperation)) throw new Error('Unsupported recovery list')
        if (active.current) { setOperations(page.items); setNext(page.next); setReadiness(undefined) }
      }) }}>Check pending recoveries</Button>
    </div>
    {readiness && <p role="status">{reasons[readiness.reason]}</p>}
    {message && <p role="alert">{message}</p>}
    {uncertain && <div className="space-y-1"><p>Recovery outcome is unconfirmed.</p>
      <Button variant="secondary" size="sm" disabled={busy || denied} onClick={() => { void run(async signal => { accept(await api.getMaintenanceRecoveryOperation(uncertain, signal)) }) }}>Check outcome</Button>
    </div>}
    {operations.map(operation => <div key={operation.operationId} className="space-y-1 border-t border-border-default pt-2">
      <p>{operation.status === 'success' ? 'Maintenance recovered.' : operation.status === 'failure' ? 'Recovery did not complete.' : 'Recovery pending.'}</p>
      {operation.status === 'pending' && <div className="flex flex-wrap gap-2">
        <Button variant="secondary" size="sm" disabled={busy || denied} onClick={() => { void run(async signal => { accept(await api.getMaintenanceRecoveryOperation(operation.operationId, signal)) }) }}>Check outcome</Button>
        <Button variant="secondary" size="sm" disabled={busy || denied} onClick={() => { void run(async signal => { accept(await api.resolveMaintenanceRecovery(operation.operationId, signal)) }) }}>Resolve recovery</Button>
      </div>}
      <details className="text-xs"><summary>Recovery details</summary><p className="break-all">{operation.operationId}</p><p>{new Date(operation.observedAt).toLocaleString()}</p></details>
    </div>)}
    {next && <Button variant="secondary" size="sm" disabled={busy || denied} onClick={() => { void run(async signal => {
      const page = await api.getPendingMaintenanceRecoveryOperations(next, signal)
      if (!page || !Array.isArray(page.items) || !page.items.every(validOperation)) throw new Error('Unsupported recovery list')
      if (active.current) { setOperations(current => [...current, ...page.items.filter(item => !current.some(old => old.operationId === item.operationId))]); setNext(page.next) }
    }) }}>More pending recoveries</Button>}
  </div>
}
