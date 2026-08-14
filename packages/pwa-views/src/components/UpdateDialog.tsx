import { ExternalLink } from 'lucide-react'
import { ConfirmDialog } from './ConfirmDialog'
import { useAgents, useApplyUpdate } from '../hooks/useAPI'
import { formatAgo } from '../lib/dashboard'
import type { AgentPhase, UpdateStatus } from '../lib/types'

/**
 * UpdateDialog — the one place Kyber describes an available update and asks
 * whether to take it.
 *
 * It exists as a component rather than as markup inside the Settings card
 * because there are now two ways in: the card, and the indicator beside the
 * version in the header. Those two must state the same consequences in the
 * same words. Two copies of a warning drift, and the moment they drift one of
 * the entry points is quietly the dangerous one.
 *
 * The dialog is deliberately not the same shape in both directions. When this
 * cluster may install the update it is a confirmation, and the thing being
 * confirmed is not "the control plane restarts" — it is "every agent loses its
 * session". When the cluster may NOT install it (ArgoCD owns it, self-upgrade
 * is off, the version is pinned) it becomes an announcement with nothing to
 * confirm: the operator still needs to know a newer version exists, and still
 * needs to be sent somewhere they can act on it.
 */

interface Props {
  open: boolean
  status: UpdateStatus
  onClose: () => void
}

/**
 * Agents that will actually lose work. Stopped agents are excluded because
 * restarting them costs nothing, but anything mid-boot IS rolled and loses that
 * boot — counting only 'Running' understates the radius on a cluster that is
 * actively spinning agents up.
 */
const AT_RISK: AgentPhase[] = ['Running', 'Starting', 'Creating', 'Restarting']

/** Whether pressing Install on THIS cluster would do anything. */
export function canInstallUpdate(s: UpdateStatus): boolean {
  return s.applySupported && s.canSelfUpgrade && !s.policy.pinnedVersion
}

/**
 * Why the install button is absent, in the operator's terms.
 *
 * The checker supplies `reason` for ownership and pinning, but says nothing
 * when the control plane simply has no applier wired up — that is a build-time
 * choice, not a cluster state, so the explanation lives here.
 */
function whyNotInstallable(s: UpdateStatus): string {
  if (s.reason) return s.reason
  if (!s.applySupported) {
    return 'This control plane cannot install updates itself. Enable selfUpgrade in your values, or upgrade with Helm directly.'
  }
  return 'This cluster will not install the update itself.'
}

/** "12 Aug 2026 · 2d ago", or null when the source publishes no date. */
function formatReleased(iso: string | undefined, now: number): string | null {
  if (!iso) return null
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return null
  const date = new Date(t).toLocaleDateString(undefined, {
    day: 'numeric',
    month: 'short',
    year: 'numeric',
  })
  return `${date} · ${formatAgo(iso, now)}`
}

export function UpdateDialog({ open, status, onClose }: Props) {
  const agents = useAgents()
  const apply = useApplyUpdate()

  const target = status.latestVersion ?? 'the new version'
  const installable = canInstallUpdate(status)
  const onMain = status.policy.channel === 'main'
  const released = formatReleased(status.latestPublishedAt, Date.now())

  const liveAgents = (agents.data ?? []).filter((a) => AT_RISK.includes(a.phase))
  // "no agents" and "we couldn't find out" are different answers, and only one
  // of them justifies telling the operator nothing is at risk.
  const agentsUnknown = agents.isLoading || agents.isError

  // What the operator is being asked about, before what it costs. An update is
  // a decision about a specific version released on a specific day, and until
  // now the UI offered neither fact at the moment of choosing.
  const facts = (
    <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-xs">
      <dt className="text-text-disabled">Version</dt>
      <dd className="font-mono text-text-primary">
        {status.currentVersion || 'unknown'} → {status.latestVersion ?? '—'}
      </dd>

      {released && (
        <>
          <dt className="text-text-disabled">Released</dt>
          <dd className="text-text-primary">{released}</dd>
        </>
      )}

      {status.latestUrl && (
        <>
          <dt className="text-text-disabled">{onMain ? 'Chart' : 'Notes'}</dt>
          <dd>
            <a
              href={status.latestUrl}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-accent hover:text-text-primary"
            >
              {onMain ? 'View the chart package' : `What changed in ${target}`}
              <ExternalLink className="h-3 w-3" aria-hidden />
            </a>
          </dd>
        </>
      )}
    </dl>
  )

  // On `main` there is no release and therefore no notes and no date — the
  // chart registry publishes neither. Saying so is better than an operator
  // wondering which part of the UI failed to load.
  const noNotes = onMain && !released && (
    <p className="mt-3 text-xs text-text-disabled">
      This is an unreleased build from the main branch. There are no release
      notes and no publish date for it — that is what tracking every change means.
    </p>
  )

  const consequences = (
    <>
      <p className="mt-4">This restarts the whole cluster, in this order:</p>
      <ol className="mt-2 list-decimal space-y-1 pl-5">
        <li>
          The control plane restarts. <strong>This page goes offline for about a
          minute</strong> and comes back on the new version.
        </li>
        <li>
          Then every agent restarts, a few at a time.{' '}
          <strong>Each one loses its current session and any work in progress.</strong>{' '}
          They do not pick up where they left off.
        </li>
      </ol>
      {agentsUnknown ? (
        <p className="mt-3 text-warning">
          Couldn't check which agents are running — assume any that are will be
          restarted and lose their sessions.
        </p>
      ) : (
        liveAgents.length > 0 && (
          <p className="mt-3">
            Working right now:{' '}
            <span className="font-mono text-text-primary">
              {liveAgents.map((a) => a.id).join(', ')}
            </span>
          </p>
        )
      )}
      <p className="mt-3 text-text-disabled">
        If the upgrade fails, Kyber returns the cluster to {status.currentVersion} on its own.
      </p>
    </>
  )

  return (
    <ConfirmDialog
      open={open}
      title={installable ? `Install ${target}?` : `${target} is available`}
      message={
        <>
          {facts}
          {noNotes}
          {installable ? (
            consequences
          ) : (
            <p className="mt-4">{whyNotInstallable(status)}</p>
          )}
        </>
      }
      confirmLabel={
        !agentsUnknown && liveAgents.length > 0
          ? `Install — restart ${liveAgents.length} ${liveAgents.length === 1 ? 'agent' : 'agents'}`
          : 'Install'
      }
      // "Not now" rather than "Cancel": nothing is being abandoned, and the
      // update stays available. On the announcement variant it is the only
      // button, so it says what it does.
      cancelLabel={installable ? 'Not now' : 'Close'}
      hideConfirm={!installable}
      dangerous
      loading={apply.isPending}
      onConfirm={() => {
        onClose()
        // Pin the version the dialog just named. With no version the server
        // resolves latestVersion at request time, so a check landing between
        // render and confirm would install something other than what the
        // operator agreed to.
        apply.mutate(status.latestVersion)
      }}
      onCancel={onClose}
    />
  )
}
