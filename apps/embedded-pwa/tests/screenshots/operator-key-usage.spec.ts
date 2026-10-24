// Fixture previews of the activity UI; live dev verification is recorded separately.
import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'
import { usageCredential, usageView } from '../../../../packages/pwa-views/src/test/operator-usage-fixture'
import type { OperatorCredentialInventory } from '../../../../packages/pwa-views/src/lib/types'
const out = process.env.KYBER_SCREENSHOT_DIR ?? path.resolve('screenshots')
test.beforeAll(() => { fs.mkdirSync(out, { recursive: true }) })
test.beforeEach(async ({ page }) => {
  const usage = usageView()
  usage.identities[0].authority = 'configuration'; usage.items[0].authority = 'configuration'
  usage.items[0].authenticated = '124'; usage.items[0].actions = { 'agents:read': '100', 'credentials:read': '24' }
  const integration = { credentialId: 'task-worker', principalId: 'task-worker', tenantId: 'tasks', authority: 'configuration' as const }
  usage.identities.push(integration)
  usage.items.push({ ...integration, transport: 'bearer', authenticated: '18', denied: '1', generation: '1', lastAcceptedAt: '2026-10-24T12:03:00Z', actions: { 'tasks:write': '14', 'tasks:read': '4' } })
  const inventory: OperatorCredentialInventory = {
    currentCredentialId: 'admin', inventoryScope: 'current-control-plane', observedAt: usage.observedAt,
    configurationAvailable: true, usageAvailable: true, warnings: [], credentials: [
      { ...usageCredential, source: 'installation', name: 'installation-admin', displayName: 'Installation administrator', configured: true, permissions: ['Full access to this Kyber installation'], machineResources: ['*'] },
      { id: 'task-worker', principalId: 'task-worker', tenantId: 'tasks', activityTrackable: true, name: 'Task worker', displayName: 'Task worker', accessLevel: 'restricted', state: 'accepted', source: 'configuration', configured: true, scopes: ['tasks:create', 'tasks:read'], agentResources: ['kyber-system/example'], permissions: ['Submit and read tasks for Example'] },
    ],
  }
  await page.clock.setFixedTime(new Date('2026-10-24T12:05:00Z'))
  await page.addInitScript(({ inventory, usage }) => {
    const original = window.fetch.bind(window)
    window.fetch = async (input, init) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.origin)
      const reply = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } }))
      if (url.pathname === '/api/v1/operator-credentials') return reply(inventory)
      if (url.pathname === '/api/v1/operator-credentials/usage') {
        if (new URLSearchParams(location.search).has('delayed')) return reply({ ...usage, coverage: { ...usage.coverage, currentObserverFresh: false, dropped: '3' } })
        return reply(usage)
      }
      return original(input, init)
    }
  }, { inventory, usage })
  await page.goto('/settings/access')
  await page.waitForFunction(() => document.documentElement.getAttribute('data-mocks') === 'ready')
})
test('observed key activity fits desktop and mobile', async ({ page }, info) => {
  const coverage = page.getByText('Observed key activity · Partial coverage').locator('..')
  await expect(coverage).toBeVisible()
  await coverage.screenshot({ path: path.join(out, `${info.project.name}-operator-usage-coverage.png`) })
  const row = page.getByRole('heading', { name: 'Task worker', exact: true }).locator('..').locator('..')
  await row.scrollIntoViewIfNeeded()
  await row.getByText('Observed request types').click()
  await expect(row.getByText('Submit or manage tasks')).toBeVisible()
  const bounds = await row.boundingBox()
  expect(bounds!.x).toBeGreaterThanOrEqual(0)
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(page.viewportSize()!.width)
  await row.screenshot({ path: path.join(out, `${info.project.name}-operator-usage-key.png`) })
})
test('reporting gaps remain visible', async ({ page }, info) => {
  await page.goto('/settings/access?delayed=1')
  const coverage = page.getByText('Observed key activity · Partial coverage').locator('..')
  await expect(coverage.getByText('Some activity observations were lost.')).toBeVisible()
  await expect(coverage.getByText('This server’s activity reports are delayed.')).toBeVisible()
  await coverage.screenshot({ path: path.join(out, `${info.project.name}-operator-usage-gap.png`) })
})
