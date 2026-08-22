import { RefreshCw } from 'lucide-react'
import type { Machine } from '../lib/types'
import { DiagnosticDetails } from './DiagnosticDetails'

export function MachineRecoveryBanner({ machine }: { machine: Machine }) {
  const recovering =
    machine.phase === 'Preempted' ||
    machine.phase === 'Replacing' ||
    machine.status.availability === 'Pending' ||
    machine.status.availability === 'Recovering'
  if (!recovering) return null

  const interruptible = machine.spec.availabilityClass === 'costOptimized' || machine.spec.spot
  const replacing = machine.phase === 'Preempted' || machine.phase === 'Replacing'
  const details = [
    `machine: ${machine.id}`,
    `phase: ${machine.phase}`,
    machine.status.availability ? `availability: ${machine.status.availability}` : '',
    machine.spec.location || machine.spec.zone ? `location: ${machine.spec.location || machine.spec.zone}` : '',
    machine.status.providerRef ? `providerRef: ${machine.status.providerRef}` : '',
    machine.status.nodeName ? `lastNode: ${machine.status.nodeName}` : '',
    machine.status.message ? `message: ${machine.status.message}` : '',
  ].filter(Boolean).join('\n')

  return (
    <div role="status" className="mb-4 rounded-lg border border-accent/40 bg-accent/10 p-4">
      <div className="flex items-start gap-3">
        <RefreshCw className="mt-0.5 h-5 w-5 shrink-0 animate-spin text-accent" aria-hidden="true" />
        <div className="min-w-0 flex-1 space-y-2">
          <h2 className="text-sm font-semibold text-text-primary">
            {replacing ? 'Machine capacity is recovering' : 'Machine capacity is starting'}
          </h2>
          <p className="text-xs text-text-secondary">
            {replacing && interruptible
              ? 'The provider reclaimed this cost-optimized node. Kyber is waiting for replacement capacity and will resume its agents automatically.'
              : replacing
                ? 'This machine lost its Ready node. Kyber is repairing its capacity and will resume its agents automatically.'
                : 'Kyber requested provider capacity for this machine. Its agents will start automatically when a Ready node joins.'}
          </p>
          {details && <DiagnosticDetails details={details} />}
        </div>
      </div>
    </div>
  )
}
