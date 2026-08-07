// AddWebhookWizard tests — template selection pre-fills, validation gates the
// Next button on the Name step, full happy path drives every step, and the
// success panel surfaces the secret on resolved create.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { TooltipProvider } from './ui/tooltip'

vi.mock('../hooks/useAPI', () => ({
  useCreateInboundBinding: vi.fn(),
  useUpdateInboundBinding: vi.fn(),
  useInboundDebug: vi.fn(),
}))

import * as useAPIModule from '../hooks/useAPI'
import { AddWebhookWizard } from './AddWebhookWizard'
import type { InboundCreateResponse } from '../lib/types'

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

function setupCreateMock(mutateAsync: ReturnType<typeof vi.fn>) {
  vi.mocked(useAPIModule.useCreateInboundBinding).mockReturnValue({
    mutate: vi.fn(),
    mutateAsync,
    isPending: false,
  } as unknown as ReturnType<typeof useAPIModule.useCreateInboundBinding>)
  vi.mocked(useAPIModule.useUpdateInboundBinding).mockReturnValue({
    mutate: vi.fn(),
    mutateAsync: vi.fn(),
    isPending: false,
  } as unknown as ReturnType<typeof useAPIModule.useUpdateInboundBinding>)
  // Default debug mock — individual tests override when they care about the
  // call. Without a default the wizard's lazy-mounted TestPayloadPanel still
  // calls useInboundDebug() at render and would explode.
  vi.mocked(useAPIModule.useInboundDebug).mockReturnValue({
    mutate: vi.fn(),
    mutateAsync: vi.fn(),
    isPending: false,
  } as unknown as ReturnType<typeof useAPIModule.useInboundDebug>)
}

function setupUpdateMock(mutateAsync: ReturnType<typeof vi.fn>) {
  vi.mocked(useAPIModule.useUpdateInboundBinding).mockReturnValue({
    mutate: vi.fn(),
    mutateAsync,
    isPending: false,
  } as unknown as ReturnType<typeof useAPIModule.useUpdateInboundBinding>)
  vi.mocked(useAPIModule.useCreateInboundBinding).mockReturnValue({
    mutate: vi.fn(),
    mutateAsync: vi.fn(),
    isPending: false,
  } as unknown as ReturnType<typeof useAPIModule.useCreateInboundBinding>)
  vi.mocked(useAPIModule.useInboundDebug).mockReturnValue({
    mutate: vi.fn(),
    mutateAsync: vi.fn(),
    isPending: false,
  } as unknown as ReturnType<typeof useAPIModule.useInboundDebug>)
}

describe('AddWebhookWizard — template step', () => {
  beforeEach(() => {
    setupCreateMock(vi.fn())
  })

  it('Next button is disabled until a template is selected', () => {
    renderWithQuery(<AddWebhookWizard agentName="my-agent" onClose={() => {}} />)
    expect(screen.getByRole('button', { name: /^next$/i })).toBeDisabled()
  })

  it('GitHub template enables Next and pre-fills auth fields on later steps', async () => {
    const user = userEvent.setup()
    renderWithQuery(<AddWebhookWizard agentName="my-agent" onClose={() => {}} />)
    // Pick the GitHub template
    await user.click(screen.getByRole('button', { name: /github webhook/i }))
    // Next should now be enabled
    const next = screen.getByRole('button', { name: /^next$/i })
    expect(next).not.toBeDisabled()

    // Advance to Name, fill it, advance to Auth and confirm pre-fill
    await user.click(next)
    await user.type(screen.getByPlaceholderText('github-prs'), 'my-source')
    await user.click(screen.getByRole('button', { name: /^next$/i }))

    // Auth step is now visible; signatureHeader pre-filled with X-Hub-Signature-256
    const headerInput = screen.getByDisplayValue('X-Hub-Signature-256')
    expect(headerInput).toBeInTheDocument()
    expect(screen.getByDisplayValue('sha256=')).toBeInTheDocument()
    expect(screen.getByDisplayValue('X-GitHub-Event')).toBeInTheDocument()
  })

  it('Generic template advances without pre-filled fields', async () => {
    const user = userEvent.setup()
    renderWithQuery(<AddWebhookWizard agentName="my-agent" onClose={() => {}} />)
    await user.click(screen.getByRole('button', { name: /^generic/i }))
    await user.click(screen.getByRole('button', { name: /^next$/i }))
    // Name step visible
    expect(screen.getByPlaceholderText('github-prs')).toBeInTheDocument()
  })
})

