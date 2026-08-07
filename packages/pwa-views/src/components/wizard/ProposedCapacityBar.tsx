import { BAND_BG_CLASS } from '../../lib/capacityBars'

export interface ProposedCapacityBarProps {
  /** Row label (e.g. "CPU"). */
  label: string
  /** What other agents on this machine already consume. */
  usedByOthers: number
  /** What the operator is about to add via this wizard. */
  newRequest: number
  /** Assignable total — the bar's denominator. */
  total: number
  /** Unit suffix shown after numbers (e.g. "", " GiB"). */
  unit: string
  /** Decimal places for the numeric readout. */
  decimals: number
}

/**
 * ProposedCapacityBar — two-segment horizontal bar showing
 * `usedByOthers + newRequest` against `total`. Gray segment = existing
 * usage; cyan segment = the wizard's proposed new agent. When the sum
 * exceeds total, the new segment renders red and visually extends past
 * the bar's right edge so the over-allocation is unmistakable.
 *
 * Different shape from `pwa/src/components/CapacityBar.tsx` (single
 * `used / total` segment) — kept as its own component because the
 * "you are proposing this" semantic only makes sense in the wizard.
 */
export function ProposedCapacityBar({
  label,
  usedByOthers,
  newRequest,
  total,
  unit,
  decimals,
}: ProposedCapacityBarProps) {
  const sum = usedByOthers + newRequest
  const exceeds = total > 0 && sum > total
  const usedPct = total > 0 ? (usedByOthers / total) * 100 : 0
  const newPct = total > 0 ? (newRequest / total) * 100 : 0
  // Cap "used" at 100% so a pre-existing over-allocation doesn't push the
  // new segment off-screen. The new segment is what the operator can act on.
  const usedDisplayPct = Math.min(usedPct, 100)
  // The new-segment width is "everything past the gray" — when it exceeds,
  // this number is > (100 - usedDisplayPct) and the bar overflows visibly.
  const newDisplayWidth = Math.max(0, usedPct + newPct - usedDisplayPct)

  const newBandClass = exceeds ? BAND_BG_CLASS.red : 'bg-info'

  return (
    <div data-testid={`proposed-bar-${label.toLowerCase()}`}>
      <div className="flex justify-between font-mono text-[11px] tabular-nums text-text-secondary mb-0.5">
        <span>{label}</span>
        <span
          aria-label={`${label}: used ${usedByOthers.toFixed(decimals)}${unit}, adding ${newRequest.toFixed(decimals)}${unit}, total ${sum.toFixed(decimals)} of ${total.toFixed(decimals)}${unit}`}
          data-exceeds={exceeds || undefined}
          className={exceeds ? 'text-danger' : undefined}
        >
          {usedByOthers.toFixed(decimals)} + {newRequest.toFixed(decimals)} = {sum.toFixed(decimals)} / {total.toFixed(decimals)}
          {unit}
        </span>
      </div>
      {/* Outer wrapper allows the red segment to extend past the bar when
          the proposal exceeds total. The bar background sits in a separate
          inline-block so it doesn't get visually extended. */}
      <div className="relative h-2">
        <div className="absolute inset-0 rounded bg-surface-overlay" />
        <div
          data-band="used"
          className="absolute inset-y-0 left-0 rounded-l bg-text-muted"
          style={{ width: `${usedDisplayPct}%` }}
        />
        <div
          data-band={exceeds ? 'red' : 'new'}
          className={`absolute inset-y-0 ${newBandClass} ${exceeds ? 'rounded-r' : ''}`}
          style={{
            left: `${usedDisplayPct}%`,
            width: `${newDisplayWidth}%`,
          }}
        />
      </div>
    </div>
  )
}
