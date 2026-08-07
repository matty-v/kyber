import { useState } from 'react'
import { Link } from 'react-router-dom'
import { RotateCcw } from 'lucide-react'
import type { Agent } from '../lib/types'
import { Card } from './Card'
import { EmptyState } from './EmptyState'
import { ConfirmDialog } from './ConfirmDialog'
import { usePrefixedPath } from '../lib/route-prefix'
import { useRestartAgentSession } from '../hooks/useAPI'
import { agentActionConfirmMessage } from '../lib/agentMessages'
import { rankByContext, contextTone, formatTokens, DEFAULT_LIST_LIMIT } from '../lib/dashboard'
import { toneBarClasses, toneTextClasses } from '../lib/design/status'

export function ContextPressureList({
  agents, limit = DEFAULT_LIST_LIMIT,
}: { agents: Agent[]; limit?: number }) {
  const prefixed = usePrefixedPath()
  const rows = rankByContext(agents, limit)
  // `pending` holds the agent id awaiting confirmation — one shared dialog for
  // the whole list. Restart-session is the same action offered on AgentDetail
  // (#128); here it's surfaced per row so a high-pressure agent can be reset
  // without drilling in (#618).
  const [pending, setPending] = useState<string | null>(null)
  const restartSession = useRestartAgentSession()

  async function confirmReset() {
    if (!pending) return
    try {
      await restartSession.mutateAsync(pending)
    } finally {
      // The mutation hook owns the success/error toast via its meta; we only
      // dismiss the dialog. Keep it closing even on error so it isn't wedged.
      setPending(null)
    }
  }

  return (
    <Card>
      <div className="mb-2 font-mono text-[10px] uppercase tracking-[0.2em] text-text-muted">
        Context pressure
      </div>
      {rows.length === 0 ? (
        <EmptyState title="No context data yet" description="Agents report context usage once running." />
      ) : (
        <ul className="space-y-2">
          {rows.map((row) => {
            const tone = contextTone(row.percentage)
            const pct = Math.min(100, Math.max(0, row.percentage))
            // Session reset only applies to a live pod — parity with
            // AgentDetail's `canRestartSession` (phase === 'Running').
            const canReset = row.agent.phase === 'Running'
            const resetting = restartSession.isPending && pending === row.agent.id
            return (
              <li key={row.agent.id} className="flex items-stretch gap-1.5">
                <Link
                  to={prefixed(`/agents/${row.agent.id}`)}
                  className="-ml-1 block min-w-0 flex-1 rounded px-1 py-1 transition-colors hover:bg-surface-overlay/40"
                >
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <span className="text-sm text-text-primary">{row.agent.id}</span>
                    <span className="rounded bg-surface-overlay px-1.5 py-0.5 font-mono text-[10px] text-text-muted">
                      {row.model}
                    </span>
                    <span className="text-xs text-text-muted">
                      {formatTokens(row.used)} / {formatTokens(row.limit)}
                    </span>
                    <span className={`ml-auto text-xs tabular-nums ${toneTextClasses[tone]}`}>
                      {Math.round(row.percentage)}%
                    </span>
                  </div>
                  <div className="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-surface-sunken">
                    <div className={`h-full rounded-full ${toneBarClasses[tone]}`} style={{ width: `${pct}%` }} />
                  </div>
                </Link>
                {canReset && (
                  <button
                    type="button"
                    aria-label={`Reset session for ${row.agent.id}`}
                    onClick={() => setPending(row.agent.id)}
                    disabled={resetting}
                    className="flex w-9 flex-none items-center justify-center rounded-md border border-border-default text-text-muted transition-colors hover:border-accent-ring hover:bg-accent/10 hover:text-accent focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent-ring disabled:cursor-not-allowed disabled:opacity-40"
                  >
                    <RotateCcw className={`h-3.5 w-3.5 ${resetting ? 'animate-spin' : ''}`} />
                  </button>
                )}
              </li>
            )
          })}
        </ul>
      )}
      <ConfirmDialog
        open={pending !== null}
        title="Restart session?"
        message={pending ? agentActionConfirmMessage('restart-session', pending) : ''}
        confirmLabel="Confirm"
        loading={restartSession.isPending}
        onConfirm={() => void confirmReset()}
        onCancel={() => setPending(null)}
      />
    </Card>
  )
}
