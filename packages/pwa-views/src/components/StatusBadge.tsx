import { useEffect, useRef, useState } from 'react'
import { phaseStyle, toneBadgeClasses, type PhaseKey } from '../lib/design/status'

interface Props {
  phase: PhaseKey
  label?: string
  pulse?: boolean
  className?: string
}

export function StatusBadge({ phase, label, pulse: pulseOverride, className = '' }: Props) {
  const { tone, pulse: phasePulse } = phaseStyle(phase)
  const pulse = pulseOverride ?? phasePulse
  const prev = useRef<PhaseKey>(phase)
  const [flash, setFlash] = useState(false)

  useEffect(() => {
    if (prev.current === phase) return
    prev.current = phase
    setFlash(true)
    const id = window.setTimeout(() => setFlash(false), 900)
    return () => window.clearTimeout(id)
  }, [phase])

  return (
    <span
      className={`kyber-status-badge relative inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 font-mono text-[11px] font-medium uppercase tracking-[0.05em] ring-1 ring-inset ${toneBadgeClasses[tone]} ${className}`}
      style={flash ? { animation: 'kyber-flash 800ms ease-out' } : undefined}
    >
      {pulse && (
        <span className="h-1.5 w-1.5 rounded-full bg-current opacity-80" />
      )}
      {label ?? (phase || 'Unknown')}
    </span>
  )
}
