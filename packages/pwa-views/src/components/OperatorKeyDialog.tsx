import { useEffect, useId, useRef, useState } from 'react'
import type { ApiClient } from '../lib/api'
import type { Agent, Machine, OperatorCredential, OperatorCredentialInventory, OperatorCredentialMutationRequest, OperatorCredentialOperation, OperatorCredentialOptions, OperatorCredentialPolicy } from '../lib/types'
import { newOperatorOperationId, prepareOperatorKey } from '../lib/operator-key'
import { Button } from './Button'
import { Input } from './ui/input'

export type OperatorKeyAction = { kind: 'create' } | { kind: 'edit' | 'rotate' | 'revoke'; credential: OperatorCredential }
interface Props {
  action: OperatorKeyAction
  inventory: OperatorCredentialInventory
  options: OperatorCredentialOptions
  agents: Agent[]
  machines: Machine[]
  targetsUnavailable: boolean
  api: ApiClient
  bearerConnection: boolean
  onClose: () => void
  onCompleted: () => void
}

const inputClass = 'w-full rounded-lg border border-border-default bg-surface-base px-3 py-2 text-sm text-text-primary'
function localDate(value?: string): string {
  if (!value) return ''
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return ''
  return new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16)
}
function errorCode(error: unknown): string { return typeof error === 'object' && error !== null && 'code' in error ? String(error.code) : '' }
function errorMessage(error: unknown): string { return error instanceof Error ? error.message : 'Could not complete the request.' }

