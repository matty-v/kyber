import type { RuntimeDescriptor } from '../../lib/types'
import type { AgentImportApply, ArchiveSummary, IdentityRepoMode, ModelInfo } from '../../lib/types'

/**
 * IdentityRepoMode: which of the three identity-repo flows the operator picked.
 * 'template' = create a new repo from `matty-v/kyber-agent-template`,
 * 'existing' = link an already-created repo by `owner/repo`,
 * 'none'     = no identity repo; the agent keeps its state on its own disk.
 * Defined next to the config type because the server reports the same modes.
 */
export type { IdentityRepoMode } from '../../lib/types'

/**
 * WizardState is the canonical form-state shape carried through every step of
 * the Create Agent wizard. Lifted from the original useState literal in
 * CreateAgent.tsx (#131 Phase A) so sections can be props-driven and tested
 * in isolation.
 */
export interface WizardState {
  runtimeContract?: RuntimeDescriptor
  runtimes?: RuntimeDescriptor[]
  runtimeApiKey?: string
  name: string
  machine: string
  runtime: string
  model: string
  cpu: string
  memory: string
  disk: string
  soulDescription: string
  startupPrompt: string
  telegramEnabled: boolean
  authType: 'oauth' | 'api-key'
  oauthCode: string
  pkceVerifier: string
  pkceState: string
  anthropicApiKey: string
  openaiApiKey: string
  telegramBotToken: string
  telegramAllowedUserIds: string
  slackEnabled: boolean
  slackBotToken: string
  slackAppToken: string
  slackAllowedUserIds: string
  slackAllowedChannelIds: string
  // Custom inference endpoint (spec.inference). Off by default: an agent with
  // inferenceEnabled false uses its harness's built-in provider, which is what
  // every agent did before this field existed. Only offered for runtimes that
  // declare the custom-inference-endpoint feature.
  inferenceEnabled: boolean
  inferenceBaseURL: string
  inferenceModel: string
  // The endpoint's key as a value. Kyber stores it in a managed Secret, the
  // same way it does an OpenRouter or Anthropic key — the operator never
  // creates a Secret by hand.
  inferenceApiKey: string
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
  // Modes the control plane accepts, from GET /api/v1/config. Derived at
  // render time like `runtimes`, never stored; undefined means not yet known,
  // which isIdentityValid treats as 'none' only.
  identityRepoModes?: IdentityRepoMode[]
  identityRepoExisting: string
  // True when template-mode and the target repo already exists under
  // the configured identity owner. The IdentitySection populates this
  // from the live /github/repos/{owner}/{name}/exists check; the step
  // gate (isIdentityValid) blocks Continue while it's true so users can
  // pick a different agent name before submission. Defaults to false.
  identityRepoCollision: boolean
  // Create from a disk archive (MAT-88). Undefined means an empty disk; an
  // object without an ID means the operator chose "a disk archive" but has
  // not picked one yet.
  archiveSource?: ArchiveSourceChoice
  // Restore files that look like credentials, and crontabs the source agent
  // installed itself (both off by default).
  keepCredentialFiles: boolean
  keepCrontabs: boolean
  // What the import carries beyond the disk (MAT-90). Defaults match the
  // server's: apply the config, jobs paused, bindings disabled, secrets
  // copied when the source agent is on this installation.
  archiveApply: Required<AgentImportApply>
}

export interface ArchiveSourceChoice {
  exportId?: string
  uploadId?: string
  label: string
  summary?: ArchiveSummary
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
 * Default starting state. Model remains empty so agent creation inherits the
 * runtime-scoped fleet default instead of pinning a per-agent override.
 */
export function initialWizardState(_models: ModelInfo[]): WizardState {
  return {
    keepCredentialFiles: false,
    keepCrontabs: false,
    archiveApply: defaultArchiveApply(),
    name: '',
    machine: '',
    runtime: 'claude-code',
    model: '',
    cpu: '1',
    memory: '2Gi',
    disk: '50Gi',
    soulDescription: '',
    startupPrompt: '',
    telegramEnabled: false,
    authType: 'oauth',
    oauthCode: '',
    pkceVerifier: '',
    pkceState: '',
    anthropicApiKey: '',
    openaiApiKey: '',
    telegramBotToken: '',
    telegramAllowedUserIds: '',
    slackEnabled: false,
    slackBotToken: '',
    slackAppToken: '',
    slackAllowedUserIds: '',
    slackAllowedChannelIds: '',
    inferenceEnabled: false,
    inferenceBaseURL: '',
    inferenceModel: '',
    inferenceApiKey: '',
    discordEnabled: false,
    discordBotToken: '',
    discordGuildIds: '',
    discordChannelIds: '',
    discordAllowedUserIds: '',
    discordMentionOnly: false,
    // Starts at 'none' so nothing GitHub-backed is assumed before config
    // loads; CreateAgent moves it to the template default once the control
    // plane says it can create one.
    identityRepoMode: 'none',
    identityRepoExisting: '',
    identityRepoCollision: false,
  }
}

export function defaultArchiveApply(): Required<AgentImportApply> {
  return { config: true, jobs: 'paused', bindings: 'disabled', secrets: 'copy-if-local' }
}
