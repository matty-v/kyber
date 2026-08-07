import type { Machine } from './types'
import { parseCpu, parseMemoryGi } from './machineTypes'

/**
 * Three-band color classification used by capacity bars and resource sliders.
 * Thresholds match #142's spec: green < 70%, yellow 70-90%, red >= 90%.
 */
export type CapacityBand = 'green' | 'yellow' | 'red'

/**
 * Returns the integer percentage of `used / total`, floored. Returns 0 when
 * total is 0 (avoid divide-by-zero). Caps at 100 when used > total — this is
 * the right behavior for a UI bar (filled) but callers needing the raw
 * over-allocation ratio should compute it themselves.
 */
export function pctUsed(used: number, total: number): number {
  if (total <= 0) return 0
  const pct = Math.floor((used / total) * 100)
  return pct > 100 ? 100 : pct
}

/**
 * capacityBand returns the color band for a usage ratio. Boundaries:
 *   green:  pct < 70
 *   yellow: 70 <= pct < 90
 *   red:    pct >= 90
 */
export function capacityBand(used: number, total: number): CapacityBand {
  const pct = total > 0 ? (used / total) * 100 : 0
  if (pct >= 90) return 'red'
  if (pct >= 70) return 'yellow'
  return 'green'
}

/**
 * Tailwind background-color class per band. Centralised here so every
 * capacity-bar surface (CapacityBar, MachineList Available cell, future
 * wizard capacity card) maps the same band → the same colour. Drift would
 * confuse operators reading bars across pages.
 */
export const BAND_BG_CLASS: Record<CapacityBand, string> = {
  green: 'bg-success',
  yellow: 'bg-warn',
  red: 'bg-danger',
}

/**
 * formatAvailLabel renders the per-machine label used in the Create Agent
 * machine picker. Reads `Machine.status.availableCapacity` (the post-#140
 * field) and falls back to a "capacity unknown" string when the controller
 * hasn't published it yet (e.g. mock-provider machines, pre-first-reconcile).
 *
 * Format: "<id> — <cpu> CPU / <memory> GiB avail"
 *   cpu:    integer or one-decimal, no unit suffix
 *   memory: integer or one-decimal, GiB units (so "256Mi" → "0.25 GiB")
 */
export function formatAvailLabel(m: Machine): string {
  const avail = m.status?.availableCapacity
  if (!avail || (!avail.cpu && !avail.memory)) {
    return `${m.id} — capacity unknown`
  }
  const cpu = avail.cpu ? formatNum(parseCpu(avail.cpu)) : '?'
  const mem = avail.memory ? formatNum(parseMemoryGi(avail.memory)) : '?'
  return `${m.id} — ${cpu} CPU / ${mem} GiB avail`
}

// Internal: render a number without trailing zeros (2 → "2", 0.5 → "0.5", 0.25 → "0.25").
function formatNum(n: number): string {
  if (Number.isInteger(n)) return String(n)
  return n.toFixed(2).replace(/\.?0+$/, '')
}
