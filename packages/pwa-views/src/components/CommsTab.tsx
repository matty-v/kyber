// Comms tab for AgentDetail (kyber#664) — configure the channels a human uses
// to reach this agent, and it uses to answer.
//
// One card per channel. Scope is Telegram and two-way Discord; the older
// outbound-only Discord webhook is a different, one-way thing and is
// deliberately not surfaced here.
//
// Every save is a PUT to /api/v1/agents/{name}/comms/{channel}, which does the
// whole job in one call — including generating the HMAC secret that the manual
// setup makes a human copy between two places, where a mismatch fails silently.

import { useEffect, useState } from 'react'
import { MessageSquare, RotateCw, Send, Trash2 } from 'lucide-react'
import {
  useAgentComms,
  useDeleteAgentComms,
  usePutDiscordComms,
  usePutTelegramComms,
} from '../hooks/useAPI'
import type { CommsChannel } from '../lib/types'
import { Button } from './Button'
import { Card } from './Card'
import { ConfirmDialog } from './ConfirmDialog'

// Matches discordSnowflakeRe in pkg/api/routes_agent_comms.go. Validating here
// too turns a round-trip into an inline hint, and catches the usual paste error
// — a channel *name* or a URL instead of an ID.
const SNOWFLAKE_RE = /^[0-9]{5,25}$/

const inputClass =
  'w-full rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-text-primary placeholder:text-text-muted focus:outline-none focus:ring-1 focus:ring-accent'
const labelClass = 'block text-xs font-medium text-text-muted mb-1'

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : 'Unknown error'
}

/** Parses the comma/newline/space-separated ID lists the form collects. */
export function parseIdList(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean)
}

/** Returns the first entry that isn't a Discord snowflake, or null. */
export function firstInvalidId(ids: string[]): string | null {
  return ids.find((id) => !SNOWFLAKE_RE.test(id)) ?? null
}

interface Props {
  agentName: string
  /** Opens the existing Restart pod flow, so "apply this" is one click. */
  onRestartPod?: () => void
}

export function CommsTab({ agentName, onRestartPod }: Props) {
  const { data: channels, isLoading, error, refetch } = useAgentComms(agentName)

  const telegram = channels?.find((c) => c.channel === 'telegram')
  const discord = channels?.find((c) => c.channel === 'discord')

  return (
    <div className="space-y-3">
      {isLoading && <Card className="text-sm text-text-muted">Loading channels…</Card>}

      {error && (
        <Card className="border-danger/40 bg-danger-muted text-sm text-danger">
          Failed to load channels: {errorMessage(error)}
          <div className="mt-2">
            <Button variant="ghost" size="sm" onClick={() => void refetch()}>
              Retry
            </Button>
          </div>
        </Card>
      )}

      {!isLoading && !error && (
        <>
          <TelegramCard agentName={agentName} channel={telegram} onRestartPod={onRestartPod} />
          <DiscordCard agentName={agentName} channel={discord} onRestartPod={onRestartPod} />
        </>
      )}
    </div>
  )
}

/**
 * Shown when the saved config is newer than the running pod. Kyber never rolls
 * the pod itself: the sidecar is injected at pod-build time and the Telegram
 * token is injected as env, so applying a change means a new pod — and that
 * destroys the agent's live session. The operator decides when to pay that.
 */
function RestartNotice({ onRestartPod }: { onRestartPod?: () => void }) {
  return (
    <div className="mt-3 flex items-center gap-2 rounded-md border border-warn/40 bg-warn-muted px-2.5 py-2">
      <RotateCw className="h-3.5 w-3.5 shrink-0 text-warn" />
      <span className="text-xs text-warn">
        Saved, but not live yet — the agent picks this up on its next pod.
        Restarting ends its current session.
      </span>
      {onRestartPod && (
        <Button variant="ghost" size="sm" className="ml-auto shrink-0" onClick={onRestartPod}>
          Restart pod
        </Button>
      )}
    </div>
  )
}

