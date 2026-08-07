import { IDENTITY_REPO_SLUG_RE } from './identity-utils'
import type { WizardState } from './types'

/**
 * Validation result for a wizard step. The discriminated union lets the
 * orchestrator both gate Next/Submit (`.ok`) AND surface a human-readable
 * reason next to the disabled button instead of a generic "fill the required
 * fields" hint. Per-step granularity is good enough for V1 — pushing the
 * reason to specific fields would require threading per-field error state
 * through every section component, which is a follow-up.
 */
export type StepValidation = { ok: true } | { ok: false; reason: string }

const OK: StepValidation = { ok: true }

export interface WizardStepDef {
  id: 1 | 2 | 3 | 4 | 5
  label: string
  isValid: (state: WizardState) => StepValidation
}

/** Step 1 — Basics: name + machine selected. */
export function isBasicsValid(state: WizardState): StepValidation {
  if (state.name.length === 0) return { ok: false, reason: 'Pick a name for the agent.' }
  if (state.machine.length === 0) return { ok: false, reason: 'Pick a machine.' }
  return OK
}

/**
 * Step 2 — Resources: cpu/memory/disk all non-empty.
 *
 * Note: capacity-fit checking against `machineAvailable` is enforced separately
 * on the Submit button via `fitCheckPasses` in the orchestrator. The gate here
 * only ensures non-empty selections so the user can navigate forward and read
 * the capacity hint while adjusting.
 */
export function isResourcesValid(state: WizardState): StepValidation {
  if (state.cpu.length === 0) return { ok: false, reason: 'Pick a CPU value.' }
  if (state.memory.length === 0) return { ok: false, reason: 'Pick a memory value.' }
  if (state.disk.length === 0) return { ok: false, reason: 'Pick a disk value.' }
  return OK
}

/**
 * Step 3 — Identity:
 *   - 'none'     always pass.
 *   - 'template' pass unless the live collision check has flagged the
 *                target repo as already existing (#134). Cleared by the
 *                IdentitySection when the user changes the agent name.
 *   - 'existing' requires a GitHub-shaped slug.
 */
export function isIdentityValid(state: WizardState): StepValidation {
  if (state.identityRepoMode === 'none') return OK
  if (state.identityRepoMode === 'template') {
    if (state.identityRepoCollision) {
      return { ok: false, reason: 'A repo with that name already exists — change the agent name.' }
    }
    return OK
  }
  if (!IDENTITY_REPO_SLUG_RE.test(state.identityRepoExisting)) {
    return { ok: false, reason: 'Enter an existing repo as owner/repo (e.g. matty-v/dave-agent).' }
  }
  return OK
}

/**
 * Step 4 — Auth: oauth needs both verifier (set when the user clicked the
 * Authorize button) and a pasted code; api-key needs the key. Telegram channel
 * fields are optional even if `telegramEnabled` is true.
 */
export function isAuthValid(state: WizardState): StepValidation {
  if (state.runtime === 'codex') {
    if (state.authType === 'api-key' && state.openaiApiKey.trim().length === 0) {
      return { ok: false, reason: 'Paste your OpenAI API key.' }
    }
    return OK
  }
  if (state.authType === 'oauth') {
    if (state.pkceVerifier.length === 0) {
      return { ok: false, reason: 'Click "Authorize with Claude" to start the OAuth flow.' }
    }
    if (state.oauthCode.length === 0) {
      return { ok: false, reason: 'Paste the authorization code Anthropic showed you.' }
    }
    return OK
  }
  if (state.anthropicApiKey.length === 0) {
    return { ok: false, reason: 'Paste your Anthropic API key.' }
  }
  return OK
}

/** Step 5 — Review: read-only summary, always valid. */
export function isReviewValid(_state: WizardState): StepValidation {
  return OK
}

export const WIZARD_STEPS: readonly WizardStepDef[] = [
  { id: 1, label: 'Basics', isValid: isBasicsValid },
  { id: 2, label: 'Resources', isValid: isResourcesValid },
  { id: 3, label: 'Identity', isValid: isIdentityValid },
  { id: 4, label: 'Auth', isValid: isAuthValid },
  { id: 5, label: 'Review', isValid: isReviewValid },
] as const

/**
 * Returns the smallest step id (1..5) whose validator returns a non-ok result.
 * Returns 5 (Review, the read-only last step) when every prior gate passes —
 * effectively "no invalid step before Review."
 *
 * Used by the deep-link guard: if `?step=N` requests a step beyond the earliest
 * invalid step, the orchestrator bounces the URL back to the earliest invalid
 * step so the user can fix things in order.
 */
export function earliestInvalidStep(state: WizardState): number {
  for (const step of WIZARD_STEPS) {
    if (!step.isValid(state).ok) return step.id
  }
  return 5
}
