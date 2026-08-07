// Webhooks tab for AgentDetail (#208) — list, create, rotate, delete inbound
// prompt bindings. Each binding exposes a public webhook URL at
// /webhooks/inbound/<agent>/<binding> verified by HMAC. The receiver itself
// already shipped in Phase 1; this tab is the operator UI for managing the
// bindings spec + reading per-binding stats.
//
// Phase 3 (#208) additions:
//   - expandable per-binding row showing the last ~10 runs from
//     agent.status.inboundRuns, each with a Replay button.
//   - standalone "Test a payload" debugger that posts to /api/v1/inbound-debug.

import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import {
  ChevronDown,
  ChevronRight,
  Copy,
  Inbox,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  Trash2,
} from 'lucide-react'
import {
  useAgent,
  useDeleteInboundBinding,
  useInboundBindings,
  useReplayInboundRun,
  useRotateInboundSecret,
} from '../hooks/useAPI'
import type {
  AgentInboundDropReason,
  AgentInboundRun,
  InboundBindingWithStats,
  InboundDropCounts,
} from '../lib/types'
import { AddWebhookWizard } from './AddWebhookWizard'
import { Button } from './Button'
import { Card } from './Card'
import { ConfirmDialog } from './ConfirmDialog'
import { EmptyState } from './EmptyState'
import { InboundDebugger } from './InboundDebugger'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'

interface Props {
  agentName: string
}

const DROP_REASONS: AgentInboundDropReason[] = [
  'rate-limited',
  'queue-full',
  'sig-mismatch',
  'missing-secret',
  'unmatched-event',
  'filter-rejected',
  'dedup',
]

// Threshold above which the drop badge shows next to the binding name. The
// dropped counts are over the bounded ring buffer, so 5 is a reasonable "this
// is misbehaving" signal without flagging the very first transient drop.
const DROP_BADGE_THRESHOLD = 5

// Recent-runs panel cap. agent.status.inboundRuns is itself a bounded ring
// buffer; we slice to this many after filtering by binding so the panel stays
// scannable.
const MAX_RECENT_RUNS = 10

function sumDropped(d: InboundDropCounts): number {
  let total = 0
  for (const r of DROP_REASONS) {
    total += d[r] ?? 0
  }
  return total
}

function formatTimestamp(iso: string | undefined): string {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString(undefined, {
      dateStyle: 'short',
      timeStyle: 'short',
    })
  } catch {
    return iso
  }
}

async function copyToClipboard(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`${label} copied`)
  } catch {
    toast.error('Could not copy', {
      description: 'Clipboard access blocked — copy manually.',
    })
  }
}

function DropBadge({ count }: { count: number }) {
  if (count <= DROP_BADGE_THRESHOLD) return null
  return (
    <span
      className="ml-2 inline-flex items-center rounded-full bg-danger/20 px-1.5 py-0.5 text-[10px] font-semibold text-danger ring-1 ring-inset ring-danger/30"
      title={`${count} drops in the recent ring buffer`}
    >
      {count} drops
    </span>
  )
}

function DroppedCell({ dropped }: { dropped: InboundDropCounts }) {
  const total = sumDropped(dropped)
  if (total === 0) {
    return <span className="text-text-muted">0</span>
  }
  const breakdown = DROP_REASONS.filter((r) => (dropped[r] ?? 0) > 0)
    .map((r) => `${r}: ${dropped[r]}`)
    .join('\n')
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className={total > DROP_BADGE_THRESHOLD ? 'text-danger' : 'text-warn'}>
          {total}
        </span>
      </TooltipTrigger>
      <TooltipContent>
        <pre className="whitespace-pre text-xs">{breakdown}</pre>
      </TooltipContent>
    </Tooltip>
  )
}

