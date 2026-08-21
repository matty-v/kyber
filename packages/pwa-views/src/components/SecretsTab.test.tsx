// SecretsTab tests — covers both the empty-state and populated-state paths.
// The data-fetching hooks (useAgentSecrets, useDeleteAgentSecret, etc.) are
// mocked so tests are deterministic and don't need a live server.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { TooltipProvider } from './ui/tooltip'

// ---- mock the hooks module ----
// We hoist vi.mock so vitest can rewrite the import before SecretsTab loads.
vi.mock('../hooks/useAPI', () => ({
  useAgentSecrets: vi.fn(),
  useDeleteAgentSecret: vi.fn(),
  useImportAgentSecretsKV: vi.fn(),
  usePutAgentSecretKV: vi.fn(),
  usePutAgentSecretFile: vi.fn(),
}))

// Import mocks after vi.mock declaration so the factory has already run.
import * as useAPIModule from '../hooks/useAPI'
import { SecretsTab } from './SecretsTab'
import type { AgentSecret } from '../lib/types'

// ---- helpers ----

function makeQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } })
}

function renderWithQuery(ui: React.ReactElement) {
  const queryClient = makeQueryClient()
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>{ui}</TooltipProvider>
    </QueryClientProvider>,
  )
}

// Each call returns a fresh mutation mock with its own vi.fn() so per-test
// assertions on a specific mutation's mock.calls don't bleed across mutations
// or across tests. Sharing a single vi.fn() across the three mutation hooks
// would make any `expect(deleteSecret.mutate).toHaveBeenCalledWith(...)` style
// assertion non-deterministic.
function newMutationMock() {
  return { mutate: vi.fn(), isPending: false }
}

// `as unknown as ReturnType<...>` per pwa/CLAUDE.md — React Query's
// UseQueryResult/UseMutationResult are too wide for partial fixtures.
function setupMocks(secrets: AgentSecret[] | null, loading = false, error: Error | null = null) {
  vi.mocked(useAPIModule.useAgentSecrets).mockReturnValue({
    data: secrets ?? undefined,
    isLoading: loading,
    error: error ?? null,
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useAPIModule.useAgentSecrets>)
  vi.mocked(useAPIModule.useDeleteAgentSecret).mockReturnValue(newMutationMock() as unknown as ReturnType<typeof useAPIModule.useDeleteAgentSecret>)
  const importMutation = newMutationMock()
  vi.mocked(useAPIModule.useImportAgentSecretsKV).mockReturnValue(importMutation as unknown as ReturnType<typeof useAPIModule.useImportAgentSecretsKV>)
  vi.mocked(useAPIModule.usePutAgentSecretKV).mockReturnValue(newMutationMock() as unknown as ReturnType<typeof useAPIModule.usePutAgentSecretKV>)
  vi.mocked(useAPIModule.usePutAgentSecretFile).mockReturnValue(newMutationMock() as unknown as ReturnType<typeof useAPIModule.usePutAgentSecretFile>)
  return { importMutation }
}

// ---- tests ----

describe('SecretsTab — empty state', () => {
  beforeEach(() => {
    setupMocks([])
  })

  it('shows the EmptyState with "Add secret" CTA and opens dialog on click', async () => {
    const user = userEvent.setup()
    renderWithQuery(<SecretsTab agentName="my-agent" />)

    // CTA button exists in the empty state
    const ctaButton = screen.getByRole('button', { name: /add secret/i })
    expect(ctaButton).toBeInTheDocument()

    // Clicking it opens the "Add secret" dialog
    await user.click(ctaButton)
    expect(screen.getByRole('dialog', { name: /add secret/i })).toBeInTheDocument()
  })

  it('imports a key=value file as individual kv entries', async () => {
    const { importMutation } = setupMocks([])
    const user = userEvent.setup()
    const { container } = renderWithQuery(<SecretsTab agentName="my-agent" />)

    await user.click(screen.getByRole('button', { name: /add secret/i }))
    await user.click(screen.getByRole('button', { name: /key=value file/i }))

    const input = container.querySelector('input[type="file"]') as HTMLInputElement
    await user.upload(input, new File(['FOO=bar\nTOKEN=a=b'], 'secrets.env', { type: 'text/plain' }))
    await user.click(screen.getByRole('button', { name: 'Import' }))

    await waitFor(() => {
      expect(importMutation.mutate).toHaveBeenCalledWith(
        {
          name: 'my-agent',
          entries: [
            { key: 'FOO', value: 'bar' },
            { key: 'TOKEN', value: 'a=b' },
          ],
        },
        expect.any(Object),
      )
    })
  })

  it('disclosure trigger is present and toggles aria-expanded on click', async () => {
    const user = userEvent.setup()
    renderWithQuery(<SecretsTab agentName="my-agent" />)

    const trigger = screen.getByRole('button', { name: /how secrets work/i })
    expect(trigger).toBeInTheDocument()

    // Initially closed — Radix Collapsible sets data-state="closed" and applies
    // the `hidden` attribute to CollapsibleContent. RTL's queryByText respects
    // the `hidden` attribute by default and returns null for hidden elements,
    // which is why this assertion works even though jsdom doesn't compute layout.
    expect(screen.queryByText('Format')).not.toBeInTheDocument()

    await user.click(trigger)
    expect(screen.getByText('Format')).toBeInTheDocument()
    expect(screen.getByText('Updates')).toBeInTheDocument()
    expect(screen.getByText('Limits')).toBeInTheDocument()
  })
})

describe('SecretsTab — populated state', () => {
  const fakeSecrets: AgentSecret[] = [
    {
      key: 'API_TOKEN',
      kind: 'kv',
      size: 128,
      sha256Prefix: 'abc123',
      createdAt: '2026-04-01T09:00:00Z',
      updatedAt: '2026-04-01T10:00:00Z',
    },
  ]

  beforeEach(() => {
    setupMocks(fakeSecrets)
  })

  it('renders the secret row with key name', () => {
    renderWithQuery(<SecretsTab agentName="my-agent" />)
    expect(screen.getByText('API_TOKEN')).toBeInTheDocument()
  })

  it('shows the help (ⓘ) popover trigger and renders SecretsHelpContent inside it on click', async () => {
    const user = userEvent.setup()
    renderWithQuery(<SecretsTab agentName="my-agent" />)

    const helpTrigger = screen.getByRole('button', { name: /how secrets work/i })
    expect(helpTrigger).toBeInTheDocument()

    // Help content is not visible before opening the popover
    expect(screen.queryByText('Format')).not.toBeInTheDocument()

    await user.click(helpTrigger)

    // Radix Popover renders into a portal; screen queries still find it.
    expect(screen.getByText('Format')).toBeInTheDocument()
    expect(screen.getByText('Updates')).toBeInTheDocument()
    expect(screen.getByText('Limits')).toBeInTheDocument()
  })

  it('has an "Add secret" button in the populated header', () => {
    renderWithQuery(<SecretsTab agentName="my-agent" />)
    expect(screen.getByRole('button', { name: /add secret/i })).toBeInTheDocument()
  })
})
