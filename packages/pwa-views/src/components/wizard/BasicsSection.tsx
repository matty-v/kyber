import { formatAvailLabel } from '../../lib/capacityBars'
import { toKebabCase, toKebabCaseTyping } from '../../lib/names'
import type { Agent, Machine } from '../../lib/types'
import { inputClass, labelClass } from './styles'
import type { WizardSetter, WizardState } from './types'

export interface BasicsSectionProps {
  state: WizardState
  set: WizardSetter
  machines: Machine[]
  agents: Agent[]
}

/**
 * BasicsSection — the first section of the Create Agent wizard.
 *
 * Renders: Name (required, kebab-cased on input), Description (free text,
 * baked into identity-repo soul on scaffold), Machine picker (showing each
 * machine's available capacity via formatAvailLabel).
 *
 * Props-driven; all state is owned by the orchestrator.
 */
export function BasicsSection({ state, set, machines, agents: _agents }: BasicsSectionProps) {
  return (
    <section className="space-y-5">
      <div>
        <label htmlFor="agent-name" className={labelClass}>
          Name
        </label>
        <input
          id="agent-name"
          type="text"
          required
          value={state.name}
          onChange={(e) => set('name', toKebabCaseTyping(e.target.value))}
          onBlur={(e) => set('name', toKebabCase(e.target.value))}
          placeholder="alice"
          className={inputClass}
        />
        <p className="mt-1.5 text-xs text-text-muted">
          Auto-converted to kebab-case (lowercase + hyphens)
        </p>
      </div>

      <div>
        <label htmlFor="agent-description" className={labelClass}>
          Description
        </label>
        <textarea
          id="agent-description"
          value={state.soulDescription}
          onChange={(e) => set('soulDescription', e.target.value)}
          placeholder="What is this agent for?"
          rows={3}
          className={inputClass}
        />
        <p className="mt-1.5 text-xs text-text-muted">
          Shown in the PWA and baked into the identity-repo soul on scaffold.
        </p>
      </div>

      <div>
        <label htmlFor="agent-machine" className={labelClass}>
          Machine
        </label>
        <select
          id="agent-machine"
          required
          value={state.machine}
          onChange={(e) => set('machine', e.target.value)}
          className={inputClass}
        >
          <option value="" disabled>
            Select a machine…
          </option>
          {machines.map((m) => (
            <option key={m.id} value={m.id}>
              {formatAvailLabel(m)}
            </option>
          ))}
        </select>
      </div>
    </section>
  )
}
