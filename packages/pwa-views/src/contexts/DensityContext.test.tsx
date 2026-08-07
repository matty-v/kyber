import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { DensityProvider, useDensity } from './DensityContext'

function Probe() {
  const { density, effectiveDensity, setDensity } = useDensity()
  return (
    <div>
      <span data-testid="stored">{density}</span>
      <span data-testid="effective">{effectiveDensity}</span>
      <button onClick={() => setDensity('compact')}>set-compact</button>
      <button onClick={() => setDensity('comfortable')}>set-comfortable</button>
    </div>
  )
}

function setMatchMedia(matchesMobile: boolean) {
  const listeners: Array<(e: MediaQueryListEvent) => void> = []
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: matchesMobile,
      media: query,
      addEventListener: (_e: string, cb: (e: MediaQueryListEvent) => void) => {
        listeners.push(cb)
      },
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
      onchange: null,
    })),
  })
  return {
    fireResize: (matches: boolean) => {
      listeners.forEach((cb) => cb({ matches } as MediaQueryListEvent))
    },
  }
}

beforeEach(() => {
  window.localStorage.clear()
  document.body.removeAttribute('data-density')
  setMatchMedia(false) // default: desktop
})

describe('DensityProvider', () => {
  it('defaults to comfortable when no localStorage entry exists', () => {
    render(
      <DensityProvider>
        <Probe />
      </DensityProvider>,
    )
    expect(screen.getByTestId('stored').textContent).toBe('comfortable')
    expect(screen.getByTestId('effective').textContent).toBe('comfortable')
    expect(document.body.dataset.density).toBe('comfortable')
  })

  it('reads compact from localStorage on mount', () => {
    window.localStorage.setItem('kyber:density', 'compact')
    render(
      <DensityProvider>
        <Probe />
      </DensityProvider>,
    )
    expect(screen.getByTestId('stored').textContent).toBe('compact')
    expect(screen.getByTestId('effective').textContent).toBe('compact')
    expect(document.body.dataset.density).toBe('compact')
  })

  it('setDensity updates state, localStorage, and the body attribute', async () => {
    const user = userEvent.setup()
    render(
      <DensityProvider>
        <Probe />
      </DensityProvider>,
    )
    await user.click(screen.getByText('set-compact'))
    expect(screen.getByTestId('stored').textContent).toBe('compact')
    expect(screen.getByTestId('effective').textContent).toBe('compact')
    expect(document.body.dataset.density).toBe('compact')
    expect(window.localStorage.getItem('kyber:density')).toBe('compact')

    await user.click(screen.getByText('set-comfortable'))
    expect(window.localStorage.getItem('kyber:density')).toBe('comfortable')
    expect(document.body.dataset.density).toBe('comfortable')
  })

  it('on mobile viewport effective density collapses to comfortable but stored preference is preserved', () => {
    window.localStorage.setItem('kyber:density', 'compact')
    setMatchMedia(true) // mobile
    render(
      <DensityProvider>
        <Probe />
      </DensityProvider>,
    )
    expect(screen.getByTestId('stored').textContent).toBe('compact')
    expect(screen.getByTestId('effective').textContent).toBe('comfortable')
    expect(document.body.dataset.density).toBe('comfortable')
  })

  it('responds to viewport resize across the mobile breakpoint', () => {
    window.localStorage.setItem('kyber:density', 'compact')
    const mq = setMatchMedia(false) // start desktop
    render(
      <DensityProvider>
        <Probe />
      </DensityProvider>,
    )
    expect(screen.getByTestId('effective').textContent).toBe('compact')

    // Resize down to mobile.
    act(() => mq.fireResize(true))
    expect(screen.getByTestId('effective').textContent).toBe('comfortable')

    // Resize back up.
    act(() => mq.fireResize(false))
    expect(screen.getByTestId('effective').textContent).toBe('compact')
  })

  it('useDensity throws when consumed outside a provider', () => {
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    expect(() => render(<Probe />)).toThrow(/useDensity must be used within a DensityProvider/)
    errSpy.mockRestore()
  })
})
