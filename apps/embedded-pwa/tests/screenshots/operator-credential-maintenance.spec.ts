// Declared fixtures for the read-only maintenance UI; real API proof is separate.
import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'
import { usageCredential } from '../../../../packages/pwa-views/src/test/operator-usage-fixture'
const out = process.env.KYBER_SCREENSHOT_DIR ?? path.resolve('screenshots')
test.beforeAll(() => fs.mkdirSync(out, { recursive: true }))
test('maintenance block fits desktop and mobile', async ({ page }, info) => {
  await page.addInitScript((credential) => {
    const original = window.fetch.bind(window)
    window.fetch = async (input, init) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.origin)
      const reply = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } }))
      if (url.pathname === '/api/v1/operator-credentials') return reply({ maintenanceAvailable: true, currentCredentialId: 'admin', inventoryScope: 'current-control-plane', observedAt: '2026-10-24T12:00:00Z', configurationAvailable: true, usageAvailable: false, warnings: [], credentials: [{ ...credential, source: 'installation', displayName: 'Installation administrator', configured: true }] })
      if (url.pathname === '/api/v1/operator-credentials/maintenance') return reply({ observedAt: '2026-10-24T12:00:00Z', blocked: true, version: `"${'A'.repeat(43)}"`, operation: { kind: 'upgrade', executionId: 'e703854e-4d01-45d7-a321-5c737d173d46', startedAt: '2026-10-24T10:00:00Z' } })
      return original(input, init)
    }
  }, usageCredential)
  await page.goto('/settings/access')
  const block = page.getByText('Credential changes are blocked by an upgrade operation.', { exact: true }).locator('..')
  await expect(block).toBeVisible()
  await block.getByText('Operation details').click()
  await block.evaluate(el => el.scrollIntoView({ block: 'center' }))
  const bounds = await block.boundingBox()
  expect(bounds!.x).toBeGreaterThanOrEqual(0)
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(page.viewportSize()!.width)
  await expect(block.getByRole('button')).toHaveCount(0)
  await block.screenshot({ path: path.join(out, `${info.project.name}-maintenance-block.png`) })
})
