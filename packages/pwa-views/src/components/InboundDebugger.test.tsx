// InboundDebugger tests (#208 Phase 3) — binding picker, payload validation,
// successful debug rendering. Hooks are mocked.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { TooltipProvider } from './ui/tooltip'

vi.mock('../hooks/useAPI', () => ({
  useInboundDebug: vi.fn(),
}))

import * as useAPIModule from '../hooks/useAPI'
import { InboundDebugger } from './InboundDebugger'
import type { InboundBindingWithStats, InboundDebugResponse } from '../lib/types'

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

function setupDebugMock(
  mutateAsync: ReturnType<typeof vi.fn>,
  isPending = false,
) {
  vi.mocked(useAPIModule.useInboundDebug).mockReturnValue({
    mutate: vi.fn(),
    mutateAsync,
    isPending,
  } as unknown as ReturnType<typeof useAPIModule.useInboundDebug>)
}

function makeBinding(name = 'github-prs'): InboundBindingWithStats {
  return {
    name,
    existingSecret: `${name}-secret`,
    signatureHeader: 'X-Hub-Signature-256',
    signaturePrefix: 'sha256=',
    eventHeader: 'X-GitHub-Event',
    action: 'Triage this PR.',
    stats: {
      totalDispatched: 0,
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
  }
}

describe('InboundDebugger — binding picker', () => {
  beforeEach(() => {
    setupDebugMock(vi.fn())
  })

  it('renders the dropdown with all bindings', () => {
    const bindings = [makeBinding('github-prs'), makeBinding('linear-issues')]
    renderWithQuery(<InboundDebugger agentName="my-agent" bindings={bindings} />)
    const select = screen.getByLabelText(/binding/i) as HTMLSelectElement
    expect(select).toBeInTheDocument()
    // Both option texts should be present
    expect(screen.getByRole('option', { name: 'github-prs' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'linear-issues' })).toBeInTheDocument()
  })

  it('shows the no-bindings placeholder when list is empty', () => {
    renderWithQuery(<InboundDebugger agentName="my-agent" bindings={[]} />)
    expect(screen.getByRole('option', { name: /no bindings configured/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^run$/i })).toBeDisabled()
  })
})

describe('InboundDebugger — payload validation', () => {
  beforeEach(() => {
    setupDebugMock(vi.fn())
  })

  it('disables Run when payload is invalid JSON', async () => {
    renderWithQuery(<InboundDebugger agentName="my-agent" bindings={[makeBinding()]} />)
    const textarea = screen.getByLabelText(/payload \(json\)/i) as HTMLTextAreaElement
    // userEvent.type interprets `{{`/`}}` specially; use fireEvent.change to
    // set the raw value so we can land arbitrary JSON-shaped text in one shot.
    fireEvent.change(textarea, { target: { value: 'not json{' } })
    fireEvent.blur(textarea)
    expect(screen.getByText(/Invalid JSON/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^run$/i })).toBeDisabled()
  })

  it('enables Run when payload parses', () => {
    renderWithQuery(<InboundDebugger agentName="my-agent" bindings={[makeBinding()]} />)
    const textarea = screen.getByLabelText(/payload \(json\)/i) as HTMLTextAreaElement
    fireEvent.change(textarea, { target: { value: '{"foo":"bar"}' } })
    fireEvent.blur(textarea)
    expect(screen.getByRole('button', { name: /^run$/i })).not.toBeDisabled()
  })
})

describe('InboundDebugger — successful run', () => {
  it('renders the matched diagnostic and the rendered envelope', async () => {
    const fakeResponse: InboundDebugResponse = {
      match: true,
      resolvedEvent: 'pull_request.opened',
      filterResults: [
        {
          jsonPath: '$.repository.full_name',
          passed: true,
          extractedValue: 'my-org/my-repo',
        },
      ],
      fieldResults: [
        {
          label: 'PR title',
          jsonPath: '$.pull_request.title',
          extractedValue: 'Demo PR',
          truncated: false,
        },
      ],
      envelope:
        '[inbound:test] binding=github-prs agent=my-agent\ndata:\n  PR title: Demo PR\naction:\n  Triage.',
    }
    const mutateAsync = vi.fn().mockResolvedValue(fakeResponse)
    setupDebugMock(mutateAsync)

    const user = userEvent.setup()
    renderWithQuery(<InboundDebugger agentName="my-agent" bindings={[makeBinding()]} />)

    const textarea = screen.getByLabelText(/payload \(json\)/i) as HTMLTextAreaElement
    fireEvent.change(textarea, { target: { value: '{"action":"opened"}' } })
    fireEvent.blur(textarea)
    await user.click(screen.getByRole('button', { name: /^run$/i }))

    expect(mutateAsync).toHaveBeenCalledOnce()
    const arg = mutateAsync.mock.calls[0][0] as {
      binding: { name: string }
      payload: unknown
      agent?: string
    }
    expect(arg.binding.name).toBe('github-prs')
    expect(arg.payload).toEqual({ action: 'opened' })
    expect(arg.agent).toBe('my-agent')

    // Match indicator
    expect(await screen.findByLabelText('Matched')).toBeInTheDocument()
    // Resolved event
    expect(screen.getByText('pull_request.opened')).toBeInTheDocument()
    // Filter row
    expect(screen.getByText('$.repository.full_name')).toBeInTheDocument()
    expect(screen.getByText('my-org/my-repo')).toBeInTheDocument()
    // Field row
    expect(screen.getByText('PR title')).toBeInTheDocument()
    expect(screen.getByText('Demo PR')).toBeInTheDocument()
    // Envelope
    expect(screen.getByText(/binding=github-prs agent=my-agent/i)).toBeInTheDocument()
  })

  it('renders Dropped + drop reason when match=false', async () => {
    const fakeResponse: InboundDebugResponse = {
      match: false,
      dropReason: 'unmatched-event',
      resolvedEvent: 'release.published',
      filterResults: [],
      fieldResults: [],
    }
    const mutateAsync = vi.fn().mockResolvedValue(fakeResponse)
    setupDebugMock(mutateAsync)

    const user = userEvent.setup()
    renderWithQuery(<InboundDebugger agentName="my-agent" bindings={[makeBinding()]} />)
    const textarea = screen.getByLabelText(/payload \(json\)/i) as HTMLTextAreaElement
    fireEvent.change(textarea, { target: { value: '{"foo":1}' } })
    fireEvent.blur(textarea)
    await user.click(screen.getByRole('button', { name: /^run$/i }))

    expect(await screen.findByLabelText('Dropped')).toBeInTheDocument()
    expect(screen.getByText(/Dropped: unmatched-event/i)).toBeInTheDocument()
  })
})