export function OperatorKeyDialog(props: Props) {
  const credential = props.action.kind === 'create' ? undefined : props.action.credential
  const [name, setName] = useState(credential?.displayName ?? '')
  const [role, setRole] = useState<'administrator' | 'restricted'>(credential?.accessLevel ?? 'restricted')
  const [scopes, setScopes] = useState(new Set(credential?.scopes ?? []))
  const [agentTargets, setAgentTargets] = useState(new Set(credential?.agentResources.filter((target) => target !== '*') ?? []))
  const [machineTargets, setMachineTargets] = useState(new Set(credential?.machineResources?.filter((target) => target !== '*') ?? []))
  const [expires, setExpires] = useState(localDate(credential?.expiresAt))
  const [expiryEnabled, setExpiryEnabled] = useState(!!credential?.expiresAt)
  // Snapshot the reviewed version. Polling must never silently advance it.
  const [reviewedVersion] = useState(props.inventory.version ?? '')
  const [stage, setStage] = useState<'policy' | 'value' | 'confirm' | 'pending' | 'failed'>(props.action.kind === 'revoke' ? 'confirm' : props.action.kind === 'rotate' ? 'value' : 'policy')
  const [prepared, setPrepared] = useState<{ key: string; operationId: string }>()
  const [saved, setSaved] = useState(false)
  const [copied, setCopied] = useState(false)
  const [operationId, setOperationId] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [conflicted, setConflicted] = useState(false)
  const panel = useRef<HTMLDivElement>(null)
  const active = useRef(true)
  const inFlight = useRef<AbortController | undefined>(undefined)
  const heading = useId()
  const nameId = useId()
  const expiryId = useId()
  const self = !!credential && credential.id === props.inventory.currentCredentialId
  const title = props.action.kind === 'create' ? 'New access key' : props.action.kind === 'edit' ? 'Edit access key' : props.action.kind === 'revoke' ? 'Revoke access key' : 'Rotate access key'

  useEffect(() => {
    active.current = true
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : undefined
    const overflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => { active.current = false; inFlight.current?.abort(); document.body.style.overflow = overflow; if (previous?.isConnected) previous.focus() }
  }, [])
  useEffect(() => { panel.current?.querySelector<HTMLElement>('input,select,button')?.focus() }, [stage])

  const selected = props.options.permissions.filter((option) => scopes.has(option.scope))
  const needsAgents = role === 'restricted' && selected.some((option) => option.target === 'agent')
  const needsMachines = role === 'restricted' && selected.some((option) => option.target === 'machine')
  const duplicateName = props.inventory.credentials.some((item) => item.id !== credential?.id && item.name.toLowerCase() === name.trim().toLowerCase())
  const invalidExpiry = expiryEnabled && (!expires || !Number.isFinite(new Date(expires).getTime()) || new Date(expires).getTime() <= Date.now())
  const valid = !!name.trim() && !duplicateName && !invalidExpiry && (role === 'administrator' || scopes.size > 0) && (!needsAgents || agentTargets.size > 0) && (!needsMachines || machineTargets.size > 0)
  const policy: OperatorCredentialPolicy = {
    name: name.trim(), role,
    scopes: role === 'administrator' ? [] : [...scopes],
    agentResources: role === 'administrator' ? [] : [...agentTargets],
    machineResources: role === 'administrator' ? [] : [...machineTargets],
    expiresAt: expiryEnabled && !invalidExpiry ? new Date(expires).toISOString() : null,
  }

  function generate() {
    try { setPrepared(prepareOperatorKey()); setSaved(false); setCopied(false); setError('') }
    catch (failure) { setError(errorMessage(failure)) }
  }
  async function copy() {
    if (!prepared) return
    try { await navigator.clipboard.writeText(prepared.key); if (active.current) setCopied(true) }
    catch { if (active.current) setError('Clipboard access is unavailable. Select and save the value manually.') }
  }
  function finish(operation: OperatorCredentialOperation) {
    if (!active.current) return
    if (operation.status === 'success') { setPrepared(undefined); props.onCompleted(); props.onClose() }
    else if (operation.status === 'failure') { setPrepared(undefined); setStage('failed'); setError('This change was not applied. Refresh the key list before trying again.') }
    else { setStage('pending'); setError('') }
  }
  async function check(reconcile = false) {
    if (!operationId || busy) return
    setBusy(true); setError('')
    const controller = new AbortController(); inFlight.current = controller
    const timeout = setTimeout(() => controller.abort(), 12_000)
    try { finish(await (reconcile ? props.api.reconcileOperatorCredentialOperation(operationId, controller.signal) : props.api.getOperatorCredentialOperation(operationId, controller.signal))) }
    catch (failure) { if (active.current) setError(typeof failure === 'object' && failure !== null && 'status' in failure && failure.status === 404 ? 'This change has not been observed yet. Check again shortly.' : 'The outcome could not be checked. Try again.') }
    finally { clearTimeout(timeout); if (active.current) setBusy(false) }
  }
  async function submit() {
    if (busy || conflicted || !reviewedVersion) return
    if ((props.action.kind === 'create' || props.action.kind === 'edit') && !valid) return
    if ((props.action.kind === 'create' || props.action.kind === 'rotate') && (!prepared || !saved)) return
    let request: OperatorCredentialMutationRequest
    try {
      if (props.action.kind === 'create') request = { operationId: prepared!.operationId, action: 'create', key: prepared!.key, policy }
      else if (props.action.kind === 'rotate') request = { operationId: prepared!.operationId, action: 'rotate', credentialId: props.action.credential.id, key: prepared!.key }
      else if (props.action.kind === 'revoke') request = { operationId: newOperatorOperationId(), action: 'revoke', credentialId: props.action.credential.id }
      else request = { operationId: newOperatorOperationId(), action: 'update', credentialId: props.action.credential.id, policy }
    } catch (failure) { setError(errorMessage(failure)); return }
    setOperationId(request.operationId); setBusy(true); setError('')
    const controller = new AbortController(); inFlight.current = controller
    const timeout = setTimeout(() => controller.abort(), 12_000)
    try { finish(await props.api.mutateOperatorCredential(request, reviewedVersion, controller.signal)) }
    catch (failure) {
      if (!active.current) return
      const code = errorCode(failure)
      if (['credential_version_changed', 'last_administrator_required', 'invalid_credential_change', 'credential_version_required', 'forbidden'].includes(code)) {
        setError(errorMessage(failure)); setConflicted(true)
      } else { setStage('pending'); setError('The response was interrupted. Check this change before starting another.') }
    } finally { clearTimeout(timeout); if (active.current) setBusy(false) }
  }

  function targets(kind: 'agent' | 'machine') {
    const chosen = kind === 'agent' ? agentTargets : machineTargets
    const available = new Map<string, string>()
    for (const item of kind === 'agent' ? props.agents : props.machines) available.set(`${props.options.namespace}/${item.id}`, item.id)
    for (const item of chosen) if (!available.has(item)) available.set(item, `${item.split('/').at(-1)} (not present)`)
    return <fieldset className="rounded-lg border border-border-subtle p-3">
      <legend className="px-1 text-sm font-medium">{kind === 'agent' ? 'Agents' : 'Machines'}</legend>
      {props.targetsUnavailable && <p className="text-xs text-warning">Could not refresh targets. Previously selected targets are retained.</p>}
      {!available.size && <p className="text-sm text-text-muted">No {kind === 'agent' ? 'Agents' : 'Machines'} available.</p>}
      <div className="max-h-40 space-y-2 overflow-y-auto">
        {[...available].map(([value, label]) => <label key={value} className="flex items-start gap-2 text-sm"><input type="checkbox" checked={chosen.has(value)} disabled={busy || conflicted} onChange={() => {
          const next = new Set(chosen); if (next.has(value)) next.delete(value); else next.add(value)
          if (kind === 'agent') setAgentTargets(next); else setMachineTargets(next)
        }} /><span className="min-w-0 break-words">{label}</span></label>)}
      </div>
    </fieldset>
  }

  return <div className="fixed inset-0 z-50 flex items-center justify-center p-3 sm:p-4" onKeyDown={(event) => {
    if (event.key === 'Escape') { event.preventDefault(); props.onClose() }
    if (event.key === 'Tab') {
      const focusable = [...(panel.current?.querySelectorAll<HTMLElement>('button:not(:disabled),input:not(:disabled),select:not(:disabled),a[href]') ?? [])]
      const first = focusable[0], last = focusable.at(-1)
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus() }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus() }
    }
  }}>
    <div className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm" onClick={props.onClose} />
    <div ref={panel} role="dialog" aria-modal="true" aria-labelledby={heading} className="relative z-10 flex max-h-[90dvh] w-full max-w-xl flex-col overflow-y-auto rounded-xl border border-border-default bg-surface-overlay p-4 shadow-xl sm:p-6">
      <h2 id={heading} className="font-display text-base font-semibold text-text-primary">{title}</h2>
      {credential && <p className="mt-1 break-words text-sm text-text-muted">{credential.displayName}{self ? ' · You’re using this key' : ''}</p>}
      {stage === 'policy' && <form className="mt-4 space-y-4" onSubmit={(event) => { event.preventDefault(); if (!valid || busy || conflicted) return; if (props.action.kind === 'edit') void submit(); else { setStage('value'); generate() } }}>
        <div><label htmlFor={nameId} className="mb-1 block text-sm">Name</label><Input id={nameId} value={name} maxLength={256} onChange={(event) => setName(event.target.value)} disabled={busy || conflicted} autoComplete="off" />{duplicateName && <p role="alert" className="mt-1 text-xs text-status-danger">That name is already in use.</p>}</div>
        <label className="block text-sm">Access<select className={`${inputClass} mt-1`} value={role} onChange={(event) => setRole(event.target.value as typeof role)} disabled={busy || conflicted}><option value="restricted">Restricted</option><option value="administrator">Administrator</option></select></label>
        {role === 'administrator' ? <p className="text-sm text-text-muted">Full access to this Kyber installation.</p> : <fieldset><legend className="mb-2 text-sm font-medium">Allowed actions</legend><div className="max-h-56 space-y-2 overflow-y-auto rounded-lg border border-border-subtle p-3">{props.options.permissions.map((option) => <label key={option.scope} className="flex items-start gap-2 text-sm"><input type="checkbox" checked={scopes.has(option.scope)} disabled={busy || conflicted} onChange={() => setScopes((previous) => { const next = new Set(previous); if (next.has(option.scope)) next.delete(option.scope); else next.add(option.scope); return next })} /><span>{option.label}</span></label>)}</div></fieldset>}
        {role === 'restricted' && [...scopes].filter((scope) => !props.options.permissions.some((option) => option.scope === scope)).map((scope) => <label key={scope} className="flex items-start gap-2 text-sm"><input type="checkbox" checked disabled={busy || conflicted} onChange={() => setScopes((previous) => { const next = new Set(previous); next.delete(scope); return next })} /><span>{scope === 'tasks:admin' ? 'Manage all tasks (existing access)' : scope === 'tasks:platform-admin' ? 'Manage tasks across tenants (existing access)' : `Existing permission: ${scope}`}</span></label>)}
        {needsAgents && targets('agent')}{needsMachines && targets('machine')}
        <div><label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={expiryEnabled} onChange={(event) => setExpiryEnabled(event.target.checked)} disabled={busy || conflicted} />Set an expiry</label>{expiryEnabled && <div className="mt-2"><label htmlFor={expiryId} className="sr-only">Expires</label><input id={expiryId} className={inputClass} type="datetime-local" value={expires} onChange={(event) => setExpires(event.target.value)} disabled={busy || conflicted} />{invalidExpiry && <p className="mt-1 text-xs text-status-danger">Choose a future date and time.</p>}</div>}</div>
        {error && <p role="alert" className="text-sm text-status-danger">{error}</p>}
        <div className="flex justify-end gap-2"><Button type="button" variant="ghost" onClick={props.onClose}>Close</Button><Button variant="primary" type="submit" disabled={!valid || busy || conflicted} loading={busy}>{props.action.kind === 'edit' ? 'Save changes' : 'Next'}</Button></div>
      </form>}
      {stage === 'value' && <div className="mt-4 space-y-4">
        <p className="text-sm text-text-muted">Save this value before activating it. It cannot be retrieved later.</p>
        {!prepared ? <Button onClick={generate}>Generate value</Button> : <><label className="block text-sm">New key value<Input value={prepared.key} readOnly className="mt-1 font-mono text-xs" onFocus={(event) => event.target.select()} /></label><Button onClick={() => void copy()}>{copied ? 'Copied' : 'Copy value'}</Button><label className="flex items-start gap-2 text-sm"><input type="checkbox" checked={saved} onChange={(event) => setSaved(event.target.checked)} disabled={busy || conflicted} /><span>I’ve saved this key value.</span></label></>}
        {self && <p className="text-sm text-text-muted">{props.bearerConnection ? 'Update this connection with the new value after activation.' : 'This browser stays signed in. Other connections using this key need the new value.'}</p>}
        {credential?.state === 'expired' && <p className="text-sm text-warning">This key stays expired until you change its expiry.</p>}
        {error && <p role="alert" className="text-sm text-status-danger">{error}</p>}
        <div className="flex justify-end gap-2"><Button variant="ghost" onClick={props.onClose}>Close</Button><Button variant="primary" onClick={() => void submit()} disabled={!saved || !prepared || busy || conflicted} loading={busy}>Activate key</Button></div>
      </div>}
      {stage === 'confirm' && <div className="mt-4 space-y-4"><p className="text-sm">Connections using this key will lose access.{self ? ' This includes your current connection.' : ''}</p>{error && <p role="alert" className="text-sm text-warning">{error}</p>}<div className="flex justify-end gap-2"><Button onClick={props.onClose}>Cancel</Button><Button variant="danger" disabled={busy || conflicted} loading={busy} onClick={() => void submit()}>Revoke key</Button></div></div>}
      {stage === 'pending' && <div className="mt-4 space-y-3"><p role="status" className="text-sm">The key change is not confirmed yet.</p>{error && <p role="alert" className="text-sm text-warning">{error}</p>}<p className="text-xs text-text-muted">Resolve confirms a completed change or stops an unfinished one.</p><div className="flex flex-wrap justify-end gap-2"><Button variant="ghost" onClick={props.onClose}>Close</Button><Button onClick={() => void check()} disabled={busy} loading={busy}>Check status</Button><Button onClick={() => void check(true)} disabled={busy}>Resolve change</Button></div></div>}
      {stage === 'failed' && <div className="mt-4 space-y-3"><p role="alert" className="text-sm text-warning">{error}</p><div className="flex justify-end"><Button onClick={() => { props.onCompleted(); props.onClose() }}>Close</Button></div></div>}
    </div>
  </div>
}
