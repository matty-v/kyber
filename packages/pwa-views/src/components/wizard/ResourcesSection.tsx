import type { Machine } from '../../lib/types'
import { parseCpu, parseMemoryGi, type MachineAvailability } from '../../lib/machineTypes'
import { bandFor } from './capacity'
import { inputClass, labelClass } from './styles'
import type { WizardSetter, WizardState } from './types'
import { WizardCapacityCard } from './WizardCapacityCard'

const CPU_OPTIONS = ['0.25', '0.5', '1', '2', '4', '8']
const MEMORY_OPTIONS = ['1Gi', '2Gi', '4Gi', '8Gi', '16Gi']
const DISK_OPTIONS = ['10Gi', '20Gi', '50Gi', '100Gi', '200Gi']
export interface ResourcesSectionProps {
  state: WizardState
  set: WizardSetter
  /** The selected Machine, or null if no machine picked. Used by the capacity hint. */
  selectedMachine: Machine | null
  /** The selected machine's available capacity, or null when unknown. */
  machineAvailable: MachineAvailability | null
}

export function ResourcesSection({
  state,
  set,
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
          onChange={(e) => set('runtime', e.target.value)}
          className={inputClass}
        >
          <option value="claude-code">Claude Code</option>
          <option value="codex">Codex (ChatGPT)</option>
        </select>
      </div>

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
