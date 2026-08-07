import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AuthSection } from './AuthSection'
import { initialWizardState } from './types'

describe('AuthSection', () => {
  it('switching auth type to api-key clears telegramEnabled', async () => {
    const user = userEvent.setup()
    const set = vi.fn()
    render(
      <AuthSection
        state={{ ...initialWizardState([]), telegramEnabled: true }}
        set={set}
      />,
    )
    const authSelect = screen.getByLabelText('Authentication')
    await user.selectOptions(authSelect, 'api-key')
    // Verify both setter calls fired (order may vary).
    const calls = set.mock.calls.map((c) => c[0])
    expect(calls).toContain('authType')
    expect(calls).toContain('telegramEnabled')
    const telegramCall = set.mock.calls.find((c) => c[0] === 'telegramEnabled')
    expect(telegramCall?.[1]).toBe(false)
  })

  it('renders the API-key input only when authType === "api-key"', () => {
    const { rerender } = render(
      <AuthSection state={initialWizardState([])} set={vi.fn()} />,
    )
    expect(screen.queryByLabelText(/anthropic api key/i)).not.toBeInTheDocument()

    rerender(
      <AuthSection
        state={{ ...initialWizardState([]), authType: 'api-key' }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByLabelText(/anthropic api key/i)).toBeInTheDocument()
  })

  it('uses device login by default for Codex and never asks for auth.json', () => {
    render(
      <AuthSection
        state={{ ...initialWizardState([]), runtime: 'codex' }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByText(/codex login --device-auth/i)).toBeInTheDocument()
    expect(screen.queryByText(/auth\.json/i)).not.toBeInTheDocument()
  })

  it('shows an OpenAI key field for Codex api-key mode', () => {
    render(
      <AuthSection
        state={{ ...initialWizardState([]), runtime: 'codex', authType: 'api-key' }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByLabelText(/openai api key/i)).toBeInTheDocument()
    expect(screen.queryByLabelText(/anthropic api key/i)).not.toBeInTheDocument()
  })

  it('renders the Telegram bot-token input only when telegramEnabled and authType === "oauth"', () => {
    const { rerender } = render(
      <AuthSection
        state={{ ...initialWizardState([]), telegramEnabled: false }}
        set={vi.fn()}
      />,
    )
    expect(screen.queryByLabelText(/telegram bot token/i)).not.toBeInTheDocument()

    rerender(
      <AuthSection
        state={{ ...initialWizardState([]), telegramEnabled: true }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByLabelText(/telegram bot token/i)).toBeInTheDocument()
  })

  // ---- Discord (kyber#664) ----

  it('keeps Discord collapsed until it is checked', () => {
    const { rerender } = render(
      <AuthSection state={initialWizardState([])} set={vi.fn()} />,
    )
    // The checkbox is offered, but none of its six fields cost the user
    // any scrolling until they ask for it.
    expect(screen.getByText('Discord')).toBeInTheDocument()
    expect(screen.queryByLabelText(/discord bot token/i)).not.toBeInTheDocument()

    rerender(
      <AuthSection
        state={{ ...initialWizardState([]), discordEnabled: true }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByLabelText(/discord bot token/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/who can talk to it/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/only when mentioned/i)).toBeInTheDocument()
  })

  it('says Discord can be set up later, since it needs a bot that already exists', () => {
    render(
      <AuthSection
        state={{ ...initialWizardState([]), discordEnabled: true }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByText(/Comms tab/i)).toBeInTheDocument()
  })

  it('switching auth type to api-key clears Discord too', async () => {
    const user = userEvent.setup()
    const set = vi.fn()
    render(
      <AuthSection
        state={{ ...initialWizardState([]), discordEnabled: true }}
        set={set}
      />,
    )
    await user.selectOptions(screen.getByLabelText('Authentication'), 'api-key')
    const discordCall = set.mock.calls.find((c) => c[0] === 'discordEnabled')
    expect(discordCall?.[1]).toBe(false)
  })

  it('routes the mention-only toggle through the boolean setter, not the text one', async () => {
    const user = userEvent.setup()
    const set = vi.fn()
    render(
      <AuthSection
        state={{ ...initialWizardState([]), discordEnabled: true }}
        set={set}
      />,
    )
    await user.click(screen.getByLabelText(/only when mentioned/i))
    const call = set.mock.calls.find((c) => c[0] === 'discordMentionOnly')
    expect(call?.[1]).toBe(true)
  })

  it('does not render the authorization-code input until pkceVerifier is set', () => {
    const { rerender } = render(
      <AuthSection state={initialWizardState([])} set={vi.fn()} />,
    )
    expect(screen.queryByLabelText(/paste authorization code/i)).not.toBeInTheDocument()

    rerender(
      <AuthSection
        state={{ ...initialWizardState([]), pkceVerifier: 'abc123' }}
        set={vi.fn()}
      />,
    )
    expect(screen.getByLabelText(/paste authorization code/i)).toBeInTheDocument()
  })
})
