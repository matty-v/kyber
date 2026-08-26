import { useEffect, useState } from 'react'
import { toast } from 'sonner'

import { useCodexDeviceAuthStatus, useStartCodexDeviceAuth } from '../hooks/useAPI'
import type { AgentPhase } from '../lib/types'
import { Button } from './Button'
import { Card } from './Card'

/**
 * Codex device login, rendered natively.
 *
 * Kyber runs `codex login --device-auth` inside the agent. It prints a link and
 * a one-time code, and an operator has to carry both into a browser. This panel
 * shows them directly — an anchor they can click and a code they can copy in
 * one tap — instead of embedding a read-only terminal and asking them to select
 * text out of it on a phone.
 *
 * The states are the flow's, not the UI's: `absent` means nothing is running,
 * `starting` covers both a booting pod and a session that has not drawn its
 * prompt, `ready` has a usable code, `expired` had one.
 */

interface Props {
  name: string
  phase: AgentPhase
}

/** Whole seconds left, or null when there is no trustworthy deadline. */
function secondsUntil(iso: string | undefined, now: number): number | null {
  if (!iso) return null
  const at = Date.parse(iso)
  if (Number.isNaN(at)) return null
  return Math.max(0, Math.round((at - now) / 1000))
}

function formatCountdown(seconds: number): string {
  const m = Math.floor(seconds / 60)
  const s = seconds % 60
  return `${m}:${String(s).padStart(2, '0')}`
}

