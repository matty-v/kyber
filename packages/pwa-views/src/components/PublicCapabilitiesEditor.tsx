import { useEffect, useMemo, useState } from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { useAgentSkills, useSetPublicCapabilities } from '../hooks/useAPI'
import type { Agent, PublicCapability, PublicCapabilitiesManifest } from '../lib/types'
import { Button } from './Button'
import { Card } from './Card'

const modes = ['text/plain', 'text/markdown', 'application/json', 'application/octet-stream', 'image/png', 'image/jpeg', 'image/webp', 'audio/mpeg', 'audio/wav', 'audio/ogg']
const features = ['durable', 'progress', 'typed-results', 'files', 'cancellation', 'multi-turn', 'authorization-request', 'event-replay']
const emptyManifest = (): PublicCapabilitiesManifest => ({ schemaVersion: 'v1alpha1', identity: { displayName: '', description: '' }, capabilities: [] })
const emptyCapability = (): PublicCapability => ({ id: '', version: '1.0', name: '', description: '', inputModes: ['text/plain'], outputModes: ['text/markdown'], taskFeatures: [] })

function csv(value?: string[]) { return value?.join(', ') ?? '' }
function parseCSV(value: string) { return [...new Set(value.split(',').map((item) => item.trim()).filter(Boolean))] }

