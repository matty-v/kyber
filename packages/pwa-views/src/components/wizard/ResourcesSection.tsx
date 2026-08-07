import type { AvailableModel, Machine } from '../../lib/types'
import { parseCpu, parseMemoryGi, type MachineAvailability } from '../../lib/machineTypes'
import { bandFor } from './capacity'
import { inputClass, labelClass } from './styles'
import type { WizardSetter, WizardState } from './types'
import { WizardCapacityCard } from './WizardCapacityCard'

const CPU_OPTIONS = ['0.25', '0.5', '1', '2', '4', '8']
const MEMORY_OPTIONS = ['1Gi', '2Gi', '4Gi', '8Gi', '16Gi']
const DISK_OPTIONS = ['10Gi', '20Gi', '50Gi', '100Gi', '200Gi']
const CODEX_FALLBACK_MODELS: AvailableModel[] = [
  { id: 'gpt-5.6-sol', displayName: 'GPT-5.6 Sol', contextWindow: 0, contextWindowKnown: false },
  { id: 'gpt-5.6-terra', displayName: 'GPT-5.6 Terra', contextWindow: 0, contextWindowKnown: false },
  { id: 'gpt-5.6-luna', displayName: 'GPT-5.6 Luna', contextWindow: 0, contextWindowKnown: false },
]

export interface ResourcesSectionProps {
  state: WizardState
  set: WizardSetter
  models: AvailableModel[]
  /** The selected Machine, or null if no machine picked. Used by the capacity hint. */
  selectedMachine: Machine | null
  /** The selected machine's available capacity, or null when unknown. */
  machineAvailable: MachineAvailability | null
}

