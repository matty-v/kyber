import { describe, it, expect } from 'vitest'
import { IDENTITY_REPO_SLUG_RE } from './identity-utils'

describe('IDENTITY_REPO_SLUG_RE', () => {
  it.each([
    'matty-v/kyber-agent-template',
    'foo/bar',
    'a/b',
    'foo-bar/Repo_Name.with-dots',
    'matty-v/MyRepo',
  ])('accepts valid slug: %s', (slug) => {
    expect(IDENTITY_REPO_SLUG_RE.test(slug)).toBe(true)
  })

  it.each([
    '',
    'foo',
    'foo/',
    '/bar',
    '-foo/bar',
    'foo/-bar',
    'Foo/bar', // uppercase owner not allowed
    'foo bar/baz',
    'foo/bar/baz',
  ])('rejects invalid slug: %s', (slug) => {
    expect(IDENTITY_REPO_SLUG_RE.test(slug)).toBe(false)
  })
})
