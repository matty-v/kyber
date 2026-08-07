// Screenshot the Settings page Rotate API key flow (#143). Three frames:
// the new card on the page, the confirm dialog, and the post-rotation
// readback modal showing the new key + copy affordance.

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
  await page.goto('/settings')
  await page.waitForFunction(
    () => document.documentElement.getAttribute('data-mocks') === 'ready',
  )
  await expect(page.getByRole('heading', { name: /^settings$/i })).toBeVisible()
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-rotate-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

test('Settings — Rotate API key card visible', async ({ page }) => {
  await expect(page.getByRole('heading', { name: /rotate api key/i })).toBeVisible()
  await shot(page, '01-card')
})

test('Settings — confirm dialog open', async ({ page }) => {
  await page.getByRole('button', { name: /rotate api key/i }).click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await expect(page.getByText(/rotate api key\?/i)).toBeVisible()
  await shot(page, '02-confirm')
})

test('Settings — post-rotation readback modal', async ({ page }) => {
  await page.getByRole('button', { name: /rotate api key/i }).click()
  await expect(page.getByRole('dialog')).toBeVisible()
  // Click the dialog's Rotate (confirm) button — the inline dialog uses
  // the "Rotate" confirmLabel.
  await page.getByRole('button', { name: /^rotate$/i }).click()
  await expect(page.getByRole('dialog', { name: /new api key/i })).toBeVisible()
  await expect(page.getByText(/will not be shown again/i)).toBeVisible()
  await shot(page, '03-readback')
})