// SecretRevealDialog is shown after a successful rotate — the secret is shown
// exactly once and then the server only retains a hash. Mirrors the post-create
// "setup complete" panel in AddWebhookWizard but scoped to a single secret.
function SecretRevealDialog({
  bindingName,
  secret,
  onClose,
}: {
  bindingName: string
  secret: string
  onClose: () => void
}) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-surface-sunken/60 backdrop-blur-sm" onClick={onClose} />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="rotated-secret-title"
        className="relative z-10 w-full max-w-lg rounded-xl border border-border-subtle bg-surface-raised p-6 shadow-xl"
      >
        <h2 id="rotated-secret-title" className="text-base font-semibold text-text-primary">
          New secret for {bindingName}
        </h2>
        <p className="mt-2 text-xs text-warn">
          Copy this now. Kyber stores only a hash — once this dialog closes, the value cannot be
          recovered.
        </p>
        <div className="mt-3 rounded-lg border border-border-default bg-surface-base p-3">
          <pre className="overflow-auto whitespace-pre-wrap break-all font-mono text-xs text-text-primary">
            {secret}
          </pre>
        </div>
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="secondary" size="sm" onClick={() => void copyToClipboard(secret, 'Secret')}>
            <Copy className="h-3.5 w-3.5" /> Copy secret
          </Button>
          <Button variant="primary" size="sm" onClick={onClose}>
            Done
          </Button>
        </div>
      </div>
    </div>
  )
}

// Outcome badge for a single run row.
function RunOutcomeBadge({ run }: { run: AgentInboundRun }) {
  if (run.outcome === 'dispatched') {
    return (
      <span className="inline-flex items-center rounded-full bg-success/15 px-1.5 py-0.5 text-[10px] font-medium text-success">
        dispatched
      </span>
    )
  }
  return (
    <span className="inline-flex items-center rounded-full bg-warn/15 px-1.5 py-0.5 text-[10px] font-medium text-warn">
      dropped
    </span>
  )
}