describe('AddWebhookWizard — name validation', () => {
  beforeEach(() => {
    setupCreateMock(vi.fn())
  })

  it('blocks Next when the name is invalid (uppercase)', async () => {
    const user = userEvent.setup()
    renderWithQuery(<AddWebhookWizard agentName="my-agent" onClose={() => {}} />)
    await user.click(screen.getByRole('button', { name: /github webhook/i }))
    await user.click(screen.getByRole('button', { name: /^next$/i }))
    await user.type(screen.getByPlaceholderText('github-prs'), 'INVALID-CAPS')
    expect(screen.getByRole('button', { name: /^next$/i })).toBeDisabled()
    expect(
      screen.getByText(/Must match \^\[a-z0-9\]/i),
    ).toBeInTheDocument()
  })

  it('enables Next when the name matches the DNS-1123 pattern', async () => {
    const user = userEvent.setup()
    renderWithQuery(<AddWebhookWizard agentName="my-agent" onClose={() => {}} />)
    await user.click(screen.getByRole('button', { name: /github webhook/i }))
    await user.click(screen.getByRole('button', { name: /^next$/i }))
    await user.type(screen.getByPlaceholderText('github-prs'), 'github-prs')
    expect(screen.getByRole('button', { name: /^next$/i })).not.toBeDisabled()
  })
})

describe('AddWebhookWizard — happy path', () => {
  it('walks through every step and submits, then reveals the secret', async () => {
    const fakeResponse: InboundCreateResponse = {
      binding: {
        name: 'github-prs',
        existingSecret: 'my-agent-github-prs',
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
      },
      secret: 'super-secret-value-shown-once',
      url: 'https://kyber.example.com/webhooks/inbound/my-agent/github-prs',
    }
    const mutateAsync = vi.fn().mockResolvedValue(fakeResponse)
    setupCreateMock(mutateAsync)

    const user = userEvent.setup()
    renderWithQuery(<AddWebhookWizard agentName="my-agent" onClose={() => {}} />)

    // 1. Template
    await user.click(screen.getByRole('button', { name: /github webhook/i }))
    await user.click(screen.getByRole('button', { name: /^next$/i }))

    // 2. Name
    await user.type(screen.getByPlaceholderText('github-prs'), 'github-prs')
    await user.click(screen.getByRole('button', { name: /^next$/i }))

    // 3. Auth (pre-filled, just advance)
    await user.click(screen.getByRole('button', { name: /^next$/i }))

    // 4. Matching (skip)
    await user.click(screen.getByRole('button', { name: /^next$/i }))

    // 5. Fields (skip)
    await user.click(screen.getByRole('button', { name: /^next$/i }))

    // 6. Action — required
    const actionTextarea = screen.getByPlaceholderText(/triage this pr/i)
    await user.type(actionTextarea, 'Triage this PR.')
    await user.click(screen.getByRole('button', { name: /^next$/i }))

    // 7. Limits (default 10 is valid)
    await user.click(screen.getByRole('button', { name: /^next$/i }))

    // 8. Review — submit
    await user.click(screen.getByRole('button', { name: /^create$/i }))

    expect(mutateAsync).toHaveBeenCalledOnce()
    const call = mutateAsync.mock.calls[0][0] as { name: string; body: { name: string; action: string } }
    expect(call.name).toBe('my-agent')
    expect(call.body.name).toBe('github-prs')
    expect(call.body.action).toBe('Triage this PR.')

    // After resolve, the success panel surfaces the secret + URL
    expect(await screen.findByText('Setup complete')).toBeInTheDocument()
    expect(screen.getByText('super-secret-value-shown-once')).toBeInTheDocument()
    expect(
      screen.getByText('https://kyber.example.com/webhooks/inbound/my-agent/github-prs'),
    ).toBeInTheDocument()
  })

  it('shows a missing-publicUrl warning when the response has no url', async () => {
    const fakeResponse: InboundCreateResponse = {
      binding: {
        name: 'github-prs',
        existingSecret: 'my-agent-github-prs',
        signatureHeader: 'X-Hub-Signature-256',
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
      },
      secret: 'super-secret-value-shown-once',
    }
    const mutateAsync = vi.fn().mockResolvedValue(fakeResponse)
    setupCreateMock(mutateAsync)

    const user = userEvent.setup()
    renderWithQuery(<AddWebhookWizard agentName="my-agent" onClose={() => {}} />)

    // Quick walk: github → name → ... → submit
    await user.click(screen.getByRole('button', { name: /github webhook/i }))
    await user.click(screen.getByRole('button', { name: /^next$/i }))
    await user.type(screen.getByPlaceholderText('github-prs'), 'github-prs')
    await user.click(screen.getByRole('button', { name: /^next$/i }))
    await user.click(screen.getByRole('button', { name: /^next$/i })) // Auth
    await user.click(screen.getByRole('button', { name: /^next$/i })) // Matching
    await user.click(screen.getByRole('button', { name: /^next$/i })) // Fields
    await user.type(screen.getByPlaceholderText(/triage this pr/i), 'Triage this PR.')
    await user.click(screen.getByRole('button', { name: /^next$/i }))
    await user.click(screen.getByRole('button', { name: /^next$/i })) // Limits
    await user.click(screen.getByRole('button', { name: /^create$/i }))

    expect(await screen.findByText('Setup complete')).toBeInTheDocument()
    expect(screen.getByText(/PublicURL configured/i)).toBeInTheDocument()
  })
})

