import { Bot } from 'lucide-react'
import type { Agent } from '../lib/types'
import { phaseCounts } from '../lib/dashboard'
import { phaseStyle, toneBarClasses } from '../lib/design/status'
import { StatusBadge } from './StatusBadge'
import { Card } from './Card'
import { EmptyState } from './EmptyState'

export function AgentStatusBar({ agents }: { agents: Agent[] }) {
  const total = agents.length
  const counts = phaseCounts(agents)

  if (total === 0) {
    return (
      <Card>
        <EmptyState title="No agents" description="Create an agent to see fleet status." />
      </Card>
    )
  }

  return (
    <Card>
      <div className="flex items-center gap-2">
        <Bot className="h-4 w-4 text-accent" strokeWidth={1.5} />
        <span className="font-mono text-[10px] uppercase tracking-[0.2em] text-text-muted">
          Agent status
        </span>
        <span className="ml-auto text-xs text-text-muted tabular-nums">{total} total</span>
      </div>

      <div
        className="mt-3 flex h-3 gap-0.5 overflow-hidden rounded-full bg-surface-sunken"
        role="img"
        aria-label="Agent phase distribution"
      >
        {counts.map(({ phase, count }) => (
          <div
            key={phase}
            className={toneBarClasses[phaseStyle(phase).tone]}
            style={{ flexGrow: count }}
            title={`${phase}: ${count}`}
          />
        ))}
      </div>

      <div className="mt-4 grid grid-cols-2 gap-x-6 gap-y-2 sm:grid-cols-3">
        {counts.map(({ phase, count }) => (
          <div key={phase} className="flex items-center gap-2">
            <StatusBadge phase={phase} />
            <span className="ml-auto text-xs text-text-muted tabular-nums">{count}</span>
          </div>
        ))}
      </div>
    </Card>
  )
}
