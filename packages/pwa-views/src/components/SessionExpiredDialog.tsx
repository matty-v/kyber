import { useEffect, useId, useState } from 'react'
import { Button } from './Button'
import { Input } from './ui/input'
import { establishEmbeddedBrowserSession, onSessionExpired } from '../lib/api'

/** Prompts for a missing, expired or retired key session. Signed sessions
 * survive control-plane restarts; every request resolves current key access.
 * Embedded mode only; bearer connection credentials belong to the host app.
 */
export function SessionExpiredDialog() {
  const [open, setOpen] = useState(false)
  const [apiKey, setApiKey] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
  const titleId = useId()
  const fieldId = useId()

  useEffect(() => onSessionExpired(() => setOpen(true)), [])

  // Deliberately NOT Escape-dismissable and no backdrop-click-to-close.
  // Dismissing returns the operator to the same broken page with the same
  // unactionable error — the state this dialog exists to replace. The only
  // exits are a working key or a reload.

  async function reconnect() {
    if (!apiKey || submitting) return
    setSubmitting(true)
    setError('')
    try {
      await establishEmbeddedBrowserSession(apiKey)
      // Full reload rather than closing the dialog: every query in the tree
      // failed while the session was dead, and several are not retried on
      // their own. A reload is the honest way to get a consistent page, and
      // costs a second.
      window.location.reload()
    } catch {
      setError('That key was rejected, or the control plane is unreachable.')
      setSubmitting(false)
    }
  }

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm" />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="relative z-10 flex max-h-[90vh] w-full max-w-sm flex-col overflow-y-auto rounded-xl border border-border-default bg-surface-overlay p-6 shadow-xl"
      >
        <h2 id={titleId} className="font-display text-base font-semibold text-text-primary">
          Session expired
        </h2>
        <p className="mt-2 text-sm text-text-muted">
          Sign in again with a Kyber access key.
        </p>
        <form
          className="mt-4"
          onSubmit={(e) => {
            e.preventDefault()
            void reconnect()
          }}
        >
          <label htmlFor={fieldId} className="block text-sm text-text-muted mb-1">
            API Key
          </label>
          <Input
            id={fieldId}
            type="password"
            autoFocus
            autoComplete="off"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            disabled={submitting}
            aria-label="API Key"
          />
          {error && (
            <p role="alert" className="mt-2 text-xs text-status-danger">
              {error}
            </p>
          )}
          <div className="mt-5 flex justify-end">
            <Button type="submit" variant="primary" size="sm" loading={submitting} disabled={!apiKey}>
              Reconnect
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}
