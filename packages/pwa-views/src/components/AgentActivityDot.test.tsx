import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import type { Agent } from '../lib/types'
import { AgentActivityDot, formatRelativeTime } from './AgentActivityDot'
import { recordActivityReceipt } from '../lib/activity'
describe('AgentActivityDot', () => {
 it('does not show a stopped agent working', () => {
  const {container}=render(<AgentActivityDot agent={{phase:'Stopped',activity:{state:'working'}} as Agent}/>);expect(container).toBeEmptyDOMElement()
 })
 it('shows legacy reporting as unavailable', () => {
  render(<AgentActivityDot agent={{phase:'Running',activity:{state:'working'}} as Agent}/>);expect(screen.getByTestId('agent-activity-dot')).toHaveAttribute('data-state','unavailable')
 })
 it('shows a fresh current inventory as working', () => {
  const now=Date.now();const a=recordActivityReceipt([{phase:'Running',activity:{work:{state:'working',serverTime:new Date(now).toISOString(),observedAt:new Date(now).toISOString(),expiresAt:new Date(now+60000).toISOString(),foregroundCount:1,backgroundCount:0}}} as Agent])[0]
  render(<AgentActivityDot agent={a}/>);expect(screen.getByTestId('agent-activity-dot')).toHaveAttribute('data-state','working')
 })
})
describe('formatRelativeTime', () => {
  const NOW = new Date('2026-05-03T22:00:00Z')

  it('seconds for <60s', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 5_000).toISOString(), NOW)).toBe('5s ago')
  })
  it('minutes for 1m-59m', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 8 * 60_000).toISOString(), NOW)).toBe('8m ago')
  })
  it('hours for 1h-23h', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 4 * 3600_000).toISOString(), NOW)).toBe('4h ago')
  })
  it('days past 24h', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 3 * 86400_000).toISOString(), NOW)).toBe('3d ago')
  })
  it('"recently" for unparseable timestamp', () => {
    expect(formatRelativeTime('garbage', NOW)).toBe('recently')
  })
})
