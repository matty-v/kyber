import { capacityBand, pctUsed, BAND_BG_CLASS } from '../lib/capacityBars'

export interface CapacityBarProps {
  used: number
  total: number
  label: string
}

/**
 * CapacityBar — a single horizontal bar showing `used / total` as a colored
 * fill. The color band thresholds come from `capacityBand` (green < 70%,
 * yellow 70-90%, red >= 90%). Width caps at 100% even when over-allocated;
 * the red band signals the over-allocation.
 *
 * The label area shows: "<label>: <pct>%" — the bar itself is below.
 */
export function CapacityBar({ used, total, label }: CapacityBarProps) {
  const band = capacityBand(used, total)
  const pct = pctUsed(used, total)
  return (
    <div className="flex flex-col gap-1">
      <div className="flex justify-between text-xs text-text-secondary">
        <span>{label}</span>
        <span>{pct}%</span>
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
