import { useState } from 'react'
import { AlertTriangle, RefreshCw } from 'lucide-react'
import { Card } from './Card'
import { Button } from './Button'
import { ConfirmDialog } from './ConfirmDialog'
import { UpdateDialog, canInstallUpdate } from './UpdateDialog'
import {
  useUpdates,
  useSetUpdatePolicy,
  useCheckUpdates,
  useApplyUpdate,
} from '../hooks/useAPI'
import type { UpdateChannel, UpdateStatus } from '../lib/types'

/**
 * UpdatesCard — how an operator sees and controls what version this cluster
 * runs.
 *
 * The defaults it renders are the product decision (Matt, 2026-08-13):
 * published releases, and nothing installs itself. An operator can opt into
 * tracking every merge to main, and that opt-in carries a stability warning
 * because it means running code that has passed CI and nothing else.
 *
 * The card deliberately distinguishes three different "no" states that all
 * look alike if you collapse them:
 *
 *   - the control plane has no apply path at all (applySupported)
 *   - it has one, but THIS cluster may not use it (canSelfUpgrade — ArgoCD)
 *   - it may, but there is nothing newer to install
 *
 * Telling an operator "this feature doesn't exist" when the truth is "your
 * cluster is managed by something else" sends them to the wrong place.
 */
export function UpdatesCard() {
  const updates = useUpdates()
  const setPolicy = useSetUpdatePolicy()
  const check = useCheckUpdates()
  const apply = useApplyUpdate()
  const [confirmMainOpen, setConfirmMainOpen] = useState(false)
  const [confirmInstallOpen, setConfirmInstallOpen] = useState(false)

  if (updates.isLoading) {
    return (
      <Card className="max-w-3xl mt-4">
        <h2 className="text-sm font-medium text-text-muted mb-1">Updates</h2>
        <p className="text-xs text-text-muted">Checking…</p>
      </Card>
    )
  }

  // 503 means update checking is switched off on this install entirely.
  if (updates.error || !updates.data) {
    return (
      <Card className="max-w-3xl mt-4">
        <h2 className="text-sm font-medium text-text-muted mb-1">Updates</h2>
        <p className="text-xs text-text-muted">
          Update checking is not enabled on this control plane. Set{' '}
          <code className="text-text-muted">updates.enabled: true</code> in your values.
        </p>
      </Card>
    )
  }

  const s: UpdateStatus = updates.data
  const onMain = s.policy.channel === 'main'
  const pinned = Boolean(s.policy.pinnedVersion)
  const running = s.lastRun?.phase === 'pending' || s.lastRun?.phase === 'running'

  function chooseChannel(next: UpdateChannel) {
    if (next === s.policy.channel) return
    // Opting INTO main gets a confirmation; leaving it does not. The warning
    // is about what you are taking on, and going back to releases takes on
    // nothing.
    if (next === 'main') {
      setConfirmMainOpen(true)
      return
    }
    setPolicy.mutate({ channel: next })
  }

  return (
    <>
      <Card className="max-w-3xl mt-4">
        <h2 className="text-sm font-medium text-text-muted mb-1">Updates</h2>
        <p className="text-xs text-text-muted mb-3">
          What version this cluster runs, and how it takes new ones.
        </p>

        <dl className="grid grid-cols-2 gap-3 text-xs mb-4">
          <div>
            <dt className="text-text-disabled">Running</dt>
            <dd className="mt-1 text-text-primary font-mono">{s.currentVersion || 'unknown'}</dd>
          </div>
          <div>
            <dt className="text-text-disabled">Available</dt>
            <dd className="mt-1 text-text-primary font-mono">
              {s.latestVersion || '—'}
              {s.updateAvailable && (
                <span className="ml-2 font-sans text-accent">update available</span>
              )}
            </dd>
          </div>
        </dl>

        {s.lastError && (
          <p className="mb-3 text-xs text-warning">
            Last check failed: {s.lastError}
          </p>
        )}

        {/* Where updates come from. */}
        <section className="rounded-lg border border-border-default bg-surface-overlay p-4 mb-3">
          <h3 className="text-sm font-semibold text-text-primary mb-1">Source</h3>
          <div className="mt-2 space-y-2">
            <label className="flex items-start gap-2 cursor-pointer">
              <input
                type="radio"
                name="update-channel"
                className="mt-1"
                checked={!onMain}
                onChange={() => chooseChannel('stable')}
                disabled={setPolicy.isPending || running}
              />
              <span className="text-xs">
                <span className="text-text-primary">Published releases</span>
                <span className="block text-text-muted">
                  Tested versions, released deliberately. Recommended.
                </span>
              </span>
            </label>
            <label className="flex items-start gap-2 cursor-pointer">
              <input
                type="radio"
                name="update-channel"
                className="mt-1"
                checked={onMain}
                onChange={() => chooseChannel('main')}
                disabled={setPolicy.isPending || running}
              />
              <span className="text-xs">
                <span className="text-text-primary">Every change as it lands</span>
                <span className="block text-text-muted">
                  Tracks the latest merged code. Not recommended for anything you rely on.
                </span>
              </span>
            </label>
          </div>

          {onMain && (
            <p className="mt-3 flex items-start gap-2 rounded border border-warning/40 bg-warning/10 p-2 text-[11px] text-warning">
              <AlertTriangle className="h-3.5 w-3.5 shrink-0 mt-px" aria-hidden />
              <span>
                This cluster follows unreleased code. Changes arrive as soon as they
                are merged, without a release having been cut or soaked. Expect
                breakage, and don't run anything you depend on here.
              </span>
            </p>
          )}
        </section>

        {/* What happens when one is found. Manual is the default and the only
            mode this build honours; automatic is not implemented, so it is not
            offered — an option that silently did nothing would be worse than
            its absence. */}
        <section className="rounded-lg border border-border-default bg-surface-overlay p-4">
          <h3 className="text-sm font-semibold text-text-primary">Installing</h3>
          <p className="mt-1 text-xs text-text-muted">
            Updates are never installed for you. Start one here, or call{' '}
            <code className="text-text-muted">POST /api/v1/updates/apply</code> against
            this cluster.
          </p>

          {pinned && (
            <p className="mt-3 text-xs text-text-muted">
              Held at <span className="font-mono text-text-primary">{s.policy.pinnedVersion}</span>.
              Clear the hold to take updates.{' '}
              <button
                type="button"
                className="underline hover:text-text-primary"
                onClick={() => setPolicy.mutate({ pinnedVersion: null })}
                disabled={setPolicy.isPending || running}
              >
                Clear
              </button>
            </p>
          )}

          {!s.applySupported && (
            <p className="mt-3 text-xs text-text-muted">
              This control plane cannot install updates itself. Enable{' '}
              <code className="text-text-muted">selfUpgrade</code> in your values, or
              upgrade with Helm directly.
            </p>
          )}
          {s.applySupported && !s.canSelfUpgrade && s.reason && (
            <p className="mt-3 text-xs text-text-muted">{s.reason}</p>
          )}

          <div className="mt-3 flex items-center gap-2">
            <Button
              variant="secondary"
              size="md"
              onClick={() => check.mutate()}
              loading={check.isPending}
              disabled={check.isPending}
            >
              <RefreshCw className="h-3.5 w-3.5 mr-1" aria-hidden />
              Check now
            </Button>
            {canInstallUpdate(s) && (
              <Button
                variant="primary"
                size="md"
                onClick={() => setConfirmInstallOpen(true)}
                loading={apply.isPending}
                disabled={apply.isPending || running || !s.updateAvailable}
              >
                {running ? 'Installing…' : `Install ${s.latestVersion ?? ''}`.trim()}
              </Button>
            )}
          </div>

          {s.lastRun && (
            <div className="mt-3 text-xs">
              <span className="text-text-disabled">Last install: </span>
              <span className="font-mono text-text-primary">{s.lastRun.targetVersion}</span>
              <span className="text-text-muted"> — {s.lastRun.phase}</span>
              {s.lastRun.message && (
                <p className="mt-1 text-text-muted">{s.lastRun.message}</p>
              )}
            </div>
          )}
        </section>

        {s.lastChecked && (
          <p className="mt-3 text-[11px] text-text-disabled">
            Last checked {new Date(s.lastChecked).toLocaleString()}
          </p>
        )}
      </Card>

      <ConfirmDialog
        open={confirmMainOpen}
        title="Follow every change as it lands?"
        message={
          'This cluster will take code as soon as it is merged — no release, no soak, ' +
          'no guarantee it works. It is meant for a throwaway cluster you use to catch ' +
          'problems early, not for anything you depend on. You can switch back at any time.'
        }
        confirmLabel="I understand — track every change"
        cancelLabel="Cancel"
        dangerous
        loading={setPolicy.isPending}
        onConfirm={() => {
          setConfirmMainOpen(false)
          setPolicy.mutate({ channel: 'main' })
        }}
        onCancel={() => setConfirmMainOpen(false)}
      />

      {/*
        Shared with the header indicator on purpose. The consequence an operator
        actually feels is NOT "the control plane restarts" — it is "every agent
        loses its session" — and that warning has to be identical whichever way
        they arrived, or one of the two entry points is the dangerous one.
      */}
      <UpdateDialog
        open={confirmInstallOpen}
        status={s}
        onClose={() => setConfirmInstallOpen(false)}
      />
    </>
  )
}
