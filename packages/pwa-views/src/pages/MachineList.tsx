import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { usePrefixedPath } from '../lib/route-prefix'
import { Plus, Play, Server, Square, RotateCcw, Trash2, Zap, MoreHorizontal } from 'lucide-react'
import type { ColumnDef } from '@tanstack/react-table'
import { useMachines, useStartMachine, useStopMachine, useRebootMachine, useDeleteMachine, useRestartMachineAgents } from '../hooks/useAPI'
import { machineCapacity, parseCpu, parseMemoryGi } from '../lib/machineTypes'
import { capacityBand, pctUsed, BAND_BG_CLASS } from '../lib/capacityBars'
import { CapacityBar } from '../components/CapacityBar'
import { Card } from '../components/Card'
import { StatusBadge } from '../components/StatusBadge'
import { Button } from '../components/Button'
import { ConfirmDialog } from '../components/ConfirmDialog'
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
import type { Machine } from '../lib/types'

type MachineActionKind = 'start' | 'stop' | 'reboot' | 'delete' | 'restart-agents'

interface ActionState {
  kind: MachineActionKind
  machine: Machine
}

export function MachineList() {
  const navigate = useNavigate()
  const prefixed = usePrefixedPath()
  const { data: machines, isLoading, error } = useMachines()
  const [pending, setPending] = useState<ActionState | null>(null)

  const startMachine = useStartMachine()
  const stopMachine = useStopMachine()
  const rebootMachine = useRebootMachine()
  const deleteMachine = useDeleteMachine()
  const restartAgents = useRestartMachineAgents()

  function confirm(kind: MachineActionKind, machine: Machine) {
    setPending({ kind, machine })
  }

  const columns = useMemo<ColumnDef<Machine, unknown>[]>(
    () => [
      {
        accessorKey: 'id',
        header: 'Machine',
        cell: ({ row }) => (
          <span className="font-mono text-sm font-medium text-text-primary">
            {row.original.id}
          </span>
        ),
      },
      {
        accessorKey: 'phase',
        header: 'Status',
        cell: ({ row }) => <StatusBadge phase={row.original.phase} />,
      },
      {
        id: 'type',
        accessorFn: (m) => m.spec.machineType ?? m.spec.provider,
        header: 'Type',
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-text-secondary">
            {String(getValue() ?? '—')}
          </span>
        ),
      },
      {
        id: 'zone',
        accessorFn: (m) => m.spec.zone ?? '',
        header: 'Zone',
        cell: ({ row }) => (
          <span className="text-xs text-text-secondary">
            {row.original.spec.zone ?? '—'}
            {row.original.spec.spot && (
              <span className="ml-1.5 font-mono text-[10px] uppercase tracking-[0.1em] text-warn">
                spot
              </span>
            )}
          </span>
        ),
      },
      {
        id: 'available',
        // "Available" is what operators want to see: how much room is left
        // for new agents on this machine. Total capacity is implied (free is
        // shown over total per resource); the previous "Capacity" framing
        // showed only the operator-typed ceiling and gave no usage signal.
        // Source: status.assignableCapacity (denominator) + availableCapacity
        // (free), populated by the controller from node.allocatable since #142.
        // Falls back to the raw spec.capacity number when the controller
        // hasn't yet observed the node (Provisioning machine).
        header: 'Available',
        enableSorting: false,
        cell: ({ row }) => <MachineAvailableCell machine={row.original} />,
      },
      {
        id: 'agents',
        accessorFn: (m) => m.status.agentCount ?? 0,
        header: 'Agents',
        cell: ({ getValue }) => {
          const n = Number(getValue() ?? 0)
          return (
            <span
              className={`font-mono text-xs tabular-nums ${
                n > 0 ? 'text-text-secondary' : 'text-text-disabled'
              }`}
            >
              {n}
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
            <MachineActionsMenu machine={row.original} onAction={confirm} />
          </div>
        ),
      },
    ],
    [],
  )

  async function executeAction() {
    if (!pending) return
    const { kind, machine } = pending
    try {
      if (kind === 'start') await startMachine.mutateAsync(machine.id)
      if (kind === 'stop') await stopMachine.mutateAsync(machine.id)
      if (kind === 'reboot') await rebootMachine.mutateAsync(machine.id)
      if (kind === 'restart-agents') await restartAgents.mutateAsync(machine.id)
      if (kind === 'delete') {
        await deleteMachine.mutateAsync(machine.id)
        setPending(null)
        return
      }
    } finally {
      setPending(null)
    }
  }

  const isActing =
    startMachine.isPending ||
    stopMachine.isPending ||
    rebootMachine.isPending ||
    deleteMachine.isPending ||
    restartAgents.isPending

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-xl font-bold text-text-primary">Machines</h1>
        <Button
          variant="primary"
          size="sm"
          onClick={() => navigate(prefixed('/machines/new'))}
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
          Failed to load machines: {error.message}
        </div>
      )}

      {machines && machines.length === 0 && (
        <EmptyState
          icon={<Server className="h-6 w-6" strokeWidth={1.5} />}
          title="No machines provisioned"
          description="Provision a machine to host one or more agents."
          action={(
            <Button
              variant="primary"
              size="sm"
              onClick={() => navigate(prefixed('/machines/new'))}
            >
              <Plus className="h-4 w-4" />
              New machine
            </Button>
          )}
        />
      )}

      {machines && machines.length > 0 && (
        <>
          {/* Mobile: card view */}
          <div className="space-y-3 md:hidden">
            {machines.map((m) => (
              <Card key={m.id} onClick={() => navigate(prefixed(`/machines/${m.id}`))}>
                <div className="flex items-start justify-between gap-4">
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-medium text-text-primary truncate">{m.id}</span>
                      <StatusBadge phase={m.phase} />
                    </div>
                    <p className="mt-1 text-xs text-text-muted">
                      {m.spec.machineType ?? m.spec.provider}
                      {m.spec.zone && <> &middot; {m.spec.zone}</>}
                      {m.spec.spot && ' \u00b7 spot'}
                    </p>
                    {/*
                     * "Capacity:" subline removed (#245). It rendered
                     * spec.capacity (operator-typed at create time) which
                     * disagreed with the bars below — those compute from
                     * status.assignableCapacity / availableCapacity, the
                     * post-#240 controller-published source of truth. The
                     * bars and the Machine detail page now agree; this
                     * card no longer publishes a contradictory total.
                     */}
                    {m.status.agentCount !== undefined && m.status.agentCount > 0 && (
                      <p className="mt-0.5 text-xs text-text-muted">
                        {m.status.agentCount} agent{m.status.agentCount !== 1 ? 's' : ''}
                      </p>
                    )}
                  </div>
                  <MachineActionsMenu machine={m} onAction={confirm} />
                </div>
                {(() => {
                  const asn = m.status?.assignableCapacity
                  const avl = m.status?.availableCapacity
                  if (!asn || !avl) return null
                  const cpuTotal = parseCpu(asn.cpu ?? '0')
                  const cpuUsed  = cpuTotal - parseCpu(avl.cpu ?? '0')
                  const memTotal = parseMemoryGi(asn.memory ?? '0')
                  const memUsed  = memTotal - parseMemoryGi(avl.memory ?? '0')
                  // Disk: omit the row entirely on machines that haven't
                  // surfaced ephemeralStorage yet (pre-#129 PR-C / no node
                  // yet) so the layout doesn't show an empty bar.
                  const hasDisk = !!asn.ephemeralStorage && !!avl.ephemeralStorage
                  const diskTotal = hasDisk ? parseMemoryGi(asn.ephemeralStorage!) : 0
                  const diskUsed  = hasDisk ? diskTotal - parseMemoryGi(avl.ephemeralStorage!) : 0
                  return (
                    <div className="mt-2 flex flex-col gap-2">
                      <CapacityBar used={cpuUsed} total={cpuTotal} label="CPU" />
                      <CapacityBar used={memUsed} total={memTotal} label="Memory" />
                      {hasDisk && <CapacityBar used={diskUsed} total={diskTotal} label="Disk" />}
                    </div>
                  )
                })()}
              </Card>
            ))}
          </div>

          {/* Desktop: data table */}
          <div className="hidden md:block">
            <DataTable
              columns={columns}
              data={machines}
              getRowId={(m) => m.id}
              onRowClick={(m) => navigate(prefixed(`/machines/${m.id}`))}
            />
          </div>
        </>
      )}

      <ConfirmDialog
        open={pending !== null}
        title={
          pending?.kind === 'delete'
            ? 'Delete machine?'
            : pending?.kind === 'restart-agents'
              ? 'Restart all agents on machine?'
              : `${pending?.kind === 'start' ? 'Start' : pending?.kind === 'stop' ? 'Stop' : 'Reboot'} machine?`
        }
        message={
          pending?.kind === 'restart-agents'
            ? `Each agent on "${pending?.machine.id}" will be brought down and back up in parallel with its session brief preserved. Expect ~30s of unavailability per agent. Skipped: Suspended, Stopped, Draining.`
            : `${pending?.kind === 'delete' ? 'This will permanently delete' : 'This will ' + pending?.kind} machine "${pending?.machine.id}".`
        }
        confirmLabel={pending?.kind === 'delete' ? 'Delete' : 'Confirm'}
        dangerous={pending?.kind === 'delete'}
        loading={isActing}
        onConfirm={() => void executeAction()}
        onCancel={() => setPending(null)}
      />
    </div>
  )
}