export function WebhooksTab({ agentName }: Props) {
  const { data: bindings, isLoading, error } = useInboundBindings(agentName)
  // Need agent.status.inboundRuns for the per-binding recent-runs panel.
  // The query is shared across the page; this is a free read off the cache
  // when AgentDetail has already mounted useAgent.
  const { data: agent } = useAgent(agentName)
  const deleteBinding = useDeleteInboundBinding()
  const rotateSecret = useRotateInboundSecret()
  const replayRun = useReplayInboundRun()

  const [showAdd, setShowAdd] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<InboundBindingWithStats | null>(null)
  const [pendingRotate, setPendingRotate] = useState<InboundBindingWithStats | null>(null)
  const [pendingEdit, setPendingEdit] = useState<InboundBindingWithStats | null>(null)
  const [pendingReplay, setPendingReplay] = useState<{ binding: string; requestId: string } | null>(null)
  const [revealedSecret, setRevealedSecret] = useState<{ bindingName: string; secret: string } | null>(
    null,
  )
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [showDebugger, setShowDebugger] = useState(false)

  const list = useMemo(() => bindings ?? [], [bindings])

  const allRuns = useMemo<AgentInboundRun[]>(() => agent?.inboundRuns ?? [], [agent])

  // Per-binding recent runs — sorted by startedAt desc, capped at MAX_RECENT_RUNS.
  // Memo by binding count so toggling expansion doesn't re-sort everything.
  const runsByBinding = useMemo<Record<string, AgentInboundRun[]>>(() => {
    const out: Record<string, AgentInboundRun[]> = {}
    for (const r of allRuns) {
      const bucket = out[r.bindingName] ?? []
      bucket.push(r)
      out[r.bindingName] = bucket
    }
    for (const k of Object.keys(out)) {
      out[k] = out[k]
        .slice()
        .sort((a, b) => (a.startedAt < b.startedAt ? 1 : a.startedAt > b.startedAt ? -1 : 0))
        .slice(0, MAX_RECENT_RUNS)
    }
    return out
  }, [allRuns])

  function toggleExpanded(name: string) {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(name)) next.delete(name)
      else next.add(name)
      return next
    })
  }

  function confirmDelete() {
    if (!pendingDelete) return
    deleteBinding.mutate(
      { name: agentName, bindingName: pendingDelete.name },
      { onSuccess: () => setPendingDelete(null) },
    )
  }

  function confirmRotate() {
    if (!pendingRotate) return
    const target = pendingRotate
    rotateSecret.mutate(
      { name: agentName, bindingName: target.name },
      {
        onSuccess: (data) => {
          setPendingRotate(null)
          setRevealedSecret({ bindingName: target.name, secret: data.secret })
        },
        onError: () => {
          // Toast is fired by the mutation cache; just close the confirm.
          setPendingRotate(null)
        },
      },
    )
  }

  function confirmReplay() {
    if (!pendingReplay) return
    const { binding, requestId } = pendingReplay
    replayRun.mutate(
      { name: agentName, bindingName: binding, requestId },
      {
        onSuccess: () => {
          setPendingReplay(null)
        },
        onError: (err: unknown) => {
          // useReplayInboundRun deliberately omits meta.errorPrefix so the
          // global mutationCache toast doesn't fire — every replay failure
          // is status-coded and warrants a specific operator message.
          // Detection is duck-typed since KyberAPIError isn't exported.
          const e = err as { status?: number; message?: string } | undefined
          const status = e?.status
          if (status === 410) {
            toast.error('Envelope expired', {
              description: "Runs older than 7 days can't be replayed.",
            })
          } else if (status === 429) {
            toast.error('Queue full', {
              description: 'Inbound queue is at capacity — try again shortly.',
            })
          } else if (status === 404) {
            toast.error('Binding deleted', {
              description: 'The binding for this run no longer exists. Recreate it before replaying.',
            })
          } else {
            toast.error('Replay failed', {
              description: e?.message ?? 'Unknown error — check the server logs.',
            })
          }
          setPendingReplay(null)
        },
      },
    )
  }

  return (
    <div className="space-y-3">
      {isLoading && <Card className="text-sm text-text-muted">Loading webhooks…</Card>}

      {error && (
        <Card className="border-danger/40 bg-danger-muted text-sm text-danger">
          Failed to load webhooks:{' '}
          {error instanceof Error ? error.message : 'Unknown error'}
        </Card>
      )}

      {!isLoading && !error && list.length === 0 && (
        <EmptyState
          icon={<Inbox className="h-6 w-6" strokeWidth={1.5} />}
          title="No webhooks"
          description="No webhooks configured. Add one to receive events from GitHub, Linear, Sentry, etc."
          action={
            <Button variant="primary" size="sm" onClick={() => setShowAdd(true)}>
              <Plus className="h-3.5 w-3.5" /> Add Webhook
            </Button>
          }
        />
      )}

      {!isLoading && !error && list.length > 0 && (
        <>
          <div className="flex items-center justify-end">
            <Button variant="primary" size="sm" onClick={() => setShowAdd(true)}>
              <Plus className="h-3.5 w-3.5" /> Add Webhook
            </Button>
          </div>
          <Card className="overflow-hidden p-0">
            <div className="overflow-x-auto">
              <table className="w-full min-w-[640px] text-sm">
                <thead>
                  <tr className="border-b border-border-subtle bg-surface-raised/50 text-left text-xs uppercase text-text-secondary">
                    <th className="w-8 p-3" aria-label="Expand row" />
                    <th className="p-3">Name</th>
                    <th className="p-3">Last received</th>
                    <th className="p-3">Dispatched</th>
                    <th className="p-3">Dropped</th>
                    <th className="p-3 text-right">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((b) => {
                    const droppedTotal = sumDropped(b.stats.dropped)
                    const hasUrl = Boolean(b.url)
                    const isOpen = expanded.has(b.name)
                    const recent = runsByBinding[b.name] ?? []
                    return (
                      <BindingRows
                        key={b.name}
                        binding={b}
                        droppedTotal={droppedTotal}
                        hasUrl={hasUrl}
                        isOpen={isOpen}
                        recent={recent}
                        replayPending={replayRun.isPending}
                        onToggle={() => toggleExpanded(b.name)}
                        onCopyUrl={() => {
                          if (b.url) void copyToClipboard(b.url, 'URL')
                        }}
                        onRotate={() => setPendingRotate(b)}
                        onDelete={() => setPendingDelete(b)}
                        onEdit={() => setPendingEdit(b)}
                        onReplay={(requestId) =>
                          setPendingReplay({ binding: b.name, requestId })
                        }
                      />
                    )
                  })}
                </tbody>
              </table>
            </div>
          </Card>

          {/* Standalone debugger lives below the table; collapsed by default. */}
          <Card className="space-y-2">
            <button
              type="button"
              className="flex w-full items-center justify-between text-left"
              onClick={() => setShowDebugger((v) => !v)}
              aria-expanded={showDebugger}
              aria-controls="inbound-debugger-panel"
            >
              <div>
                <div className="text-sm font-medium text-text-primary">Test a payload</div>
                <div className="mt-0.5 text-xs text-text-muted">
                  Diagnose drops by running the receiver against an arbitrary JSON payload.
                </div>
              </div>
              {showDebugger ? (
                <ChevronDown className="h-4 w-4 text-text-muted" />
              ) : (
                <ChevronRight className="h-4 w-4 text-text-muted" />
              )}
            </button>
            {showDebugger && (
              <div id="inbound-debugger-panel" className="border-t border-border-subtle pt-3">
                <InboundDebugger agentName={agentName} bindings={list} />
              </div>
            )}
          </Card>
        </>
      )}

      {showAdd && (
        <AddWebhookWizard agentName={agentName} onClose={() => setShowAdd(false)} />
      )}

      {pendingEdit && (
        <AddWebhookWizard
          agentName={agentName}
          editing={pendingEdit}
          onClose={() => setPendingEdit(null)}
        />
      )}

      {pendingDelete && (
        <ConfirmDialog
          open={true}
          title="Delete webhook?"
          message={`Permanently remove "${pendingDelete.name}" from agent "${agentName}". External systems posting to its webhook URL will start receiving 404s.`}
          confirmLabel="Delete"
          dangerous
          loading={deleteBinding.isPending}
          onConfirm={confirmDelete}
          onCancel={() => setPendingDelete(null)}
        />
      )}

      {pendingRotate && (
        <ConfirmDialog
          open={true}
          title="Rotate secret?"
          message={`Generate a new HMAC secret for "${pendingRotate.name}". The current secret enters a 24h grace period during which both old and new are accepted, then stops working — update external systems to the new secret within 24h.`}
          confirmLabel="Rotate"
          loading={rotateSecret.isPending}
          onConfirm={confirmRotate}
          onCancel={() => setPendingRotate(null)}
        />
      )}

      {pendingReplay && (
        <ConfirmDialog
          open={true}
          title="Replay this inbound?"
          message="Re-dispatches the cached envelope and bypasses dedup. The new run lands in this binding's history with a fresh request id."
          confirmLabel="Replay"
          loading={replayRun.isPending}
          onConfirm={confirmReplay}
          onCancel={() => setPendingReplay(null)}
        />
      )}

      {revealedSecret && (
        <SecretRevealDialog
          bindingName={revealedSecret.bindingName}
          secret={revealedSecret.secret}
          onClose={() => setRevealedSecret(null)}
        />
      )}
    </div>
  )
}

