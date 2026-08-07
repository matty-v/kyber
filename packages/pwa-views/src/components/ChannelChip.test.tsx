import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { ChannelChip } from './ChannelChip'

describe('ChannelChip', () => {
  it('renders a friendly label for a telegram source', () => {
    render(<ChannelChip channel={{ source: 'plugin:telegram:telegram', user: '1000000001' }} />)
    expect(screen.getByText(/Telegram/i)).toBeInTheDocument()
  })
})
