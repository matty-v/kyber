import { useState } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { usePrefixedPath } from '../lib/route-prefix'
import {
  ArrowLeft,
  Bot,
  MoreHorizontal,
  Play,
  RotateCcw,
  Square,
  Trash2,
  Zap,
  ScrollText,
} from 'lucide-react'
import {
  useMachine,
  useAgents,
  useStartMachine,
  useStopMachine,
  useRebootMachine,
  useDeleteMachine,
  useRestartMachineAgents,
} from '../hooks/useAPI'
import { MachineCapacityCard } from '../components/MachineCapacityCard'
import { MachineRecoveryBanner } from '../components/MachineRecoveryBanner'
import { StatusBadge } from '../components/StatusBadge'
import { Button } from '../components/Button'
import { Card } from '../components/Card'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { EmptyState } from '../components/EmptyState'
import { Skeleton } from '../components/Skeleton'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { lifecycleItemsInMore } from '../lib/design/machine-actions'

type ActionKind = 'start' | 'stop' | 'reboot' | 'delete' | 'restart-agents'

export function MachineDetail() {
  const { name = '' } = useParams<{ name: string }>()
  const navigate = useNavigate()
  const prefixed = usePrefixedPath()
  const { data: machine, isLoading, error } = useMachine(name)
  const { data: allAgents } = useAgents()
  const [pending, setPending] = useState<ActionKind | null>(null)

  const startMachine = useStartMachine()
  const stopMachine = useStopMachine()
  const rebootMachine = useRebootMachine()
  const deleteMachine = useDeleteMachine()
  const restartAgents = useRestartMachineAgents()

  const hostedAgents = (allAgents ?? []).filter((a) => a.machine === name)
  // Count of agents eligible for a machine-wide restart. Mirrors the backend
  // eligibility set (pkg/api/routes_machines.go): include anything that has
  // or should have a pod; skip explicit pauses and in-flight infra states.
  const restartEligibleCount = hostedAgents.filter((a) =>
    ['Creating', 'Starting', 'Running', 'Restarting', 'Failed', 'NeedsAuth'].includes(a.phase),
  ).length
  const machineActive = machine?.phase === 'Ready' || machine?.phase === 'Running'

  const isActing =
    startMachine.isPending ||
    stopMachine.isPending ||
    rebootMachine.isPending ||
    deleteMachine.isPending ||
    restartAgents.isPending

  async function executeAction() {
    if (!pending) return
    try {
      if (pending === 'start') await startMachine.mutateAsync(name)
      if (pending === 'stop') await stopMachine.mutateAsync(name)
      if (pending === 'reboot') await rebootMachine.mutateAsync(name)
      if (pending === 'restart-agents') await restartAgents.mutateAsync(name)
      if (pending === 'delete') {
        await deleteMachine.mutateAsync(name)
        navigate(prefixed('/machines'))
        return
      }
    } finally {
      setPending(null)
    }
  }

  if (isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-48 rounded" />
        <Skeleton className="h-40 rounded-xl" />
      </div>
    )
  }

  if (error || !machine) {
    return (
      <div className="rounded-lg border border-danger/40 bg-danger-muted p-4 text-sm text-danger">
        {error?.message ?? 'Machine not found'}
      </div>
    )
  }

  // Single-row header (#292) — all lifecycle actions live in the More
  // dropdown. Restart-agents, Start, etc. were standalone primary buttons
  // until they pushed the row past the 390px mobile breakpoint; consolidating
  // into the dropdown matches AgentDetail's #262 fix.
  const moreLifecycle = lifecycleItemsInMore(machine.phase)

  return (
    <div>
      <div className="flex items-center gap-3 mb-6">
        <Button variant="ghost" size="sm" onClick={() => navigate(prefixed('/machines'))}>
          <ArrowLeft className="h-4 w-4" />
        </Button>
        <h1 className="min-w-0 truncate text-xl font-bold text-text-primary">{machine.id}</h1>
        <StatusBadge phase={machine.phase} />
        <div className="ml-auto flex items-center gap-2">
          <Button variant="secondary" size="sm" onClick={() => navigate(prefixed(`/logs?machine=${encodeURIComponent(name)}`))}>
            <ScrollText className="h-4 w-4" /> Logs
          </Button>
          {moreLifecycle.length > 0 && (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={isActing}
                  aria-label="More actions"
                >
                  <MoreHorizontal className="h-4 w-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                {(moreLifecycle.includes('start') ||
                  moreLifecycle.includes('stop') ||
                  moreLifecycle.includes('reboot') ||
                  moreLifecycle.includes('restart-agents')) && (
                  <DropdownMenuLabel>Lifecycle</DropdownMenuLabel>
                )}
                {moreLifecycle.includes('start') && (
                  <DropdownMenuItem onSelect={() => setPending('start')}>
                    <Play className="h-3.5 w-3.5" />
                    Start
                  </DropdownMenuItem>
                )}
                {moreLifecycle.includes('stop') && (
                  <DropdownMenuItem onSelect={() => setPending('stop')}>
                    <Square className="h-3.5 w-3.5" />
                    Stop
                  </DropdownMenuItem>
                )}
                {moreLifecycle.includes('reboot') && (
                  <DropdownMenuItem onSelect={() => setPending('reboot')}>
                    <RotateCcw className="h-3.5 w-3.5" />
                    Reboot
                  </DropdownMenuItem>
                )}
                {moreLifecycle.includes('restart-agents') && (
                  <DropdownMenuItem
                    onSelect={() => setPending('restart-agents')}
                    disabled={!machineActive || restartEligibleCount === 0}
                  >
                    <Zap className="h-3.5 w-3.5" />
                    Restart all agents
                  </DropdownMenuItem>
                )}
                {moreLifecycle.includes('delete') && (
                  <>
                    <DropdownMenuSeparator />
                    <DropdownMenuItem
                      variant="danger"
                      onSelect={() => setPending('delete')}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                      Delete
                    </DropdownMenuItem>
                  </>
                )}
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </div>
      </div>

      <MachineRecoveryBanner machine={machine} />

      <div className="grid gap-4 sm:grid-cols-2">
        {/* Spec */}
        <Card>
          <h2 className="text-sm font-medium text-text-muted mb-3">Spec</h2>
          <dl className="space-y-2 text-sm">
            {machine.spec.machineType && (
              <div className="flex justify-between">
                <dt className="text-text-muted">Machine type</dt>
                <dd className="text-text-primary font-mono text-xs">{machine.spec.machineType}</dd>
              </div>
            )}
            {machine.spec.zone && (
              <div className="flex justify-between">
                <dt className="text-text-muted">Zone</dt>
                <dd className="text-text-primary font-mono text-xs">{machine.spec.zone}</dd>
              </div>
            )}
            {machine.spec.diskSizeGb !== undefined && (
              <div className="flex justify-between">
                <dt className="text-text-muted">Disk</dt>
                <dd className="text-text-primary">{machine.spec.diskSizeGb} GB</dd>
              </div>
            )}
            {machine.spec.spot !== undefined && (
              <div className="flex justify-between">
                <dt className="text-text-muted">Spot</dt>
                <dd className="text-text-primary">{machine.spec.spot ? 'Yes' : 'No'}</dd>
              </div>
            )}
            <div className="flex justify-between">
              <dt className="text-text-muted">Provider</dt>
              <dd className="text-text-primary">{machine.spec.provider}</dd>
            </div>
          </dl>
        </Card>

        {/* Status */}
        <Card>
          <h2 className="text-sm font-medium text-text-muted mb-3">Status</h2>
          <dl className="space-y-2 text-sm">
            {machine.status.instanceId && (
              <div className="flex justify-between gap-2">
                <dt className="text-text-muted shrink-0">Instance ID</dt>
                <dd className="text-text-primary font-mono text-xs truncate">{machine.status.instanceId}</dd>
              </div>
            )}
            {machine.status.externalIP && (
              <div className="flex justify-between">
                <dt className="text-text-muted">External IP</dt>
                <dd className="text-text-primary font-mono text-xs">{machine.status.externalIP}</dd>
              </div>
            )}
            {machine.status.internalIP && (
              <div className="flex justify-between">
                <dt className="text-text-muted">Internal IP</dt>
                <dd className="text-text-primary font-mono text-xs">{machine.status.internalIP}</dd>
              </div>
            )}
            {machine.status.message && (
              <div>
                <dt className="text-text-muted mb-1">Message</dt>
                <dd className="text-text-secondary text-xs">{machine.status.message}</dd>
              </div>
            )}
          </dl>
        </Card>
      </div>

      <div className="mt-6">
        <MachineCapacityCard machine={machine} />
      </div>

      {/* Hosted agents */}
      <div className="mt-6">
        <h2 className="text-sm font-medium text-text-muted mb-3">
          Hosted Agents ({hostedAgents.length})
        </h2>
        {hostedAgents.length === 0 ? (
          <EmptyState
            icon={<Bot className="h-6 w-6" strokeWidth={1.5} />}
            title="No agents on this machine"
            description="Agents that schedule onto this machine will appear here."
          />
        ) : (
          <div className="space-y-2">
            {hostedAgents.map((a) => (
              <Card
                key={a.id}
                onClick={() => navigate(prefixed(`/agents/${a.id}`))}
              >
                <div className="flex items-center justify-between">
                  <span className="text-sm font-medium text-text-primary">{a.id}</span>
                  <StatusBadge phase={a.phase} />
                </div>
                <p className="mt-1 text-xs text-text-muted">{a.model}</p>
              </Card>
            ))}
          </div>
        )}
      </div>

      <ConfirmDialog
        open={pending !== null}
        title={
          pending === 'delete'
            ? 'Delete machine?'
            : pending === 'restart-agents'
              ? `Restart ${restartEligibleCount} agent${restartEligibleCount !== 1 ? 's' : ''} on machine?`
              : `${pending === 'start' ? 'Start' : pending === 'stop' ? 'Stop' : 'Reboot'} machine?`
        }
        message={
          pending === 'restart-agents'
            ? `Each will be brought down and back up in parallel with its session brief preserved. Expect ~30s of unavailability per agent. Skipped: Stopped, Draining.`
            : `This will ${pending === 'delete' ? 'permanently delete' : pending} machine "${name}".`
        }
        confirmLabel={pending === 'delete' ? 'Delete' : 'Confirm'}
        dangerous={pending === 'delete'}
        loading={isActing}
        onConfirm={() => void executeAction()}
        onCancel={() => setPending(null)}
      />
    </div>
  )
}
