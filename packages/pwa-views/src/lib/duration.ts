/**
 * Format a duration given in seconds as a compact, human-readable string
 * showing the two most-significant units, selected by magnitude. Used by the
 * Agent Activity Breakdown table on the Metrics tab (kyber#426), where raw
 * second counts (e.g. `689402.0s`) are unreadable.
 *
 * Bands:
 *   >= 1 day    -> "Nd Nh"  (e.g. "7d 23h")
 *   >= 1 hour   -> "Nh Nm"  (e.g. "1h 38m")
 *   >= 1 minute -> "Nm Ns"  (e.g. "12m 46s")
 *   < 1 minute  -> "N.Ns"   (e.g. "59.4s"); exactly 0 (or negative/NaN) -> "0s"
 *
 * Secondary units are TRUNCATED (floored), never rounded, so the output can
 * never produce an invalid roll-over like "1h 60m" or "7d 24h". This is
 * display-only — the underlying metrics payload stays in seconds.
 */
export function formatDuration(secs: number): string {
  if (!Number.isFinite(secs) || secs <= 0) return '0s'

  // Sub-minute: one decimal, mirroring the prior `${secs.toFixed(1)}s` style.
  if (secs < 60) return `${secs.toFixed(1)}s`

  const total = Math.floor(secs)
  if (total < 3600) {
    const m = Math.floor(total / 60)
    const s = total % 60
    return `${m}m ${s}s`
  }
  if (total < 86400) {
    const h = Math.floor(total / 3600)
    const m = Math.floor((total % 3600) / 60)
    return `${h}h ${m}m`
  }
  const d = Math.floor(total / 86400)
  const h = Math.floor((total % 86400) / 3600)
  return `${d}d ${h}h`
}
