import { Link } from 'react-router-dom'
import { ChevronRight } from 'lucide-react'
import type { Agent, AgentActivityState } from '../lib/types'
import { Card } from './Card'
import { EmptyState } from './EmptyState'
import { StatusBadge } from './StatusBadge'
import { usePrefixedPath } from '../lib/route-prefix'
import { rankByActivity, isStale, isAttentionPhase, formatAgo, DEFAULT_LIST_LIMIT } from '../lib/dashboard'

const dotClass: Record<AgentActivityState, string> = {
  working: 'bg-success',
  idle: 'bg-warn',
  unknown: 'bg-text-disabled',
  '': 'bg-text-disabled',
}

export function RecentlyActiveList({
  agents, limit = DEFAULT_LIST_LIMIT, now = Date.now(),
}: { agents: Agent[]; limit?: number; now?: number }) {
  const prefixed = usePrefixedPath()
  const rows = rankByActivity(agents, limit)

  return (
    <Card>
      <div className="mb-2 font-mono text-[10px] uppercase tracking-[0.2em] text-text-muted">
        Recently active
      </div>
      {rows.length === 0 ? (
        <EmptyState title="No activity yet" />
      ) : (
        <ul>
          {rows.map((agent) => {
            const state: AgentActivityState = agent.activity?.state ?? 'unknown'
            const label = state && state !== 'unknown' ? ` · ${state}` : ''
            return (
              <li key={agent.id}>
                <Link
                  to={prefixed(`/agents/${agent.id}`)}
                  className="-mx-1 flex items-center gap-2 rounded border-b border-border-subtle px-1 py-2 transition-colors last:border-0 hover:bg-surface-overlay/40"
                >
                  <span className={`h-2 w-2 shrink-0 rounded-full ${dotClass[state]}`} aria-hidden />
                  <span className="text-sm text-text-primary">{agent.id}</span>
                  {isAttentionPhase(agent.phase) && <StatusBadge phase={agent.phase} />}
                  <span className="ml-auto whitespace-nowrap text-xs text-text-muted">
                    {formatAgo(agent.activity?.lastActivityAt ?? agent.activity?.lastHeartbeatAt, now)}
                    {label}
                  </span>
                  {isStale(agent, now) && (
                    <span className="text-xs text-warn" title="Heartbeat stale">⚠</span>
                  )}
                  <ChevronRight className="h-3.5 w-3.5 shrink-0 text-text-disabled" />
                </Link>
              </li>
            )
          })}
        </ul>
      )}
    </Card>
  )
}
