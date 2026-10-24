import { useEffect, useRef, useState } from 'react'
import type { ApiClient } from '../lib/api'
import type { OperatorCredentialOperation } from '../lib/types'
import { Button } from './Button'

export function OperatorKeyPendingChanges({ api, onCompleted }: { api: ApiClient; onCompleted: () => void }) {
  const [result, setResult] = useState<{ client: ApiClient; items: OperatorCredentialOperation[]; next?: string }>()
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [revision, setRevision] = useState(0)
  useEffect(() => {
    let active = true
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), 4_000)
    void api.getPendingOperatorCredentialOperations(undefined, controller.signal).then((page) => {
      if (active) { setResult({ client: api, ...page }); setError('') }
    }).catch(() => { if (active) { setResult(undefined); setError('Could not check pending key changes.') } })
    return () => { active = false; clearTimeout(timer); controller.abort() }
  }, [api, revision])
  // Abort is not an undo: explicit recovery may already have been admitted.
  const activeRequest = useRef<AbortController | undefined>(undefined)
  const active = useRef(true)
  useEffect(() => { active.current = true; setBusy(false); setError(''); return () => { active.current = false; activeRequest.current?.abort(); activeRequest.current = undefined } }, [api])
  async function resolve(operation: OperatorCredentialOperation) {
    if (busy) return
    setBusy(true); setError('')
    const controller = new AbortController(); activeRequest.current = controller
    const timer = setTimeout(() => controller.abort(), 12_000)
    try {
      const next = await api.reconcileOperatorCredentialOperation(operation.operationId, controller.signal)
      if (controller.signal.aborted) return
      if (next.status === 'pending') setError('This change is still pending. Refresh to check again.')
      else { setRevision((n) => n + 1); onCompleted() }
    } catch { if (!controller.signal.aborted) setError('Could not resolve this change. Refresh to check its status.') }
    finally { clearTimeout(timer); if (active.current && activeRequest.current === controller) { setBusy(false); if (controller.signal.aborted) setError('The response was interrupted. Refresh to check this change.') } }
  }
  const current = result?.client === api ? result : undefined
  return <div className="space-y-2">
    {error && <p role="alert" className="text-sm text-warning">{error}</p>}
    {!!current?.items.length && <><h3 className="text-sm font-medium">Pending key changes</h3><p className="text-xs text-text-muted">Resolve confirms a completed change or stops an unfinished one.</p>
      <ul className="space-y-2">{current.items.map((item) => <li key={item.operationId} className="flex flex-wrap items-center justify-between gap-2 text-sm"><span className="min-w-0 break-all">{item.action.replace('credential:', '')} · {item.credentialId}</span><Button size="sm" disabled={busy} onClick={() => void resolve(item)}>Resolve change</Button></li>)}</ul>
      {current.next && <p className="text-xs text-text-muted">More changes are pending. Resolve these, then refresh.</p>}</>}
    <Button size="sm" variant="ghost" disabled={busy} onClick={() => setRevision((n) => n + 1)}>Refresh pending changes</Button>
  </div>
}
