import { describe, it, expect } from 'vitest'
import { capacityBand, pctUsed, formatAvailLabel } from './capacityBars'
import type { Machine } from './types'

describe('pctUsed', () => {
  it('returns 0 when total is 0 (avoid divide-by-zero)', () => {
    expect(pctUsed(0, 0)).toBe(0)
    expect(pctUsed(5, 0)).toBe(0)
  })
  it('returns the integer percentage rounded down', () => {
    expect(pctUsed(1, 4)).toBe(25)
    expect(pctUsed(2.5, 4)).toBe(62) // rounded down from 62.5
    expect(pctUsed(4, 4)).toBe(100)
  })
  it('caps at 100 when used > total (over-allocation guard)', () => {
    expect(pctUsed(10, 4)).toBe(100)
  })
})

describe('capacityBand', () => {
  it('green when usage < 70%', () => {
    expect(capacityBand(0, 4)).toBe('green')
    expect(capacityBand(2.7, 4)).toBe('green')
    expect(capacityBand(2.79, 4)).toBe('green')
  })
  it('yellow at exactly 70%', () => {
    expect(capacityBand(2.8, 4)).toBe('yellow')
  })
  it('yellow when usage in [70, 90)', () => {
    expect(capacityBand(3.0, 4)).toBe('yellow')
    expect(capacityBand(3.59, 4)).toBe('yellow')
  })
  it('red at 90% or above', () => {
    expect(capacityBand(3.6, 4)).toBe('red')
    expect(capacityBand(4, 4)).toBe('red')
    expect(capacityBand(5, 4)).toBe('red') // over-allocated
  })
})

describe('formatAvailLabel', () => {
  // Minimal Machine fixture — only the fields the helper reads.
  const mk = (avail: { cpu?: string; memory?: string } | null): Machine =>
    ({
      id: 'razer',
      status: avail
        ? { availableCapacity: { cpu: avail.cpu, memory: avail.memory } }
        : {},
    } as unknown as Machine)

  it('formats with available CPU and Memory when both set', () => {
    expect(formatAvailLabel(mk({ cpu: '2', memory: '4Gi' }))).toBe(
      'razer — 2 CPU / 4 GiB avail',
    )
  })
  it('falls back to a neutral label when AvailableCapacity is missing', () => {
    expect(formatAvailLabel(mk(null))).toBe('razer — capacity unknown')
  })
  it('handles fractional CPU (m suffix)', () => {
    expect(formatAvailLabel(mk({ cpu: '500m', memory: '256Mi' }))).toBe(
      'razer — 0.5 CPU / 0.25 GiB avail',
    )
  })
})