// BindingRows renders the visible row + the optional expanded detail row.
// Pulled out so the recent-runs sub-table doesn't bloat the parent table loop.
function BindingRows({
  binding,
  droppedTotal,
  hasUrl,
  isOpen,
  recent,
  replayPending,
  onToggle,
  onCopyUrl,
  onRotate,
  onDelete,
  onEdit,
  onReplay,
}: {
  binding: InboundBindingWithStats
  droppedTotal: number
  hasUrl: boolean
  isOpen: boolean
  recent: AgentInboundRun[]
  replayPending: boolean
  onToggle: () => void
  onCopyUrl: () => void
  onRotate: () => void
  onDelete: () => void
  onEdit: () => void
  onReplay: (requestId: string) => void
}) {
  const b = binding
  return (
    <>
      <tr className="border-b border-border-subtle/50 last:border-0">
        <td className="p-3">
          <button
            type="button"
            onClick={onToggle}
            aria-expanded={isOpen}
            aria-label={isOpen ? `Collapse ${b.name} runs` : `Expand ${b.name} runs`}
            className="rounded-md p-1 text-text-muted hover:bg-surface-overlay hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring"
          >
            {isOpen ? (
              <ChevronDown className="h-3.5 w-3.5" />
            ) : (
              <ChevronRight className="h-3.5 w-3.5" />
            )}
          </button>
        </td>
        <td className="p-3 font-mono">
          {b.name}
          <DropBadge count={droppedTotal} />
        </td>
        <td className="p-3 text-xs text-text-muted">
          {formatTimestamp(b.stats.lastReceivedAt)}
        </td>
        <td className="p-3">{b.stats.totalDispatched}</td>
        <td className="p-3">
          <DroppedCell dropped={b.stats.dropped} />
        </td>
        <td className="p-3 text-right">
          <div className="inline-flex items-center gap-1">
            <Tooltip>
              <TooltipTrigger asChild>
                <span>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={!hasUrl}
                    onClick={onCopyUrl}
                    aria-label="Copy webhook URL"
                  >
                    <Copy className="h-3.5 w-3.5" />
                  </Button>
                </span>
              </TooltipTrigger>
              <TooltipContent>
                {hasUrl
                  ? 'Copy webhook URL'
                  : 'Public URL not configured on control plane'}
              </TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={onEdit}
                  aria-label="Edit webhook"
                >
                  <Pencil className="h-3.5 w-3.5" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>Edit webhook</TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={onRotate}
                  aria-label="Rotate secret"
                >
                  <RefreshCw className="h-3.5 w-3.5" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>Rotate secret</TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={onDelete}
                  aria-label="Delete webhook"
                >
                  <Trash2 className="h-3.5 w-3.5 text-danger" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>Delete webhook</TooltipContent>
            </Tooltip>
          </div>
        </td>
      </tr>
      {isOpen && (
        <tr className="bg-surface-base/40">
          <td colSpan={6} className="p-3">
            <RecentRunsPanel
              bindingName={b.name}
              runs={recent}
              replayPending={replayPending}
              onReplay={onReplay}
            />
          </td>
        </tr>
      )}
    </>
  )
}

