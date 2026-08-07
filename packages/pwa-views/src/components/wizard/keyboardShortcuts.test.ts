import { describe, it, expect, vi } from 'vitest'
import { renderHook } from '@testing-library/react'
import { useWizardKeyboardShortcuts } from './keyboardShortcuts'

function fireKey(key: string, target?: HTMLElement) {
  const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true })
  if (target) {
    Object.defineProperty(event, 'target', { value: target, writable: false })
  }
  window.dispatchEvent(event)
  return event
}

describe('useWizardKeyboardShortcuts', () => {
  it('Esc fires onEsc', () => {
    const onEsc = vi.fn()
    const onEnter = vi.fn()
    renderHook(() =>
      useWizardKeyboardShortcuts({ onEsc, onEnter, isCurrentStepValid: true, enabled: true }),
    )
    fireKey('Escape')
    expect(onEsc).toHaveBeenCalledTimes(1)
    expect(onEnter).not.toHaveBeenCalled()
  })

  it('Enter fires onEnter when current step is valid and focus is not in a textarea', () => {
    const onEsc = vi.fn()
    const onEnter = vi.fn()
    renderHook(() =>
      useWizardKeyboardShortcuts({ onEsc, onEnter, isCurrentStepValid: true, enabled: true }),
    )
    fireKey('Enter')
    expect(onEnter).toHaveBeenCalledTimes(1)
    expect(onEsc).not.toHaveBeenCalled()
  })

  it('Enter is a no-op when focus is in a textarea', () => {
    const onEsc = vi.fn()
    const onEnter = vi.fn()
    renderHook(() =>
      useWizardKeyboardShortcuts({ onEsc, onEnter, isCurrentStepValid: true, enabled: true }),
    )
    const textarea = document.createElement('textarea')
    fireKey('Enter', textarea)
    expect(onEnter).not.toHaveBeenCalled()
  })

  it('Enter is a no-op when focus is in a select', () => {
    const onEsc = vi.fn()
    const onEnter = vi.fn()
    renderHook(() =>
      useWizardKeyboardShortcuts({ onEsc, onEnter, isCurrentStepValid: true, enabled: true }),
    )
    const select = document.createElement('select')
    fireKey('Enter', select)
    expect(onEnter).not.toHaveBeenCalled()
  })

  it('Enter is a no-op when isCurrentStepValid is false', () => {
    const onEsc = vi.fn()
    const onEnter = vi.fn()
    renderHook(() =>
      useWizardKeyboardShortcuts({ onEsc, onEnter, isCurrentStepValid: false, enabled: true }),
    )
    fireKey('Enter')
    expect(onEnter).not.toHaveBeenCalled()
  })

  it('neither Esc nor Enter fires when enabled is false', () => {
    const onEsc = vi.fn()
    const onEnter = vi.fn()
    renderHook(() =>
      useWizardKeyboardShortcuts({ onEsc, onEnter, isCurrentStepValid: true, enabled: false }),
    )
    fireKey('Escape')
    fireKey('Enter')
    expect(onEsc).not.toHaveBeenCalled()
    expect(onEnter).not.toHaveBeenCalled()
  })
})
