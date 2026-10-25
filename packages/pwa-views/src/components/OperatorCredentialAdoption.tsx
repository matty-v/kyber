import { useEffect, useRef, useState } from 'react'
import type { ApiClient } from '../lib/api'
import type { OperatorCredentialAdoptionOperation, OperatorCredentialAdoptionPlan } from '../lib/types'
import { Button } from './Button'

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
function validOperation(value: OperatorCredentialAdoptionOperation): boolean {
  return !!value && uuid.test(value.operationId) && ['pending', 'failed', 'finishing', 'succeeded'].includes(value.status)
    && typeof value.recoverable === 'boolean' && Number.isFinite(Date.parse(value.observedAt))
}

export function OperatorCredentialAdoption(props: { api: ApiClient; allowStart: boolean; onCompleted: () => void }) {
  // Remount all protected state when either the server or credential changes.
  const connection = useRef({ api: props.api, revision: 0 })
  if (connection.current.api !== props.api) connection.current = { api: props.api, revision: connection.current.revision + 1 }
  return <AdoptionControls key={connection.current.revision} {...props} />
}
function AdoptionControls({ api, allowStart, onCompleted }: { api: ApiClient; allowStart: boolean; onCompleted: () => void }) {
  const [plan, setPlan] = useState<OperatorCredentialAdoptionPlan>()
  const [confirmed, setConfirmed] = useState(false)
  const [operations, setOperations] = useState<OperatorCredentialAdoptionOperation[]>([])
  const [uncertain, setUncertain] = useState<string>()
  const [message, setMessage] = useState('')
  const [busy, setBusy] = useState(false)
  const [denied, setDenied] = useState(false)
  const active = useRef(true), inFlight = useRef(false)
  const controllers = useRef(new Set<AbortController>())
  useEffect(() => {
    active.current = true
    return () => { active.current = false; for (const controller of controllers.current) controller.abort() }
  }, [])
  async function run(action: (signal: AbortSignal) => Promise<void>) {
    if (inFlight.current || denied) return
    inFlight.current = true; setBusy(true); setMessage('')
    const controller = new AbortController(); controllers.current.add(controller)
    const timeout = setTimeout(() => controller.abort(), 55_000)
    try { await action(controller.signal) } catch (error) {
      if (!active.current) return
      setPlan(undefined); setConfirmed(false); setOperations([])
      const status = typeof error === 'object' && error !== null && 'status' in error ? Number(error.status) : 0
      if (status === 401 || status === 403) {
        setDenied(true); setUncertain(undefined)
        setMessage('Administrator access is no longer available.')
      } else setMessage('Setup could not be confirmed. Check its status before trying again.')
    } finally {
      clearTimeout(timeout); controllers.current.delete(controller); inFlight.current = false
      if (active.current) setBusy(false)
    }
  }
  function accept(value: OperatorCredentialAdoptionOperation, expected: string) {
    if (!validOperation(value) || value.operationId !== expected) throw new Error('Unsupported setup status')
    if (!active.current) return
    setOperations(previous => [value, ...previous.filter(item => item.operationId !== value.operationId)])
    setPlan(undefined); setConfirmed(false); setUncertain(undefined)
    if (value.status === 'succeeded') onCompleted()
  }
  const pending = !!uncertain || operations.some(item => item.status === 'pending' || item.status === 'finishing')
  return <div className="mt-4 space-y-3 text-sm text-text-primary">
    <div className="flex flex-wrap gap-2">
      {allowStart && <Button size="sm" disabled={busy || denied || pending} onClick={() => { void run(async signal => {
        const value = await api.prepareOperatorCredentialAdoption(signal)
        if (!value || !/^[0-9a-f]{64}$/.test(value.version) || !Number.isSafeInteger(value.keyCount) || value.keyCount < 1
          || !Number.isSafeInteger(value.reconnectCount) || value.reconnectCount < 0) throw new Error('Unsupported setup plan')
        if (active.current) { setPlan(value); setConfirmed(false) }
      }) }}>Manage existing keys in Kyber</Button>}
      <Button size="sm" variant="secondary" disabled={busy || denied} onClick={() => { void run(async signal => {
        const page = await api.getPendingOperatorCredentialAdoptions(signal)
        if (!page || page.limit !== 25 || !Array.isArray(page.items) || page.items.length > 25 || !page.items.every(validOperation)) throw new Error('Unsupported setup list')
        if (active.current) { setOperations(page.items); setPlan(undefined); setConfirmed(false) }
      }) }}>Check key setup</Button>
    </div>
    {plan && <div className="space-y-3 rounded-lg border border-border-subtle p-3">
      <p>Keep {plan.keyCount} existing {plan.keyCount === 1 ? 'key' : 'keys'} and their access.</p>
      {plan.reconnectCount > 0 && <p className="text-warning">{plan.reconnectCount} {plan.reconnectCount === 1 ? 'caller needs' : 'callers need'} to sign in again.</p>}
      <label className="flex items-start gap-2"><input type="checkbox" checked={confirmed} disabled={busy} onChange={event => setConfirmed(event.target.checked)} /><span>I’ll manage these keys here instead of through installation configuration.</span></label>
      <div className="flex flex-wrap gap-2">
        <Button size="sm" disabled={busy || !confirmed || pending} onClick={() => { void run(async signal => {
          const operationId = crypto.randomUUID()
          // Retain the original ID before sending. Never automatically resend a write.
          setUncertain(operationId)
          accept(await api.startOperatorCredentialAdoption({ operationId, version: plan.version, confirmOwnership: true }, signal), operationId)
        }) }}>Confirm setup</Button>
        <Button size="sm" variant="secondary" disabled={busy} onClick={() => { setPlan(undefined); setConfirmed(false) }}>Cancel</Button>
      </div>
    </div>}
    {busy && <p role="status">Checking key setup…</p>}
    {message && <p role="alert" className="text-warning">{message}</p>}
    {uncertain && <div className="space-y-2"><p>Setup outcome is unconfirmed.</p><p className="break-all text-xs">Operation {uncertain}</p><Button size="sm" variant="secondary" disabled={busy || denied} onClick={() => { void run(async signal => { accept(await api.getOperatorCredentialAdoption(uncertain, signal), uncertain) }) }}>Check outcome</Button></div>}
    {operations.map(operation => <div key={operation.operationId} className="space-y-2 border-t border-border-subtle pt-3">
      <p role="status">{operation.status === 'succeeded' ? 'Keys are managed in Kyber.' : operation.status === 'failed' ? 'Setup stopped before changing keys.' : operation.status === 'finishing' ? 'Finishing key setup.' : 'Key setup is pending.'}</p>
      <div className="flex flex-wrap gap-2">
        <Button size="sm" variant="secondary" disabled={busy || denied} onClick={() => { void run(async signal => { accept(await api.getOperatorCredentialAdoption(operation.operationId, signal), operation.operationId) }) }}>Check status</Button>
        {operation.recoverable && <Button size="sm" disabled={busy || denied || !!uncertain} onClick={() => { void run(async signal => {
          setUncertain(operation.operationId)
          accept(await api.recoverOperatorCredentialAdoption(operation.operationId, crypto.randomUUID(), signal), operation.operationId)
        }) }}>Continue setup</Button>}
      </div>
      <details className="text-xs"><summary className="cursor-pointer">Operation details</summary><p className="mt-2 break-all">{operation.operationId}</p><p>Checked {new Date(operation.observedAt).toLocaleString()}.</p></details>
    </div>)}
  </div>
}
