import { describe, expect, it } from 'vitest'
import type { LoggingTarget } from '../lib/types'
import { filterLogSelections, formatLogLine } from './Logs'

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
