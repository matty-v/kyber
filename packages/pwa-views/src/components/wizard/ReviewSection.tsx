import type { WizardState } from './types'

/** Step ids the user can jump back to from Review (Review itself is not editable). */
type EditableStep = 1 | 2 | 3 | 4

export interface ReviewSectionProps {
  state: WizardState
  /**
   * Optional edit-jump callback. When present, each editable group's first
   * row renders an "Edit" link calling onEdit(stepId). When undefined, no
   * Edit links render — preserves backward-compat for any non-wizard consumer.
   */
  onEdit?: (stepId: EditableStep) => void
}

function fmtIdentity(state: WizardState): string {
  switch (state.identityRepoMode) {
    case 'template':
      return state.name ? `${state.name}-agent (new from template)` : 'new from template'
    case 'existing':
      return state.identityRepoExisting || '(none)'
    case 'none':
      return 'none'
  }
}

function fmtChannels(state: WizardState): string {
  const ch: string[] = []
  if (state.telegramEnabled) ch.push('Telegram')
  return ch.length ? ch.join(', ') : 'none'
}

interface ReviewRow {
  label: string
  value: string
  /** When set, this row is the first of its editable group and renders an Edit link. */
  editStep?: EditableStep
}

export function ReviewSection({ state, onEdit }: ReviewSectionProps) {
  const rows: ReviewRow[] = [
    { label: 'Name', value: state.name || '(unset)', editStep: 1 },
    { label: 'Machine', value: state.machine || '(unset)' },
    { label: 'Runtime', value: state.runtime, editStep: 2 },
    { label: 'Model', value: state.model || '(unset)' },
    { label: 'Scaling', value: state.scaling },
    { label: 'Resources', value: `${state.cpu} CPU / ${state.memory} / ${state.disk}` },
    { label: 'Identity', value: fmtIdentity(state), editStep: 3 },
    { label: 'Auth', value: state.authType === 'oauth' ? 'OAuth' : 'API key', editStep: 4 },
    { label: 'Channels', value: fmtChannels(state) },
  ]

  return (
    <section>
      <h3 className="text-sm font-medium text-text-muted mb-3">Review</h3>
      <dl className="grid grid-cols-[120px_1fr_auto] gap-x-3 gap-y-2 text-sm">
        {rows.map(({ label, value, editStep }) => (
          <div key={label} className="contents">
            <dt className="text-text-muted">{label}</dt>
            <dd className="text-text-primary">{value}</dd>
            <dd>
              {onEdit && editStep ? (
                <button
                  type="button"
                  onClick={() => onEdit(editStep)}
                  className="text-xs text-accent hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring rounded"
                >
                  Edit
                </button>
              ) : null}
            </dd>
          </div>
        ))}
      </dl>
    </section>
  )
}
