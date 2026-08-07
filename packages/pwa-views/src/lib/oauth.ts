/**
 * Parse the pasted Anthropic OAuth-callback input. The manual-redirect page
 * displays the authorization code in `<code>#<state>` format. We also
 * accept a full callback URL or a raw URL-encoded query-string fallback.
 *
 * Returns `null` only when the input is empty.
 */
export function parseAuthorizationInput(
  input: string,
): { code: string; state?: string } | null {
  const value = input.trim()
  if (!value) return null
  try {
    const url = new URL(value)
    const code = url.searchParams.get('code')
    const state = url.searchParams.get('state') ?? undefined
    if (code) return { code, state }
  } catch {
    // Not a URL — fall through.
  }
  if (value.includes('#')) {
    const [code, state] = value.split('#', 2)
    return { code, state }
  }
  if (value.includes('code=')) {
    const params = new URLSearchParams(value)
    const code = params.get('code')
    if (code) return { code, state: params.get('state') ?? undefined }
  }
  return { code: value }
}
