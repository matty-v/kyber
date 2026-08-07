import { describe, it, expect, vi } from 'vitest'
import { renderHook } from '@testing-library/react'
import { useCommandPaletteShortcut } from './useCommandPaletteShortcut'

function fireKey(opts: KeyboardEventInit & { key: string }) {
  const event = new KeyboardEvent('keydown', { bubbles: true, cancelable: true, ...opts })
  window.dispatchEvent(event)
  return event
}

describe('useCommandPaletteShortcut', () => {
  it('fires onToggle on Cmd+K (metaKey)', () => {
    const onToggle = vi.fn()
    renderHook(() => useCommandPaletteShortcut({ onToggle }))
    fireKey({ key: 'k', metaKey: true })
    expect(onToggle).toHaveBeenCalledTimes(1)
  })

  it('fires onToggle on Ctrl+K', () => {
    const onToggle = vi.fn()
    renderHook(() => useCommandPaletteShortcut({ onToggle }))
    fireKey({ key: 'k', ctrlKey: true })
    expect(onToggle).toHaveBeenCalledTimes(1)
  })

  it('matches uppercase K too (caps lock or shift-released-late edge)', () => {
    const onToggle = vi.fn()
    renderHook(() => useCommandPaletteShortcut({ onToggle }))
    fireKey({ key: 'K', metaKey: true })
    expect(onToggle).toHaveBeenCalledTimes(1)
  })

  it('ignores plain k without a modifier', () => {
    const onToggle = vi.fn()
    renderHook(() => useCommandPaletteShortcut({ onToggle }))
    fireKey({ key: 'k' })
    expect(onToggle).not.toHaveBeenCalled()
  })

  it('ignores Ctrl+Shift+K (browser devtools)', () => {
    const onToggle = vi.fn()
    renderHook(() => useCommandPaletteShortcut({ onToggle }))
    fireKey({ key: 'k', ctrlKey: true, shiftKey: true })
    expect(onToggle).not.toHaveBeenCalled()
  })

  it('ignores Alt+K', () => {
    const onToggle = vi.fn()
    renderHook(() => useCommandPaletteShortcut({ onToggle }))
    fireKey({ key: 'k', altKey: true })
    expect(onToggle).not.toHaveBeenCalled()
  })

  it('preventDefault is called so the browser does not steal Ctrl+K (omnibox)', () => {
    const onToggle = vi.fn()
    renderHook(() => useCommandPaletteShortcut({ onToggle }))
    const event = fireKey({ key: 'k', ctrlKey: true })
    expect(event.defaultPrevented).toBe(true)
  })

  it('cleans up its listener on unmount', () => {
    const onToggle = vi.fn()
    const { unmount } = renderHook(() => useCommandPaletteShortcut({ onToggle }))
    unmount()
    fireKey({ key: 'k', metaKey: true })
    expect(onToggle).not.toHaveBeenCalled()
  })

  it('does not fire while disabled (e.g., help overlay open)', () => {
    const onToggle = vi.fn()
    renderHook(() => useCommandPaletteShortcut({ onToggle, enabled: false }))
    fireKey({ key: 'k', metaKey: true })
    expect(onToggle).not.toHaveBeenCalled()
  })

  it('re-attaches its listener when enabled flips back to true', () => {
    const onToggle = vi.fn()
    const { rerender } = renderHook(
      ({ enabled }: { enabled: boolean }) => useCommandPaletteShortcut({ onToggle, enabled }),
      { initialProps: { enabled: false } },
    )
    fireKey({ key: 'k', metaKey: true })
    expect(onToggle).not.toHaveBeenCalled()
    rerender({ enabled: true })
    fireKey({ key: 'k', metaKey: true })
    expect(onToggle).toHaveBeenCalledTimes(1)
  })
})
