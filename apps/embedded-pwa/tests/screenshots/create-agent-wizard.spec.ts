// Screenshot capture for the Create Agent wizard — runs in both desktop and
// mobile Playwright projects (see playwright.config.ts). Produces a pair of
// PNGs per scenario for Matt's pre-merge review.
//
// Goal of these screenshots: confirm Phase D (#213) renders correctly across
// viewports, especially the chip strip on the mobile width (390x844). The
// chip strip uses overflow-x-auto so 5 chips can scroll horizontally without
// breaking the card layout.

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
  await page.goto('/agents/new')
  await page.waitForFunction(
    () => document.documentElement.getAttribute('data-mocks') === 'ready',
  )
  // Wait for the wizard to render (Name field is on step 1).
  await expect(page.getByLabel(/name/i)).toBeVisible({ timeout: 10_000 })
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-wizard-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

test('step 1 (Basics) — empty state', async ({ page }) => {
  await shot(page, '01-basics-empty')
})

test('step 1 (Basics) — Next-disabled hint visible after typing partial', async ({ page }) => {
  // Type just a name (no machine) so the step is invalid → hint renders.
  await page.getByLabel(/name/i).fill('alice')
  await shot(page, '02-basics-partial-with-hint')
})

test('step 2 (Resources) — capacity card (fits)', async ({ page }) => {
  await page.getByLabel(/name/i).fill('alice')
  // The mock machines list has at least one option; pick the first non-placeholder.
  const machineSelect = page.getByLabel(/machine/i)
  const options = await machineSelect.locator('option').all()
  // Find the first non-placeholder option (placeholder has value="")
  for (const opt of options) {
    const value = await opt.getAttribute('value')
    if (value) {
      await machineSelect.selectOption(value)
      break
    }
  }
  await page.getByRole('button', { name: /next/i }).click()
  await expect(page.getByLabel(/runtime/i)).toBeVisible()
  // Wait for the capacity card to render (defaults: cpu=1, memory=2Gi, disk=50Gi,
  // fits the mock machine 10/20/140). Disk row is part of the card since #129 PR-C.
  await expect(page.getByTestId('wizard-capacity-card')).toBeVisible()
  await expect(page.getByTestId('proposed-bar-disk')).toBeVisible()
  await shot(page, '03-resources-capacity-fits')
})

// Note: the "exceeds" visual state for the capacity card isn't covered by a
// screenshot here because the wizard's CPU dropdown maxes at 8 and the
// global machine mock has 10 CPU available — no in-UI combination triggers
// the exceeds branch. The unit tests in WizardCapacityCard.test.tsx cover
// that path directly. Future: add a small-machine fixture and re-enable.

test('step 5 (Review) — populated summary with Edit affordances', async ({ page }) => {
  // Walk all 5 steps with the api-key path (simpler than OAuth for screenshots).
  await page.getByLabel(/name/i).fill('alice')
  const machineSelect = page.getByLabel(/machine/i)
  const options = await machineSelect.locator('option').all()
  for (const opt of options) {
    const value = await opt.getAttribute('value')
    if (value) {
      await machineSelect.selectOption(value)
      break
    }
  }
  await page.getByRole('button', { name: /next/i }).click() // → Resources
  await page.getByRole('button', { name: /next/i }).click() // → Identity
  await page.getByRole('button', { name: /next/i }).click() // → Auth
  await page.getByLabel(/^Authentication$/).selectOption('api-key')
  await page.getByLabel(/anthropic api key/i).fill('sk-ant-screenshot')
  await page.getByRole('button', { name: /next/i }).click() // → Review
  // The chip strip also has a Review button; target the section heading instead.
  await expect(page.getByRole('heading', { name: /^review$/i })).toBeVisible()
  await shot(page, '04-review-populated')
})

test('discard-changes dialog open after Cancel on dirty state', async ({ page }) => {
  await page.getByLabel(/name/i).fill('alice')
  await page.getByRole('button', { name: /^cancel$/i }).first().click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await shot(page, '05-discard-dialog')
})
