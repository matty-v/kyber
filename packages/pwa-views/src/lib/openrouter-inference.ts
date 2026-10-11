import type { RuntimeDescriptor } from './types'

export const OPENROUTER_INFERENCE_URL = 'https://openrouter.ai/api/v1'

export function validCodexOpenRouterModel(model: string): boolean {
  return model.length <= 253 && /^[A-Za-z0-9][A-Za-z0-9._-]*\/[A-Za-z0-9][A-Za-z0-9._:-]*$/.test(model)
}

// Evidence is supplied by the control plane's runtime descriptor. A model ID
// alone cannot establish which upstream OpenRouter selected for a request.
export function codexOpenRouterEvidence(contract: RuntimeDescriptor | undefined, model: string): string {
  const id = model.trim()
  if (!id) return 'Enter a model ID to see its Kyber task-contract evidence.'
  const evidence = contract?.routeModelEvidence?.find(item => item.route === 'openrouter-responses' && item.model === id)
  if (!evidence) return 'No Kyber task-contract evidence for this OpenRouter model. Interactive and tool behavior are unverified.'
  const observed = Object.entries(evidence.outcomes).filter(([, outcome]) => outcome !== 'untested')
    .map(([workflow, outcome]) => `${workflow.replaceAll('-', ' ')} ${outcome}`).join(', ')
  return `Codex ${evidence.harnessVersion} dev pilot (${evidence.observedAt}): ${observed}. Upstream provider ${evidence.upstream}; this is limited evidence, not a model guarantee.`
}

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
