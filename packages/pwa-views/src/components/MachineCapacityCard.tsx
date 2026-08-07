import { Card } from './Card'
import { parseCpu, parseMemoryGi } from '../lib/machineTypes'
import { capacityBand, pctUsed, BAND_BG_CLASS } from '../lib/capacityBars'
import type { Machine } from '../lib/types'

export interface MachineCapacityCardProps {
  machine: Machine
}

/**
 * MachineCapacityCard — operator-facing view of how much room is left for
 * new agents on this machine. Renders one bar per resource (CPU, Memory,
 * and Disk when populated) showing `<used> / <total> free`, where:
 *   total = status.assignableCapacity[resource]
 *   free  = status.availableCapacity[resource]
 *   used  = total - free
 *
 * Internal data-model concepts (Observed / Reservation / Assignable) are
 * intentionally not surfaced — the operator only cares about how much room
 * remains for new agents. Per-agent CPU/memory/disk breakdown that the old
 * expandable disclosure surfaced is no longer rendered here; if needed,
 * tap into an individual agent from the Hosted Agents list below.
 *
 * When the controller hasn't yet reported capacity for this machine
 * (assignableCapacity.cpu missing), shows a friendly placeholder instead
 * of three zero-filled bars.
 */
export function MachineCapacityCard({ machine }: MachineCapacityCardProps) {
  const asn = machine.status?.assignableCapacity
  const avl = machine.status?.availableCapacity

  // The bare-minimum signal that the controller has observed the node and
  // populated capacity. If this is missing, the rest will render as zero
  // bars — show a friendly placeholder instead.
  if (!asn?.cpu) {
    return (
      <Card>
        <h2 className="text-sm font-medium text-text-muted mb-3">Capacity available for new agents</h2>
        <p className="text-sm text-text-secondary">
          Capacity not yet reported — the controller is still bringing this machine online.
        </p>
      </Card>
    )
  }

  const cpuTotal = parseCpu(asn.cpu ?? '0')
  const cpuFree = parseCpu(avl?.cpu ?? '0')
  const cpuUsed = Math.max(0, cpuTotal - cpuFree)

  const memTotal = parseMemoryGi(asn.memory ?? '0')
  const memFree = parseMemoryGi(avl?.memory ?? '0')
  const memUsed = Math.max(0, memTotal - memFree)

  // Disk: omit on machines where ephemeralStorage isn't populated on both
  // assignable + available (pre-#129 PR-C clusters or older controllers).
  // Mirrors the gating in MachineList's MachineAvailableCell.
  const hasDisk = !!asn.ephemeralStorage && !!avl?.ephemeralStorage
  const diskTotal = hasDisk ? parseMemoryGi(asn.ephemeralStorage!) : 0
  const diskFree = hasDisk ? parseMemoryGi(avl!.ephemeralStorage!) : 0
  const diskUsed = hasDisk ? Math.max(0, diskTotal - diskFree) : 0

  return (
    <Card>
      <h2 className="text-sm font-medium text-text-muted mb-3">Capacity available for new agents</h2>
      <div className="flex flex-col gap-3">
        <ResourceRow label="CPU" used={cpuUsed} free={cpuFree} total={cpuTotal} unit="" decimals={2} />
        <ResourceRow label="Memory" used={memUsed} free={memFree} total={memTotal} unit=" GiB" decimals={1} />
        {hasDisk && (
          <ResourceRow label="Disk" used={diskUsed} free={diskFree} total={diskTotal} unit=" GiB" decimals={1} />
        )}
      </div>
    </Card>
  )
}

interface ResourceRowProps {
  label: string
  used: number
  free: number
  total: number
  unit: string
  decimals: number
}

function ResourceRow({ label, used, free, total, unit, decimals }: ResourceRowProps) {
  // Render label + free/total numbers inline (one row), then the bar below.
  // We don't reuse <CapacityBar> here because that component bakes its own
  // label + percent row, which would double up with our free/total row.
  // Band thresholds + colour mapping come from capacityBars so behaviour
  // stays aligned with MachineList's per-row bars.
  const band = capacityBand(used, total)
  const pct = pctUsed(used, total)
  return (
    <div className="flex flex-col gap-1">
      <div className="flex justify-between text-xs text-text-secondary">
        <span>{label}</span>
        <span aria-label={`${label}: ${free.toFixed(decimals)}${unit} free of ${total.toFixed(decimals)}${unit}`}>
          {free.toFixed(decimals)} / {total.toFixed(decimals)}
          {unit} free
        </span>
      </div>
      <div className="h-2 w-full overflow-hidden rounded-full bg-surface-overlay">
        <div
          data-band={band}
          className={`h-full ${BAND_BG_CLASS[band]}`}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  )
}
