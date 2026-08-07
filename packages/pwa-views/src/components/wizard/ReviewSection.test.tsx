import { describe, it, expect, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { render, screen } from '@testing-library/react'
import { ReviewSection } from './ReviewSection'
import { initialWizardState } from './types'

describe('ReviewSection', () => {
  it('renders the agent name + machine + runtime + model + resources', () => {
    render(
      <ReviewSection
        state={{
          ...initialWizardState([]),
          name: 'alice',
          machine: 'razer',
          model: 'claude-opus-4-7',
          cpu: '2',
          memory: '4Gi',
          disk: '50Gi',
        }}
      />,
    )
    expect(screen.getByText('alice')).toBeInTheDocument()
    expect(screen.getByText('razer')).toBeInTheDocument()
    expect(screen.getByText('claude-code')).toBeInTheDocument() // default runtime
    expect(screen.getByText('claude-opus-4-7')).toBeInTheDocument()
    expect(screen.getByText(/2 CPU/)).toBeInTheDocument()
    expect(screen.getByText(/4Gi/)).toBeInTheDocument()
    expect(screen.getByText(/50Gi/)).toBeInTheDocument()
  })

  it('shows OAuth or API key based on authType', () => {
    const { rerender } = render(
      <ReviewSection
        state={{ ...initialWizardState([]), authType: 'oauth' }}
      />,
    )
    expect(screen.getByText(/OAuth/)).toBeInTheDocument()

    rerender(
      <ReviewSection
        state={{ ...initialWizardState([]), authType: 'api-key' }}
      />,
    )
    expect(screen.getByText(/API key/)).toBeInTheDocument()
  })

  it('shows the identity-repo mode', () => {
    render(
      <ReviewSection
        state={{
          ...initialWizardState([]),
          identityRepoMode: 'existing',
          identityRepoExisting: 'matty-v/some-repo',
          name: 'alice',
        }}
      />,
    )
    expect(screen.getByText(/matty-v\/some-repo/)).toBeInTheDocument()
  })

  it('renders "(unset)" placeholders for empty name / machine / model', () => {
    render(<ReviewSection state={initialWizardState([])} />)
    // Three rows show (unset) when those fields are empty: Name, Machine, Model
    expect(screen.getAllByText('(unset)')).toHaveLength(3)
  })

  it('renders Identity as "(none)" when mode is existing with empty slug', () => {
    render(
      <ReviewSection
        state={{ ...initialWizardState([]), identityRepoMode: 'existing' }}
      />,
    )
    expect(screen.getByText('(none)')).toBeInTheDocument()
  })

  it('renders Identity as "none" when mode === "none"', () => {
    render(
      <ReviewSection
        state={{ ...initialWizardState([]), identityRepoMode: 'none' }}
      />,
    )
    // Identity row shows literal "none"; Channels also shows "none" when no channel enabled
    expect(screen.getAllByText('none')).toHaveLength(2)
  })

  it('renders Channels as "Telegram" when telegramEnabled', () => {
    render(
      <ReviewSection
        state={{ ...initialWizardState([]), telegramEnabled: true }}
      />,
    )
    expect(screen.getByText('Telegram')).toBeInTheDocument()
  })
})

describe('ReviewSection — onEdit', () => {
  it('renders no Edit links when onEdit is undefined', () => {
    render(<ReviewSection state={initialWizardState([])} />)
    expect(screen.queryAllByRole('button', { name: /edit/i })).toHaveLength(0)
  })

  it('renders one Edit link per editable group when onEdit is provided', () => {
    render(<ReviewSection state={initialWizardState([])} onEdit={vi.fn()} />)
    // Name (step 1), Runtime (step 2), Identity (step 3), Auth (step 4) → 4 Edit buttons
    expect(screen.getAllByRole('button', { name: /edit/i })).toHaveLength(4)
  })

  it('clicking the Name row\'s Edit calls onEdit(1)', async () => {
    const user = userEvent.setup()
    const onEdit = vi.fn()
    render(<ReviewSection state={initialWizardState([])} onEdit={onEdit} />)
    const editButtons = screen.getAllByRole('button', { name: /edit/i })
    await user.click(editButtons[0]) // first edit = Basics (step 1)
    expect(onEdit).toHaveBeenCalledWith(1)
  })

  it('clicking the Auth row\'s Edit calls onEdit(4)', async () => {
    const user = userEvent.setup()
    const onEdit = vi.fn()
    render(<ReviewSection state={initialWizardState([])} onEdit={onEdit} />)
    const editButtons = screen.getAllByRole('button', { name: /edit/i })
    await user.click(editButtons[3]) // fourth edit = Auth (step 4)
    expect(onEdit).toHaveBeenCalledWith(4)
  })
})