function MachineActionsMenu({
  machine,
  onAction,
}: {
  machine: Machine
  onAction: (kind: MachineActionKind, machine: Machine) => void
}) {
  return (
    <div
      className="flex items-center shrink-0"
      onClick={(e) => e.stopPropagation()}
    >
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="sm" aria-label="Machine actions">
            <MoreHorizontal className="h-4 w-4" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => onAction('start', machine)}>
            <Play className="h-3.5 w-3.5" />
            Start
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => onAction('stop', machine)}>
            <Square className="h-3.5 w-3.5" />
            Stop
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => onAction('reboot', machine)}>
            <RotateCcw className="h-3.5 w-3.5" />
            Reboot
          </DropdownMenuItem>
          <DropdownMenuItem
            onSelect={() => onAction('restart-agents', machine)}
            disabled={machine.phase !== 'Ready' && machine.phase !== 'Running'}
          >
            <Zap className="h-3.5 w-3.5" />
            Restart all agents
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            variant="danger"
            onSelect={() => onAction('delete', machine)}
          >
            <Trash2 className="h-3.5 w-3.5" />
            Delete
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}

/**
 * MachineAvailableCell — desktop-table cell for the "Available" column.
 * Renders one mini bar + free/total numbers per CPU + Memory + Disk. Color
 * band follows the standard capacityBars thresholds (green < 70%, yellow
 * 70-90%, red >= 90%). Falls back to the operator-typed spec.capacity total
 * when the controller hasn't populated assignable/available yet (Pending
 * machine or pre-#142 CR that hasn't been reconciled).
 *
 * Disk row is omitted on machines that haven't surfaced ephemeralStorage
 * yet (pre-#129 PR-C clusters or machines without an observed node) — the
 * field is `omitempty` on the wire, so its absence is a real signal.
 */
