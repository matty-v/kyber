// Tests for the pure appendSample reducer powering useFleetHistory.
// The hook itself is tested by the FleetOverview integration tests; the
// reducer is tested here so the rolling-window math has explicit coverage.

import { describe, it, expect } from 'vitest'
import { appendSample, FLEET_HISTORY_WINDOW, type FleetHistory } from './useFleetHistory'
import type { FleetSummary } from '../lib/types'

function sample(over: Partial<FleetSummary> = {}): FleetSummary {
  return {
    machineCount: 1,
    agentCount: 1,
    machinesByPhase: { Running: 1 },
    agentsByPhase: { Running: 1 },
    ...over,
  }
}

describe('appendSample', () => {
  it('appends a new sample to existing series', () => {
    const empty = { machinesByPhase: {}, agentsByPhase: {}, samples: 0 }
    const after = appendSample(empty, sample())
    expect(after.samples).toBe(1)
    expect(after.machinesByPhase.Running).toEqual([1])
    expect(after.agentsByPhase.Running).toEqual([1])
  })

  it('back-fills new phases with zeros so all series stay the same length', () => {
    let h: FleetHistory = { machinesByPhase: {}, agentsByPhase: {}, samples: 0 }
    h = appendSample(h, sample({ machinesByPhase: { Running: 2 } }))
    h = appendSample(h, sample({ machinesByPhase: { Running: 3 } }))
    // Now a new phase appears.
    h = appendSample(h, sample({ machinesByPhase: { Running: 3, Failed: 1 } }))

    expect(h.machinesByPhase.Running).toEqual([2, 3, 3])
    expect(h.machinesByPhase.Failed).toEqual([0, 0, 1])
    expect(h.machinesByPhase.Running.length).toBe(h.machinesByPhase.Failed.length)
  })

  it('back-fills disappeared phases with 0 instead of leaving the series stale', () => {
    let h: FleetHistory = { machinesByPhase: {}, agentsByPhase: {}, samples: 0 }
    h = appendSample(h, sample({ machinesByPhase: { Running: 3, Failed: 1 } }))
    h = appendSample(h, sample({ machinesByPhase: { Running: 4 } })) // Failed gone

    expect(h.machinesByPhase.Failed).toEqual([1, 0])
    expect(h.machinesByPhase.Running).toEqual([3, 4])
  })

  it('caps the rolling window at FLEET_HISTORY_WINDOW samples', () => {
    let h: FleetHistory = { machinesByPhase: {}, agentsByPhase: {}, samples: 0 }
    for (let i = 0; i < FLEET_HISTORY_WINDOW + 5; i++) {
      h = appendSample(h, sample({ machinesByPhase: { Running: i } }))
    }
    expect(h.samples).toBe(FLEET_HISTORY_WINDOW)
    expect(h.machinesByPhase.Running.length).toBe(FLEET_HISTORY_WINDOW)
    // Oldest 5 samples were trimmed off the front.
    expect(h.machinesByPhase.Running[0]).toBe(5)
    expect(h.machinesByPhase.Running[FLEET_HISTORY_WINDOW - 1]).toBe(FLEET_HISTORY_WINDOW + 4)
  })

  it('keeps machines and agents histories independent', () => {
    let h: FleetHistory = { machinesByPhase: {}, agentsByPhase: {}, samples: 0 }
    h = appendSample(
      h,
      sample({
        machinesByPhase: { Running: 1 },
        agentsByPhase: { Running: 2, Failed: 3 },
      }),
    )
    expect(Object.keys(h.machinesByPhase)).toEqual(['Running'])
    expect(Object.keys(h.agentsByPhase).sort()).toEqual(['Failed', 'Running'])
  })
})
