import { Button } from '../Button'
import { generatePkcePair } from '../../lib/pkce'
import { inputClass, labelClass } from './styles'
import type { WizardSetter, WizardState } from './types'

import { wizardContract, wizardAuth, wizardApiKey } from '../../lib/runtime-contract'

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
  | 'slackBotToken'
  | 'slackAppToken'
  | 'slackAllowedUserIds'
  | 'slackAllowedChannelIds'
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
  id: 'telegram' | 'discord' | 'slack'
  label: string
  /** Maps to a boolean key on WizardState. */
  enabledKey: 'telegramEnabled' | 'discordEnabled' | 'slackEnabled'
  /** Rendered under the checkbox before the fields — sets expectations. */
  blurb?: string
  fields: ChannelField[]
}

const CHANNELS: ChannelDef[] = [
  {
    id: 'telegram',
    label: 'Telegram',
    enabledKey: 'telegramEnabled',
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
  {
    id: 'slack',
    label: 'Slack',
    enabledKey: 'slackEnabled',
    blurb: 'Needs a Slack app with Socket Mode enabled. You can also set this up later from the agent’s Comms tab.',
    fields: [
      { kind: 'secret', name: 'slackBotToken', required: true, label: 'Slack bot token', placeholder: 'xoxb-…', helperText: 'OAuth token from your Slack app. Stored as a k8s Secret.' },
      { kind: 'secret', name: 'slackAppToken', required: true, label: 'Slack app-level token', placeholder: 'xapp-…', helperText: 'Requires Socket Mode and connections:write.' },
      { kind: 'text', name: 'slackAllowedUserIds', required: true, label: 'Allowed user IDs', placeholder: 'U012ABC, …', helperText: 'Slack user IDs, comma-separated.' },
      { kind: 'text', name: 'slackAllowedChannelIds', required: true, label: 'Allowed channel IDs', placeholder: 'C012ABC, …', helperText: 'Slack channel IDs, comma-separated. Required for fail-closed replies.' },
    ],
  },
]

export interface AuthSectionProps {
  state: WizardState
  set: WizardSetter
}

export function AuthSection({ state, set }: AuthSectionProps) {
  const contract = wizardContract(state)
  const auth = wizardAuth(state)
  function setApiKey(value: string) {
    if (auth?.inputField === 'anthropicApiKey') set('anthropicApiKey', value)
    else if (auth?.inputField === 'openaiApiKey') set('openaiApiKey', value)
    else set('runtimeApiKey', value)
  }
  async function startOAuth() {
    const { verifier, challenge } = await generatePkcePair()
    const oauthState = crypto.randomUUID()
    set('pkceVerifier', verifier)
    set('pkceState', oauthState)
    set('oauthCode', '')
    if (!auth?.authorizationUrl) return
    const url = new URL(auth.authorizationUrl)
    for (const [key, value] of Object.entries(auth.authorizationParams ?? {})) url.searchParams.set(key, value)
    url.searchParams.set('code_challenge', challenge)
    url.searchParams.set('code_challenge_method', 'S256')
    url.searchParams.set('state', oauthState)
    window.open(url.toString(), '_blank', 'noopener')
  }

  return (
    <section className="space-y-5">
      <div>
        <label htmlFor="agent-auth-type" className={labelClass}>Authentication</label>
        <select id="agent-auth-type" value={state.authType} className={inputClass} onChange={e => {
          const mode = e.target.value as 'oauth' | 'api-key'
          set('authType', mode)
          const channels = contract?.authModes.find(candidate => candidate.id === mode)?.channels ?? (mode === 'oauth' ? ['telegram', 'discord', 'slack'] : ['slack'])
          for (const channel of CHANNELS) {
            if (!channels.includes(channel.id)) set(channel.enabledKey, false)
          }
        }}>
          {contract?.authModes.map(mode => <option key={mode.id} value={mode.id}>{mode.name}</option>)}
        </select>
      </div>
      {!auth && <p role="alert">Authentication is unavailable for this harness.</p>}
      {auth?.flow === 'device-code' && <p className="text-sm text-text-muted">After creation, Kyber will show a device code. Open the displayed URL, enter the code, and the agent will start automatically.</p>}
      {auth?.flow === 'api-key' && <div>
        <label htmlFor="agent-api-key" className={labelClass}>{auth.name}</label>
        <input id="agent-api-key" type="password" required value={wizardApiKey(state)} onChange={e => setApiKey(e.target.value)} className={inputClass} />
        <p className="mt-1.5 text-xs text-text-muted">Stored as a Secret and injected only into this agent.</p>
      </div>}
      {auth?.flow === 'authorization-code' && <div className="space-y-3">
        <Button type="button" variant="secondary" size="md" disabled={!auth.authorizationUrl} onClick={() => void startOAuth()}>{state.pkceVerifier ? 'Re-authorize' : `Open ${contract?.name ?? 'provider'} login`}</Button>
        <p className="text-xs text-text-muted">Open the login page, sign in and authorize, then paste the authorization code below.</p>
        {state.pkceVerifier && <div>
          <label htmlFor="agent-oauth-code" className={labelClass}>Paste authorization code</label>
          <input id="agent-oauth-code" type="text" required value={state.oauthCode} onChange={e => set('oauthCode', e.target.value)} placeholder="code#state or full callback URL" className={inputClass} />
        </div>}
      </div>}


      {/* Channel picker: extensible per the CHANNELS table. Every channel is
          off by default — most agents want neither, and Discord in particular
          needs a bot that already exists, so its fields stay collapsed until
          asked for. Both can also be configured later from the Comms tab. */}
      {(
        <div className="space-y-3">
          {CHANNELS.filter((ch) => (auth?.channels ?? (state.authType === 'oauth' ? ['telegram', 'discord', 'slack'] : ['slack'])).includes(ch.id)).map((ch) => {
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
