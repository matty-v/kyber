import type { AgentImportApply, ArchiveSummary } from '../../lib/types'
import type { WizardSetter, WizardState } from './types'

// prefillFromArchive copies the archive's recorded configuration into the
// wizard so the operator sees, and can change, what the new agent will be
// created with. The server applies the same fields to anything the request
// leaves empty, so this only makes the defaults visible.
export function prefillFromArchive(summary: ArchiveSummary, state: WizardState, set: WizardSetter, runtime: string) {
  const c = summary.source.config
  if (!c) return
  if (c.model && (c.runtime ?? summary.source.runtime) === runtime) set('model', c.model)
  if (c.startupPrompt) set('startupPrompt', c.startupPrompt)
  if (c.resources?.cpu) set('cpu', c.resources.cpu)
  if (c.resources?.memory) set('memory', c.resources.memory)
  if (c.soulDescription) set('soulDescription', c.soulDescription)
  if (c.identityRepo && (state.identityRepoModes ?? []).includes('existing')) {
    set('identityRepoMode', 'existing')
    set('identityRepoExisting', c.identityRepo)
  }
}

function Choice<V extends string>({ label, value, options, onChange, disabled }: {
  label: string
  value: V
  options: [V, string][]
  onChange: (v: V) => void
  disabled?: boolean
}) {
  return (
    <select
      aria-label={label}
      className="rounded-md border border-border-default bg-surface-base px-2 py-1 text-xs text-text-primary"
      value={value}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value as V)}
    >
      {options.map(([v, text]) => <option key={v} value={v}>{text}</option>)}
    </select>
  )
}

// ArchiveApplySection is the wizard's "From the archive" block (MAT-90): what
// the import carries beyond the disk, and in what state. Jobs arrive paused
// and bindings disabled by default so nothing fires on both agents before
// cutover.
export function ArchiveApplySection({ summary, state, set }: { summary: ArchiveSummary; state: WizardState; set: WizardSetter }) {
  const c = summary.source.config
  const apply = state.archiveApply
  const update = (patch: AgentImportApply) => set('archiveApply', { ...apply, ...patch })
  const off = !apply.config
  const jobs = c?.jobs ?? []
  const bindings = c?.inboundBindings ?? []
  const secrets = c?.userSecrets ?? []
  const login = summary.login ?? []

  return (
    <div className="space-y-2 border-t border-border-subtle pt-2" data-testid="archive-apply">
      <p className="text-sm font-medium text-text-primary">From the archive</p>
      {c ? (
        <label className="flex items-start gap-2 text-text-primary">
          <input type="checkbox" className="mt-0.5" checked={apply.config} onChange={(e) => update({ config: e.target.checked })} />
          <span>
            Apply {summary.source.agent}'s configuration
            <span className="block text-text-muted">
              Profile, public capabilities, A2A peers, jobs, webhook bindings and user secrets, as chosen below.
              Settings in the earlier steps were prefilled from it; your edits there win.
            </span>
          </span>
        </label>
      ) : (
        <p>This archive records no configuration; only the disk is restored.</p>
      )}
      {c && (c.version ?? 0) < 2 && (
        <p className="text-warn">
          This archive predates full configuration capture: its profile, capabilities, peers, binding definitions and user secret list were not recorded.
        </p>
      )}
      {jobs.length > 0 && (
        <div className="flex flex-wrap items-center justify-between gap-2" data-testid="archive-apply-jobs">
          <span>
            {jobs.length} scheduled job{jobs.length === 1 ? '' : 's'}: {jobs.map((j) => j.name).join(', ')}
          </span>
          <Choice label="Scheduled jobs" value={apply.jobs} disabled={off} onChange={(v) => update({ jobs: v })}
            options={[['paused', 'Restore paused'], ['skip', 'Leave out']]} />
        </div>
      )}
      {bindings.length > 0 && (
        <div className="flex flex-wrap items-center justify-between gap-2" data-testid="archive-apply-bindings">
          <span>
            {bindings.length} webhook binding{bindings.length === 1 ? '' : 's'}: {bindings.join(', ')}
            {apply.bindings === 'disabled' && !off && <span className="block text-text-muted">Restored disabled, each with a new signing secret.</span>}
          </span>
          <Choice label="Webhook bindings" value={apply.bindings} disabled={off} onChange={(v) => update({ bindings: v })}
            options={[['disabled', 'Restore disabled'], ['skip', 'Leave out']]} />
        </div>
      )}
      {secrets.length > 0 && (
        <div className="flex flex-wrap items-center justify-between gap-2" data-testid="archive-apply-secrets">
          <span>
            {secrets.length} user secret{secrets.length === 1 ? '' : 's'}: {secrets.map((s) => `${s.key} (${s.kind})`).join(', ')}
            <span className="block text-text-muted">Values are copied inside the cluster from an export whose agent ({summary.source.agent}) is still on this installation; from an upload, re-enter them.</span>
          </span>
          <Choice label="User secrets" value={apply.secrets} disabled={off} onChange={(v) => update({ secrets: v })}
            options={[['copy-if-local', 'Copy values'], ['skip', 'Leave out']]} />
        </div>
      )}
      {login.length > 0 && (
        <p data-testid="archive-apply-login">
          Harness login files are never restored ({login.join(', ')}). The new agent signs in with its own credential.
        </p>
      )}
    </div>
  )
}
