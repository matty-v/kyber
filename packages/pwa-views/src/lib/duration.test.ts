import { describe, it, expect } from 'vitest'
import { formatDuration } from './duration'

// kyber#426 — Agent Activity Breakdown durations must render as compound,
// human-readable strings (two most-significant units), not raw seconds.
describe('formatDuration', () => {
  describe('magnitude bands', () => {
    it('renders >= 1 day as "Nd Nh"', () => {
      // 689402s = 7d 23h 30m 2s → two most-significant units, hours truncated
      expect(formatDuration(689402)).toBe('7d 23h')
      expect(formatDuration(98867.1)).toBe('1d 3h')
    })

    it('renders >= 1 hour and < 1 day as "Nh Nm"', () => {
      expect(formatDuration(5880)).toBe('1h 38m') // 1h 38m 0s
      expect(formatDuration(86399)).toBe('23h 59m')
    })

    it('renders >= 1 minute and < 1 hour as "Nm Ns"', () => {
      expect(formatDuration(90)).toBe('1m 30s')
      expect(formatDuration(3599)).toBe('59m 59s')
    })

    it('renders < 1 minute as "N.Ns" (one decimal)', () => {
      expect(formatDuration(12.8)).toBe('12.8s')
      expect(formatDuration(45.7)).toBe('45.7s')
    })
  })

  describe('boundaries', () => {
    it('renders 0 as a finite "0s", not blank', () => {
      expect(formatDuration(0)).toBe('0s')
    })

    it('renders a sub-minute value with one decimal', () => {
      expect(formatDuration(59.4)).toBe('59.4s')
    })

    it('renders exactly 60s as "1m 0s"', () => {
      expect(formatDuration(60)).toBe('1m 0s')
    })

    it('renders exactly 3600s as "1h 0m"', () => {
      expect(formatDuration(3600)).toBe('1h 0m')
    })

    it('renders exactly 86400s as "1d 0h"', () => {
      expect(formatDuration(86400)).toBe('1d 0h')
    })

    it('renders a multi-day value as "Nd Nh"', () => {
      expect(formatDuration(180000)).toBe('2d 2h') // 2d 2h 0m
    })
  })

  describe('robustness', () => {
    it('truncates secondary units (never rolls over to an invalid unit)', () => {
      // 7199s = 1h 59m 59s → "1h 59m", never "1h 60m"
      expect(formatDuration(7199)).toBe('1h 59m')
      // 86399s already covered: 23h 59m, never "24h 0m"
    })

    it('clamps negative/non-finite input to "0s"', () => {
      expect(formatDuration(-5)).toBe('0s')
      expect(formatDuration(NaN)).toBe('0s')
    })
  })
})
