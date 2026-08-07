// Name-input sanitizers shared by CreateAgent (BasicsSection) and CreateMachine.
//
// Two flavors because we want different behavior between keystroke and
// commit:
// - toKebabCaseTyping: every keystroke. Lowercase + collapse runs of
//   non-alphanumeric to a single '-' + slice to 63. Does NOT strip leading
//   or trailing hyphens — that would eat the user's '-' before they can
//   type the next character (the bug in #189).
// - toKebabCase: blur + submit. Full sanitize, including stripping
//   leading/trailing hyphens. Visual feedback on blur, defense-in-depth
//   on submit so an Enter-key advance that skips blur still produces a
//   valid kebab-case name in the API request.

const NON_ALNUM_RUN = /[^a-z0-9]+/g
const LEAD_OR_TRAIL_HYPHENS = /^-+|-+$/g
const MAX_LEN = 63

/**
 * Per-keystroke sanitizer. Preserves trailing hyphens so the user can type
 * "my-agent" character-by-character without the input eating the '-' between
 * 'y' and 'a'. Mid-string hyphen runs still collapse to one (typing "  "
 * produces a single '-'), since that doesn't break user intent.
 */
export function toKebabCaseTyping(s: string): string {
  return s.toLowerCase().replace(NON_ALNUM_RUN, '-').slice(0, MAX_LEN)
}

/**
 * Full sanitizer. Strips leading and trailing hyphens. Use on blur (visual
 * feedback when the user leaves the field) and on submit (final safety net
 * before the API call).
 */
export function toKebabCase(s: string): string {
  return s
    .toLowerCase()
    .replace(NON_ALNUM_RUN, '-')
    .replace(LEAD_OR_TRAIL_HYPHENS, '')
    .slice(0, MAX_LEN)
}