export function CodexDeviceAuthPanel({ name, phase }: Props) {
  const startLogin = useStartCodexDeviceAuth()

  // Only poll while a login could plausibly be running. Each poll execs into
  // the agent pod, so an always-on query here would be a steady stream of
  // execs against every Codex agent in the fleet.
  const polling = phase === 'Starting' || phase === 'NeedsAuth'
  const { data, isLoading } = useCodexDeviceAuthStatus(name, polling)

  // Drives the countdown only. The server hands back an absolute deadline, so
  // this never decides whether a code is valid — it just re-renders the clock.
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (data?.state !== 'ready' || !data.expiresAt) return
    const deadline = Date.parse(data.expiresAt)
    if (Number.isNaN(deadline)) return
    const t = setInterval(() => {
      const tick = Date.now()
      setNow(tick)
      // Past the deadline the clock has nothing left to say, and the panel has
      // already flipped to `expired`. Stop rather than re-render once a second
      // for as long as the page stays open.
      if (tick >= deadline) clearInterval(t)
    }, 1000)
    return () => clearInterval(t)
  }, [data?.state, data?.expiresAt])

  const remaining = secondsUntil(data?.expiresAt, now)
  // Trust the clock once it runs out rather than waiting for the next poll —
  // polling stops on `ready`, so nothing else would notice the expiry.
  const expired = data?.state === 'expired' || (data?.state === 'ready' && remaining === 0)
  const ready = data?.state === 'ready' && !expired

  async function copyCode(code: string) {
    try {
      await navigator.clipboard.writeText(code)
      toast.success('Code copied')
    } catch {
      toast.error('Could not copy', { description: 'Clipboard access blocked — copy manually.' })
    }
  }

  // `absent` only means "no login session in the pod right now", and what that
  // implies depends on why we are looking:
  //
  //   NeedsAuth, nothing asked for — the resting state. Offer the button.
  //   Starting — the pod is booting toward a login and has not reached the
  //     `codex login` step yet, which is the first several seconds of every
  //     boot. Offering the button here hands the operator a destructive
  //     restart in the middle of the boot that is about to print their code.
  //   Just after a start was accepted — the flow is on its way whatever this
  //     one poll caught, and the pod is very likely still the old one. The
  //     button reappearing here invites a second click that wipes the auth
  //     Secret and restarts the agent all over again.
  //
  // All three of those are "wait", not "ask again".
  const startRequested = startLogin.isPending || startLogin.isSuccess
  const bootingTowardLogin = data?.state === 'absent' && phase === 'Starting'
  // `failed` means Kyber could not read the flow at all. It outranks every
  // "wait" below, because waiting is exactly the wrong advice — this one does
  // not fix itself, and a spinner over it is how the panel's own broken probe
  // went unnoticed for a release.
  const failed = data?.state === 'failed'
  const waiting =
    !ready &&
    !expired &&
    !failed &&
    (isLoading || data?.state === 'starting' || bootingTowardLogin || startRequested)
  const idle = !waiting && !ready && !expired && !failed

  return (
    <Card className="border-accent/40 bg-accent/10">
      <h2 className="mb-1 text-sm font-semibold text-text-primary">
        {phase === 'NeedsAuth' ? 'Codex login required' : 'Finish Codex device login'}
      </h2>

      {idle && (
        <>
          <p className="mb-3 text-xs text-text-muted">
            Kyber signs this agent in with a one-time code. Start the login and the link and
            code will appear here.
          </p>
          <Button
            type="button"
            variant="primary"
            size="sm"
            loading={startLogin.isPending}
            onClick={() => startLogin.mutate(name)}
          >
            Start device login
          </Button>
        </>
      )}

      {waiting && (
        <div className="flex items-center gap-2 py-2 text-xs text-text-muted">
          <span
            className="h-3.5 w-3.5 animate-spin rounded-full border-2 border-border-default border-t-accent"
            aria-hidden="true"
          />
          <span role="status">Starting login — your code will appear here…</span>
        </div>
      )}

      {ready && (
        <div className="space-y-3">
          <ol className="space-y-3 text-xs text-text-muted">
            <li>
              <span className="mb-1 block">1. Open this page and sign in:</span>
              <a
                href={data?.verificationUrl}
                target="_blank"
                rel="noreferrer noopener"
                className="break-all font-medium text-accent underline underline-offset-2 hover:brightness-110"
              >
                {data?.verificationUrl}
              </a>
            </li>
            <li>
              <span className="mb-1 block">2. Enter this code:</span>
              <div className="flex flex-wrap items-center gap-2">
                <code className="select-all rounded-lg bg-surface-overlay px-3 py-2 font-mono text-lg tracking-widest text-text-primary">
                  {data?.userCode}
                </code>
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  onClick={() => data?.userCode && void copyCode(data.userCode)}
                >
                  Copy code
                </Button>
              </div>
            </li>
          </ol>
          <p className="text-xs text-text-muted">
            {remaining === null
              ? 'Waiting for you to approve it in your browser…'
              : `Waiting for you to approve it in your browser — this code expires in ${formatCountdown(remaining)}.`}
          </p>
        </div>
      )}

      {failed && (
        <>
          <p className="mb-1 text-xs text-text-primary">
            Kyber couldn&apos;t read the login from this agent.
          </p>
          {data?.detail && (
            <pre className="mb-3 overflow-x-auto whitespace-pre-wrap break-words rounded-lg bg-surface-overlay px-3 py-2 font-mono text-[11px] text-text-muted">
              {data.detail}
            </pre>
          )}
          <p className="mb-3 text-xs text-text-muted">
            The login may still be running inside the agent — the Terminal tab&apos;s device-login
            view shows it directly.
          </p>
          <Button
            type="button"
            variant="secondary"
            size="sm"
            loading={startLogin.isPending}
            onClick={() => startLogin.mutate(name)}
          >
            Start again
          </Button>
        </>
      )}

      {expired && (
        <>
          <p className="mb-3 text-xs text-text-muted">
            That code expired before the login finished. Codes are only good for a few minutes.
          </p>
          <Button
            type="button"
            variant="primary"
            size="sm"
            loading={startLogin.isPending}
            onClick={() => startLogin.mutate(name)}
          >
            Start again
          </Button>
        </>
      )}
    </Card>
  )
}
