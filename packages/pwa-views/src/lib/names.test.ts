import { describe, it, expect } from 'vitest'
import { toKebabCase, toKebabCaseTyping } from './names'

describe('toKebabCaseTyping', () => {
  it.each([
    ['my-', 'my-'],
    ['my-agent', 'my-agent'],
    ['-', '-'],
    ['-foo', '-foo'],
    ['my-agent-', 'my-agent-'],
    ['My Agent', 'my-agent'],
    ['my  agent', 'my-agent'],
    ['my!@#agent', 'my-agent'],
    ['ALICE', 'alice'],
    ['', ''],
  ])('preserves trailing hyphens / lowercases / collapses runs: %j → %j', (input, expected) => {
    expect(toKebabCaseTyping(input)).toBe(expected)
  })

  it('truncates input to 63 characters', () => {
    const long = 'a'.repeat(100)
    expect(toKebabCaseTyping(long)).toHaveLength(63)
  })

  // The keystroke-level regression case from #189.
  it('typing "my-agent" character by character produces "my-agent" at every step', () => {
    const target = 'my-agent'
    let buf = ''
    for (const ch of target) {
      buf += ch
      // simulate the input's value pipeline: append char, run keystroke sanitize
      buf = toKebabCaseTyping(buf)
    }
    expect(buf).toBe('my-agent')
  })
})

describe('toKebabCase (full sanitize for blur / submit)', () => {
  it.each([
    ['my-', 'my'],
    ['my-agent', 'my-agent'],
    ['-foo-bar-', 'foo-bar'],
    ['---', ''],
    ['My Agent ', 'my-agent'],
    [' my agent ', 'my-agent'],
    ['ALICE', 'alice'],
    ['', ''],
  ])('strips leading/trailing hyphens + lowercases + collapses: %j → %j', (input, expected) => {
    expect(toKebabCase(input)).toBe(expected)
  })

  it('truncates input to 63 characters', () => {
    const long = 'a'.repeat(100)
    expect(toKebabCase(long)).toHaveLength(63)
  })
})
