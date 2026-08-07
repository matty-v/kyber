import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { CapacityBar } from './CapacityBar'

describe('CapacityBar', () => {
  it('renders with the provided label', () => {
    render(<CapacityBar used={1} total={4} label="CPU" />)
    expect(screen.getByText(/CPU/)).toBeInTheDocument()
  })

  it('shows the integer percentage in the label area', () => {
    render(<CapacityBar used={2} total={4} label="CPU" />)
    expect(screen.getByText(/50%/)).toBeInTheDocument()
  })

  it('uses the green band for usage under 70%', () => {
    const { container } = render(<CapacityBar used={1} total={4} label="X" />)
    expect(container.querySelector('[data-band="green"]')).not.toBeNull()
  })

  it('uses the yellow band at 70%', () => {
    const { container } = render(<CapacityBar used={2.8} total={4} label="X" />)
    expect(container.querySelector('[data-band="yellow"]')).not.toBeNull()
  })

  it('uses the red band at 90%', () => {
    const { container } = render(<CapacityBar used={3.6} total={4} label="X" />)
    expect(container.querySelector('[data-band="red"]')).not.toBeNull()
  })

  it('caps the visual width at 100% when over-allocated', () => {
    const { container } = render(<CapacityBar used={10} total={4} label="X" />)
    const fill = container.querySelector<HTMLElement>('[data-band="red"]')
    expect(fill).not.toBeNull()
    expect(fill?.style.width).toBe('100%')
  })

  it('renders 0% when total is zero (capacity unknown)', () => {
    render(<CapacityBar used={0} total={0} label="X" />)
    expect(screen.getByText(/0%/)).toBeInTheDocument()
  })
})
