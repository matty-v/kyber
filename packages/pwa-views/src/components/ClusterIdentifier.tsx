import { useState } from 'react'
import { RotateCw } from 'lucide-react'
import { useCluster } from '../lib/cluster-context'
import { useLiveVersion } from '../hooks/useLiveVersion'
import { useUpdates } from '../hooks/useAPI'
import { UpdateDialog } from './UpdateDialog'

interface Props {
  /** When true, renders inline (for mobile header). When false (default), block with top margin. */
  inline?: boolean
}

export function ClusterIdentifier({ inline = false }: Props) {
  const cluster = useCluster()
  const { unreachable, liveChartVersion } = useLiveVersion()
  // Owning this query here — rather than only on the Settings card — is what
  // makes the indicator work at all. The card is where update policy is set,
  // but it is not where an operator spends their time, and an update nobody is
  // looking at is not a notification.
  const updates = useUpdates()
  const [dialogOpen, setDialogOpen] = useState(false)

  const name = cluster.name || '—'
  // Show what the cluster is running NOW, not the version this tab loaded with.
  // These differ for the whole window between an upgrade completing and someone
  // reloading, and during that window the old number is simply wrong — the
  // header is where an operator looks to confirm an upgrade actually landed.
  // Falls back to the load-time version until the first poll answers.
  const version = liveChartVersion ?? cluster.version

  const status = updates.data
  // An upgrade already in flight owns the screen through the app-wide banner,
  // and updateAvailable stays true until the new version is actually serving.
  // Two things announcing the same upgrade is worse than one.
  const upgrading = status?.lastRun?.phase === 'pending' || status?.lastRun?.phase === 'running'
  const updateAvailable = Boolean(status?.updateAvailable) && !upgrading

  if (unreachable) {
    return (
      <span
        className={`${inline ? '' : 'mt-1 block'} max-w-full truncate font-mono text-[10px] text-text-disabled`}
        title={`${name} · version unavailable`}
        data-testid="cluster-identifier"
      >
        {name} · version unavailable
      </span>
    )
  }

  return (
    <>
      <span
        className={`${inline ? '' : 'mt-1 block'} flex items-center gap-1 font-mono text-[10px] text-text-muted`}
        data-testid="cluster-identifier"
      >
        <span className="max-w-full truncate" title={`${name} ${version}`}>
          {name} {version}
        </span>
        {updateAvailable && (
          <button
            type="button"
            data-testid="cluster-identifier-action"
            onClick={() => setDialogOpen(true)}
            className="shrink-0 rounded p-0.5 text-accent hover:bg-accent-muted focus:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring"
            title={`${status?.latestVersion ?? 'A new version'} is available. See what's in it.`}
            aria-label={`Version ${status?.latestVersion ?? 'update'} is available. Open update details.`}
          >
            <RotateCw className="h-3 w-3" />
          </button>
        )}
      </span>

      {/*
        Gated on updateAvailable, not on status — the same condition as the
        icon. UpdateDialog calls useAgents() at the top level, so mounting it
        whenever a status exists would run a full agent-list poll every 30s from
        the header, on every page, forever, for a dialog that can never open on
        an up-to-date cluster. Mounting it alongside the icon keeps the agent
        list warm exactly while the dialog is reachable.
      */}
      {status && updateAvailable && (
        <UpdateDialog open={dialogOpen} status={status} onClose={() => setDialogOpen(false)} />
      )}
    </>
  )
}
