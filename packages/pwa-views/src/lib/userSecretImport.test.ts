import { describe, expect, it } from 'vitest'
import {
  MAX_USER_SECRET_ENTRY_BYTES,
  parseUserSecretImport,
  userSecretFilePath,
  validateUserSecretKey,
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

// Mirrors pkg/usersecrets TestKeyGrammarPerKind (MAT-90 G13).
describe('validateUserSecretKey per kind', () => {
  it.each([
    ['vault-cert.pem', false, true, '/user-secrets/vault-cert.pem'],
    ['id_ed25519', false, true, '/user-secrets/id_ed25519'],
    ['APP_PEM', true, true, '/user-secrets/app_pem.bin'],
    ['../x', false, false, ''],
    ['a..b', false, false, ''],
    ['.hidden', false, false, ''],
    ['app_pem.bin', false, false, ''],
    ['App_Pem.bin', false, true, '/user-secrets/App_Pem.bin'],
    ['KYBER_TOKEN', false, false, ''],
    ['a'.repeat(253), false, true, `/user-secrets/${'a'.repeat(253)}`],
    ['a'.repeat(254), false, false, ''],
  ])('%s: kv valid=%s, file valid=%s', (key, kvValid, fileValid, path) => {
    expect(validateUserSecretKey(key, 'kv') === null).toBe(kvValid)
    expect(validateUserSecretKey(key, 'file') === null).toBe(fileValid)
    if (fileValid) expect(userSecretFilePath(key)).toBe(path)
  })
})
