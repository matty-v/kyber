import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ThinkingBlock } from './ThinkingBlock'

describe('ThinkingBlock', () => {
  it('hides the thinking text until expanded', async () => {
    const user = userEvent.setup()
    render(<ThinkingBlock text="deep thoughts" />)
    expect(screen.queryByText('deep thoughts')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /thinking/i }))
    expect(screen.getByText('deep thoughts')).toBeInTheDocument()
  })
})
