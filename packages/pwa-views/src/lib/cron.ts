// Tiny cron humanizer for the Jobs tab Schedule column (#243). Handles
// the patterns operators actually write — daily / hourly / weekly /
// every-N-minutes — and falls back to the raw cron string for anything
// else so the operator still has the canonical form to read.
//
// Deliberately not a full cron parser: the goal is "Daily at 9 AM"
// next to "0 9 * * *", not a 100% correct natural-language renderer.
// Cases the helper doesn't handle (named months, ranges combined with
// steps, etc.) just return null and the caller shows the raw cron.

const MINUTE_RE = /^([0-9*/,-]+)$/
const HOUR_RE = MINUTE_RE
const FIELD_RE = MINUTE_RE

const DOW_NAMES = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

// formatHour12 returns "9 AM", "12 PM", "1 PM" for hour-of-day 0..23.
function formatHour12(hour: number): string {
  if (hour === 0) return '12 AM'
  if (hour === 12) return '12 PM'
  if (hour < 12) return `${hour} AM`
  return `${hour - 12} PM`
}

// parseSimpleList parses a single-or-comma-list field (e.g. "1,2,3" or
// "9") into a sorted unique number array. Returns null on anything
// containing wildcards, ranges, or steps so the caller can fall back.
function parseSimpleList(field: string): number[] | null {
  if (field === '*' || field.includes('/') || field.includes('-')) return null
  const parts = field.split(',').map((p) => p.trim())
  const out: number[] = []
  for (const p of parts) {
    if (!/^\d+$/.test(p)) return null
    out.push(Number(p))
  }
  return Array.from(new Set(out)).sort((a, b) => a - b)
}

// stepValue parses "*/N" → N. Returns null if the field isn't an
// every-N-units shape.
function stepValue(field: string): number | null {
  const m = /^\*\/(\d+)$/.exec(field)
  if (!m) return null
  const n = Number(m[1])
  return Number.isInteger(n) && n > 0 ? n : null
}

// humanizeCron translates a 5-field cron to a short English phrase.
// Returns null when the pattern isn't one we handle — the caller is
// expected to fall back to rendering the raw cron string.
//
// Recognised patterns (in priority order):
//   - "* * * * *"                 → Every minute
//   - "*/N * * * *"               → Every N minutes
//   - "M * * * *"                 → Every hour at :MM
//   - "M H * * *"                 → Daily at H:MM (am/pm)
//   - "M H,H,H * * *" (single M)  → Daily at H, H, H AM/PM
//   - "M H * * D"                 → Weekly on Day at H:MM
export function humanizeCron(cron: string): string | null {
  const trimmed = cron.trim()
  const fields = trimmed.split(/\s+/)
  if (fields.length !== 5) return null
  const [min, hour, dom, mon, dow] = fields
  if (!FIELD_RE.test(min) || !HOUR_RE.test(hour)) return null

  // Every minute / every-N-minutes shortcuts. Only valid when hour, dom,
  // mon, dow are wildcards — otherwise it's a more complex pattern.
  const allWildBeyondMin = hour === '*' && dom === '*' && mon === '*' && dow === '*'
  if (allWildBeyondMin) {
    if (min === '*') return 'Every minute'
    const step = stepValue(min)
    if (step !== null) return step === 1 ? 'Every minute' : `Every ${step} minutes`
  }

  // Every hour at :MM (single integer minute, hour wildcard).
  if (hour === '*' && dom === '*' && mon === '*' && dow === '*') {
    const mins = parseSimpleList(min)
    if (mins && mins.length === 1) {
      return `Every hour at :${mins[0].toString().padStart(2, '0')}`
    }
  }

  // Daily / weekly only — dom and mon must be wildcards. Anything more
  // specific (named month, day-of-month) returns null.
  if (dom !== '*' || mon !== '*') return null

  const minList = parseSimpleList(min)
  const hourList = parseSimpleList(hour)
  if (!minList || !hourList || minList.length === 0 || hourList.length === 0) return null
  if (minList.length > 1) return null // ambiguous to phrase, fall back to raw
  const mm = minList[0]
  if (mm < 0 || mm > 59) return null
  for (const h of hourList) {
    if (h < 0 || h > 23) return null
  }

  // Weekly: dow !== '*'. Single day name only.
  if (dow !== '*') {
    const days = parseSimpleList(dow)
    if (!days || days.length !== 1 || hourList.length !== 1) return null
    const d = days[0]
    if (d < 0 || d > 6) return null
    const time = mm === 0 ? formatHour12(hourList[0]) : `${formatHour12(hourList[0])}:${mm.toString().padStart(2, '0')}`
    return `Weekly on ${DOW_NAMES[d]} at ${time}`
  }

  // Daily.
  if (hourList.length === 1) {
    const h = hourList[0]
    return mm === 0
      ? `Daily at ${formatHour12(h)}`
      : `Daily at ${formatHour12(h)}:${mm.toString().padStart(2, '0')}`
  }

  // Daily, multiple hours, single minute (most common: "0 1,2,3 * * *"
  // → "Daily at 1, 2, 3 AM"). Group into AM/PM runs only when all hours
  // share a half-of-day, otherwise spell each out.
  const allAM = hourList.every((h) => h < 12)
  const allPM = hourList.every((h) => h >= 12)
  if (mm === 0 && (allAM || allPM)) {
    const labels = hourList.map((h) => (h === 0 ? '12' : h > 12 ? String(h - 12) : String(h)))
    const suffix = allAM ? 'AM' : 'PM'
    return `Daily at ${labels.join(', ')} ${suffix}`
  }
  // Mixed AM/PM or non-zero minute: spell each hour with its own suffix.
  const parts = hourList.map((h) => {
    const time =
      mm === 0 ? formatHour12(h) : `${formatHour12(h)}:${mm.toString().padStart(2, '0')}`
    return time
  })
  return `Daily at ${parts.join(', ')}`
}
