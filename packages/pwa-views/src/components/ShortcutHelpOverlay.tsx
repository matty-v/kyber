import { useEffect, useId } from 'react'
import { SHORTCUT_BINDINGS } from '../hooks/useKeyboardShortcuts'

interface Props {
  open: boolean
  onClose: () => void
}

// ShortcutHelpOverlay renders the keyboard-shortcut cheat sheet that pops up
// when the operator presses `?`. Bindings are sourced from
// useKeyboardShortcuts.SHORTCUT_BINDINGS so the displayed list and the
// keydown handler can't drift apart.
//
// Mirrors the ConfirmDialog pattern: full-bleed backdrop + a centered
// role="dialog" panel. Esc closes; pressing `?` again also closes (handled by
// the global hook, which Layout disables while open — so a second `?` here
// short-circuits via the local listener below).
export function ShortcutHelpOverlay({ open, onClose }: Props) {
  const titleId = useId()

  useEffect(() => {
    if (!open) return
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape' || e.key === '?') {
        e.preventDefault()
        onClose()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div
        className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm"
        onClick={onClose}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="relative z-10 w-full max-w-sm rounded-xl border border-border-default bg-surface-overlay p-6 shadow-xl"
      >
        <h2
          id={titleId}
          className="font-display text-base font-semibold text-text-primary"
        >
          Keyboard shortcuts
        </h2>
        <dl className="mt-4 space-y-2 text-sm">
          {SHORTCUT_BINDINGS.map((b) => (
            <div key={b.keys} className="flex items-center justify-between gap-4">
              <dt className="text-text-muted">{b.description}</dt>
              <dd>
                <kbd className="rounded-md border border-border-default bg-surface-raised px-2 py-0.5 font-mono text-xs text-text-primary">
                  {b.keys}
                </kbd>
              </dd>
            </div>
          ))}
        </dl>
        <p className="mt-4 text-xs text-text-muted">
          Shortcuts are inactive while typing in a text field.
        </p>
      </div>
    </div>
  )
}
