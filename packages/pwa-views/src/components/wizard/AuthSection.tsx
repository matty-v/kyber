import { Button } from '../Button'
import { generatePkcePair } from '../../lib/pkce'
import { inputClass, labelClass } from './styles'
import type { WizardSetter, WizardState } from './types'

const CLAUDE_CODE_CLIENT_ID = '9d1c250a-e61b-44d9-88ed-5944d1962f5e'
const OAUTH_REDIRECT_URI = 'https://platform.claude.com/oauth/code/callback'
const OAUTH_SCOPES = [
  'org:create_api_key',
  'user:profile',
  'user:inference',
  'user:sessions:claude_code',
  'user:mcp_servers',
  'user:file_upload',
].join(' ')

function buildAuthorizeUrl(challenge: string, state: string): string {
  const u = new URL('https://claude.ai/oauth/authorize')
  u.searchParams.set('code', 'true') // Anthropic-specific required param
  u.searchParams.set('client_id', CLAUDE_CODE_CLIENT_ID)
  u.searchParams.set('response_type', 'code')
  u.searchParams.set('redirect_uri', OAUTH_REDIRECT_URI)
  u.searchParams.set('scope', OAUTH_SCOPES)
  u.searchParams.set('code_challenge', challenge)
  u.searchParams.set('code_challenge_method', 'S256')
  u.searchParams.set('state', state)
  return u.toString()
}

/**
 * ChannelDef — the wizard's auth-step channel picker. Each channel is a
 * checkbox plus the fields it needs, so adding one is a row here rather than a
 * change to this section's UI.
 *
 * Field names are keyed to WizardState, split by the input type they render:
 * text/secret fields map to string keys, toggles to boolean keys. Telegram
 * needs one token; Discord (kyber#664) needs a token plus three allowlists and
 * a toggle, which is why `fields` is a list rather than the single token slot
 * the original Telegram-only shape assumed.
 */
type ChannelTextKey =
  | 'telegramBotToken'
  | 'telegramAllowedUserIds'
  | 'discordBotToken'
  | 'discordGuildIds'
  | 'discordChannelIds'
  | 'discordAllowedUserIds'

type ChannelToggleKey = 'discordMentionOnly'

interface ChannelTextField {
  kind: 'text' | 'secret'
  name: ChannelTextKey
  required: boolean
  label: string
  placeholder: string
  helperText?: string
}

interface ChannelToggleField {
  kind: 'toggle'
  name: ChannelToggleKey
  label: string
  helperText?: string
}

type ChannelField = ChannelTextField | ChannelToggleField

interface ChannelDef {
  id: 'telegram' | 'discord'
  label: string
  /** Maps to a boolean key on WizardState. */
  enabledKey: 'telegramEnabled' | 'discordEnabled'
  /** Channels require OAuth. The auth-step gate enforces this. */
  requiresOAuth: boolean
  /** Rendered under the checkbox before the fields — sets expectations. */
  blurb?: string
  fields: ChannelField[]
}

const CHANNELS: ChannelDef[] = [
  {
    id: 'telegram',
    label: 'Telegram',
    enabledKey: 'telegramEnabled',
    requiresOAuth: true,
    fields: [
      {
        kind: 'secret',
        name: 'telegramBotToken',
        required: true,
        label: 'Telegram bot token',
        placeholder: '0000000000:ABC…',
        helperText: 'From @BotFather. Stored as a k8s Secret.',
      },
      {
        kind: 'text',
        name: 'telegramAllowedUserIds',
        required: true,
        label: 'Who can talk to it',
        placeholder: '1000000001, …',
        helperText: 'Telegram numeric user IDs, comma-separated. Ask @userinfobot for yours. Anyone not listed is ignored.',
      },
    ],
  },
  {
    id: 'discord',
    label: 'Discord',
    enabledKey: 'discordEnabled',
    requiresOAuth: true,
    blurb:
      'Needs a Discord bot you have already created, with Message Content Intent turned on. You can also set this up later from the agent’s Comms tab.',
    fields: [
      {
        kind: 'secret',
        name: 'discordBotToken',
        required: true,
        label: 'Discord bot token',
        placeholder: 'Bot token',
        helperText: 'From the Discord Developer Portal. Stored as a k8s Secret.',
      },
      {
        kind: 'text',
        name: 'discordAllowedUserIds',
        required: true,
        label: 'Who can talk to it',
        placeholder: '123456789012345678, …',
        helperText:
          'Discord user IDs, comma-separated. Required — anyone not listed is ignored. Discord Settings → Advanced → Developer Mode, then right-click → Copy ID.',
      },
      {
        kind: 'text',
        name: 'discordGuildIds',
        required: false,
        label: 'Servers',
        placeholder: 'Any server',
        helperText: 'Comma-separated server IDs. Leave blank for any.',
      },
      {
        kind: 'text',
        name: 'discordChannelIds',
        required: false,
        label: 'Channels',
        placeholder: 'Any channel',
        helperText: 'Comma-separated channel IDs. Leave blank for any.',
      },
      {
        kind: 'toggle',
        name: 'discordMentionOnly',
        label: 'Only when mentioned',
        helperText:
          'Turn this on for a channel where people also talk to each other — otherwise every side conversation costs the agent a turn.',
      },
    ],
  },
]

export interface AuthSectionProps {
  state: WizardState
  set: WizardSetter
}

