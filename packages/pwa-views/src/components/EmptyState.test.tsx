import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { EmptyState } from './EmptyState'

describe('EmptyState', () => {
  it('renders title and description', () => {
    render(
      <EmptyState
        title="Nothing here"
        description="Add something to get started."
      />,
    )
    expect(screen.getByText('Nothing here')).toBeInTheDocument()
    expect(screen.getByText('Add something to get started.')).toBeInTheDocument()
  })

  it('calls the action click handler when the CTA is clicked', async () => {
    const user = userEvent.setup()
    const handleClick = vi.fn()

    render(
      <EmptyState
        title="No secrets set"
        action={
          <button type="button" onClick={handleClick}>
            Add secret
          </button>
        }
      />,
    )

    await user.click(screen.getByRole('button', { name: 'Add secret' }))
    expect(handleClick).toHaveBeenCalledOnce()
  })

  it('does not render description or action when omitted', () => {
    const { container } = render(<EmptyState title="Empty" />)
    // Only the title paragraph should exist; no action wrapper div
    const paragraphs = container.querySelectorAll('p')
    expect(paragraphs).toHaveLength(1)
    expect(paragraphs[0].textContent).toBe('Empty')
  })
})
