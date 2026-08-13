import { Loader2 } from 'lucide-react'
import { useUpgradeProgress } from '../hooks/useUpgradeProgress'

/**
 * UpgradeBanner — the persistent, app-wide "an upgrade is happening" state.
 *
 * This exists instead of a blocking overlay, deliberately. During an upgrade the
 * control plane is the process being replaced *and* the thing serving this page,
 * so the UI stops responding for roughly a minute whether or not we ask it to.
 * An overlay would add nothing to that, and — being component state — it would
 * be wiped by the reload anyway.
 *
 * What is actually missing without this banner is not a block but an
 * *explanation*: today the page simply hangs, which reads as a bug rather than
 * as the upgrade the operator just asked for. So the banner is driven entirely
 * by the polled `lastRun`, which means it renders correctly on a fresh page load
 * in the middle of an upgrade — the case that matters most, and the one local
 * state cannot serve.
 *
 * The reconnecting state is called out as expected rather than as an error for
 * the same reason: an operator who reads "connection lost" mid-upgrade will
 * start debugging a cluster that is working exactly as intended.
 */
export function UpgradeBanner() {
  const { inFlight, reconnecting, targetVersion } = useUpgradeProgress()

  if (!inFlight) return null

  // The Job's target label is read straight from job.Labels with no fallback,
  // so a Job written by an older control plane yields an empty string — very
  // plausible here, since the control plane is the thing being upgraded.
  const version = targetVersion || 'the new version'

  return (
    <div
      role="status"
      aria-live="polite"
      data-testid="upgrade-banner"
      className="flex items-center gap-2 border-b border-accent/30 bg-accent-muted px-4 py-2 text-xs text-text-primary"
    >
      <Loader2 className="h-3.5 w-3.5 shrink-0 animate-spin text-accent" aria-hidden />
      {reconnecting ? (
        <span>
          <strong>Installing {version}</strong> — the control plane is restarting, so this
          page is briefly disconnected. This is expected; it will reconnect on its own.
        </span>
      ) : (
        <span>
          <strong>Installing {version}</strong> — agents are being restarted a few at a
          time and will lose their current sessions. Avoid starting new work until this finishes.
        </span>
      )}
    </div>
  )
}
