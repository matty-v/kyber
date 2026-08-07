// Screenshot capture for compact density mode (#105). Visits AgentList +
// Settings at desktop resolution (1440x900) twice — once in the default
// comfortable mode, once after toggling Compact in Settings — to make the
// padding delta visible in the PR.
//
// Mobile project is included via the Playwright config but the density
// rules don't fire there (the provider forces comfortable on viewports
// ≤768px), so the mobile screenshots will look identical to the
// comfortable desktop screenshots — kept for parity, not as a delta.

import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const OUT_DIR = path.resolve(__dirname, '..', '..', 'screenshots')

test.beforeAll(() => {
  fs.mkdirSync(OUT_DIR, { recursive: true })
})

test.beforeEach(async ({ page }) => {
  await page.goto('/agents')
  await page.waitForFunction(
    () => document.documentElement.getAttribute('data-mocks') === 'ready',
  )
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-density-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

async function setDensity(page: import('@playwright/test').Page, value: 'comfortable' | 'compact') {
  await page.goto('/settings')
  await page.getByRole('radio', { name: new RegExp(value, 'i') }).click()
  // The DensityProvider writes data-density on <body>; wait for it.
  await page.waitForFunction(
    (v) => document.body.dataset.density === v,
    value,
  )
}

test('AgentList — comfortable (default)', async ({ page }) => {
  await expect(page.getByRole('heading', { name: /agents/i })).toBeVisible()
  await shot(page, '01-agentlist-comfortable')
})

test('AgentList — compact', async ({ page }) => {
  // Compact is forced to comfortable on mobile by design — skip on mobile so
  // the test doesn't time out waiting for body[data-density=compact].
  test.skip(test.info().project.name === 'mobile', 'compact is desktop-only')
  await setDensity(page, 'compact')
  await page.goto('/agents')
  await expect(page.getByRole('heading', { name: /agents/i })).toBeVisible()
  await shot(page, '02-agentlist-compact')
})

test('Settings — density toggle visible', async ({ page }) => {
  await page.goto('/settings')
  await expect(page.getByRole('heading', { name: /settings/i })).toBeVisible()
  await expect(page.getByText(/^Density$/)).toBeVisible()
  await shot(page, '03-settings-density-toggle')
})
