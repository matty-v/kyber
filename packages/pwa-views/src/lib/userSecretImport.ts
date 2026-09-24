export interface ImportedUserSecret {
  key: string
  value: string
}

// Mirrors pkg/usersecrets validation so imports fail before the first API call.
// The server remains authoritative; keep these grammar and size limits in sync.
const KEY_PATTERN = /^[A-Z][A-Z0-9_]{0,63}$/
const ENV_STYLE_PATTERN = /^[A-Z][A-Z0-9_]*$/
// A file key is its own filename under /user-secrets (a Secret data key with
// no path and no leading dot). Env-style keys stay valid for files and keep
// their /user-secrets/<key>.bin mount name.
const FILE_KEY_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$/
const LEGACY_FILE_NAME_PATTERN = /^[a-z][a-z0-9_]{0,63}\.bin$/
const RESERVED_PREFIXES = ['USER_', 'KYBER_']
export const MAX_USER_SECRET_ENTRY_BYTES = 64 * 1024
export const MAX_USER_SECRETS_AGGREGATE_BYTES = 256 * 1024

export function validateUserSecretKey(key: string, kind: 'kv' | 'file' = 'kv'): string | null {
  if (!key) return 'Key is required'
  if (kind === 'file' && !ENV_STYLE_PATTERN.test(key)) {
    if (!FILE_KEY_PATTERN.test(key) || key.includes('..')) {
      return 'File name must start with a letter or digit, then letters, digits, ".", "_" or "-" (no "/" or "..", at most 253 characters)'
    }
    if (LEGACY_FILE_NAME_PATTERN.test(key)) {
      return `${key} is where the key ${key.slice(0, -4).toUpperCase()} is mounted; use that key instead`
    }
    return null
  }
  if (!KEY_PATTERN.test(key)) {
    return 'Key must match ^[A-Z][A-Z0-9_]{0,63}$ (start with A-Z, then A-Z/0-9/_)'
  }
  for (const prefix of RESERVED_PREFIXES) {
    if (key.startsWith(prefix)) return `Key must not start with reserved prefix ${prefix}`
  }
  return null
}

/** The path a file secret is mounted at inside the agent. Mirrors usersecrets.FileName. */
export function userSecretFilePath(key: string): string {
  return ENV_STYLE_PATTERN.test(key) ? `/user-secrets/${key.toLowerCase()}.bin` : `/user-secrets/${key}`
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

    const withoutLeadingWhitespace = originalLine.trimStart()
    const assignment = withoutLeadingWhitespace.startsWith('export ')
      ? withoutLeadingWhitespace.slice(7).trimStart()
      : withoutLeadingWhitespace
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
