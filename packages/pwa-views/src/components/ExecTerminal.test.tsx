import { describe, expect, it } from 'vitest'
import { terminalExtraKeys } from './terminal-controls'

describe('terminalExtraKeys', () => {
  it('provides the control sequences needed by tmux and terminal agents on touch devices', () => {
    expect(Object.fromEntries(terminalExtraKeys.map(({ label, data }) => [label, data]))).toEqual({
      Esc: '\x1b',
      Tab: '\t',
      '⇧Tab': '\x1b[Z',
      'Ctrl-C': '\x03',
      'Ctrl-B': '\x02',
      '←': '\x1b[D',
      '↑': '\x1b[A',
      '↓': '\x1b[B',
      '→': '\x1b[C',
      Enter: '\r',
    })
  })
})
