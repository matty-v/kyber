import { useEffect } from 'react'

export interface UseWizardKeyboardShortcutsOptions {
  /** Called when Esc is pressed. The orchestrator wires this to its Cancel handler. */
  onEsc: () => void
  /** Called when Enter is pressed AND the current step is valid AND focus is not in a textarea. */
  onEnter: () => void
  /** Whether the current step's validator passes. Enter is a no-op when false. */
  isCurrentStepValid: boolean
  /**
   * When false, the hook does not register its keydown listener. The orchestrator
   * passes `enabled={!dialogOpen}` so that while the discard-changes dialog is
   * open, the dialog's own Esc handler is the only one that fires (avoids a
   * race where Esc both closes the dialog AND tries to re-open it).
   */
  enabled: boolean
}

/**
 * Wires Esc → onEsc and Enter → onEnter for the wizard.
 *
 * Skip rules for Enter:
 * - Focus is in a <textarea>: avoid hijacking soulDescription's multiline typing
 * - Focus is in a <select>: native browser behavior on a focused select is
 *   browser-dependent (some confirm a highlighted option, some open the
 *   dropdown). Don't fight it — let the browser handle Enter on selects.
 * - isCurrentStepValid === false: silent no-op rather than firing a navigation
 *   that would just bounce off the disabled-Next gate
 *
 * Esc fires unconditionally (when enabled) — the orchestrator's Cancel handler
 * decides whether to navigate or open the discard-changes dialog based on its
 * `dirty` flag.
 */
export function useWizardKeyboardShortcuts({
  onEsc,
  onEnter,
  isCurrentStepValid,
  enabled,
}: UseWizardKeyboardShortcutsOptions): void {
  useEffect(() => {
    if (!enabled) return
    function handleKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        onEsc()
        return
      }
      if (e.key === 'Enter') {
        const target = e.target as HTMLElement | null
        if (target?.tagName === 'TEXTAREA' || target?.tagName === 'SELECT') return
        if (!isCurrentStepValid) return
        e.preventDefault()
        onEnter()
      }
    }
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  }, [onEsc, onEnter, isCurrentStepValid, enabled])
}
