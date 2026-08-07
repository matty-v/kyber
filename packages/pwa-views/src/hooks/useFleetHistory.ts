// Rolling per-phase history of FleetSummary samples (#104).
//
// Decision per issue #104 D1: Option B — client-side accumulation. Avoids a
// backend ring buffer for V1; sparklines start empty on page load and fill
// in as useFleetSummary polls every 30s. State is intentionally not
// persisted across tab refreshes — sparkline is an ephemeral visual cue,
// not historical record.

import { useEffect, useRef, useState } from 'react'
import type { FleetSummary } from '../lib/types'

// Window length in samples. With useFleetSummary's 30s refetchInterval
// this gives 30 minutes of history once filled — long enough to spot a
// crash loop or restart cascade, short enough to keep memory negligible.
export const FLEET_HISTORY_WINDOW = 60

export interface FleetHistory {
  // Per-phase sample series, oldest first. Each entry is the count of
  // machines/agents observed in that phase at sample time. Phases that
  // appear part-way through the window are back-filled with 0s so all
  // series have the same length — keeps multi-sparkline layouts aligned.
  machinesByPhase: Record<string, number[]>
  agentsByPhase: Record<string, number[]>
  // Number of samples currently in the window. Lets consumers render a
  // "filling…" affordance while the buffer is short.
  samples: number
}

const emptyHistory: FleetHistory = {
  machinesByPhase: {},
  agentsByPhase: {},
  samples: 0,
}

/**
 * useFleetHistory — accumulates a rolling window of FleetSummary samples.
 *
 * Pass the `data` field returned by useFleetSummary; the hook captures
 * every fresh value on render and keeps the last FLEET_HISTORY_WINDOW
 * samples in component state.
 *
 * To avoid pushing the same FleetSummary object multiple times when the
 * parent re-renders for unrelated reasons, the hook deduplicates on
 * reference identity — React Query returns the same object until a new
 * fetch resolves.
 */
export function useFleetHistory(latest: FleetSummary | undefined): FleetHistory {
  const [history, setHistory] = useState<FleetHistory>(emptyHistory)
  const lastSampleRef = useRef<FleetSummary | null>(null)

  useEffect(() => {
    if (!latest) return
    if (lastSampleRef.current === latest) return
    lastSampleRef.current = latest
    setHistory((prev) => appendSample(prev, latest))
  }, [latest])

  return history
}

// Pure transform — exposed for unit tests. Pushes one sample onto the
// per-phase ring buffers and trims to FLEET_HISTORY_WINDOW.
export function appendSample(prev: FleetHistory, sample: FleetSummary): FleetHistory {
  const samples = Math.min(prev.samples + 1, FLEET_HISTORY_WINDOW)
  return {
    samples,
    machinesByPhase: appendPhaseMap(prev.machinesByPhase, sample.machinesByPhase, prev.samples),
    agentsByPhase: appendPhaseMap(prev.agentsByPhase, sample.agentsByPhase, prev.samples),
  }
}

// appendPhaseMap pushes the new per-phase counts into the existing
// series. New phases get back-filled with 0s so they enter at the same
// length as the existing series.
function appendPhaseMap(
  prev: Record<string, number[]>,
  next: Record<string, number>,
  prevSamples: number,
): Record<string, number[]> {
  // Union of all phase keys we've ever seen — phases that disappear
  // from `next` (e.g. last machine in Failed got deleted) still need a
  // 0-pushed entry so existing sparklines keep aligned.
  const keys = new Set<string>([...Object.keys(prev), ...Object.keys(next)])
  const out: Record<string, number[]> = {}
  for (const key of keys) {
    const existing = prev[key] ?? new Array(prevSamples).fill(0)
    const value = next[key] ?? 0
    const updated = [...existing, value]
    if (updated.length > FLEET_HISTORY_WINDOW) {
      updated.splice(0, updated.length - FLEET_HISTORY_WINDOW)
    }
    out[key] = updated
  }
  return out
}
