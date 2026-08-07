import { describe, it, expect } from 'vitest'
import { humanizeCron } from './cron'

describe('humanizeCron — daily', () => {
  it('on the hour', () => {
    expect(humanizeCron('0 9 * * *')).toBe('Daily at 9 AM')
    expect(humanizeCron('0 0 * * *')).toBe('Daily at 12 AM')
    expect(humanizeCron('0 12 * * *')).toBe('Daily at 12 PM')
    expect(humanizeCron('0 13 * * *')).toBe('Daily at 1 PM')
    expect(humanizeCron('0 23 * * *')).toBe('Daily at 11 PM')
  })

  it('with a non-zero minute', () => {
    expect(humanizeCron('30 9 * * *')).toBe('Daily at 9 AM:30')
    expect(humanizeCron('15 14 * * *')).toBe('Daily at 2 PM:15')
  })

  it('multiple hours all in one half (the issue example)', () => {
    expect(humanizeCron('0 1,2,3 * * *')).toBe('Daily at 1, 2, 3 AM')
    expect(humanizeCron('0 13,14,15 * * *')).toBe('Daily at 1, 2, 3 PM')
  })

  it('multiple hours straddling AM/PM spell each suffix', () => {
    // 11 AM, 12 PM, 1 PM — straddle, so fall back to per-hour formatting.
    expect(humanizeCron('0 11,12,13 * * *')).toBe('Daily at 11 AM, 12 PM, 1 PM')
  })
})

describe('humanizeCron — weekly', () => {
  it('on the hour', () => {
    expect(humanizeCron('0 9 * * 1')).toBe('Weekly on Mon at 9 AM')
    expect(humanizeCron('0 0 * * 0')).toBe('Weekly on Sun at 12 AM')
  })

  it('with a non-zero minute', () => {
    expect(humanizeCron('30 17 * * 5')).toBe('Weekly on Fri at 5 PM:30')
  })

  it('returns null for multi-day weekly (ambiguous to phrase)', () => {
    expect(humanizeCron('0 9 * * 1,3,5')).toBeNull()
  })
})

describe('humanizeCron — every-minute / every-hour', () => {
  it('every minute', () => {
    expect(humanizeCron('* * * * *')).toBe('Every minute')
  })

  it('every N minutes', () => {
    expect(humanizeCron('*/5 * * * *')).toBe('Every 5 minutes')
    expect(humanizeCron('*/15 * * * *')).toBe('Every 15 minutes')
    // */1 reads more naturally as plain "Every minute".
    expect(humanizeCron('*/1 * * * *')).toBe('Every minute')
  })

  it('every hour at :MM', () => {
    expect(humanizeCron('30 * * * *')).toBe('Every hour at :30')
    expect(humanizeCron('5 * * * *')).toBe('Every hour at :05')
  })
})

describe('humanizeCron — fallback (returns null)', () => {
  it('returns null on patterns we don\'t handle', () => {
    expect(humanizeCron('0 9 * jan *')).toBeNull() // named month
    expect(humanizeCron('0 9 1 * *')).toBeNull() // specific day-of-month
    expect(humanizeCron('0-30 9 * * *')).toBeNull() // ranges in minute field
    expect(humanizeCron('not a cron')).toBeNull()
    expect(humanizeCron('')).toBeNull()
    expect(humanizeCron('0 9 * *')).toBeNull() // 4 fields
  })

  it('returns null for multi-minute daily (would phrase awkwardly)', () => {
    expect(humanizeCron('0,30 9 * * *')).toBeNull()
  })

  it('returns null for out-of-range values', () => {
    expect(humanizeCron('0 24 * * *')).toBeNull()
    expect(humanizeCron('60 9 * * *')).toBeNull()
    expect(humanizeCron('0 9 * * 7')).toBeNull()
  })
})
