import { useCluster } from '../lib/cluster-context'
import { useEffect, useId, useState } from 'react'
import type { Agent, ReasoningEffortOptions } from '../lib/types'
import { useEffortOptions, useSetAgentEffort } from '../hooks/useReasoningEffort'

export function EffortSelect({ value, onChange, options, loading, unavailable, disabled }: {
  value: string; onChange: (value: string) => void; options?: ReasoningEffortOptions
  loading?: boolean; unavailable?: boolean; disabled?: boolean
}) {
  const id = useId()
  const levels = options?.state === 'supported' ? options.levels : []
  return <div className="min-w-0 space-y-2">
    <label htmlFor={id} className="block text-sm font-medium text-text-primary">Reasoning effort</label>
    <select id={id} value={value} onChange={e => onChange(e.target.value)} disabled={disabled}
      className="min-h-11 w-full min-w-0 rounded-lg border border-border-default bg-surface-overlay px-3 py-2 text-sm text-text-primary">
      <option value="">Default (no override)</option>
      {value && !levels.includes(value) && <option value={value} disabled>{value} (not available)</option>}
      {levels.map(level => <option key={level} value={level}>{level}</option>)}
    </select>
    {loading ? <p className="text-xs text-text-muted">Checking support…</p>
      : unavailable ? <p className="text-xs text-text-muted">Support unavailable. Default remains available.</p>
        : options?.state !== 'supported' && <p className="text-xs text-text-muted">{options?.reason || 'Only Default is available for this combination.'}</p>}
  </div>
}

export function CreateEffortSelect({ runtime, model, customEndpoint, value, onChange }: {
  runtime: string; model: string; customEndpoint: boolean; value: string; onChange: (value: string) => void
}) {
  const query = useEffortOptions({ runtime, model, customEndpoint })
  return <EffortSelect value={value} onChange={onChange} options={query.isError ? undefined : query.data} loading={query.isLoading} unavailable={query.isError} />
}

export function AgentEffortSettings({ agent }: { agent: Agent }) {
  const cluster = useCluster()
  const [editing, setEditing] = useState(false)
  const [value, setValue] = useState(agent.effort ?? '')
  const query = useEffortOptions({ agent: agent.id, runtime: agent.runtime, model: agent.model, customEndpoint: Boolean(agent.inference) })
  const mutation = useSetAgentEffort()
  useEffect(() => { setEditing(false); setValue(agent.effort ?? '') }, [cluster.id, cluster.baseURL, cluster.apiKey, agent.id])
  const valid = value === '' || (!query.isError && query.data?.state === 'supported' && query.data.levels.includes(value))
  return <section aria-label="Reasoning effort" className="rounded-lg border border-border-subtle bg-surface-raised p-4 space-y-3">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div><h2 className="text-sm font-medium text-text-primary">Reasoning effort</h2>
        <p className="text-sm text-text-secondary">{agent.effort || 'Default (no override)'}{agent.effortPending ? (['Stopped', 'Failed', 'NeedsAuth'].includes(agent.phase) ? ' · Applies on next start' : ' · Pending restart') : ''}</p></div>
      {!editing && <button type="button" className="min-h-11 rounded-lg border border-border-default px-3 py-2 text-sm text-text-primary disabled:opacity-50"
        disabled={query.isError && !agent.effort} onClick={() => { setValue(agent.effort ?? ''); mutation.reset(); setEditing(true) }}>Change effort</button>}
    </div>
    {agent.effortProblem && <p role="alert" className="text-sm text-danger">{agent.effortProblem}</p>}
    {editing && <form className="space-y-3" onSubmit={e => { e.preventDefault(); if (valid) mutation.mutate({ name: agent.id, effort: value }, { onSuccess: () => setEditing(false) }) }}>
      <EffortSelect value={value} onChange={setValue} options={query.isError ? undefined : query.data} loading={query.isLoading} unavailable={query.isError} disabled={mutation.isPending} />
      <p className="text-xs text-text-muted">Applying restarts a running agent.</p>
      {mutation.error && <p role="alert" className="text-sm text-danger">{mutation.error instanceof Error ? mutation.error.message : 'Could not change effort.'}</p>}
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="min-h-11 px-3 py-2 text-sm text-text-secondary" disabled={mutation.isPending} onClick={() => setEditing(false)}>Cancel</button>
        <button type="submit" className="min-h-11 rounded-lg bg-accent px-3 py-2 text-sm text-white disabled:opacity-50" disabled={!valid || mutation.isPending || value === (agent.effort ?? '')}>{mutation.isPending ? 'Applying…' : 'Apply'}</button>
      </div>
    </form>}
    {!editing && query.isError && <p className="text-xs text-text-muted">Effort settings are unavailable on this cluster.</p>}
  </section>
}
