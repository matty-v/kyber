import { useAgents } from '../hooks/useAPI'
import { Skeleton } from '../components/Skeleton'
import { AgentStatusBar } from '../components/AgentStatusBar'
import { RecentlyActiveList } from '../components/RecentlyActiveList'
import { ContextPressureList } from '../components/ContextPressureList'
import { TerminalPeek } from '../components/TerminalPeek'

export function Dashboard() {
  const { data, isLoading, error } = useAgents()
  const agents = data ?? []

  return (
    <div className="space-y-6">
      <h1 className="text-xl font-bold text-text-primary">Dashboard</h1>

      {isLoading && (
        <div className="space-y-6">
          <Skeleton className="h-24 rounded-xl border border-border-subtle" />
          <div className="grid gap-4 lg:grid-cols-2">
            <Skeleton className="h-56 rounded-xl border border-border-subtle" />
            <Skeleton className="h-56 rounded-xl border border-border-subtle" />
          </div>
          <Skeleton className="h-72 rounded-xl border border-border-subtle" />
        </div>
      )}

      {error && (
        <div className="rounded-lg border border-danger/40 bg-danger-muted p-4 text-sm text-danger">
          Failed to load agents: {error.message}
        </div>
      )}

      {data && (
        <>
          <AgentStatusBar agents={agents} />
          <div className="grid gap-4 lg:grid-cols-2">
            <RecentlyActiveList agents={agents} />
            <ContextPressureList agents={agents} />
          </div>
          <TerminalPeek agents={agents} />
        </>
      )}
    </div>
  )
}
