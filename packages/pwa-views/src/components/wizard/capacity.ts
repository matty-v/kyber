import { capacityBand } from '../../lib/capacityBars'

/**
 * bandFor maps a resource request against available headroom to a color band.
 * Uses avail as the denominator so 70%/90% thresholds fire at 70%/90% of
 * remaining capacity. avail==0 + request>0 → red (machine full).
 *
 * Used by ResourcesSection's CPU + Memory band rendering. Originally lived
 * in pwa/src/pages/CreateAgent.tsx; relocated during #131 Phase A so multiple
 * Section components can share it.
 */
export function bandFor(
  requested: number,
  avail: number | undefined,
): 'green' | 'yellow' | 'red' {
  if (avail === undefined) return 'green' // unknown → don't alarm
  if (avail <= 0) return requested > 0 ? 'red' : 'green'
  return capacityBand(requested, avail)
}