describe('AddWebhookWizard — test-payload affordance on Action step', () => {
  beforeEach(() => {
    setupCreateMock(vi.fn())
  })

  // Helper: walk the wizard up to the Action step.
  async function advanceToAction(user: ReturnType<typeof userEvent.setup>) {
    await user.click(screen.getByRole('button', { name: /github webhook/i }))
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Name
    await user.type(screen.getByPlaceholderText('github-prs'), 'github-prs')
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Auth
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Matching
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Fields
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Action
  }

  it('opens the test-payload panel when "Test with payload" is clicked', async () => {
    const user = userEvent.setup()
    renderWithQuery(<AddWebhookWizard agentName="my-agent" onClose={() => {}} />)
    await advanceToAction(user)

    // Panel collapsed by default
    expect(screen.queryByLabelText(/payload \(json\)/i)).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /test with payload/i }))

    // Panel now visible
    expect(screen.getByLabelText(/payload \(json\)/i)).toBeInTheDocument()
  })

  it('Run button fires inboundDebug with the wizard binding draft', async () => {
    const debugAsync = vi.fn().mockResolvedValue({
      match: true,
      resolvedEvent: 'pull_request.opened',
      filterResults: [],
      fieldResults: [],
      envelope: 'rendered envelope',
    })
    vi.mocked(useAPIModule.useInboundDebug).mockReturnValue({
      mutate: vi.fn(),
      mutateAsync: debugAsync,
      isPending: false,
    } as unknown as ReturnType<typeof useAPIModule.useInboundDebug>)

    const user = userEvent.setup()
    renderWithQuery(<AddWebhookWizard agentName="my-agent" onClose={() => {}} />)
    await advanceToAction(user)

    // Action text is required for the page-level Next gate, but the test
    // panel doesn't depend on it. We type something so the panel sees
    // bindingDraft.action populated.
    await user.type(screen.getByPlaceholderText(/triage this pr/i), 'Do the thing.')

    await user.click(screen.getByRole('button', { name: /test with payload/i }))

    const payload = screen.getByLabelText(/payload \(json\)/i) as HTMLTextAreaElement
    fireEvent.change(payload, { target: { value: '{"action":"opened"}' } })
    fireEvent.blur(payload)

    await user.click(screen.getByRole('button', { name: /^run$/i }))

    expect(debugAsync).toHaveBeenCalledOnce()
    const arg = debugAsync.mock.calls[0][0] as {
      binding: { name: string; action: string; signatureHeader: string }
      payload: unknown
      agent?: string
    }
    expect(arg.binding.name).toBe('github-prs')
    expect(arg.binding.signatureHeader).toBe('X-Hub-Signature-256')
    expect(arg.binding.action).toBe('Do the thing.')
    expect(arg.payload).toEqual({ action: 'opened' })
    expect(arg.agent).toBe('my-agent')

    // Result panel renders the matched diagnostic.
    expect(await screen.findByLabelText('Matched')).toBeInTheDocument()
  })
})

