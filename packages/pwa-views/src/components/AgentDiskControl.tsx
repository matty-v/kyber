import { useEffect, useState } from 'react'
import { useAgentDiskOptions, useIncreaseAgentDisk } from '../hooks/useAPI'
import type { Agent } from '../lib/types'
import { Button } from './Button'
import { Card } from './Card'

const gib = 1024 ** 3

function measuredGiB(bytes?: number): string {
  return bytes && bytes > 0 ? `${(bytes / gib).toFixed(1)} GiB` : 'Waiting for measurement'
}

export function AgentDiskControl({ agent }: { agent: Agent }) {
  const [open, setOpen] = useState(false)
  const [target, setTarget] = useState<number | ''>('')
  const options = useAgentDiskOptions(agent.id, true)
  const increase = useIncreaseAgentDisk()
  const data = options.data
  const operation = agent.status.diskExpansion ?? data?.operation
  const active = operation && operation.stage !== 'Complete' && operation.stage !== 'Blocked'
  const minimum = data?.minimumTargetGiB ?? 1
  const valid = Number.isInteger(target) && Number(target) >= minimum && Number(target) <= 65536

  useEffect(() => {
    if (open && data) {
      setTarget((current) => current === '' ? (data.suggestedTargetsGiB.find((size) => size >= minimum) ?? minimum) : current)
    }
  }, [open, data, minimum])

  async function submit() {
    if (!valid || target === '') return
    await increase.mutateAsync({ name: agent.id, sizeGiB: target })
    setOpen(false)
    setTarget('')
  }

  return <>
    <Card>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-sm font-semibold text-text-primary">Disk</h2>
          <p className="mt-1 text-xs text-text-muted">Agent files on the persistent volume</p>
        </div>
        <Button size="sm" variant="secondary" disabled={!data?.supported || Boolean(active)} onClick={() => setOpen(true)}>Increase disk</Button>
      </div>
      <dl className="mt-4 grid gap-x-8 gap-y-2 border-t border-border-subtle pt-3 text-sm sm:grid-cols-2">
        <div className="flex justify-between gap-3"><dt className="text-text-muted">Requested</dt><dd>{data?.currentTargetGiB ?? agent.resources.disk} {data ? 'GiB' : ''}</dd></div>
        <div className="flex justify-between gap-3"><dt className="text-text-muted">Provisioned</dt><dd>{data?.pvCapacityGiB ? `${data.pvCapacityGiB} GiB` : 'Checking…'}</dd></div>
        <div className="flex justify-between gap-3"><dt className="text-text-muted">Filesystem</dt><dd>{measuredGiB(operation?.filesystemTotalBytes || data?.filesystemTotalBytes)}</dd></div>
        <div className="flex justify-between gap-3"><dt className="text-text-muted">Available</dt><dd>{measuredGiB(operation?.filesystemAvailableBytes || data?.filesystemAvailableBytes)}</dd></div>
      </dl>
      {operation && <p role="status" className={`mt-3 text-xs ${operation.stage === 'Blocked' ? 'text-danger' : 'text-text-muted'}`}>
        {operation.stage === 'Complete' ? `Expanded to ${operation.targetGiB} GiB` : `${operation.stage}: ${operation.targetGiB} GiB`}{operation.reason ? ` — ${operation.reason}` : ''}
      </p>}
      {data && !data.supported && <p className="mt-3 text-xs text-text-muted">Increase unavailable: {data.reason}</p>}
      {options.isError && <p className="mt-3 text-xs text-danger">Could not check disk expansion support.</p>}
    </Card>

    {open && data && <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm" onClick={() => { if (!increase.isPending) setOpen(false) }} />
      <div role="dialog" aria-modal="true" aria-labelledby="increase-disk-title" className="relative z-10 max-h-[calc(100dvh-2rem)] w-full max-w-sm overflow-y-auto rounded-xl border border-border-subtle bg-surface-raised p-6 shadow-xl">
        <h2 id="increase-disk-title" className="text-base font-semibold text-text-primary">Increase disk</h2>
        <p className="mt-2 text-sm text-text-muted">Current disk: {data.pvCapacityGiB ?? data.currentTargetGiB} GiB. Choose a larger size.</p>
        <label className="mt-4 block text-sm text-text-secondary">New size (GiB)
          <input type="number" inputMode="numeric" min={minimum} max={65536} step={data.stepGiB} value={target}
            onChange={(event) => setTarget(event.target.value === '' ? '' : Number(event.target.value))}
            className="mt-1 w-full rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-text-primary" />
        </label>
        <div className="mt-3 flex flex-wrap gap-2">
          {data.suggestedTargetsGiB.filter((size) => size >= minimum).slice(0, 4).map((size) =>
            <button key={size} type="button" onClick={() => setTarget(size)} className="rounded-lg border border-border-default px-3 py-1 text-sm text-text-secondary hover:bg-surface-overlay">{size} GiB</button>)}
        </div>
        <p className="mt-4 text-xs text-text-muted">The existing disk and files stay in place. {agent.phase === 'Running' ? 'Kyber will restart this Agent once after expansion.' : 'This will not start the Agent.'} A disk increase cannot be undone in place.</p>
        {increase.isError && <p role="alert" className="mt-2 text-xs text-danger">Could not request disk expansion. Check the error and try again.</p>}
        <div className="mt-5 flex justify-end gap-3">
          <Button size="sm" variant="ghost" disabled={increase.isPending} onClick={() => setOpen(false)}>Cancel</Button>
          <Button size="sm" variant="primary" disabled={!valid || increase.isPending} loading={increase.isPending} onClick={() => void submit()}>Increase to {target || '…'} GiB</Button>
        </div>
      </div>
    </div>}
  </>
}
