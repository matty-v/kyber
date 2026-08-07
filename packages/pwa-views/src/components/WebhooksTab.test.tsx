// WebhooksTab tests — empty state, populated table, drop badge, delete confirm,
// expandable recent-runs panel, and replay flow (#208 Phase 3). Hooks are
// mocked so tests don't hit the network. Mirrors SecretsTab.test.tsx for
// consistency with the existing codebase pattern.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { TooltipProvider } from './ui/tooltip'

vi.mock('../hooks/useAPI', () => ({
  useAgent: vi.fn(),
  useInboundBindings: vi.fn(),
  useDeleteInboundBinding: vi.fn(),
  useRotateInboundSecret: vi.fn(),
  useCreateInboundBinding: vi.fn(),
  useUpdateInboundBinding: vi.fn(),
  useReplayInboundRun: vi.fn(),
  useInboundDebug: vi.fn(),
}))

vi.mock('sonner', () => ({
  toast: {
    success: vi.fn(),
    error: vi.fn(),
  },
}))

import { toast } from 'sonner'
import * as useAPIModule from '../hooks/useAPI'
import { WebhooksTab } from './WebhooksTab'
import type {
  Agent,
  AgentInboundRun,
  InboundBindingWithStats,
} from '../lib/types'

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

interface MutationMock {
  mutate: ReturnType<typeof vi.fn>
  mutateAsync: ReturnType<typeof vi.fn>
  isPending: boolean
}

function newMutationMock(): MutationMock {
  return { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false }
}

interface SetupOpts {
  bindings?: InboundBindingWithStats[] | null
  loading?: boolean
  error?: Error | null
  inboundRuns?: AgentInboundRun[]
  replay?: MutationMock
}

function setupMocks(opts: SetupOpts = {}) {
  const {
    bindings = [],
    loading = false,
    error = null,
    inboundRuns = [],
    replay = newMutationMock(),
  } = opts
  vi.mocked(useAPIModule.useInboundBindings).mockReturnValue({
    data: bindings ?? undefined,
    isLoading: loading,
    error: error ?? null,
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useAPIModule.useInboundBindings>)

  const fakeAgent = {
    id: 'my-agent',
    inboundRuns,
  } as unknown as Agent
  vi.mocked(useAPIModule.useAgent).mockReturnValue({
    data: fakeAgent,
    isLoading: false,
    error: null,
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useAPIModule.useAgent>)

  vi.mocked(useAPIModule.useDeleteInboundBinding).mockReturnValue(
    newMutationMock() as unknown as ReturnType<typeof useAPIModule.useDeleteInboundBinding>,
  )
  vi.mocked(useAPIModule.useRotateInboundSecret).mockReturnValue(
    newMutationMock() as unknown as ReturnType<typeof useAPIModule.useRotateInboundSecret>,
  )
  vi.mocked(useAPIModule.useCreateInboundBinding).mockReturnValue(
    newMutationMock() as unknown as ReturnType<typeof useAPIModule.useCreateInboundBinding>,
  )
  vi.mocked(useAPIModule.useUpdateInboundBinding).mockReturnValue(
    newMutationMock() as unknown as ReturnType<typeof useAPIModule.useUpdateInboundBinding>,
  )
  vi.mocked(useAPIModule.useReplayInboundRun).mockReturnValue(
    replay as unknown as ReturnType<typeof useAPIModule.useReplayInboundRun>,
  )
  vi.mocked(useAPIModule.useInboundDebug).mockReturnValue(
    newMutationMock() as unknown as ReturnType<typeof useAPIModule.useInboundDebug>,
  )
  return { replay }
}

function makeBinding(overrides: Partial<InboundBindingWithStats> = {}): InboundBindingWithStats {
  return {
    name: 'github-prs',
    existingSecret: 'github-prs-webhook',
    signatureHeader: 'X-Hub-Signature-256',
    signaturePrefix: 'sha256=',
    eventHeader: 'X-GitHub-Event',
    action: 'Triage this PR.',
    url: 'https://kyber.example.com/webhooks/inbound/my-agent/github-prs',
    stats: {
      lastReceivedAt: '2026-04-28T10:00:00Z',
      totalDispatched: 12,
      dropped: {
        'rate-limited': 0,
        'queue-full': 0,
        'sig-mismatch': 0,
        'missing-secret': 0,
        'unmatched-event': 0,
        'filter-rejected': 0,
        dedup: 0,
      },
    },
    ...overrides,
  }
}

beforeEach(() => {
  vi.mocked(toast.error).mockClear()
  vi.mocked(toast.success).mockClear()
})

describe('WebhooksTab — empty state', () => {
  beforeEach(() => {
    setupMocks({ bindings: [] })
  })

  it('renders the empty-state copy and CTA', () => {
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    expect(
      screen.getByText(/No webhooks configured/i),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /add webhook/i })).toBeInTheDocument()
  })

  it('opens the wizard dialog when "Add Webhook" is clicked', async () => {
    const user = userEvent.setup()
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    await user.click(screen.getByRole('button', { name: /add webhook/i }))
    expect(screen.getByRole('dialog', { name: /add webhook/i })).toBeInTheDocument()
  })
})

