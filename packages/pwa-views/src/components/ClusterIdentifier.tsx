import { RotateCw } from 'lucide-react'
import { useCluster } from '../lib/cluster-context'
import { useLiveVersion } from '../hooks/useLiveVersion'

interface Props {
  /** When true, renders inline (for mobile header). When false (default), block with top margin. */
  inline?: boolean
}

export function ClusterIdentifier({ inline = false }: Props) {
  const cluster = useCluster()
  const { isStale, unreachable } = useLiveVersion()

  const name = cluster.name || '—'
  const version = cluster.version

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
    <span
      className={`${inline ? '' : 'mt-1 block'} flex items-center gap-1 font-mono text-[10px] text-text-muted`}
      data-testid="cluster-identifier"
    >
      <span className="max-w-full truncate" title={`${name} ${version}`}>
        {name} {version}
      </span>
      {isStale && (
        <button
          type="button"
          onClick={() => window.location.reload()}
          className="shrink-0 rounded p-0.5 text-accent hover:bg-accent-muted focus:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring"
          title="New version available — refresh"
          aria-label="New version available — refresh"
        >
          <RotateCw className="h-3 w-3" />
        </button>
      )}
    </span>
  )
}
