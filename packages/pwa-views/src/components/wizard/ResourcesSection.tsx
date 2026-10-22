import { legacyRuntimeContracts } from '../../lib/runtime-contract'
import type { AgentResourceOptions, Machine } from '../../lib/types'
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
  resourceOptions?: AgentResourceOptions | null
}

export function ResourcesSection({
  state,
  set,
  selectedMachine,
  machineAvailable,
  resourceOptions,
}: ResourcesSectionProps) {
  const cpuOptions: { value: string; reason?: string }[] = resourceOptions?.cpu ?? CPU_OPTIONS.map(value => ({ value }))
  const memoryOptions: { value: string; reason?: string }[] = resourceOptions?.memory ?? MEMORY_OPTIONS.map(value => ({ value }))
  const selectedCpu = cpuOptions.find(option => parseCpu(option.value) === parseCpu(state.cpu))?.value ?? state.cpu
  const selectedMemory = memoryOptions.find(option => parseMemoryGi(option.value) === parseMemoryGi(state.memory))?.value ?? state.memory
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
          onChange={(e) => {
            set('runtime', e.target.value)
            // The endpoint panel is gated on the new runtime's features, so it
            // vanishes on a switch. Leaving the flag set would submit an
            // `inference` the API rejects, with no visible field to correct.
            set('inferenceEnabled', false)
          }}
          className={inputClass}
        >
          {(state.runtimes ?? legacyRuntimeContracts).map(runtime => <option key={runtime.id} value={runtime.id}>{runtime.name}</option>)}
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
          value={selectedCpu}
          onChange={(e) => set('cpu', e.target.value)}
          className={inputClass}
        >
          {cpuOptions.map(({ value, reason }) => (
            <option key={value} value={value} disabled={Boolean(reason)}>
              {parseCpu(value)} vCPU{reason ? ` — ${reason}` : ''}
            </option>
          ))}
          {!cpuOptions.some(option => option.value === selectedCpu) && <option value={selectedCpu}>{parseCpu(selectedCpu)} vCPU</option>}
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
          value={selectedMemory}
          onChange={(e) => set('memory', e.target.value)}
          className={inputClass}
        >
          {memoryOptions.map(({ value, reason }) => (
            <option key={value} value={value} disabled={Boolean(reason)}>
              {value}{reason ? ` — ${reason}` : ''}
            </option>
          ))}
          {!memoryOptions.some(option => option.value === selectedMemory) && <option value={selectedMemory}>{selectedMemory}</option>}
        </select>
      </div>
      {resourceOptions && <details className="text-sm text-text-secondary">
        <summary className="cursor-pointer">Custom CPU or memory</summary>
        <div className="mt-2 grid grid-cols-1 gap-3 sm:grid-cols-2">
          <label>CPU (cores)
            <input type="number" min="0.1" step="0.05" value={Number.isFinite(newCpu) ? newCpu : ''}
              onChange={event => set('cpu', `${Math.round(Number(event.target.value) * 1000)}m`)} className={inputClass} />
          </label>
          <label>Memory (GiB)
            <input type="number" min="0.125" step="0.125" value={Number.isFinite(newMem) ? newMem : ''}
              onChange={event => set('memory', `${Math.round(Number(event.target.value) * 1024)}Mi`)} className={inputClass} />
          </label>
        </div>
      </details>}

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