describe('WebhooksTab — populated state', () => {
  it('renders the binding row with name and stats', () => {
    setupMocks({ bindings: [makeBinding()] })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    expect(screen.getByText('github-prs')).toBeInTheDocument()
    // totalDispatched cell
    expect(screen.getByText('12')).toBeInTheDocument()
  })

  it('opens the edit wizard pre-filled with the binding when Edit is clicked (#222)', async () => {
    const user = userEvent.setup()
    setupMocks({ bindings: [makeBinding()] })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    await user.click(screen.getByRole('button', { name: /edit webhook/i }))
    expect(
      screen.getByRole('dialog', { name: /edit webhook — github-prs/i }),
    ).toBeInTheDocument()
    // Auth step is the landing — pre-filled signatureHeader is visible.
    expect(screen.getByLabelText(/signature header/i)).toHaveValue('X-Hub-Signature-256')
  })

  it('shows a drop badge when dropped total > 5', () => {
    const noisy = makeBinding({
      stats: {
        totalDispatched: 1,
        dropped: {
          'rate-limited': 4,
          'queue-full': 0,
          'sig-mismatch': 3,
          'missing-secret': 0,
          'unmatched-event': 0,
          'filter-rejected': 0,
          dedup: 0,
        },
      },
    })
    setupMocks({ bindings: [noisy] })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    // "7 drops" badge appears next to the binding name (4 + 3 = 7)
    expect(screen.getByText(/7 drops/i)).toBeInTheDocument()
  })

  it('does NOT show a drop badge when dropped total is at or below threshold', () => {
    const quiet = makeBinding({
      stats: {
        totalDispatched: 100,
        dropped: {
          'rate-limited': 2,
          'queue-full': 0,
          'sig-mismatch': 1,
          'missing-secret': 0,
          'unmatched-event': 0,
          'filter-rejected': 0,
          dedup: 0,
        },
      },
    })
    setupMocks({ bindings: [quiet] })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    // Match the specific "<N> drops" badge format — the debugger card's
    // copy ("diagnosing why a real webhook was dropped") would also match a
    // bare /drops/i regex.
    expect(screen.queryByText(/^\d+ drops$/i)).not.toBeInTheDocument()
  })

  it('opens a confirm dialog when delete is clicked', async () => {
    const user = userEvent.setup()
    setupMocks({ bindings: [makeBinding()] })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)

    const deleteBtn = screen.getByRole('button', { name: /delete webhook/i })
    await user.click(deleteBtn)

    // ConfirmDialog mounts as a dialog with the title above
    const dialog = screen.getByRole('dialog', { name: /delete webhook\?/i })
    expect(dialog).toBeInTheDocument()
    // The dialog body mentions the binding name being deleted.
    expect(dialog.textContent).toContain('github-prs')
  })

  it('disables the copy-URL button when binding.url is missing', () => {
    setupMocks({ bindings: [makeBinding({ url: undefined })] })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    const copyBtn = screen.getByRole('button', { name: /copy webhook url/i })
    expect(copyBtn).toBeDisabled()
  })

  it('opens a confirm dialog when rotate is clicked', async () => {
    const user = userEvent.setup()
    setupMocks({ bindings: [makeBinding()] })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    await user.click(screen.getByRole('button', { name: /rotate secret/i }))
    expect(screen.getByRole('dialog', { name: /rotate secret\?/i })).toBeInTheDocument()
  })
})

// ---- Phase 3: recent runs + replay ----

function makeRun(overrides: Partial<AgentInboundRun> = {}): AgentInboundRun {
  return {
    bindingName: 'github-prs',
    requestId: 'req-001',
    startedAt: '2026-04-28T10:00:00Z',
    finishedAt: '2026-04-28T10:00:01Z',
    outcome: 'dispatched',
    ...overrides,
  }
}

