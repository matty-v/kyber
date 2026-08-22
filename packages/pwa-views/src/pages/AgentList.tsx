import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { usePrefixedPath } from '../lib/route-prefix'
import { Bot, Plus, Play, Square, RotateCcw, Trash2, MoreHorizontal } from 'lucide-react'
import type { ColumnDef } from '@tanstack/react-table'
import {
  useAgents,
  useStartAgent,
  useStopAgent,
  useRestartAgent,
  useDeleteAgent,
} from '../hooks/useAPI'
import { Card } from '../components/Card'
import { StatusBadge } from '../components/StatusBadge'
import { SchedulingFailureBadge } from '../components/SchedulingFailureBadge'
import { AgentActivityBadge } from '../components/AgentActivityBadge'
import { Button } from '../components/Button'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { agentActionConfirmMessage } from '../lib/agentMessages'
import { EmptyState } from '../components/EmptyState'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { DataTable } from '@/components/ui/data-table'
import { Skeleton } from '../components/Skeleton'
import type { Agent } from '../lib/types'

type ActionKind = 'start' | 'stop' | 'restart' | 'delete'

interface ActionState {
  kind: ActionKind
  agent: Agent
}

export function AgentList() {
  const navigate = useNavigate()
  const prefixed = usePrefixedPath()
  const { data: agents, isLoading, error } = useAgents()
  const [pending, setPending] = useState<ActionState | null>(null)

  const startAgent = useStartAgent()
  const stopAgent = useStopAgent()
  const restartAgent = useRestartAgent()
  const deleteAgent = useDeleteAgent()

  const isActing =
    startAgent.isPending ||
    stopAgent.isPending ||
    restartAgent.isPending ||
    deleteAgent.isPending

  function confirm(kind: ActionKind, agent: Agent) {
    setPending({ kind, agent })
  }

  const columns = useMemo<ColumnDef<Agent, unknown>[]>(
    () => [
      {
        accessorKey: 'id',
        header: 'Agent',
        cell: ({ row }) => (
          <span className="font-mono text-sm font-medium text-text-primary">
            {row.original.id}
          </span>
        ),
      },
      {
        accessorKey: 'phase',
        header: 'Status',
        cell: ({ row }) => (
          <div className="inline-flex items-center gap-1.5">
            <StatusBadge phase={row.original.phase} />
            <SchedulingFailureBadge agent={row.original} />
            <AgentActivityBadge agent={row.original} showDot={false} />
          </div>
        ),
      },
      {
        id: 'model',
        accessorFn: (row) => row.currentModel || row.model || '',
        header: 'Model',
        cell: ({ row }) => {
          const model = row.original.currentModel || row.original.model
          return (
            <span className={`font-mono text-xs ${model ? 'text-text-secondary' : 'text-text-disabled'}`}>
              {model || '—'}
            </span>
          )
        },
      },
      {
        accessorKey: 'machine',
        header: 'Machine',
        cell: ({ row }) => (
          <span className="text-xs text-text-secondary">{row.original.machine}</span>
        ),
      },
      {
        id: 'runtimeVersion',
        // Sort by version string; empty strings sort to the bottom on desc.
        accessorFn: (row) => row.runtimeVersion?.installedVersion ?? '',
        header: 'Runtime',
        cell: ({ row }) => {
          const v = row.original.runtimeVersion?.installedVersion
          if (!v) {
            return (
              <span className="font-mono text-xs text-text-disabled" title="Not yet reported">
                {row.original.runtime} —
              </span>
            )
          }
          return (
            <span className="font-mono text-xs text-text-secondary">
              {row.original.runtime} {v}
            </span>
          )
        },
      },
      {
        id: 'context',
        // Sort by percentage so operators see the agents closest to compaction
        // first. Rows without a token-usage snapshot sort to the bottom on
        // desc (treated as -1).
        accessorFn: (row) => row.tokenUsage?.percentage ?? -1,
        header: 'Context',
        cell: ({ row }) => {
          const usage = row.original.tokenUsage
          if (!usage) {
            return <span className="font-mono text-xs text-text-disabled">—</span>
          }
          const pct = usage.percentage
          const color =
            pct >= 90 ? 'text-danger'
            : pct >= 75 ? 'text-warn'
            : 'text-text-secondary'
          return (
            <span className={`font-mono text-xs tabular-nums ${color}`}>
              {formatTokens(usage.tokens.used)} / {formatTokens(usage.tokens.limit)}{' '}
              <span className="text-text-muted">({formatPct(pct)})</span>
            </span>
          )
        },
      },
      {
        id: 'actions',
        header: '',
        enableSorting: false,
        cell: ({ row }) => (
          <div className="flex justify-end">
            <AgentActionsMenu agent={row.original} onAction={confirm} />
          </div>
        ),
      },
    ],
    [],
  )

  async function executeAction() {
    if (!pending) return
    const { kind, agent } = pending
    try {
      if (kind === 'start') await startAgent.mutateAsync(agent.id)
      if (kind === 'stop') await stopAgent.mutateAsync(agent.id)
      if (kind === 'restart') await restartAgent.mutateAsync(agent.id)
      if (kind === 'delete') await deleteAgent.mutateAsync(agent.id)
    } finally {
      setPending(null)
    }
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-xl font-bold text-text-primary">Agents</h1>
        <Button
          variant="primary"
          size="sm"
          onClick={() => navigate(prefixed('/agents/new'))}
        >
          <Plus className="h-4 w-4" />
          New
        </Button>
      </div>

      {isLoading && (
        <div className="space-y-3">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-24 rounded-xl border border-border-subtle" />
          ))}
        </div>
      )}

      {error && (
        <div className="rounded-lg border border-danger/40 bg-danger-muted p-4 text-sm text-danger">
          Failed to load agents: {error.message}
        </div>
      )}

      {agents && agents.length === 0 && (
        <EmptyState
          icon={<Bot className="h-6 w-6" strokeWidth={1.5} />}
          title="No agents deployed"
          description="Create an agent to run a Claude process on one of your machines."
          action={
            <Button
              variant="primary"
              size="sm"
              onClick={() => navigate(prefixed('/agents/new'))}
            >
              <Plus className="h-4 w-4" />
              New agent
            </Button>
          }
        />
      )}

      {agents && agents.length > 0 && (
        <>
          {/* Mobile: card view */}
          <div className="space-y-3 md:hidden">
            {agents.map((a) => (
              <Card key={a.id} onClick={() => navigate(prefixed(`/agents/${a.id}`))}>
                <div className="flex items-start justify-between gap-4">
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-medium text-text-primary truncate">{a.id}</span>
                      <StatusBadge phase={a.phase} />
                      <SchedulingFailureBadge agent={a} />
                    </div>
                    {/* Activity badge on its own line — keeps the truncating
                        id + badges in the header row from overflowing on
                        narrow viewports (kyber#417). Gate the line on the same
                        visible-by-absence condition the badge itself uses, so a
                        no-activity card keeps its exact prior layout (no empty
                        spacer div nudging the model·machine line down). */}
                    {a.activity?.state && a.activity.state !== 'unknown' && (
                      <div className="mt-1">
                        <AgentActivityBadge agent={a} showDot={false} />
                      </div>
                    )}
                    <p className="mt-1 text-xs text-text-muted">
                      {a.currentModel || a.model || '—'} &middot; {a.machine}
                    </p>
                    <p className="mt-0.5 text-xs text-text-muted">
                      Context:{' '}
                      {a.tokenUsage ? (
                        <span
                          className={`font-mono tabular-nums ${
                            a.tokenUsage.percentage >= 90 ? 'text-danger'
                            : a.tokenUsage.percentage >= 75 ? 'text-warn'
                            : 'text-text-secondary'
                          }`}
                        >
                          {formatTokens(a.tokenUsage.tokens.used)} /{' '}
                          {formatTokens(a.tokenUsage.tokens.limit)} (
                          {formatPct(a.tokenUsage.percentage)})
                        </span>
                      ) : (
                        <span className="font-mono text-text-disabled">—</span>
                      )}
                    </p>
                  </div>
                  <AgentActionsMenu agent={a} onAction={confirm} />
                </div>
              </Card>
            ))}
          </div>

          {/* Desktop: data table */}
          <div className="hidden md:block">
            <DataTable
              columns={columns}
              data={agents}
              getRowId={(a) => a.id}
              onRowClick={(a) => navigate(prefixed(`/agents/${a.id}`))}
              initialSorting={[{ id: 'context', desc: true }]}
            />
          </div>
        </>
      )}

      <ConfirmDialog
        open={pending !== null}
        title={pending ? `${capitalize(pending.kind)} agent?` : ''}
        message={pending ? agentActionConfirmMessage(pending.kind, pending.agent.id) : ''}
        confirmLabel={pending?.kind === 'delete' ? 'Delete' : 'Confirm'}
        dangerous={pending?.kind === 'delete'}
        loading={isActing}
        onConfirm={() => void executeAction()}
        onCancel={() => setPending(null)}
      />
    </div>
  )
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1)
}