export function ResourcesSection({
  state,
  set,
  models,
  selectedMachine,
  machineAvailable,
}: ResourcesSectionProps) {
  const newCpu = parseCpu(state.cpu)
  const newMem = parseMemoryGi(state.memory)
  const newDisk = parseMemoryGi(state.disk)
  const cpuBand = bandFor(newCpu, machineAvailable?.cpu)
  const memBand = bandFor(newMem, machineAvailable?.memoryGi)
  // Disk band only highlights when the machine actually published an
  // ephemeral-storage budget (>0). Pre-PR-C / pending machines surface 0
  // and we don't want to show every disk size as red against a 0 budget.
  const diskBand = bandFor(newDisk, machineAvailable?.diskGi && machineAvailable.diskGi > 0 ? machineAvailable.diskGi : undefined)
  const runtimeModels = state.runtime === 'codex' && models.length === 0 ? CODEX_FALLBACK_MODELS : models

  return (
    <section className="space-y-5">
      {/* Capacity card — appears once a machine is picked. Replaces the
          1-line hint that lived here pre-#129 PR-B; renders the operator's
          proposed CPU/memory against the machine's live capacity. */}
      {state.machine && (
        <WizardCapacityCard
          selectedMachine={selectedMachine}
          machineAvailable={machineAvailable}
          newCpu={newCpu}
          newMemoryGi={newMem}
          newDiskGi={newDisk}
        />
      )}
      <div>
        <label htmlFor="agent-runtime" className={labelClass}>
          Runtime
        </label>
        <select
          id="agent-runtime"
          value={state.runtime}
          onChange={(e) => {
            const runtime = e.target.value
            set('runtime', runtime)
            set('model', runtime === 'codex' ? CODEX_FALLBACK_MODELS[0].id : (models[0]?.id ?? ''))
          }}
          className={inputClass}
        >
          <option value="claude-code">Claude Code</option>
          <option value="codex">Codex (ChatGPT)</option>
        </select>
      </div>

      <ModelPicker
        models={runtimeModels}
        value={state.model}
        onChange={(v) => set('model', v)}
      />
      <ManualModelEntry
        models={runtimeModels}
        value={state.model}
        onChange={(v) => set('model', v)}
      />

      <div>
        <label htmlFor="agent-scaling" className={labelClass}>
          Scaling
        </label>
        <select
          id="agent-scaling"
          value={state.scaling}
          onChange={(e) =>
            set('scaling', e.target.value as WizardState['scaling'])
          }
          className={inputClass}
        >
          <option value="warm">Warm (always-on)</option>
          <option value="scale-to-zero">Scale to zero</option>
        </select>
      </div>

      <div
        data-band={cpuBand}
        data-resource="cpu"
        className={cpuBand === 'red' ? 'ring-2 ring-danger rounded' : ''}
      >
        <label htmlFor="agent-cpu" className={labelClass}>
          CPU
        </label>
        <select
          id="agent-cpu"
          value={state.cpu}
          onChange={(e) => set('cpu', e.target.value)}
          className={inputClass}
        >
          {CPU_OPTIONS.map((v) => (
            <option key={v} value={v}>
              {v} vCPU
            </option>
          ))}
        </select>
      </div>

      <div
        data-band={memBand}
        data-resource="memory"
        className={memBand === 'red' ? 'ring-2 ring-danger rounded' : ''}
      >
        <label htmlFor="agent-memory" className={labelClass}>
          Memory
        </label>
        <select
          id="agent-memory"
          value={state.memory}
          onChange={(e) => set('memory', e.target.value)}
          className={inputClass}
        >
          {MEMORY_OPTIONS.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </select>
      </div>

      <div
        data-band={diskBand}
        data-resource="disk"
        className={diskBand === 'red' ? 'ring-2 ring-danger rounded' : ''}
      >
        <label htmlFor="agent-disk" className={labelClass}>
          Disk
        </label>
        <select
          id="agent-disk"
          value={state.disk}
          onChange={(e) => set('disk', e.target.value)}
          className={inputClass}
        >
          {DISK_OPTIONS.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </select>
      </div>
    </section>
  )
}

// formatContextWindow renders a model's window as `1M ctx` or `200K ctx`,
// adding `(context unknown)` when the value came from the floor default
// rather than the operator-supplied override map. The unknown indicator
// is the AC-pinned UX signal that the listed window is a safe guess, not
// an authoritative value (kyber#378 AC: "An unmapped model ID renders in
// the picker with a clear 'context unknown' indicator").
function formatContextWindow(m: AvailableModel): string {
  const k = Math.round(m.contextWindow / 1000)
  const display = k >= 1000 ? `${(k / 1000).toFixed(0)}M ctx` : `${k}K ctx`
  if (!m.contextWindowKnown) {
    return `${display} (context unknown)`
  }
  return display
}

interface ModelPickerProps {
  models: AvailableModel[]
  value: string
  onChange: (v: string) => void
}

function ModelPicker({ models, value, onChange }: ModelPickerProps) {
  const currentInList = models.some((m) => m.id === value)
  return (
    <div>
      <label htmlFor="agent-model" className={labelClass}>
        Model
      </label>
      <select
        id="agent-model"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className={inputClass}
      >
        {/* When the value isn't in the list (manual entry the operator
            typed before the picker re-rendered), keep it selectable so
            we don't silently clobber their input. */}
        {!currentInList && value && (
          <option value={value}>{value} (manual)</option>
        )}
        {models.map((m) => (
          <option key={m.id} value={m.id}>
            {m.displayName || m.id} · {formatContextWindow(m)}
          </option>
        ))}
      </select>
    </div>
  )
}

interface ManualModelEntryProps {
  models: AvailableModel[]
  value: string
  onChange: (v: string) => void
}

// ManualModelEntry is the AC-required safety valve (kyber#378 AC: "the
// operator can type a model ID the API didn't surface (manual override)
// and the agent accepts it; the entered ID round-trips through PATCH +
// restart"). Empty input is a no-op; non-empty replaces state.model on
// blur or Enter so a typo in the input doesn't fire on every keystroke.
function ManualModelEntry({ models, value, onChange }: ManualModelEntryProps) {
  const inList = models.some((m) => m.id === value)
  return (
    <div>
      <label htmlFor="agent-model-manual" className={labelClass}>
        Manual model override
      </label>
      <input
        id="agent-model-manual"
        type="text"
        placeholder="e.g. claude-opus-5-0 (not in dropdown yet)"
        defaultValue={inList ? '' : value}
        onBlur={(e) => {
          const trimmed = e.currentTarget.value.trim()
          if (trimmed && trimmed !== value) onChange(trimmed)
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            const trimmed = (e.currentTarget as HTMLInputElement).value.trim()
            if (trimmed && trimmed !== value) onChange(trimmed)
          }
        }}
        className={inputClass}
        autoComplete="off"
        spellCheck={false}
      />
      <p className="mt-1 text-[11px] text-text-disabled">
        Use only when Anthropic ships a model before the picker catches up.
        Press Enter or blur to apply. The control-plane accepts any string;
        boot will fail visibly if the model isn't installed.
      </p>
    </div>
  )
}