describe('WebhooksTab — recent runs panel', () => {
  it('shows an empty state in the panel when no runs exist for the binding', async () => {
    const user = userEvent.setup()
    setupMocks({ bindings: [makeBinding()], inboundRuns: [] })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    await user.click(screen.getByRole('button', { name: /expand github-prs runs/i }))
    expect(screen.getByText(/No runs recorded yet\./i)).toBeInTheDocument()
  })

  it('renders the most recent runs sorted desc by startedAt', async () => {
    const user = userEvent.setup()
    const runs: AgentInboundRun[] = [
      makeRun({ requestId: 'req-old', startedAt: '2026-04-27T10:00:00Z' }),
      makeRun({ requestId: 'req-new', startedAt: '2026-04-28T12:00:00Z' }),
      makeRun({
        requestId: 'req-dropped',
        startedAt: '2026-04-28T11:00:00Z',
        outcome: 'dropped',
        dropReason: 'unmatched-event',
      }),
    ]
    setupMocks({ bindings: [makeBinding()], inboundRuns: runs })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    await user.click(screen.getByRole('button', { name: /expand github-prs runs/i }))

    // All three request IDs visible in the panel
    expect(screen.getByText('req-old')).toBeInTheDocument()
    expect(screen.getByText('req-new')).toBeInTheDocument()
    expect(screen.getByText('req-dropped')).toBeInTheDocument()

    // The dropReason cell shows for the dropped run
    expect(screen.getByText('unmatched-event')).toBeInTheDocument()

    // Order: req-new (newest) before req-dropped before req-old. Inspect
    // the recent-runs table's flat row textContent to confirm — the nested
    // sub-table is the one that has `Recent runs` in a sibling header, so
    // we scope by querying for the specific requestId cells instead.
    const newCell = screen.getByText('req-new')
    const droppedCell = screen.getByText('req-dropped')
    const oldCell = screen.getByText('req-old')
    // Both cells live inside the same nested table. Use compareDocumentPosition
    // to get a deterministic ordering check.
    const FOLLOWING = Node.DOCUMENT_POSITION_FOLLOWING
    expect(newCell.compareDocumentPosition(droppedCell) & FOLLOWING).toBeTruthy()
    expect(droppedCell.compareDocumentPosition(oldCell) & FOLLOWING).toBeTruthy()
  })

  it('disables the replay button on dedup-dropped runs', async () => {
    const user = userEvent.setup()
    const runs: AgentInboundRun[] = [
      makeRun({
        requestId: 'req-dedup',
        outcome: 'dropped',
        dropReason: 'dedup',
      }),
    ]
    setupMocks({ bindings: [makeBinding()], inboundRuns: runs })
    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    await user.click(screen.getByRole('button', { name: /expand github-prs runs/i }))
    const replayBtn = screen.getByRole('button', { name: /replay req-dedup/i })
    expect(replayBtn).toBeDisabled()
  })

  it('replay click opens confirm; confirm calls the mutation with the run id', async () => {
    const user = userEvent.setup()
    const runs: AgentInboundRun[] = [makeRun({ requestId: 'req-001' })]
    const replay = newMutationMock()
    setupMocks({ bindings: [makeBinding()], inboundRuns: runs, replay })

    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    await user.click(screen.getByRole('button', { name: /expand github-prs runs/i }))
    await user.click(screen.getByRole('button', { name: /replay req-001/i }))

    // Confirm dialog shows
    const dialog = screen.getByRole('dialog', { name: /replay this inbound\?/i })
    expect(dialog).toBeInTheDocument()
    // Click Replay in the confirm
    await user.click(within(dialog).getByRole('button', { name: /^replay$/i }))

    expect(replay.mutate).toHaveBeenCalledOnce()
    const args = replay.mutate.mock.calls[0][0] as {
      name: string
      bindingName: string
      requestId: string
    }
    expect(args.name).toBe('my-agent')
    expect(args.bindingName).toBe('github-prs')
    expect(args.requestId).toBe('req-001')
  })

  it('replay 410 surfaces "envelope expired" toast', async () => {
    const user = userEvent.setup()
    const runs: AgentInboundRun[] = [makeRun({ requestId: 'req-410' })]
    const replay = newMutationMock()
    // Drive onError directly when mutate is called.
    replay.mutate.mockImplementation(
      (_vars: unknown, opts?: { onError?: (e: unknown) => void }) => {
        opts?.onError?.({ status: 410 })
      },
    )
    setupMocks({ bindings: [makeBinding()], inboundRuns: runs, replay })

    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    await user.click(screen.getByRole('button', { name: /expand github-prs runs/i }))
    await user.click(screen.getByRole('button', { name: /replay req-410/i }))
    await user.click(
      within(screen.getByRole('dialog', { name: /replay this inbound\?/i })).getByRole('button', {
        name: /^replay$/i,
      }),
    )

    expect(toast.error).toHaveBeenCalledWith(
      'Envelope expired',
      expect.objectContaining({
        description: expect.stringMatching(/older than 7 days/i),
      }),
    )
  })

  it('replay 429 surfaces "queue full" toast', async () => {
    const user = userEvent.setup()
    const runs: AgentInboundRun[] = [makeRun({ requestId: 'req-429' })]
    const replay = newMutationMock()
    replay.mutate.mockImplementation(
      (_vars: unknown, opts?: { onError?: (e: unknown) => void }) => {
        opts?.onError?.({ status: 429 })
      },
    )
    setupMocks({ bindings: [makeBinding()], inboundRuns: runs, replay })

    renderWithQuery(<WebhooksTab agentName="my-agent" />)
    await user.click(screen.getByRole('button', { name: /expand github-prs runs/i }))
    await user.click(screen.getByRole('button', { name: /replay req-429/i }))
    await user.click(
      within(screen.getByRole('dialog', { name: /replay this inbound\?/i })).getByRole('button', {
        name: /^replay$/i,
      }),
    )

    expect(toast.error).toHaveBeenCalledWith(
      'Queue full',
      expect.objectContaining({
        description: expect.stringMatching(/at capacity/i),
      }),
    )
  })
})