// formatTokens humanizes a token count for the agents overview. Tight table
// cells: 0 decimals under 100, 1 decimal otherwise. "74K" not "74.4K", "1.2M"
// not "1.23M".
function formatTokens(n: number): string {
  if (n >= 1_000_000) {
    const m = n / 1_000_000
    return m >= 100 ? `${Math.round(m)}M` : `${m.toFixed(1)}M`
  }
  if (n >= 1_000) {
    const k = n / 1_000
    return k >= 100 ? `${Math.round(k)}K` : `${k.toFixed(1)}K`
  }
  return String(n)
}

function formatPct(p: number): string {
  return `${p.toFixed(p >= 100 ? 0 : 1)}%`
}

function AgentActionsMenu({
  agent,
  onAction,
}: {
  agent: Agent
  onAction: (kind: ActionKind, agent: Agent) => void
}) {
  return (
    <div
      className="flex items-center shrink-0"
      onClick={(e) => e.stopPropagation()}
    >
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="sm" aria-label="Agent actions">
            <MoreHorizontal className="h-4 w-4" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => onAction('start', agent)}>
            <Play className="h-3.5 w-3.5" />
            Start
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => onAction('stop', agent)}>
            <Square className="h-3.5 w-3.5" />
            Stop
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => onAction('restart', agent)}>
            <RotateCcw className="h-3.5 w-3.5" />
            Restart
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            variant="danger"
            onSelect={() => onAction('delete', agent)}
          >
            <Trash2 className="h-3.5 w-3.5" />
            Delete
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}
