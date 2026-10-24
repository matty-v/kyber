// Staged UI preview: deterministic metadata fixtures, not live adoption evidence.
import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'

const out = process.env.KYBER_SCREENSHOT_DIR ?? path.resolve('screenshots')
test.beforeAll(() => fs.mkdirSync(out, { recursive: true }))
test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    const original = window.fetch.bind(window)
    window.fetch = async (input, init) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.origin)
      const response = (data: unknown) => Promise.resolve(new Response(JSON.stringify(data), { headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' } }))
      if (url.pathname === '/api/v1/operator-credentials') return response({ version: 'preview-version', currentCredentialId: 'admin', managementAvailable: true, observedAt: '2026-10-24T15:00:00Z', inventoryScope: 'installation-registry', configurationAvailable: false, usageAvailable: false, warnings: [], credentials: [
        { id: 'admin', name: 'Installation administrator', displayName: 'Installation administrator', accessLevel: 'administrator', state: 'accepted', source: 'installation', scopes: [], agentResources: ['*'], machineResources: ['*'], permissions: ['Full access to this installation'], configured: false },
        { id: 'metrics', name: 'Dashboard', displayName: 'Dashboard', accessLevel: 'restricted', state: 'accepted', source: 'managed', scopes: ['metrics:read'], agentResources: [], permissions: ['Read fleet metrics'], configured: false, createdAt: '2026-10-24T12:00:00Z' },
      ] })
      if (url.pathname === '/api/v1/operator-credentials/options') return response({ version: 1, namespace: 'kyber-system', permissions: [{ scope: 'metrics:read', label: 'Read fleet metrics', target: 'none' }, { scope: 'agents:exec', label: 'Open Agent terminals', target: 'agent' }] })
      if (url.pathname === '/api/v1/operator-credential-operations/pending') return response({ items: [] })
      return original(input, init)
    }
  })
  await page.goto('/settings/access')
  await page.waitForFunction(() => document.documentElement.getAttribute('data-mocks') === 'ready')
})
test('access key controls and saved-value workflow fit the viewport', async ({ page }, info) => {
  await expect(page.getByRole('button', { name: 'New access key' })).toBeEnabled()
  await page.getByRole('button', { name: 'New access key' }).scrollIntoViewIfNeeded()
  await page.screenshot({ path: path.join(out, `${info.project.name}-operator-keys.png`), fullPage: true })
  await page.getByRole('button', { name: 'New access key' }).click()
  const dialog = page.getByRole('dialog', { name: 'New access key' })
  await expect(dialog).toBeVisible()
  await dialog.getByLabel('Name', { exact: true }).fill('Reporting dashboard')
  await dialog.getByLabel('Read fleet metrics').check()
  await page.screenshot({ path: path.join(out, `${info.project.name}-operator-key-policy.png`) })
  await dialog.getByRole('button', { name: 'Next' }).click()
  await expect(dialog.getByRole('button', { name: 'Activate key' })).toBeDisabled()
  await expect(dialog.getByLabel('New key value')).toBeVisible()
  const bounds = await dialog.boundingBox()
  const viewport = page.viewportSize()!
  expect(bounds!.x).toBeGreaterThanOrEqual(0)
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(viewport.width)
  expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(viewport.height)
  await page.screenshot({ path: path.join(out, `${info.project.name}-operator-key-value.png`), mask: [dialog.getByLabel('New key value')] })
})
