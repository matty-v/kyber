import { describe, it, expect } from 'vitest'
import { parseAuthorizationInput } from './oauth'

describe('parseAuthorizationInput', () => {
  it('returns null on empty input', () => {
    expect(parseAuthorizationInput('')).toBeNull()
    expect(parseAuthorizationInput('   ')).toBeNull()
  })

  it('parses a full callback URL', () => {
    expect(
      parseAuthorizationInput(
        'https://platform.claude.com/oauth/code/callback?code=abc&state=xyz',
      ),
    ).toEqual({ code: 'abc', state: 'xyz' })
  })

  it('parses a callback URL without state', () => {
    expect(
      parseAuthorizationInput('https://platform.claude.com/oauth/code/callback?code=abc'),
    ).toEqual({ code: 'abc', state: undefined })
  })

  it('parses code#state shorthand', () => {
    expect(parseAuthorizationInput('abc#xyz')).toEqual({ code: 'abc', state: 'xyz' })
  })

  it('parses bare code=... query string fallback', () => {
    expect(parseAuthorizationInput('code=abc&state=xyz')).toEqual({
      code: 'abc',
      state: 'xyz',
    })
  })

  it('treats a bare token as the code (no state)', () => {
    expect(parseAuthorizationInput('just-a-bare-code')).toEqual({ code: 'just-a-bare-code' })
  })
})