export function AuthSection({ state, set }: AuthSectionProps) {
  async function startOAuth() {
    const { verifier, challenge } = await generatePkcePair()
    const oauthState = crypto.randomUUID()
    set('pkceVerifier', verifier)
    set('pkceState', oauthState)
    set('oauthCode', '')
    window.open(buildAuthorizeUrl(challenge, oauthState), '_blank', 'noopener')
  }

  return (
    <section className="space-y-5">
      {state.runtime === 'codex' ? (
        <div className="space-y-4">
          <div>
            <label htmlFor="agent-codex-auth-type" className={labelClass}>Authentication</label>
            <select
              id="agent-codex-auth-type"
              value={state.authType}
              onChange={(e) => {
                const val = e.target.value as 'oauth' | 'api-key'
                set('authType', val)
                if (val === 'api-key') {
                  set('telegramEnabled', false)
                  set('discordEnabled', false)
                }
              }}
              className={inputClass}
            >
              <option value="oauth">ChatGPT subscription (device login)</option>
              <option value="api-key">OpenAI API key</option>
            </select>
          </div>
          {state.authType === 'oauth' ? (
            <p className="text-sm text-text-muted">
              After creation, Kyber will show a device code from <code>codex login --device-auth</code>.
              Open the displayed URL, enter the code, and the agent will start automatically.
            </p>
          ) : (
            <div>
              <label htmlFor="agent-openai-key" className={labelClass}>OpenAI API key</label>
              <input
                id="agent-openai-key"
                type="password"
                required
                value={state.openaiApiKey}
                onChange={(e) => set('openaiApiKey', e.target.value)}
                placeholder="sk-…"
                className={inputClass}
              />
              <p className="mt-1.5 text-xs text-text-muted">
                Stored as a Kubernetes Secret and injected only into this agent.
              </p>
            </div>
          )}
        </div>
      ) : (
      <>
      <div>
        <label htmlFor="agent-auth-type" className={labelClass}>
          Authentication
        </label>
        <select
          id="agent-auth-type"
          value={state.authType}
          onChange={(e) => {
            const val = e.target.value as 'oauth' | 'api-key'
            set('authType', val)
            // Channels require OAuth — clear them when switching to api-key.
            if (val === 'api-key') {
              set('telegramEnabled', false)
              set('discordEnabled', false)
            }
          }}
          className={inputClass}
        >
          <option value="oauth">OAuth (Claude Code subscription)</option>
          <option value="api-key">Anthropic API Key</option>
        </select>
      </div>

      {state.authType === 'oauth' && (
        <div className="space-y-3">
          <Button type="button" variant="secondary" size="md" onClick={() => void startOAuth()}>
            {state.pkceVerifier ? 'Re-authorize' : 'Open Anthropic login'}
          </Button>
          <p className="text-xs text-text-muted">
            Opens Anthropic in a new tab. Sign in and Authorize. Anthropic
            will show a page with your authorization code — copy that code
            and paste below.
          </p>
          {state.pkceVerifier && (
            <div>
              <label htmlFor="agent-oauth-code" className={labelClass}>
                Paste authorization code
              </label>
              <input
                id="agent-oauth-code"
                type="text"
                required
                value={state.oauthCode}
                onChange={(e) => set('oauthCode', e.target.value)}
                placeholder="code#state or full callback URL"
                className={inputClass}
              />
            </div>
          )}
        </div>
      )}

      {state.authType === 'api-key' && (
        <div>
          <label htmlFor="agent-anthropic-key" className={labelClass}>
            Anthropic API Key
          </label>
          <input
            id="agent-anthropic-key"
            type="password"
            value={state.anthropicApiKey}
            onChange={(e) => set('anthropicApiKey', e.target.value)}
            placeholder="sk-ant-…"
            className={inputClass}
          />
          <p className="mt-1.5 text-xs text-text-muted">
            From console.anthropic.com. Stored as a k8s Secret.
          </p>
        </div>
      )}
      </>
      )}

      {/* Channel picker: extensible per the CHANNELS table. Every channel is
          off by default — most agents want neither, and Discord in particular
          needs a bot that already exists, so its fields stay collapsed until
          asked for. Both can also be configured later from the Comms tab. */}
      {state.authType === 'oauth' && (
        <div className="space-y-3">
          {CHANNELS.map((ch) => {
            const enabled = state[ch.enabledKey]
            return (
              <div key={ch.id}>
                <label className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={enabled}
                    onChange={(e) => set(ch.enabledKey, e.target.checked)}
                  />
                  <span>{ch.label}</span>
                </label>
                {enabled && ch.blurb && (
                  <p className="mt-1.5 text-xs text-text-muted">{ch.blurb}</p>
                )}
                {enabled &&
                  ch.fields.map((f) =>
                    f.kind === 'toggle' ? (
                      <label
                        key={f.name}
                        className="mt-2 flex items-start gap-2 text-sm text-text-primary"
                      >
                        <input
                          type="checkbox"
                          className="mt-0.5"
                          checked={state[f.name]}
                          onChange={(e) => set(f.name, e.target.checked)}
                        />
                        <span>
                          {f.label}
                          {f.helperText && (
                            <span className="mt-0.5 block text-xs text-text-muted">
                              {f.helperText}
                            </span>
                          )}
                        </span>
                      </label>
                    ) : (
                      <div key={f.name} className="mt-2">
                        <label htmlFor={`agent-${f.name}`} className={labelClass}>
                          {f.label}
                        </label>
                        <input
                          id={`agent-${f.name}`}
                          type={f.kind === 'secret' ? 'password' : 'text'}
                          required={f.required}
                          value={state[f.name]}
                          onChange={(e) => set(f.name, e.target.value)}
                          placeholder={f.placeholder}
                          className={inputClass}
                        />
                        {f.helperText && (
                          <p className="mt-1.5 text-xs text-text-muted">
                            {f.helperText}
                          </p>
                        )}
                      </div>
                    ),
                  )}
              </div>
            )
          })}
        </div>
      )}
    </section>
  )
}
