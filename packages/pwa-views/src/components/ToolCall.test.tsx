import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ToolCall } from './ToolCall'

describe('ToolCall', () => {
  it('shows the tool name collapsed and reveals input/result on expand', async () => {
    const user = userEvent.setup()
    render(<ToolCall name="WebSearch" input={{ query: 'weather' }} result="sunny" isError={false} />)
    expect(screen.getByText('WebSearch')).toBeInTheDocument()
    expect(screen.queryByText(/sunny/)).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /WebSearch/ }))
    expect(screen.getByText(/sunny/)).toBeInTheDocument()
    expect(screen.getByText(/weather/)).toBeInTheDocument()
  })

  it('marks an errored tool call', () => {
    render(<ToolCall name="Bash" input={{}} result="boom" isError />)
    expect(screen.getByText('Bash').closest('button')).toHaveClass('text-danger')
  })
})
