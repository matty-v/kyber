import { describe, expect, it } from 'vitest'
import {
  MAX_USER_SECRET_ENTRY_BYTES,
  parseUserSecretImport,
} from './userSecretImport'

describe('parseUserSecretImport', () => {
  it('parses entries, comments, blank lines, export, empty values, and embedded equals', () => {
    expect(parseUserSecretImport('\uFEFF# secrets\nFOO=bar\n\nexport TOKEN=a=b=c\nEMPTY=')).toEqual([
      { key: 'FOO', value: 'bar' },
      { key: 'TOKEN', value: 'a=b=c' },
      { key: 'EMPTY', value: '' },
    ])
  })

  it('preserves whitespace in values', () => {
    expect(parseUserSecretImport('  TOKEN=  abc123   ')).toEqual([
      { key: 'TOKEN', value: '  abc123   ' },
    ])
  })

  it.each([
    ['missing separator', 'FOO', 'Line 1: expected KEY=VALUE'],
    ['invalid key', 'bad=value', 'Line 1: Key must match'],
    ['reserved key', 'USER_FOO=value', 'Line 1: Key must not start'],
    ['duplicate key', 'FOO=one\nFOO=two', 'Line 2: duplicate key FOO'],
    ['empty file', '# only comments\n', 'File contains no KEY=VALUE entries'],
  ])('rejects %s', (_name, input, message) => {
    expect(() => parseUserSecretImport(input)).toThrow(message)
  })

  it('rejects a value over the per-entry limit', () => {
    expect(() => parseUserSecretImport(`FOO=${'x'.repeat(MAX_USER_SECRET_ENTRY_BYTES + 1)}`)).toThrow(
      `FOO exceeds ${MAX_USER_SECRET_ENTRY_BYTES} bytes`,
    )
  })
})
