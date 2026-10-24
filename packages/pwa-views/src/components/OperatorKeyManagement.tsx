import { useEffect, useState, type ReactNode } from 'react'
import type { ApiClient } from '../lib/api'
import type { Agent, Machine, OperatorCredential, OperatorCredentialInventory, OperatorCredentialOptions } from '../lib/types'
import { OperatorKeyPendingChanges } from './OperatorKeyPendingChanges'
import { Button } from './Button'
import { OperatorKeyDialog, type OperatorKeyAction } from './OperatorKeyDialog'

// Draft rendering is bound to the opening connection and authenticated key.
// Drafts and prepared key values never enter a shared cache or browser storage.
export function OperatorKeyManagement({ api, inventory, bearerConnection, onCompleted, children }: {
  api: ApiClient; inventory: OperatorCredentialInventory; bearerConnection: boolean; onCompleted: () => void; children: (actions: (credential: OperatorCredential) => ReactNode) => ReactNode
}) {
  const [choices, setChoices] = useState<{ client: ApiClient; options: OperatorCredentialOptions; agents: Agent[]; machines: Machine[]; targetsUnavailable: boolean }>()
  const [action, setAction] = useState<{ client: ApiClient; value: OperatorKeyAction; reviewed: OperatorCredentialInventory }>()
  const [error, setError] = useState('')
  const [retry, setRetry] = useState(0)
  useEffect(() => {
    if (!inventory.managementAvailable || !inventory.version) return
    let active = true
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), 4_000)
    void Promise.allSettled([api.getOperatorCredentialOptions(controller.signal), api.listAgents(controller.signal), api.listMachines(controller.signal)]).then(([options, agents, machines]) => {
      if (!active) return
      setError('')
      if (options.status === 'rejected' || options.value.version !== 1) { setError('Could not load key management choices.'); return }
      setChoices({ client: api, options: options.value, agents: agents.status === 'fulfilled' ? agents.value : [], machines: machines.status === 'fulfilled' ? machines.value : [], targetsUnavailable: agents.status === 'rejected' || machines.status === 'rejected' })
    })
    return () => { active = false; clearTimeout(timer); controller.abort() }
  }, [api, inventory.managementAvailable, !!inventory.version, retry])
  const currentChoices = choices?.client === api ? choices : undefined
  return <div className="mt-4 space-y-3">
    {error && <div><p role="alert" className="text-sm text-warning">{error}</p><Button size="sm" onClick={() => setRetry((n) => n + 1)}>Retry key choices</Button></div>}
    {inventory.managementAvailable && <Button size="sm" disabled={!currentChoices} onClick={() => setAction({ client: api, reviewed: inventory, value: { kind: 'create' } })}>New access key</Button>}
    {children((key) => inventory.managementAvailable && (key.source === 'managed' || key.source === 'installation') && key.state !== 'revoked' ? <div className="mt-3 flex flex-wrap gap-2">{(['edit', 'rotate', 'revoke'] as const).map((kind) => <Button key={kind} size="sm" variant={kind === 'revoke' ? 'danger' : 'secondary'} disabled={!currentChoices} onClick={() => setAction({ client: api, reviewed: inventory, value: { kind, credential: key } })}>{kind === 'edit' ? 'Edit' : kind === 'rotate' ? 'Rotate' : 'Revoke'}</Button>)}</div> : null)}
    {inventory.managementAvailable && <OperatorKeyPendingChanges api={api} onCompleted={onCompleted} />}
    {action?.client === api && currentChoices && <OperatorKeyDialog {...currentChoices} action={action.value} inventory={action.reviewed} api={api} bearerConnection={bearerConnection} onClose={() => setAction(undefined)} onCompleted={onCompleted} />}
  </div>
}
