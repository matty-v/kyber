import { describe, expect, it } from 'vitest'
import type { LoggingTarget } from '../lib/types'
import { BoundedLogBuffer, filterLogSelections, formatLogLine, isAtLogTail, logSelectionKey } from './Logs'

const targets: LoggingTarget[] = [
  {
    namespace: 'kyber', pod: 'agent-sol', podUid: 'uid-sol', component: 'agent', workload: 'sol', agent: 'sol',
    phase: 'Running', sources: ['kubelet', 'archive'], liveAvailable: true, archiveAvailable: true,
    containers: [
      { name: 'agent', component: 'agent', effectiveLevel: 'info', managedLevel: false, init: false },
      { name: 'kyber-status-sidecar', component: 'status-sidecar', effectiveLevel: 'info', managedLevel: true, init: false },
    ],
  },
  {
    namespace: 'kyber', pod: 'control-plane', podUid: 'uid-cp', component: 'control-plane', workload: 'control-plane',
    phase: 'Running', sources: ['kubelet'], liveAvailable: true, archiveAvailable: false,
    containers: [{ name: 'control-plane', component: 'control-plane', effectiveLevel: 'info', managedLevel: true, init: false }],
  },
]

describe('fleet log selection', () => {
  it('defaults to every live agent and platform container', () => {
    expect(filterLogSelections(targets, '', '', 'kubelet')).toHaveLength(3)
  })

  it('filters independently by agent and component', () => {
    expect(filterLogSelections(targets, 'sol', 'status-sidecar', 'kubelet').map((selection) => selection.container.name))
      .toEqual(['kyber-status-sidecar'])
  })

  it('omits targets that do not support the selected source', () => {
    expect(filterLogSelections(targets, '', '', 'archive')).toHaveLength(2)
  })

  it('keeps the selection identity stable across periodic refreshes', () => {
    const selected = filterLogSelections(targets, 'sol', '', 'kubelet')
    expect(logSelectionKey(selected, 'kubelet')).toBe('kubelet:uid-sol/agent,uid-sol/kyber-status-sidecar')
    expect(logSelectionKey([...selected].reverse(), 'kubelet')).toBe(logSelectionKey(selected, 'kubelet'))
    expect(logSelectionKey(selected, 'kubelet')).not.toBe(logSelectionKey(selected, 'archive'))
    expect(logSelectionKey(selected, 'kubelet')).not.toBe(logSelectionKey(selected.slice(0, 1), 'kubelet'))
  })
})

describe('fleet log live refresh', () => {
  it('follows the tail only while the operator remains near it', () => {
    expect(isAtLogTail(461, 1000, 500)).toBe(true)
    expect(isAtLogTail(460, 1000, 500)).toBe(false)
    expect(isAtLogTail(100, 1000, 500)).toBe(false)
  })
})

describe('fleet log formatting', () => {
  const selection = filterLogSelections(targets, 'sol', 'agent', 'kubelet')[0]

  it('extracts structured fields while preserving the exact raw line', () => {
    const raw = '{"time":"2026-08-23T14:00:00Z","level":"WARN","msg":"queue filling","depth":4}'
    expect(formatLogLine(raw, selection, 0)).toMatchObject({
      timestamp: '2026-08-23T14:00:00Z', level: 'warn', agent: 'sol', component: 'agent',
      source: 'agent-sol/agent', message: 'queue filling', raw,
    })
  })

  it('keeps unstructured third-party output readable and intact', () => {
    const raw = 'redis ready to accept connections'
    expect(formatLogLine(raw, selection, 0)).toMatchObject({ message: raw, raw, timestamp: '', level: '' })
  })
})

describe('fleet log buffering', () => {
  const selection = filterLogSelections(targets, 'sol', 'agent', 'kubelet')[0]

  it('caps the merged result while sources are still being consumed', () => {
    const buffer = new BoundedLogBuffer()
    buffer.add(Array.from({ length: 6000 }, (_, index) => formatLogLine(`line ${index}`, selection, index)))
    expect(buffer.snapshot()).toHaveLength(5000)
    expect(buffer.truncated).toBe(true)
  })

  it('caps aggregate raw bytes independently of line count', () => {
    const buffer = new BoundedLogBuffer()
    buffer.add(Array.from({ length: 10 }, (_, index) => formatLogLine('x'.repeat(500_000), selection, index)))
    expect(buffer.snapshot().length).toBeLessThan(10)
    expect(buffer.truncated).toBe(true)
  })
})
