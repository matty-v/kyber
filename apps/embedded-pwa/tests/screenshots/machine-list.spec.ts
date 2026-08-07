// Screenshot capture for the MachineList mobile card (#245). Captures
// the card after dropping the stale "Capacity:" subline so reviewers
// can confirm the only totals on the card now come from the bars,
// which agree with the Machine detail page.

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
  await page.goto('/machines')
  await page.waitForFunction(
    () => document.documentElement.getAttribute('data-mocks') === 'ready',
  )
  await expect(page.getByRole('heading', { name: /machines/i })).toBeVisible({
    timeout: 10_000,
  })
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-machine-list-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

test('mobile card — no stale Capacity subline; bars are the only totals', async ({ page }) => {
  // The card should not contain a "Capacity:" line anymore (#245).
  await expect(page.getByText(/^capacity:/i)).toHaveCount(0)
  await shot(page, '01-mobile-card')
})