export function MachineAvailableCell({ machine }: { machine: Machine }) {
  const asn = machine.status?.assignableCapacity
  const avl = machine.status?.availableCapacity
  // assignable + available are populated atomically by the machine
  // controller; either-or-neither is the only observed pattern, so checking
  // both with `!asn || !avl` is sufficient.
  if (!asn || !avl) {
    const cap = machineCapacity(machine)
    if (!cap) return <span className="text-xs text-text-disabled">—</span>
    return (
      <span className="font-mono text-xs tabular-nums text-text-muted">
        capacity {cap.cpu.toFixed(2)} cpu · {cap.memoryGi.toFixed(1)} Gi
      </span>
    )
  }
  const cpuTotal = parseCpu(asn.cpu ?? '0')
  const cpuFree = parseCpu(avl.cpu ?? '0')
  const memTotal = parseMemoryGi(asn.memory ?? '0')
  const memFree = parseMemoryGi(avl.memory ?? '0')
  const hasDisk = !!asn.ephemeralStorage && !!avl.ephemeralStorage
  const diskTotal = hasDisk ? parseMemoryGi(asn.ephemeralStorage!) : 0
  const diskFree = hasDisk ? parseMemoryGi(avl.ephemeralStorage!) : 0
  return (
    <div className="flex flex-col gap-1 min-w-[180px]">
      <AvailableBar
        label="CPU"
        free={cpuFree}
        total={cpuTotal}
        unit=""
        decimals={2}
      />
      <AvailableBar
        label="Mem"
        free={memFree}
        total={memTotal}
        unit=" GiB"
        decimals={1}
      />
      {hasDisk && (
        <AvailableBar
          label="Disk"
          free={diskFree}
          total={diskTotal}
          unit=" GiB"
          decimals={1}
        />
      )}
    </div>
  )
}

interface AvailableBarProps {
  label: string
  free: number
  total: number
  unit: string
  decimals: number
}

function AvailableBar({ label, free, total, unit, decimals }: AvailableBarProps) {
  const used = Math.max(0, total - free)
  // Use the shared capacityBars helpers so the band thresholds + colour
  // mapping stay aligned with the wizard + standalone CapacityBar
  // component. capacityBand handles the total === 0 case (returns 'green'
  // with 0% fill — empty bar, no NaN math).
  const band = capacityBand(used, total)
  const pct = pctUsed(used, total)
  return (
    <div>
      <div className="flex justify-between font-mono text-[11px] tabular-nums text-text-secondary mb-0.5">
        <span>{label}</span>
        <span aria-label={`${label}: ${free.toFixed(decimals)}${unit} free of ${total.toFixed(decimals)}${unit}`}>
          {free.toFixed(decimals)} / {total.toFixed(decimals)}
          {unit} free
        </span>
      </div>
      <div className="h-1.5 rounded bg-surface-overlay overflow-hidden">
        <div
          data-band={band}
          className={`h-full ${BAND_BG_CLASS[band]}`}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  )
}
