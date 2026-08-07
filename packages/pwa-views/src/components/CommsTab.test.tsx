// CommsTab tests (kyber#664). The data hooks are mocked so the tests assert
// what the operator sees and what payload a save produces — not the network.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

vi.mock('../hooks/useAPI', () => ({
  useAgentComms: vi.fn(),
  usePutTelegramComms: vi.fn(),
  usePutDiscordComms: vi.fn(),
  useDeleteAgentComms: vi.fn(),
}))

import * as useAPIModule from '../hooks/useAPI'
import { CommsTab, firstInvalidId, parseIdList } from './CommsTab'
import type { CommsChannel } from '../lib/types'

function renderWithQuery(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>)
}

function newMutationMock() {
  return { mutate: vi.fn(), isPending: false, error: null }
}

const OFF: CommsChannel[] = [
  { channel: 'telegram', configured: false, podRestartRequired: false, botTokenSet: false },
  { channel: 'discord', configured: false, podRestartRequired: false, botTokenSet: false },
]

function setupMocks(channels: CommsChannel[] = OFF) {
  const telegram = newMutationMock()
  const discord = newMutationMock()
  const del = newMutationMock()

  vi.mocked(useAPIModule.useAgentComms).mockReturnValue({
    data: channels,
    isLoading: false,
    error: null,
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useAPIModule.useAgentComms>)
  vi.mocked(useAPIModule.usePutTelegramComms).mockReturnValue(
    telegram as unknown as ReturnType<typeof useAPIModule.usePutTelegramComms>,
  )
  vi.mocked(useAPIModule.usePutDiscordComms).mockReturnValue(
    discord as unknown as ReturnType<typeof useAPIModule.usePutDiscordComms>,
  )
  vi.mocked(useAPIModule.useDeleteAgentComms).mockReturnValue(
    del as unknown as ReturnType<typeof useAPIModule.useDeleteAgentComms>,
  )
  return { telegram, discord, del }
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('parseIdList / firstInvalidId', () => {
  it('splits on commas, spaces, and newlines, dropping empties', () => {
    expect(parseIdList(' 123456,  789012 \n345678, ')).toEqual(['123456', '789012', '345678'])
    expect(parseIdList('   ')).toEqual([])
  })

  it('flags the usual paste mistakes rather than accepting them', () => {
    expect(firstInvalidId(['123456789012345678'])).toBeNull()
    expect(firstInvalidId(['123456789012345678', '#dev-bots'])).toBe('#dev-bots')
    expect(firstInvalidId(['https://discord.com/channels/123'])).toBe(
      'https://discord.com/channels/123',
    )
  })
})

describe('CommsTab', () => {
  it('shows both channels off for a fresh agent', () => {
    setupMocks()
    renderWithQuery(<CommsTab agentName="dave" />)

    expect(screen.getByText('Telegram')).toBeInTheDocument()
    expect(screen.getByText('Discord')).toBeInTheDocument()
    expect(screen.getAllByText('Off')).toHaveLength(2)
    // Nothing to turn off yet.
    expect(screen.queryByRole('button', { name: /turn off/i })).not.toBeInTheDocument()
  })

  it('does not surface the legacy outbound-only Discord webhook', () => {
    setupMocks()
    renderWithQuery(<CommsTab agentName="dave" />)
    // Scope is Telegram + two-way Discord. A third "webhook" channel card
    // would mean the legacy path leaked into this surface.
    expect(screen.queryByText(/webhook/i)).not.toBeInTheDocument()
  })

  it('saves Telegram with the entered bot token', async () => {
    const user = userEvent.setup()
    const { telegram } = setupMocks()
    renderWithQuery(<CommsTab agentName="dave" />)

    await user.type(screen.getByLabelText('Bot token', { selector: '#comms-telegram-token' }), '123:abc')
    await user.type(screen.getByLabelText(/who can talk to it/i, { selector: '#comms-telegram-users' }), '1000000001')
    await user.click(screen.getByRole('button', { name: /enable telegram/i }))

    expect(telegram.mutate).toHaveBeenCalledWith(
      { name: 'dave', body: { botToken: '123:abc', allowedUserIds: ['1000000001'] } },
      expect.anything(),
    )
  })

  it('lets a configured Telegram be saved without re-entering the stored token', async () => {
    const user = userEvent.setup()
    const { telegram } = setupMocks([
      { channel: 'telegram', configured: true, podRestartRequired: false, botTokenSet: true, allowedUserIds: ['1000000001'] },
      OFF[1],
    ])
    renderWithQuery(<CommsTab agentName="dave" />)

    await user.click(screen.getByRole('button', { name: /^save$/i }))

    expect(telegram.mutate).toHaveBeenCalledWith(
      { name: 'dave', body: { allowedUserIds: ['1000000001'] } },
      expect.anything(),
    )
  })

  it('refuses to save Discord with an empty user allowlist', async () => {
    const user = userEvent.setup()
    const { discord } = setupMocks()
    renderWithQuery(<CommsTab agentName="dave" />)

    await user.type(screen.getByLabelText('Bot token', { selector: '#comms-discord-token' }), 'tok')
    await user.click(screen.getByRole('button', { name: /enable discord/i }))

    // An empty allowlist is fail-closed — the agent would hear nobody.
    expect(discord.mutate).not.toHaveBeenCalled()
    expect(screen.getByText(/at least one Discord user ID/i)).toBeInTheDocument()
  })

  it('rejects an ID that is not a Discord snowflake, and says how to get one', async () => {
    const user = userEvent.setup()
    const { discord } = setupMocks()
    renderWithQuery(<CommsTab agentName="dave" />)

    await user.type(screen.getByLabelText('Bot token', { selector: '#comms-discord-token' }), 'tok')
    await user.type(screen.getByLabelText(/who can talk to it/i, { selector: '#comms-discord-users' }), '#dev-bots')
    await user.click(screen.getByRole('button', { name: /enable discord/i }))

    expect(discord.mutate).not.toHaveBeenCalled()
    // Matches the error, not the field's standing help text — both mention
    // Copy ID, and only the error names the offending value.
    expect(screen.getByText(/"#dev-bots" isn't a Discord ID/i)).toBeInTheDocument()
  })

  it('saves Discord with parsed ID lists and the mention-only toggle', async () => {
    const user = userEvent.setup()
    const { discord } = setupMocks()
    renderWithQuery(<CommsTab agentName="dave" />)

    await user.type(screen.getByLabelText('Bot token', { selector: '#comms-discord-token' }), 'tok')
    await user.type(screen.getByLabelText(/who can talk to it/i, { selector: '#comms-discord-users' }), '123456789012345678, 123456789012')
    await user.type(screen.getByLabelText(/servers/i), '234567890123456789')
    await user.click(screen.getByLabelText(/only when mentioned/i))
    await user.click(screen.getByRole('button', { name: /enable discord/i }))

    expect(discord.mutate).toHaveBeenCalledWith(
      {
        name: 'dave',
        body: {
          botToken: 'tok',
          guildIds: ['234567890123456789'],
          channelIds: [],
          allowedUserIds: ['123456789012345678', '123456789012'],
          mentionOnly: true,
        },
      },
      expect.anything(),
    )
  })

  it('seeds the Discord form from the saved config', () => {
    setupMocks([
      OFF[0],
      {
        channel: 'discord',
        configured: true,
        podRestartRequired: false,
        botTokenSet: true,
        guildIds: ['234567890123456789'],
        channelIds: ['345678901234567890'],
        allowedUserIds: ['123456789012345678'],
        mentionOnly: true,
      },
    ])
    renderWithQuery(<CommsTab agentName="barf" />)

    expect(screen.getByLabelText(/who can talk to it/i, { selector: '#comms-discord-users' })).toHaveValue('123456789012345678')
    expect(screen.getByLabelText(/servers/i)).toHaveValue('234567890123456789')
    expect(screen.getByLabelText(/only when mentioned/i)).toBeChecked()
    expect(screen.getByText('On')).toBeInTheDocument()
  })

  it('warns that a saved change is not live until the pod restarts', async () => {
    const user = userEvent.setup()
    const onRestartPod = vi.fn()
    setupMocks([
      OFF[0],
      {
        channel: 'discord',
        configured: true,
        podRestartRequired: true,
        botTokenSet: true,
        allowedUserIds: ['123456789012345678'],
      },
    ])
    renderWithQuery(<CommsTab agentName="barf" onRestartPod={onRestartPod} />)

    // The warning has to say the restart costs the session — that's the whole
    // reason Kyber doesn't roll the pod on the operator's behalf.
    expect(screen.getByText(/not live yet/i)).toBeInTheDocument()
    expect(screen.getByText(/ends its current session/i)).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /restart pod/i }))
    expect(onRestartPod).toHaveBeenCalled()
  })

  it('shows Discord connection health and restart diagnostics', () => {
    setupMocks([
      OFF[0],
      {
        channel: 'discord', configured: true, podRestartRequired: false, botTokenSet: true,
        allowedUserIds: ['123456789012345678'],
        discordConnection: {
          status: 'degraded', ready: false, restartCount: 3,
          detail: 'CrashLoopBackOff: invalid token',
        },
      },
    ])
    renderWithQuery(<CommsTab agentName="barf" />)

    expect(screen.getByText('Discord connection')).toBeInTheDocument()
    expect(screen.getByText('Connection problem')).toBeInTheDocument()
    expect(screen.getByText(/CrashLoopBackOff: invalid token/)).toBeInTheDocument()
    expect(screen.getByText(/restarted 3 times/)).toBeInTheDocument()
  })

  it('confirms before turning a channel off, then deletes it', async () => {
    const user = userEvent.setup()
    const { del } = setupMocks([
      { channel: 'telegram', configured: true, podRestartRequired: false, botTokenSet: true },
      OFF[1],
    ])
    renderWithQuery(<CommsTab agentName="dave" />)

    await user.click(screen.getByRole('button', { name: /turn off/i }))
    // Opening the dialog must not have deleted anything yet.
    expect(del.mutate).not.toHaveBeenCalled()

    const dialog = screen.getByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /^turn off$/i }))
    expect(del.mutate).toHaveBeenCalledWith(
      { name: 'dave', channel: 'telegram' },
      expect.anything(),
    )
  })

  it('surfaces a load failure with a retry', () => {
    const refetch = vi.fn()
    vi.mocked(useAPIModule.useAgentComms).mockReturnValue({
      data: undefined,
      isLoading: false,
      error: new Error('boom'),
      refetch,
    } as unknown as ReturnType<typeof useAPIModule.useAgentComms>)
    vi.mocked(useAPIModule.usePutTelegramComms).mockReturnValue(
      newMutationMock() as unknown as ReturnType<typeof useAPIModule.usePutTelegramComms>,
    )
    vi.mocked(useAPIModule.usePutDiscordComms).mockReturnValue(
      newMutationMock() as unknown as ReturnType<typeof useAPIModule.usePutDiscordComms>,
    )
    vi.mocked(useAPIModule.useDeleteAgentComms).mockReturnValue(
      newMutationMock() as unknown as ReturnType<typeof useAPIModule.useDeleteAgentComms>,
    )

    renderWithQuery(<CommsTab agentName="dave" />)
    expect(screen.getByText(/failed to load channels/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })
})
