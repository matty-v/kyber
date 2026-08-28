import { describe, it, expect } from 'vitest'
import { agentActionConfirmMessage } from './agentMessages'

describe('agentActionConfirmMessage', () => {
  it('returns an expanded warning for delete', () => {
    const msg = agentActionConfirmMessage('delete', 'chewie')
    expect(msg).toContain('permanently delete agent "chewie"')
    expect(msg).toContain('wipe its persistent disk')
    expect(msg).toContain('Kyber-managed secrets')
    expect(msg).toContain('GitHub identity repo is NOT deleted')
    expect(msg).toContain('cannot be undone')
  })

  it('returns a short generic message for simple lifecycle actions', () => {
    expect(agentActionConfirmMessage('start', 'han')).toBe(
      'This will start agent "han".'
    )
    expect(agentActionConfirmMessage('stop', 'han')).toBe(
      'This will stop agent "han".'
    )
  })

  it('returns an expanded message for restart (pod-level) to disambiguate from restart-session', () => {
    const msg = agentActionConfirmMessage('restart', 'han')
    expect(msg).toContain('Restart the pod for "han"')
    expect(msg).toContain('rolls the container')
    // Explicitly points operators at the lighter-weight alternative.
    expect(msg).toContain('Restart session')
  })

  it('returns context-reset wording for restart-session (#128)', () => {
    const msg = agentActionConfirmMessage('restart-session', 'han')
    expect(msg).toContain('Reset the in-flight session for "han"')
    expect(msg).toContain('pod stays up')
    expect(msg).toContain('conversation / context is lost')
    // Reassurance on what is NOT touched.
    expect(msg).toContain('Memory and identity files on disk are not affected')
  })

  it('returns non-destructive wording for compact-session', () => {
    const msg = agentActionConfirmMessage('compact-session', 'han')
    expect(msg).toContain('Compact the session for "han"')
    expect(msg).toContain('summarizes')
    expect(msg).toContain('keeps working')
    // The whole point of a separate confirm: compaction is NOT a context
    // reset, so it must not borrow restart-session's loss warning.
    expect(msg).not.toContain('context is lost')
    // Sets the expectation that 200 != finished.
    expect(msg).toContain('may take a minute')
  })

  it('returns wedged-agent-recovery wording for force-needs-auth (#395)', () => {
    const msg = agentActionConfirmMessage('force-needs-auth', 'han')
    expect(msg).toContain('Force "han" into re-authorization')
    expect(msg).toContain('deletes its running')
    expect(msg).toContain('NeedsAuth')
    // Reassurance on what is NOT touched.
    expect(msg).toContain('Memory and identity files on disk are not affected')
  })

  it('describes runtime repair preservation and failure behavior', () => {
    const msg = agentActionConfirmMessage('repair-runtime', 'han')
    expect(msg).toContain('maintenance pod')
    expect(msg).toContain('Credentials, memory, identity files, and conversation state are preserved')
    expect(msg).toContain('remains parked in BrokenRuntime')
  })

  it('returns NeedsAuth retry wording for retry-startup, not the generic fallback (kyber#26)', () => {
    const msg = agentActionConfirmMessage('retry-startup', 'lando')
    expect(msg).toContain('Rebuild the pod for "lando"')
    // The copy has to say WHICH credentials, because the whole point is that
    // the operator is retrying with what is already there.
    expect(msg).toContain('credentials it already has')
    // And it has to point at the other half of the choice — this control is an
    // addition beside the Re-authorize panel, not a replacement for it.
    expect(msg).toContain('Re-authorize panel')
    expect(msg).toContain('Memory and identity files on disk are not affected')
    // The generic `This will <action> agent` fallback reads as
    // "This will retry-startup agent" — the failure this case exists to avoid.
    expect(msg).not.toContain('This will retry-startup')
  })
})
