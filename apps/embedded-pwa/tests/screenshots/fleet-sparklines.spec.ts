// Screenshot capture for the FleetOverview sparklines (#104). Captures
// two states per viewport: empty (just-loaded, buffer holds one sample
// → placeholder dot) and populated (after several invalidate-driven
// refetches, the rolling window has enough points to draw a line).

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
  await page.goto('/')
  await page.waitForFunction(
    () => document.documentElement.getAttribute('data-mocks') === 'ready',
  )
  await expect(page.getByRole('heading', { name: /fleet overview/i })).toBeVisible({
    timeout: 10_000,
  })
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-fleet-sparklines-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

test('empty state — placeholder dot before the buffer fills', async ({ page }) => {
  // First sample has just landed; useFleetHistory has 1 point → Sparkline
  // renders the placeholder dot. This is the first-paint reality.
  await shot(page, '01-empty')
})

test('populated — sparklines after several refetches', async ({ page }) => {
  // Force several refetches via the window-exposed queryClient (mock-mode
  // only). Each refetch lands on the jittered fleet handler, walking the
  // counts so the rolling window builds a real trend.
  for (let i = 0; i < 12; i++) {
    await page.evaluate(() => {
      const client = (window as unknown as { __kyberQueryClient?: { invalidateQueries: (a: { queryKey: string[] }) => Promise<void> } }).__kyberQueryClient
      return client?.invalidateQueries({ queryKey: ['fleet'] })
    })
    // Tiny pause so React has time to re-render between samples.
    await page.waitForTimeout(80)
  }
  await shot(page, '02-populated')
})
