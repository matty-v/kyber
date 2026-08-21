export interface ImportedUserSecret {
  key: string
  value: string
}

const KEY_PATTERN = /^[A-Z][A-Z0-9_]{0,63}$/
const RESERVED_PREFIXES = ['USER_', 'KYBER_']

export const MAX_USER_SECRET_ENTRY_BYTES = 64 * 1024
export const MAX_USER_SECRETS_AGGREGATE_BYTES = 256 * 1024

export function validateUserSecretKey(key: string): string | null {
  if (!key) return 'Key is required'
  if (!KEY_PATTERN.test(key)) {
    return 'Key must match ^[A-Z][A-Z0-9_]{0,63}$ (start with A-Z, then A-Z/0-9/_)'
  }
  for (const prefix of RESERVED_PREFIXES) {
    if (key.startsWith(prefix)) return `Key must not start with reserved prefix ${prefix}`
  }
  return null
}

/** Parse a conservative dotenv-style KEY=VALUE file without expanding values. */
export function parseUserSecretImport(raw: string): ImportedUserSecret[] {
  const entries: ImportedUserSecret[] = []
  const seen = new Set<string>()
  const encoder = new TextEncoder()

  for (const [index, originalLine] of raw.replace(/^\uFEFF/, '').split(/\r?\n/).entries()) {
    const lineNumber = index + 1
    const trimmed = originalLine.trim()
    if (!trimmed || trimmed.startsWith('#')) continue

    const assignment = trimmed.startsWith('export ') ? trimmed.slice(7).trimStart() : trimmed
    const separator = assignment.indexOf('=')
    if (separator < 1) {
      throw new Error(`Line ${lineNumber}: expected KEY=VALUE`)
    }

    const key = assignment.slice(0, separator).trim()
    const keyError = validateUserSecretKey(key)
    if (keyError) throw new Error(`Line ${lineNumber}: ${keyError}`)
    if (seen.has(key)) throw new Error(`Line ${lineNumber}: duplicate key ${key}`)

    // Preserve the value exactly after the first '='. This deliberately does
    // not expand variables, escapes, quotes, or inline comments: secret values
    // must round-trip rather than acquire shell semantics in the browser.
    const value = assignment.slice(separator + 1)
    const size = encoder.encode(value).length
    if (size > MAX_USER_SECRET_ENTRY_BYTES) {
      throw new Error(`Line ${lineNumber}: ${key} exceeds ${MAX_USER_SECRET_ENTRY_BYTES} bytes`)
    }

    seen.add(key)
    entries.push({ key, value })
  }

  if (entries.length === 0) throw new Error('File contains no KEY=VALUE entries')
  return entries
}