function RecentRunsPanel({
  bindingName,
  runs,
  replayPending,
  onReplay,
}: {
  bindingName: string
  runs: AgentInboundRun[]
  replayPending: boolean
  onReplay: (requestId: string) => void
}) {
  if (runs.length === 0) {
    return (
      <div className="rounded-md border border-border-subtle bg-surface-base p-3 text-xs text-text-muted">
        No runs recorded yet.
      </div>
    )
  }
  return (
    <div className="rounded-md border border-border-subtle bg-surface-base">
      <div className="border-b border-border-subtle/60 px-3 py-2 text-[11px] font-medium uppercase tracking-wide text-text-muted">
        Recent runs — {bindingName}
      </div>
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-border-subtle/40 text-left text-[10px] uppercase text-text-muted">
            <th className="px-3 py-1.5">Timestamp</th>
            <th className="px-3 py-1.5">Outcome</th>
            <th className="px-3 py-1.5">Drop reason</th>
            <th className="px-3 py-1.5">Request ID</th>
            <th className="px-3 py-1.5 text-right">Action</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => {
            // Replay is disabled for `dedup` drops — re-firing them through
            // /replay is a no-op because dedup re-keys deterministically off
            // the original delivery id. All other dispatched/dropped runs are
            // candidates; we let the server 410 if the envelope cache TTL has
            // elapsed and surface a friendly toast in that path.
            const isDedup = r.outcome === 'dropped' && r.dropReason === 'dedup'
            const replayable = !isDedup
            const replayTooltip = isDedup
              ? 'Dedup drops cannot be replayed (the new request would dedup again)'
              : 'Re-dispatch this run. Envelope cache only retains 7 days; older runs return 410.'
            return (
              <tr key={r.requestId} className="border-b border-border-subtle/30 last:border-0">
                <td className="px-3 py-1.5 font-mono text-[11px] text-text-secondary">
                  {formatTimestamp(r.startedAt)}
                </td>
                <td className="px-3 py-1.5">
                  <RunOutcomeBadge run={r} />
                  {/* Backend records "replay of <id>" in run.error for replays.
                      Surface that as a small badge so operators can tell from
                      the UI which runs are replays vs originals (review #6). */}
                  {r.error?.startsWith('replay of ') && (
                    <span
                      className="ml-1.5 rounded bg-info-muted px-1 py-0.5 font-mono text-[10px] text-info"
                      title={r.error}
                    >
                      replay
                    </span>
                  )}
                </td>
                <td className="px-3 py-1.5 font-mono text-[11px] text-text-muted">
                  {r.dropReason ?? '—'}
                </td>
                <td className="px-3 py-1.5 font-mono text-[11px] text-text-muted">
                  {r.requestId}
                </td>
                <td className="px-3 py-1.5 text-right">
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <span>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={!replayable || replayPending}
                          onClick={() => onReplay(r.requestId)}
                          aria-label={`Replay ${r.requestId}`}
                        >
                          <Play className="h-3.5 w-3.5" />
                        </Button>
                      </span>
                    </TooltipTrigger>
                    <TooltipContent>{replayTooltip}</TooltipContent>
                  </Tooltip>
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
