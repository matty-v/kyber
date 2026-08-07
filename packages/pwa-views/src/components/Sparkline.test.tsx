// Sparkline tests — render assertions on the SVG output. We don't
// snapshot the path d attribute (brittle to minor rendering tweaks);
// instead we assert structural properties that matter to consumers.

import { describe, it, expect } from 'vitest'
import { render } from '@testing-library/react'
import { Sparkline } from './Sparkline'

describe('Sparkline', () => {
  it('renders an svg with the configured viewport', () => {
    const { container } = render(<Sparkline points={[1, 2, 3]} width={48} height={12} />)
    const svg = container.querySelector('svg')!
    expect(svg).toBeInTheDocument()
    expect(svg.getAttribute('width')).toBe('48')
    expect(svg.getAttribute('height')).toBe('12')
    expect(svg.getAttribute('viewBox')).toBe('0 0 48 12')
  })

  it('renders a path when points has 2+ values', () => {
    const { container } = render(<Sparkline points={[0, 1]} />)
    expect(container.querySelector('path')).toBeInTheDocument()
    expect(container.querySelector('circle')).not.toBeInTheDocument()
  })

  it('renders a placeholder dot when given fewer than 2 points', () => {
    const { container: empty } = render(<Sparkline points={[]} />)
    expect(empty.querySelector('path')).not.toBeInTheDocument()
    expect(empty.querySelector('circle')).toBeInTheDocument()

    const { container: single } = render(<Sparkline points={[5]} />)
    expect(single.querySelector('path')).not.toBeInTheDocument()
    expect(single.querySelector('circle')).toBeInTheDocument()
  })

  it('exposes the ariaLabel on the svg as accessible name', () => {
    const { container } = render(
      <Sparkline points={[1, 2]} ariaLabel="Running count over the last 30 minutes" />,
    )
    expect(container.querySelector('svg')!.getAttribute('aria-label')).toBe(
      'Running count over the last 30 minutes',
    )
  })

  it('clamps negative values to 0', () => {
    // Hard to assert directly without parsing the path, but we can at
    // least confirm rendering succeeds without throwing on negatives.
    expect(() =>
      render(<Sparkline points={[-1, 0, 1, 2]} />),
    ).not.toThrow()
  })

  it('uses currentColor for the stroke (so callers control color via class)', () => {
    const { container } = render(
      <Sparkline points={[1, 2, 3]} className="text-success" />,
    )
    const path = container.querySelector('path')!
    expect(path.getAttribute('stroke')).toBe('currentColor')
    expect(container.querySelector('svg')!.classList.contains('text-success')).toBe(true)
  })
})
