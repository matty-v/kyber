import { useEffect, useId, type ReactNode } from 'react'
import { Button } from './Button'

interface Props {
  open: boolean
  title: string
  /**
   * ReactNode rather than string so a confirmation can carry structure — an
   * ordered list of consequences reads very differently from the same words
   * run together in a paragraph, and the upgrade warning depends on that.
   * Plain strings still work and render identically.
   */
  message: ReactNode
  confirmLabel?: string
  cancelLabel?: string
  /**
   * Drops the confirm button, leaving cancel as the only way out. For a dialog
   * that has something to SAY but nothing to authorize — an update an operator
   * has been told about on a cluster that may not install it. A confirm button
   * wired to "close" would read as consent to something.
   */
  hideConfirm?: boolean
  dangerous?: boolean
  loading?: boolean
  onConfirm: () => void
  onCancel: () => void
}

export function ConfirmDialog({
  open,
  title,
  message,
  confirmLabel = 'Confirm',
  cancelLabel = 'Cancel',
  hideConfirm = false,
  dangerous = false,
  loading = false,
  onConfirm,
  onCancel,
}: Props) {
  const titleId = useId()

  useEffect(() => {
    if (!open) return
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape' && !loading) onCancel()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, loading, onCancel])

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div
        className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm"
        onClick={onCancel}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        // max-h + scroll: `message` became a ReactNode, and the install warning
        // (list + agent IDs) can exceed a phone viewport. The panel is
        // vertically centred in a non-scrolling overlay, so without this it
        // overflows off both ends and the buttons become unreachable.
        className="relative z-10 flex max-h-[90vh] w-full max-w-sm flex-col overflow-y-auto rounded-xl border border-border-default bg-surface-overlay p-6 shadow-xl"
      >
        <h2 id={titleId} className="font-display text-base font-semibold text-text-primary">{title}</h2>
        {/* div, not p: a p may not legally contain the lists a structured
            confirmation uses, and React will warn about the nesting. */}
        <div className="mt-2 text-sm text-text-muted">{message}</div>
        <div className="mt-5 flex gap-3 justify-end">
          <Button variant="ghost" size="sm" onClick={onCancel} disabled={loading}>
            {cancelLabel}
          </Button>
          {!hideConfirm && (
            <Button
              variant={dangerous ? 'danger' : 'primary'}
              size="sm"
              onClick={onConfirm}
              loading={loading}
            >
              {confirmLabel}
            </Button>
          )}
        </div>
      </div>
    </div>
  )
}