// ---- edit mode (#222) ----

describe('AddWebhookWizard — edit mode', () => {
  const editingBinding = {
    name: 'ci-watch',
    existingSecret: 'my-agent-ci-watch-hmac',
    signatureHeader: 'X-Hub-Signature-256',
    signaturePrefix: 'sha256=',
    eventHeader: 'X-GitHub-Event',
    matchEvents: ['push'],
    action: 'investigate',
    limits: { maxPerMinute: 30 },
  }

  beforeEach(() => {
    setupUpdateMock(vi.fn().mockResolvedValue({ ...editingBinding }))
  })

  it('opens at the Auth step and hides Template/Name from the strip', () => {
    renderWithQuery(
      <AddWebhookWizard
        agentName="my-agent"
        editing={editingBinding}
        onClose={() => {}}
      />,
    )
    // Title shows the binding name.
    expect(screen.getByText(/edit webhook — ci-watch/i)).toBeInTheDocument()
    // Auth step is the active one — its Signature Header field should be on screen.
    expect(screen.getByLabelText(/signature header/i)).toHaveValue('X-Hub-Signature-256')
    // Template + Name buttons are filtered out of the strip.
    expect(screen.queryByRole('button', { name: /template/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^2 ?· ?name$/i })).not.toBeInTheDocument()
  })

  it('Back button is disabled at the first editable step', () => {
    renderWithQuery(
      <AddWebhookWizard
        agentName="my-agent"
        editing={editingBinding}
        onClose={() => {}}
      />,
    )
    expect(screen.getByRole('button', { name: /^back$/i })).toBeDisabled()
  })

  it('submits PATCH with editable fields and closes on success', async () => {
    const user = userEvent.setup()
    const update = vi.fn().mockResolvedValue({ ...editingBinding, action: 'updated' })
    const onClose = vi.fn()
    setupUpdateMock(update)
    renderWithQuery(
      <AddWebhookWizard
        agentName="my-agent"
        editing={editingBinding}
        onClose={onClose}
      />,
    )
    // Walk forward Auth → Matching → Fields → Action.
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Matching
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Fields
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Action
    const actionField = screen.getByPlaceholderText(/triage this pr/i)
    fireEvent.change(actionField, { target: { value: 'updated action' } })
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Limits
    await user.click(screen.getByRole('button', { name: /^next$/i })) // → Review

    // Save button replaces Create on the Review step.
    const saveBtn = screen.getByRole('button', { name: /^save$/i })
    expect(saveBtn).toBeEnabled()
    await user.click(saveBtn)

    expect(update).toHaveBeenCalledOnce()
    const args = update.mock.calls[0][0] as {
      name: string
      bindingName: string
      body: { action: string }
    }
    expect(args.name).toBe('my-agent')
    expect(args.bindingName).toBe('ci-watch')
    expect(args.body.action).toBe('updated action')

    // No setup-complete panel for edits — onClose fires instead.
    expect(onClose).toHaveBeenCalled()
    expect(screen.queryByText(/setup complete/i)).not.toBeInTheDocument()
  })
})
