import { describe, expect, it } from 'vitest'
import type { RuntimeDescriptor } from './types'
import { codexOpenRouterEvidence } from './openrouter-inference'

const codex: RuntimeDescriptor = {
  id: 'codex', name: 'Codex', contractVersion: '1.0', profile: 'interactive-tmux-v1',
  cancellation: 'notify_only', features: [], authModes: [],
  routeModelEvidence: [{ route: 'openrouter-responses', model: 'cohere/north-mini-code:free',
    harnessVersion: '0.162.1', observedAt: '2026-10-11', upstream: 'unknown',
    outcomes: { interactive: 'pass', 'task-receipt': 'fail', 'task-cancel': 'fail', jobs: 'untested' },
    evidenceUrl: 'https://linear.app/matty-v/issue/MAT-115',
  }],
}

describe('Codex OpenRouter evidence labels', () => {
  it('shows the observed failure and unknown upstream without claiming qualification', () => {
    const message = codexOpenRouterEvidence(codex, 'cohere/north-mini-code:free')
    expect(message).toContain('task receipt fail')
    expect(message).toContain('Upstream provider unknown')
    expect(message).not.toContain('jobs pass')
    expect(message).not.toContain('qualified')
  })

  it('keeps an unlisted model explicitly unverified', () => {
    expect(codexOpenRouterEvidence(codex, 'other/model')).toContain('unverified')
  })
})
