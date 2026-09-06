import type { Agent, RuntimeDescriptor } from './types'
import type { WizardState } from '../components/wizard/types'

// Compatibility with servers predating contract discovery. New integrations
// arrive through /config; an unknown runtime never inherits another provider.
export const legacyRuntimeContracts: RuntimeDescriptor[] = [
  { id: 'claude-code', legacyCatalogKey: 'models', legacyVersionsKey: 'claudeCodeVersions', name: 'Claude Code', contractVersion: '1.0', profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: [], authModes: [
    { id: 'oauth', name: 'OAuth (Claude Code subscription)', flow: 'authorization-code', inputField: 'oauthCode', reauthorizePath: 'oauth', authorizationUrl: 'https://claude.ai/oauth/authorize', authorizationParams: {
      code: 'true', client_id: '9d1c250a-e61b-44d9-88ed-5944d1962f5e', response_type: 'code', redirect_uri: 'https://platform.claude.com/oauth/code/callback', scope: 'org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload',
    } },
    { id: 'api-key', name: 'Anthropic API Key', flow: 'api-key', inputField: 'anthropicApiKey' },
  ] },
  { id: 'codex', legacyCatalogKey: 'codexModels', legacyVersionsKey: 'codexVersions', name: 'Codex (ChatGPT)', contractVersion: '1.0', profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: [], authModes: [
    { id: 'oauth', name: 'ChatGPT subscription (device login)', flow: 'device-code', inputField: 'codexAuthJson', reauthorizePath: 'codex-device-auth' },
    { id: 'api-key', name: 'OpenAI API key', flow: 'api-key', inputField: 'openaiApiKey' },
  ] },
]
export function wizardContract(state: WizardState) {
  return state.runtimeContract ?? (state.runtimes ?? legacyRuntimeContracts).find(d => d.id === state.runtime)
}
export function wizardAuth(state: WizardState) {
  return wizardContract(state)?.authModes.find(m => m.id === state.authType)
}
export function wizardApiKey(state: WizardState) {
  const field = wizardAuth(state)?.inputField
  if (field === 'anthropicApiKey') return state.anthropicApiKey
  if (field === 'openaiApiKey') return state.openaiApiKey
  return state.runtimeApiKey ?? ''
}
export function agentAuth(agent: Agent) {
  return (agent.runtimeContract ?? legacyRuntimeContracts.find(d => d.id === agent.runtime))?.authModes.find(m => m.id === agent.authType)
}
