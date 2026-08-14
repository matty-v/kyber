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
    expect(agentActionConfirmMessage('suspend', 'han')).toBe(
      'This will suspend agent "han".'
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
})
