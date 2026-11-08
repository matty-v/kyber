import type { ComputeConfig } from '../../lib/types'
import { describe, it, expect } from 'vitest'
import { IDENTITY_REPO_SLUG_RE, identityRepoTemplate, DEFAULT_IDENTITY_TEMPLATE } from './identity-utils'

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

describe('identityRepoTemplate', () => {
  it('uses the installation source independently of the destination owner', () => {
    const config = { identity: { repoOwner: 'destination-org', defaultTemplate: 'source-org/custom-template' } } as ComputeConfig
    expect(identityRepoTemplate(config)).toBe('source-org/custom-template')
  })
  it('preserves the upstream default for older control planes', () => {
    expect(identityRepoTemplate(undefined)).toBe(DEFAULT_IDENTITY_TEMPLATE)
    expect(identityRepoTemplate({ identity: { repoOwner: 'destination-org' } } as ComputeConfig)).toBe(DEFAULT_IDENTITY_TEMPLATE)
  })
})
