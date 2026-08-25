import { RefreshCw } from 'lucide-react'
import type { Machine } from '../lib/types'
import { DiagnosticDetails } from './DiagnosticDetails'
import { Button } from './Button'

type Props = {
  machine: Machine
  reliableFallbackMode?: 'Unsupported' | 'Manual' | 'Automatic'
  retrying?: boolean
  onRetryCostOptimized?: () => void
}

export function MachineRecoveryBanner({ machine, reliableFallbackMode, retrying, onRetryCostOptimized }: Props) {
  const recovering =
    machine.phase === 'Preempted' ||
    machine.phase === 'Replacing' ||
    machine.status.availability === 'Pending' ||
    machine.status.availability === 'Recovering'
  const interruptible = machine.spec.availabilityClass === 'costOptimized' || machine.spec.spot
  const reliableFallback =
    interruptible && machine.status.effectiveAvailabilityClass === 'reliable'
  const retryInProgress = Boolean(
    machine.spec.costOptimizedRetryRequest &&
      machine.spec.costOptimizedRetryRequest !== machine.status.costOptimizedRetryObserved,
  )
  if (!recovering && !reliableFallback) return null

  const replacing = machine.phase === 'Preempted' || machine.phase === 'Replacing'
  const details = [
    `machine: ${machine.id}`,
    `phase: ${machine.phase}`,
    machine.status.availability ? `availability: ${machine.status.availability}` : '',
    machine.spec.location || machine.spec.zone ? `location: ${machine.spec.location || machine.spec.zone}` : '',
    machine.status.providerRef ? `providerRef: ${machine.status.providerRef}` : '',
    machine.status.nodeName ? `lastNode: ${machine.status.nodeName}` : '',
    machine.status.message ? `message: ${machine.status.message}` : '',
    machine.status.fallbackReason ? `fallbackReason: ${machine.status.fallbackReason}` : '',
    machine.status.fallbackSince ? `fallbackSince: ${machine.status.fallbackSince}` : '',
  ].filter(Boolean).join('\n')

  return (
    <div role="status" className="mb-4 rounded-lg border border-accent/40 bg-accent/10 p-4">
      <div className="flex items-start gap-3">
        <RefreshCw className="mt-0.5 h-5 w-5 shrink-0 animate-spin text-accent" aria-hidden="true" />
        <div className="min-w-0 flex-1 space-y-2">
          <h2 className="text-sm font-semibold text-text-primary">
            {reliableFallback
              ? retryInProgress
                ? 'Retrying cost-optimized capacity'
                : 'Running on reliable fallback capacity'
              : replacing
                ? 'Machine capacity is recovering'
                : 'Machine capacity is starting'}
          </h2>
          <p className="text-xs text-text-secondary">
            {reliableFallback
              ? retryInProgress
                ? 'Kyber is briefly transitioning this Machine back toward cost-optimized capacity while preserving its exact Agent disk. If capacity is unavailable, it will return to reliable capacity.'
                : 'Cost-optimized capacity exceeded the five-minute availability threshold. Kyber preserved the exact Agent disk and switched this Machine to reliable-rate capacity in the same zone.'
              : replacing && interruptible
              ? 'The provider reclaimed this cost-optimized node. Kyber is waiting for replacement capacity and will resume its agents automatically.'
              : replacing
                ? 'This machine lost its Ready node. Kyber is repairing its capacity and will resume its agents automatically.'
                : 'Kyber requested provider capacity for this machine. Its agents will start automatically when a Ready node joins.'}
          </p>
          {reliableFallback && !retryInProgress && reliableFallbackMode && reliableFallbackMode !== 'Unsupported' && onRetryCostOptimized && (
            <Button size="sm" variant="secondary" disabled={retrying} onClick={onRetryCostOptimized}>
              <RefreshCw className="h-3.5 w-3.5" aria-hidden="true" />
              Retry cost-optimized capacity
            </Button>
          )}
          {details && <DiagnosticDetails details={details} />}
        </div>
      </div>
    </div>
  )
}
