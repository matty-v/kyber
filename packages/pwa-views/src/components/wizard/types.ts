import type { ModelInfo } from '../../lib/types'

/**
 * IdentityRepoMode: which of the three identity-repo flows the operator picked.
 * 'template' = create a new repo from `matty-v/kyber-agent-template`,
 * 'existing' = link an already-created repo by `owner/repo`,
 * 'none'     = skip identity-repo provisioning entirely.
 */
export type IdentityRepoMode = 'template' | 'existing' | 'none'

/**
 * WizardState is the canonical form-state shape carried through every step of
 * the Create Agent wizard. Lifted from the original useState literal in
 * CreateAgent.tsx (#131 Phase A) so sections can be props-driven and tested
 * in isolation.
 */
export interface WizardState {
  name: string
  machine: string
  runtime: string
  model: string
  scaling: 'warm' | 'scale-to-zero'
  cpu: string
  memory: string
  disk: string
  soulDescription: string
  telegramEnabled: boolean
  authType: 'oauth' | 'api-key'
  oauthCode: string
  pkceVerifier: string
  pkceState: string
  anthropicApiKey: string
  openaiApiKey: string
  telegramBotToken: string
  telegramAllowedUserIds: string
  // Discord (kyber#664) is optional at create time and needs a bot that already
  // exists, so it defaults off and collapsed. When enabled, CreateAgent wires it
  // through PUT /comms/discord AFTER the agent exists — the same code path the
  // Comms tab uses, so there is one implementation of "wire a channel".
  discordEnabled: boolean
  discordBotToken: string
  discordGuildIds: string
  discordChannelIds: string
  discordAllowedUserIds: string
  discordMentionOnly: boolean
  identityRepoMode: IdentityRepoMode
  identityRepoExisting: string
  // True when template-mode and the target repo already exists under
  // the configured identity owner. The IdentitySection populates this
  // from the live /github/repos/{owner}/{name}/exists check; the step
  // gate (isIdentityValid) blocks Continue while it's true so users can
  // pick a different agent name before submission. Defaults to false.
  identityRepoCollision: boolean
}

/**
 * Sectional setter: the orchestrator holds a single setForm function and
 * exposes a typed-key facade so sections call set('cpu', '2') without
 * needing to spread the full state.
 */
export type WizardSetter = <K extends keyof WizardState>(
  key: K,
  value: WizardState[K],
) => void

/**
 * Default starting state. `model` is seeded from the server's models list
 * if available; CreateAgent's existing useEffect handles the late-load case
 * (don't duplicate the effect here).
 */
export function initialWizardState(models: ModelInfo[]): WizardState {
  return {
    name: '',
    machine: '',
    runtime: 'claude-code',
    model: models[0]?.id ?? '',
    scaling: 'warm',
    cpu: '1',
    memory: '2Gi',
    disk: '50Gi',
    soulDescription: '',
    telegramEnabled: false,
    authType: 'oauth',
    oauthCode: '',
    pkceVerifier: '',
    pkceState: '',
    anthropicApiKey: '',
    openaiApiKey: '',
    telegramBotToken: '',
    telegramAllowedUserIds: '',
    discordEnabled: false,
    discordBotToken: '',
    discordGuildIds: '',
    discordChannelIds: '',
    discordAllowedUserIds: '',
    discordMentionOnly: false,
    identityRepoMode: 'template',
    identityRepoExisting: '',
    identityRepoCollision: false,
  }
}
