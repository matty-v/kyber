export const OPENROUTER_INFERENCE_URL = 'https://openrouter.ai/api/v1'

/** Exact route identity; lookalike hosts remain ordinary custom endpoints. */
export function isOpenRouterInference(baseURL: string): boolean {
  try {
    const url = new URL(baseURL)
    return url.protocol === 'https:' && url.host === 'openrouter.ai' &&
      url.pathname.replace(/\/$/, '') === '/api/v1' &&
      !url.username && !url.password && !url.search && !url.hash
  } catch {
    return false
  }
}
