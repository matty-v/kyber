import { describe, it, expect, vi, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { createElement } from 'react'
import { useKeyboardShortcuts } from './useKeyboardShortcuts'

// Wrap in MemoryRouter because the hook now calls useNavigate() internally.
// Tests that pass `navigate` as a prop still use the mock (the prop takes
// precedence), so every existing assertion is unchanged.
const wrapper = ({ children }: { children: React.ReactNode }) =>
  createElement(MemoryRouter, null, children)

// Mirror the wizard hook's test helper: dispatch a real KeyboardEvent on the
// window so the hook's useEffect-registered listener picks it up. `target` is
// stamped via Object.defineProperty because dispatchEvent leaves
// event.target null for events synthesized in JSDOM.
function fireKey(key: string, target?: HTMLElement) {
  const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true })
  if (target) {
    Object.defineProperty(event, 'target', { value: target, writable: false })
  }
  window.dispatchEvent(event)
  return event
}

afterEach(() => {
  vi.useRealTimers()
})

describe('useKeyboardShortcuts — leader chord navigation', () => {
  it('g f navigates to fleet', () => {
    const navigate = vi.fn()
    const onShowHelp = vi.fn()
    renderHook(() => useKeyboardShortcuts({ navigate, onShowHelp }), { wrapper })
    fireKey('g')
    fireKey('f')
    expect(navigate).toHaveBeenCalledWith('/')
    expect(onShowHelp).not.toHaveBeenCalled()
  })

  it('g m navigates to machines', () => {
    const navigate = vi.fn()
    renderHook(() => useKeyboardShortcuts({ navigate, onShowHelp: vi.fn() }), { wrapper })
    fireKey('g')
    fireKey('m')
    expect(navigate).toHaveBeenCalledWith('/machines')
  })

  it('g a navigates to agents', () => {
    const navigate = vi.fn()
    renderHook(() => useKeyboardShortcuts({ navigate, onShowHelp: vi.fn() }), { wrapper })
    fireKey('g')
    fireKey('a')
    expect(navigate).toHaveBeenCalledWith('/agents')
  })

  it('g s navigates to settings', () => {
    const navigate = vi.fn()
    renderHook(() => useKeyboardShortcuts({ navigate, onShowHelp: vi.fn() }), { wrapper })
    fireKey('g')
    fireKey('s')
    expect(navigate).toHaveBeenCalledWith('/settings')
  })

  it('g followed by an unrecognised key is a no-op (and clears the leader)', () => {
    const navigate = vi.fn()
    renderHook(() => useKeyboardShortcuts({ navigate, onShowHelp: vi.fn() }), { wrapper })
    fireKey('g')
    fireKey('q') // unrecognised — should not navigate
    expect(navigate).not.toHaveBeenCalled()
    // Subsequent f without a leading g must not navigate (leader was cleared).
    fireKey('f')
    expect(navigate).not.toHaveBeenCalled()
  })

  it('leader expires after leaderTimeoutMs', () => {
    vi.useFakeTimers()
    const navigate = vi.fn()
    renderHook(() =>
      useKeyboardShortcuts({ navigate, onShowHelp: vi.fn(), leaderTimeoutMs: 500 }),
    { wrapper })
    fireKey('g')
    act(() => {
      vi.advanceTimersByTime(501)
    })
    fireKey('f')
    expect(navigate).not.toHaveBeenCalled()
  })

  it('respects custom routes', () => {
    const navigate = vi.fn()
    renderHook(() =>
      useKeyboardShortcuts({
        navigate,
        onShowHelp: vi.fn(),
        routes: { fleet: '/F', machines: '/M', agents: '/A', settings: '/S' },
      }),
    { wrapper })
    fireKey('g')
    fireKey('m')
    expect(navigate).toHaveBeenCalledWith('/M')
  })
})

describe('useKeyboardShortcuts — help overlay', () => {
  it('? fires onShowHelp', () => {
    const onShowHelp = vi.fn()
    renderHook(() => useKeyboardShortcuts({ navigate: vi.fn(), onShowHelp }), { wrapper })
    fireKey('?')
    expect(onShowHelp).toHaveBeenCalledTimes(1)
  })

  it('? does not navigate', () => {
    const navigate = vi.fn()
    renderHook(() => useKeyboardShortcuts({ navigate, onShowHelp: vi.fn() }), { wrapper })
    fireKey('?')
    expect(navigate).not.toHaveBeenCalled()
  })
})

describe('useKeyboardShortcuts — input guards', () => {
  it.each(['INPUT', 'TEXTAREA', 'SELECT'] as const)(
    'is inactive when focus is in a <%s>',
    (tag) => {
      const navigate = vi.fn()
      const onShowHelp = vi.fn()
      renderHook(() => useKeyboardShortcuts({ navigate, onShowHelp }), { wrapper })
      const el = document.createElement(tag.toLowerCase())
      // Both keys of the chord must arrive with the same target stamped.
      fireKey('g', el)
      fireKey('f', el)
      fireKey('?', el)
      expect(navigate).not.toHaveBeenCalled()
      expect(onShowHelp).not.toHaveBeenCalled()
    },
  )

  it('is inactive when focus is in a contenteditable element', () => {
    const navigate = vi.fn()
    const onShowHelp = vi.fn()
    renderHook(() => useKeyboardShortcuts({ navigate, onShowHelp }), { wrapper })
    // isContentEditable resolves through the parent chain — a detached
    // element returns false in both JSDOM and real browsers, so we have to
    // attach the div before dispatching events to mirror runtime behavior.
    const div = document.createElement('div')
    div.contentEditable = 'true'
    document.body.appendChild(div)
    try {
      fireKey('g', div)
      fireKey('a', div)
      fireKey('?', div)
    } finally {
      div.remove()
    }
    expect(navigate).not.toHaveBeenCalled()
    expect(onShowHelp).not.toHaveBeenCalled()
  })

  it('ignores keys that arrive with a meta/ctrl/alt modifier', () => {
    const navigate = vi.fn()
    const onShowHelp = vi.fn()
    renderHook(() => useKeyboardShortcuts({ navigate, onShowHelp }), { wrapper })
    // metaKey on `?` would be ⌘? — leave that to the browser / future palette.
    const evt = new KeyboardEvent('keydown', { key: '?', metaKey: true, bubbles: true })
    window.dispatchEvent(evt)
    expect(onShowHelp).not.toHaveBeenCalled()
  })
})

describe('useKeyboardShortcuts — enabled gate', () => {
  it('does not fire when enabled=false', () => {
    const navigate = vi.fn()
    const onShowHelp = vi.fn()
    renderHook(() =>
      useKeyboardShortcuts({ navigate, onShowHelp, enabled: false }),
    { wrapper })
    fireKey('g')
    fireKey('f')
    fireKey('?')
    expect(navigate).not.toHaveBeenCalled()
    expect(onShowHelp).not.toHaveBeenCalled()
  })
})