function ChannelHeader({
  icon,
  title,
  configured,
  description,
}: {
  icon: React.ReactNode
  title: string
  configured: boolean
  description: string
}) {
  return (
    <div className="mb-3 flex items-start gap-2.5">
      <div className="mt-0.5 text-text-muted">{icon}</div>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <h3 className="text-sm font-semibold text-text-primary">{title}</h3>
          <span
            className={
              configured
                ? 'rounded-full bg-ok-muted px-1.5 py-0.5 text-[10px] font-medium text-ok'
                : 'rounded-full bg-surface-muted px-1.5 py-0.5 text-[10px] font-medium text-text-muted'
            }
          >
            {configured ? 'On' : 'Off'}
          </span>
        </div>
        <p className="mt-0.5 text-xs text-text-muted">{description}</p>
      </div>
    </div>
  )
}

// ---- Telegram -------------------------------------------------------------

function TelegramCard({
  agentName,
  channel,
  onRestartPod,
}: {
  agentName: string
  channel?: CommsChannel
  onRestartPod?: () => void
}) {
  const put = usePutTelegramComms()
  const del = useDeleteAgentComms()
  const [botToken, setBotToken] = useState('')
  const [users, setUsers] = useState('')
  const [confirmOff, setConfirmOff] = useState(false)
  const [localError, setLocalError] = useState<string | null>(null)

  useEffect(() => setUsers((channel?.allowedUserIds ?? []).join(', ')), [channel])

  const configured = channel?.configured ?? false
  const tokenStored = channel?.botTokenSet ?? false

  function save() {
    const allowedUserIds = parseIdList(users)
    // Fail-closed in the sidecar: with no allowlist it refuses to start rather
    // than answer strangers. Say so — this used to `return` silently, so the
    // Save button did nothing and gave no reason. Matches the Discord card.
    if (allowedUserIds.length === 0) {
      setLocalError('Add at least one Telegram user ID — otherwise nobody can talk to the agent.')
      return
    }
    setLocalError(null)
    put.mutate(
      { name: agentName, body: { ...(botToken ? { botToken } : {}), allowedUserIds } },
      { onSuccess: () => setBotToken('') },
    )
  }

  return (
    <Card>
      <ChannelHeader
        icon={<Send className="h-4 w-4" strokeWidth={1.5} />}
        title="Telegram"
        configured={configured}
        description="Two-way. The agent reads and replies through a Telegram bot."
      />

      <div className="space-y-3">
          <div>
            <label htmlFor="comms-telegram-token" className={labelClass}>
              Bot token
            </label>
            <input
              id="comms-telegram-token"
              type="password"
              value={botToken}
              onChange={(e) => setBotToken(e.target.value)}
              placeholder={tokenStored ? '•••••••• (stored — type to replace)' : '0000000000:ABC…'}
              className={inputClass}
              autoComplete="off"
            />
            <p className="mt-1 text-xs text-text-muted">
              From @BotFather.{' '}
              {tokenStored
                ? 'Leave blank to keep the stored token; enter a new one to replace it.'
                : 'Stored as a Kubernetes Secret and never shown again.'}
            </p>
          </div>

          <div>
            <label htmlFor="comms-telegram-users" className={labelClass}>Who can talk to it</label>
            <input
              id="comms-telegram-users"
              value={users}
              onChange={(e) => setUsers(e.target.value)}
              placeholder="1000000001, …"
              className={inputClass}
            />
            <p className="mt-1 text-xs text-text-muted">
              Numeric Telegram user IDs, comma-separated. Ask @userinfobot for yours. Anyone not listed is ignored.
            </p>
          </div>

          {(localError || put.error) && (
            <p className="text-xs text-danger">{localError ?? errorMessage(put.error)}</p>
          )}

          <div className="flex items-center gap-2">
            <Button
              variant="primary"
              size="sm"
			  disabled={put.isPending || (!botToken && !tokenStored) || parseIdList(users).length === 0}
              onClick={save}
            >
              {put.isPending ? 'Saving…' : configured ? 'Save' : 'Enable Telegram'}
            </Button>
            {configured && (
              <Button
                variant="ghost"
                size="sm"
                disabled={del.isPending}
                onClick={() => setConfirmOff(true)}
              >
                <Trash2 className="h-3.5 w-3.5" /> Turn off
              </Button>
            )}
          </div>

        {channel?.podRestartRequired && <RestartNotice onRestartPod={onRestartPod} />}
      </div>

      <ConfirmDialog
        open={confirmOff}
        title="Turn off Telegram?"
        message="The agent stops using Telegram and its stored bot token is deleted. You'll need the token again to turn it back on."
        confirmLabel="Turn off"
        dangerous
        loading={del.isPending}
        onCancel={() => setConfirmOff(false)}
        onConfirm={() => {
          del.mutate(
            { name: agentName, channel: 'telegram' },
            { onSuccess: () => setConfirmOff(false) },
          )
        }}
      />
    </Card>
  )
}

