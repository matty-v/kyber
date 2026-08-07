// packages/pwa-views/src/lib/pkce.test.ts
import { describe, expect, it } from 'vitest'
import { generatePkcePair } from './pkce'

describe('generatePkcePair', () => {
  it('produces a challenge that is sha256+base64url of the verifier', async () => {
    const { verifier, challenge } = await generatePkcePair()
    expect(verifier.length).toBeGreaterThanOrEqual(43)
    expect(verifier.length).toBeLessThanOrEqual(128)
    const enc = new TextEncoder().encode(verifier)
    const digest = await crypto.subtle.digest('SHA-256', enc)
    const expected = btoa(String.fromCharCode(...new Uint8Array(digest)))
      .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
    expect(challenge).toBe(expected)
  })

  it('returns different values on each call', async () => {
    const a = await generatePkcePair()
    const b = await generatePkcePair()
    expect(a.verifier).not.toBe(b.verifier)
  })
})
