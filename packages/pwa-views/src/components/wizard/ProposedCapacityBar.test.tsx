import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { ProposedCapacityBar } from './ProposedCapacityBar'

describe('ProposedCapacityBar', () => {
  it('renders used + new = sum / total numeric readout', () => {
    render(
      <ProposedCapacityBar
        label="CPU"
        usedByOthers={0.5}
        newRequest={1}
        total={4}
        unit=""
        decimals={2}
      />,
    )
    // "0.50 + 1.00 = 1.50 / 4.00"
    expect(screen.getByText(/0\.50 \+ 1\.00 = 1\.50 \/ 4\.00/)).toBeInTheDocument()
  })

  it('marks the wrapper as exceeds when sum > total', () => {
    const { container } = render(
      <ProposedCapacityBar
        label="CPU"
        usedByOthers={3}
        newRequest={2}
        total={4}
        unit=""
        decimals={2}
      />,
    )
    expect(container.querySelector('[data-band="red"]')).not.toBeNull()
    expect(container.querySelector('[data-exceeds="true"]')).not.toBeNull()
  })

  it('does not mark exceeds when sum == total', () => {
    const { container } = render(
      <ProposedCapacityBar
        label="CPU"
        usedByOthers={2}
        newRequest={2}
        total={4}
        unit=""
        decimals={2}
      />,
    )
    expect(container.querySelector('[data-band="red"]')).toBeNull()
    expect(container.querySelector('[data-band="new"]')).not.toBeNull()
  })

  it('renders 0/0 cleanly without NaN math', () => {
    render(
      <ProposedCapacityBar
        label="Mem"
        usedByOthers={0}
        newRequest={0}
        total={0}
        unit=" GiB"
        decimals={1}
      />,
    )
    // No NaN%; both segments at 0% width should still render
    expect(screen.getByText(/0\.0 \+ 0\.0 = 0\.0 \/ 0\.0 GiB/)).toBeInTheDocument()
  })

  it('caps "used" at 100% so the new segment stays visible if pre-existing over-allocation', () => {
    const { container } = render(
      <ProposedCapacityBar
        label="CPU"
        usedByOthers={6} // over total already
        newRequest={1}
        total={4}
        unit=""
        decimals={2}
      />,
    )
    const usedBar = container.querySelector('[data-band="used"]') as HTMLElement | null
    expect(usedBar?.style.width).toBe('100%')
  })

  it('aria-label reads as "used X, adding Y, total Z of W"', () => {
    render(
      <ProposedCapacityBar
        label="CPU"
        usedByOthers={0.5}
        newRequest={1}
        total={4}
        unit=""
        decimals={2}
      />,
    )
    expect(
      screen.getByLabelText(/CPU: used 0\.50, adding 1\.00, total 1\.50 of 4\.00/),
    ).toBeInTheDocument()
  })
})