function DiscordConnectionDiagnostics({
  connection,
}: {
  connection: NonNullable<CommsChannel['discordConnection']>
}) {
  const labels: Record<typeof connection.status, string> = {
    'not-configured': 'Not configured',
    'restart-required': 'Restart required',
    'not-running': 'Agent not running',
    starting: 'Connecting…',
    connected: 'Connected',
    degraded: 'Connection problem',
  }
  const healthy = connection.status === 'connected'
  return (
    <div className="rounded-md border border-border-subtle bg-surface-secondary px-3 py-2 text-xs">
      <div className="flex items-center justify-between gap-3">
        <span className="font-medium text-text-primary">Discord connection</span>
        <span className={healthy ? 'text-success' : 'text-text-muted'}>{labels[connection.status]}</span>
      </div>
      {connection.detail && <p className="mt-1 text-text-muted">{connection.detail}</p>}
      {connection.restartCount > 0 && (
        <p className="mt-1 text-text-muted">
          Sidecar restarted {connection.restartCount} {connection.restartCount === 1 ? 'time' : 'times'}.
        </p>
      )}
    </div>
  )
}

// ---- Discord --------------------------------------------------------------

function DiscordCard({
  agentName,
  channel,
  onRestartPod,
}: {
  agentName: string
  channel?: CommsChannel
  onRestartPod?: () => void
}) {
  const put = usePutDiscordComms()
  const del = useDeleteAgentComms()

  const configured = channel?.configured ?? false
  const tokenStored = channel?.botTokenSet ?? false

  const [botToken, setBotToken] = useState('')
  const [guilds, setGuilds] = useState('')
  const [channelIds, setChannelIds] = useState('')
  const [users, setUsers] = useState('')
  const [mentionOnly, setMentionOnly] = useState(false)
  const [localError, setLocalError] = useState<string | null>(null)
  const [confirmOff, setConfirmOff] = useState(false)

  // Seed the form from the server's config once it arrives, and re-seed when it
  // changes underneath us (another operator, or our own save landing).
  useEffect(() => {
    if (!channel) return
    setGuilds((channel.guildIds ?? []).join(', '))
    setChannelIds((channel.channelIds ?? []).join(', '))
    setUsers((channel.allowedUserIds ?? []).join(', '))
    setMentionOnly(channel.mentionOnly ?? false)
  }, [channel])

  function save() {
    const guildIds = parseIdList(guilds)
    const chanIds = parseIdList(channelIds)
    const allowedUserIds = parseIdList(users)

    // Fail-closed in the sidecar: an empty allowlist means nobody can reach the
    // agent, so it's a mistake rather than a valid state.
    if (allowedUserIds.length === 0) {
      setLocalError('Add at least one Discord user ID — otherwise nobody can talk to the agent.')
      return
    }
    const bad = firstInvalidId([...guildIds, ...chanIds, ...allowedUserIds])
    if (bad) {
      setLocalError(
        `"${bad}" isn't a Discord ID. Turn on Developer Mode in Discord, then right-click → Copy ID.`,
      )
      return
    }
    setLocalError(null)

    put.mutate(
      {
        name: agentName,
        body: {
          ...(botToken ? { botToken } : {}),
          guildIds,
          channelIds: chanIds,
          allowedUserIds,
          mentionOnly,
        },
      },
      { onSuccess: () => setBotToken('') },
    )
  }

  return (
    <Card>
      <ChannelHeader
        icon={<MessageSquare className="h-4 w-4" strokeWidth={1.5} />}
        title="Discord"
        configured={configured}
        description="Two-way. The agent listens in a Discord channel and replies there."
      />

      <div className="space-y-3">
        <div>
          <label htmlFor="comms-discord-token" className={labelClass}>
            Bot token
          </label>
          <input
            id="comms-discord-token"
            type="password"
            value={botToken}
            onChange={(e) => setBotToken(e.target.value)}
            placeholder={tokenStored ? '•••••••• (stored — type to replace)' : 'Bot token'}
            className={inputClass}
            autoComplete="off"
          />
          <p className="mt-1 text-xs text-text-muted">
            From the Discord Developer Portal. The bot needs Message Content
            Intent turned on, or every message arrives blank.
          </p>
        </div>

        <div>
          <label htmlFor="comms-discord-users" className={labelClass}>
            Who can talk to it
          </label>
          <input
            id="comms-discord-users"
            type="text"
            value={users}
            onChange={(e) => setUsers(e.target.value)}
            placeholder="123456789012345678, …"
            className={inputClass}
          />
          <p className="mt-1 text-xs text-text-muted">
            Discord user IDs, comma-separated. Required — anyone not listed is
            ignored.
          </p>
        </div>

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div>
            <label htmlFor="comms-discord-guilds" className={labelClass}>
              Servers
            </label>
            <input
              id="comms-discord-guilds"
              type="text"
              value={guilds}
              onChange={(e) => setGuilds(e.target.value)}
              placeholder="Any server"
              className={inputClass}
            />
          </div>
          <div>
            <label htmlFor="comms-discord-channels" className={labelClass}>
              Channels
            </label>
            <input
              id="comms-discord-channels"
              type="text"
              value={channelIds}
              onChange={(e) => setChannelIds(e.target.value)}
              placeholder="Any channel"
              className={inputClass}
            />
          </div>
        </div>
        <p className="text-xs text-text-muted">
          Leave blank for any. To get an ID: Discord Settings → Advanced →
          Developer Mode, then right-click → Copy ID.
        </p>

        <label className="flex items-start gap-2 text-sm text-text-primary">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={mentionOnly}
            onChange={(e) => setMentionOnly(e.target.checked)}
          />
          <span>
            Only when mentioned
            <span className="mt-0.5 block text-xs text-text-muted">
              Turn this on for a channel where people also talk to each other —
              otherwise every side conversation costs the agent a turn.
            </span>
          </span>
        </label>

        {(localError || put.error) && (
          <p className="text-xs text-danger">{localError ?? errorMessage(put.error)}</p>
        )}

        <div className="flex items-center gap-2">
          <Button
            variant="primary"
            size="sm"
            disabled={put.isPending || (!botToken && !tokenStored)}
            onClick={save}
          >
            {put.isPending ? 'Saving…' : configured ? 'Save' : 'Enable Discord'}
          </Button>
          {configured && (
            <Button
              variant="ghost"
              size="sm"
              disabled={del.isPending}
              onClick={() => setConfirmOff(true)}
            >
              <Trash2 className="h-3.5 w-3.5" /> Turn off
            </Button>
          )}
        </div>

        {channel?.podRestartRequired && <RestartNotice onRestartPod={onRestartPod} />}
        {configured && channel?.discordConnection && (
          <DiscordConnectionDiagnostics connection={channel.discordConnection} />
        )}
      </div>

      <ConfirmDialog
        open={confirmOff}
        title="Turn off Discord?"
        message="The agent stops listening in Discord and its bot token is deleted. Any outbound-only Discord webhook on this agent keeps working."
        confirmLabel="Turn off"
        dangerous
        loading={del.isPending}
        onCancel={() => setConfirmOff(false)}
        onConfirm={() => {
          del.mutate(
            { name: agentName, channel: 'discord' },
            { onSuccess: () => setConfirmOff(false) },
          )
        }}
      />
    </Card>
  )
}
