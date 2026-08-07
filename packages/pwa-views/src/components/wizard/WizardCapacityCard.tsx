import { parseCpu, parseMemoryGi, type MachineAvailability } from '../../lib/machineTypes'
import type { Machine } from '../../lib/types'
import { ProposedCapacityBar } from './ProposedCapacityBar'

export interface WizardCapacityCardProps {
  /** Machine the operator picked in Step 1, or null when none picked yet. */
  selectedMachine: Machine | null
  /** Live availability for that machine, or null when controller hasn't published. */
  machineAvailable: MachineAvailability | null
  /** Resource request from the wizard state — already parsed to numbers. */
  newCpu: number
  newMemoryGi: number
  newDiskGi: number
}

/**
 * WizardCapacityCard — top-of-Resources-step block that surfaces the
 * picked machine's current capacity AND how the proposed agent fits.
 * Consumes Machine.status.assignableCapacity (denominator) +
 * Machine.status.availableCapacity (free), the same fields the Machines
 * list "Available" column reads. Disk row uses the same shape as CPU/Memory
 * since #129 PR-C started populating ephemeralStorage on the wire.
 */
export function WizardCapacityCard({
  selectedMachine,
  machineAvailable,
  newCpu,
  newMemoryGi,
  newDiskGi,
}: WizardCapacityCardProps) {
  // Caller (ResourcesSection) gates rendering on state.machine being set, so
  // a missing selectedMachine here means the picked id isn't in the live
  // machines list (stale list / being deleted). Either-null surfaces the
  // same "we can't price this" signal as "capacity unknown".
  if (!selectedMachine || !machineAvailable) {
    return (
      <div className="rounded border border-border bg-surface-elevated p-3">
        <p className="text-xs text-text-muted">capacity unknown</p>
      </div>
    )
  }

  // assignableCapacity is the denominator. Fall back to total = available + 0
  // when the controller hasn't published assignable yet — same pattern the
  // Machines list uses for the "neither populated" case, but here we have
  // a live availability number so we approximate total = available.
  const asn = selectedMachine.status?.assignableCapacity
  const cpuTotal = asn?.cpu ? parseCpu(asn.cpu) : machineAvailable.cpu
  const memTotal = asn?.memory ? parseMemoryGi(asn.memory) : machineAvailable.memoryGi
  const diskTotal = asn?.ephemeralStorage ? parseMemoryGi(asn.ephemeralStorage) : machineAvailable.diskGi

  const cpuUsedByOthers = Math.max(0, cpuTotal - machineAvailable.cpu)
  const memUsedByOthers = Math.max(0, memTotal - machineAvailable.memoryGi)
  const diskUsedByOthers = Math.max(0, diskTotal - machineAvailable.diskGi)

  const cpuExceeds = newCpu > machineAvailable.cpu
  const memExceeds = newMemoryGi > machineAvailable.memoryGi
  const diskExceeds = newDiskGi > machineAvailable.diskGi
  const exceeds = cpuExceeds || memExceeds || diskExceeds

  // Verdict copy — list every resource that overflows, in the order CPU,
  // memory, disk. Builds "CPU", "CPU and memory", "CPU, memory and disk", …
  const overList: string[] = []
  if (cpuExceeds) overList.push('CPU')
  if (memExceeds) overList.push('memory')
  if (diskExceeds) overList.push('disk')
  const overflowCopy =
    overList.length <= 1
      ? overList[0] ?? ''
      : overList.slice(0, -1).join(', ') + ' and ' + overList[overList.length - 1]

  return (
    <div
      data-testid="wizard-capacity-card"
      data-exceeds={exceeds || undefined}
      className={`rounded border p-3 space-y-2.5 ${exceeds ? 'border-danger/40 bg-danger/5' : 'border-border bg-surface-elevated'}`}
    >
      <ProposedCapacityBar
        label="CPU"
        usedByOthers={cpuUsedByOthers}
        newRequest={newCpu}
        total={cpuTotal}
        unit=""
        decimals={2}
      />
      <ProposedCapacityBar
        label="Memory"
        usedByOthers={memUsedByOthers}
        newRequest={newMemoryGi}
        total={memTotal}
        unit=" GiB"
        decimals={1}
      />
      <ProposedCapacityBar
        label="Disk"
        usedByOthers={diskUsedByOthers}
        newRequest={newDiskGi}
        total={diskTotal}
        unit=" GiB"
        decimals={1}
      />
      <p
        className={`text-xs ${exceeds ? 'text-danger font-medium' : 'text-text-muted'}`}
        data-testid="capacity-verdict"
      >
        {exceeds ? (
          <>
            This agent won&apos;t fit — reduce {overflowCopy} or pick a different machine.
          </>
        ) : (
          <>
            Fits on {selectedMachine.id}. {(machineAvailable.cpu - newCpu).toFixed(2)} vCPU, {(machineAvailable.memoryGi - newMemoryGi).toFixed(1)} GiB memory and {(machineAvailable.diskGi - newDiskGi).toFixed(1)} GiB disk will remain.
          </>
        )}
      </p>
    </div>
  )
}