export function PublicCapabilitiesEditor({ agent }: { agent: Agent }) {
  const [draft, setDraft] = useState<PublicCapabilitiesManifest>(agent.publicCapabilities ?? emptyManifest())
  const mutation = useSetPublicCapabilities()
  const skills = useAgentSkills(agent.id, true)
  useEffect(() => setDraft(agent.publicCapabilities ?? emptyManifest()), [agent.publicCapabilities])

  const statusByID = useMemo(() => new Map((agent.publicCapabilitiesStatus?.capabilities ?? []).map((item) => [item.id, item])), [agent.publicCapabilitiesStatus])
  const preview = useMemo(() => ({
    schemaVersion: draft.schemaVersion,
    identity: draft.identity,
    capabilities: draft.capabilities.map(({ evidence: _privateEvidence, ...publicFields }) => ({ ...publicFields, availability: statusByID.get(publicFields.id)?.availability ?? 'pending' })),
  }), [draft, statusByID])
  const error = validateDraft(draft)

  function updateCapability(index: number, patch: Partial<PublicCapability>) {
    setDraft((current) => ({ ...current, capabilities: current.capabilities.map((item, i) => i === index ? { ...item, ...patch } : item) }))
  }

  return <Card>
    <div className="flex items-start justify-between gap-3">
      <div>
        <h2 className="text-sm font-medium text-text-primary">Public capabilities</h2>
        <p className="mt-1 text-xs text-text-muted">Only fields in the preview become a public promise. Skills are private evidence and are never auto-published.</p>
      </div>
      {agent.publicCapabilities && <Button size="sm" variant="danger" loading={mutation.isPending} onClick={() => mutation.mutate({ name: agent.id, publicCapabilities: null })}>Unpublish</Button>}
    </div>

    <div className="mt-4 grid gap-3 sm:grid-cols-2">
      <Field label="Display name" value={draft.identity.displayName} onChange={(value) => setDraft({ ...draft, identity: { ...draft.identity, displayName: value } })} />
      <Field label="Documentation URL (HTTPS)" value={draft.identity.documentationUrl ?? ''} onChange={(value) => setDraft({ ...draft, identity: { ...draft.identity, documentationUrl: value || undefined } })} />
      <label className="sm:col-span-2 text-xs text-text-muted">Description<textarea rows={2} maxLength={1024} value={draft.identity.description} onChange={(event) => setDraft({ ...draft, identity: { ...draft.identity, description: event.target.value } })} className="mt-1 w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm text-text-primary" /></label>
    </div>

    <div className="mt-4 space-y-3">
      {draft.capabilities.map((capability, index) => {
        const state = statusByID.get(capability.id)
        return <div key={`${capability.id}-${index}`} className="rounded-lg border border-border-subtle bg-surface p-3">
          <div className="flex items-center justify-between gap-2">
            <span className="text-xs font-medium text-text-primary">Capability {index + 1}{state ? ` · ${state.availability}` : ''}</span>
            <Button size="sm" variant="ghost" aria-label="Remove capability" onClick={() => setDraft({ ...draft, capabilities: draft.capabilities.filter((_, i) => i !== index) })}><Trash2 size={14} /></Button>
          </div>
          {state?.reason && <p className="mb-2 text-xs text-warning">Drift: {state.reason}</p>}
          <div className="grid gap-2 sm:grid-cols-2">
            <Field label="Stable ID" value={capability.id} onChange={(value) => updateCapability(index, { id: value })} />
            <Field label="Contract version" value={capability.version} onChange={(value) => updateCapability(index, { version: value })} />
            <Field label="Name" value={capability.name} onChange={(value) => updateCapability(index, { name: value })} />
            <Field label="Description" value={capability.description} onChange={(value) => updateCapability(index, { description: value })} />
            <Field label="Input MIME modes" value={csv(capability.inputModes)} list="capability-modes" onChange={(value) => updateCapability(index, { inputModes: parseCSV(value) })} />
            <Field label="Output MIME modes" value={csv(capability.outputModes)} list="capability-modes" onChange={(value) => updateCapability(index, { outputModes: parseCSV(value) })} />
            <Field label="Task features" value={csv(capability.taskFeatures)} list="capability-features" onChange={(value) => updateCapability(index, { taskFeatures: parseCSV(value) })} />
            <Field label="Required skills (private)" value={csv(capability.evidence?.requiredSkills)} list="observed-skills" onChange={(value) => updateCapability(index, { evidence: { ...capability.evidence, requiredSkills: parseCSV(value) } })} />
            <Field label="Runtime adapters (private)" value={csv(capability.evidence?.runtimeAdapters)} onChange={(value) => updateCapability(index, { evidence: { ...capability.evidence, runtimeAdapters: parseCSV(value) } })} />
            <Field label="Required connectors (private)" value={csv(capability.evidence?.requiredConnectors)} onChange={(value) => updateCapability(index, { evidence: { ...capability.evidence, requiredConnectors: parseCSV(value) } })} />
            <Field label="Required platform features (private)" value={csv(capability.evidence?.requiredPlatformFeatures)} list="capability-features" onChange={(value) => updateCapability(index, { evidence: { ...capability.evidence, requiredPlatformFeatures: parseCSV(value) } })} />
          </div>
        </div>
      })}
      <Button size="sm" onClick={() => setDraft({ ...draft, capabilities: [...draft.capabilities, emptyCapability()] })}><Plus size={14} /> Add capability</Button>
    </div>

    <datalist id="capability-modes">{modes.map((item) => <option key={item} value={item} />)}</datalist>
    <datalist id="capability-features">{features.map((item) => <option key={item} value={item} />)}</datalist>
    <datalist id="observed-skills">{(skills.data?.skills ?? []).map((item) => <option key={item.name} value={item.name} />)}</datalist>

    <div className="mt-4 rounded-lg border border-border-subtle bg-surface-overlay p-3">
      <div className="mb-2 text-xs font-medium text-text-primary">Exact public preview</div>
      <pre className="max-h-64 overflow-auto whitespace-pre-wrap text-[11px] text-text-muted">{JSON.stringify(preview, null, 2)}</pre>
    </div>
    <div className="mt-3 flex items-center justify-between gap-3">
      <span className={`text-xs ${error ? 'text-danger' : 'text-text-muted'}`}>{error ?? 'Publication requires this explicit save.'}</span>
      <Button variant="primary" size="sm" loading={mutation.isPending} disabled={Boolean(error) || mutation.isPending} onClick={() => mutation.mutate({ name: agent.id, publicCapabilities: draft })}>{agent.publicCapabilities ? 'Update publication' : 'Publish manifest'}</Button>
    </div>
  </Card>
}

function Field({ label, value, onChange, list }: { label: string; value: string; onChange: (value: string) => void; list?: string }) {
  return <label className="text-xs text-text-muted">{label}<input value={value} list={list} onChange={(event) => onChange(event.target.value)} className="mt-1 w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm text-text-primary" /></label>
}

function validateDraft(draft: PublicCapabilitiesManifest): string | null {
  if (!draft.identity.displayName.trim() || !draft.identity.description.trim()) return 'Display name and description are required.'
  const ids = new Set<string>()
  for (const capability of draft.capabilities) {
    if (!/^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$/.test(capability.id)) return `Capability ID “${capability.id}” must be a lowercase slug.`
    if (ids.has(capability.id)) return `Capability ID “${capability.id}” is duplicated.`
    ids.add(capability.id)
    if (!capability.version || !capability.name.trim() || !capability.description.trim() || !capability.inputModes.length || !capability.outputModes.length) return `Capability “${capability.id}” has incomplete required fields.`
  }
  return null
}
