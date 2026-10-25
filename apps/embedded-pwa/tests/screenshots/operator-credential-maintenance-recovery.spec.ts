// Declared recovery UI fixture. This is layout/interaction proof, not live recovery proof.
import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'
import { usageCredential } from '../../../../packages/pwa-views/src/test/operator-usage-fixture'
const out = process.env.KYBER_SCREENSHOT_DIR ?? path.resolve('screenshots')
test.beforeAll(() => fs.mkdirSync(out, { recursive: true }))
test('explicit maintenance recovery fits desktop and mobile', async ({ page }, info) => {
  await page.addInitScript((credential) => {
    const original = window.fetch.bind(window)
    window.fetch = async (input, init) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.origin)
      const reply = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } }))
      if (url.pathname === '/api/v1/operator-credentials') return reply({ maintenanceAvailable: true, currentCredentialId: 'admin', inventoryScope: 'current-control-plane', observedAt: '2026-10-24T12:00:00Z', configurationAvailable: true, usageAvailable: false, warnings: [], credentials: [{ ...credential, source: 'installation', displayName: 'Installation administrator', configured: true }] })
      if (url.pathname === '/api/v1/operator-credentials/maintenance') return reply({ observedAt: '2026-10-24T12:00:00Z', blocked: true, recoveryAvailable: true, version: `"${'A'.repeat(43)}"`, operation: { kind: 'upgrade', executionId: 'e703854e-4d01-45d7-a321-5c737d173d46', startedAt: '2026-10-24T10:00:00Z' } })
      if (url.pathname.endsWith('/maintenance/readiness')) return reply({ ready: true, reason: 'ready', version: `"${'A'.repeat(43)}"` })
      if (url.pathname.endsWith('/maintenance/recover')) return reply({ operationId: '57e9038a-c35e-4b7b-bcb7-afcd532c28a1', status: 'pending', observedAt: '2026-10-24T12:00:00Z' })
      if (url.pathname.endsWith('/maintenance/operations/resolve')) return reply({ operationId: '57e9038a-c35e-4b7b-bcb7-afcd532c28a1', status: 'success', observedAt: '2026-10-24T12:01:00Z' })
      return original(input, init)
    }
  }, usageCredential)
  await page.goto('/settings/access')
  await page.locator('html[data-mocks="ready"]').waitFor()
  const block = page.getByText('An upgrade operation holds credential maintenance.', { exact: true }).locator('..')
  // Initial fixture loading includes service-worker startup and the bounded
  // maintenance request/poll cycle; action assertions keep normal deadlines.
  await expect(block).toBeVisible({ timeout: 15_000 })
  await block.getByText('Check recovery', { exact: true }).click()
  await expect(block.getByText('Recover maintenance', { exact: true })).toBeVisible()
  await block.evaluate(el => el.scrollIntoView({ block: 'center' }))
  const bounds = await block.boundingBox()
  expect(bounds!.x).toBeGreaterThanOrEqual(0)
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(page.viewportSize()!.width)
  await block.screenshot({ path: path.join(out, `${info.project.name}-maintenance-ready.png`) })
  await block.getByText('Recover maintenance', { exact: true }).click()
  await expect(block.getByText('Recovery pending.', { exact: true })).toBeVisible()
  await block.evaluate(el => el.scrollIntoView({ block: 'center' }))
  await block.screenshot({ path: path.join(out, `${info.project.name}-maintenance-pending.png`) })
  await block.getByText('Resolve recovery', { exact: true }).click()
  await expect(block.getByText('Maintenance recovered.', { exact: true })).toBeVisible()
  await block.screenshot({ path: path.join(out, `${info.project.name}-maintenance-recovered.png`) })
})
