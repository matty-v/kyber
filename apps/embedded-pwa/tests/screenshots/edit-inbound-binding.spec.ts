// Screenshot capture for the WebhooksTab edit-binding flow (#222). Three
// frames per viewport: the populated Webhooks list with the new Edit
// pencil button visible, the wizard opened in edit mode at the Auth
// step (pre-filled from the binding), and the Review step with a Save
// button replacing Create.

import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const AGENT = 'alice'
const __dirname = path.dirname(fileURLToPath(import.meta.url))
const OUT_DIR = path.resolve(__dirname, '..', '..', 'screenshots')

test.beforeAll(() => {
  fs.mkdirSync(OUT_DIR, { recursive: true })
})

test.beforeEach(async ({ page }) => {
  await page.goto(`/agents/${AGENT}`)
  await page.waitForFunction(
    () => document.documentElement.getAttribute('data-mocks') === 'ready',
  )
  await expect(page.getByText(AGENT, { exact: false }).first()).toBeVisible({
    timeout: 10_000,
  })
  // Open the Webhooks tab.
  await page.getByRole('tab', { name: /webhooks/i }).click()
  await expect(page.getByText(/ci-watch/i).first()).toBeVisible()
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-edit-binding-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

test('Webhooks tab — populated list with Edit pencil', async ({ page }) => {
  // The new Edit pencil button is visible alongside Copy/Rotate/Delete.
  await expect(page.getByRole('button', { name: /edit webhook/i }).first()).toBeVisible()
  await shot(page, '01-list-with-edit')
})

test('Edit dialog — Auth step pre-filled from existing binding', async ({ page }) => {
  await page.getByRole('button', { name: /edit webhook/i }).first().click()
  await expect(page.getByRole('dialog', { name: /edit webhook — ci-watch/i })).toBeVisible()
  await expect(page.getByLabel(/signature header/i)).toHaveValue('X-Hub-Signature-256')
  await shot(page, '02-edit-auth')
})

test('Edit dialog — Review step shows Save button', async ({ page }) => {
  await page.getByRole('button', { name: /edit webhook/i }).first().click()
  await expect(page.getByRole('dialog', { name: /edit webhook/i })).toBeVisible()
  // Auth → Matching → Fields → Action → Limits → Review.
  for (let i = 0; i < 5; i++) {
    await page.getByRole('button', { name: /^next$/i }).click()
  }
  await expect(page.getByRole('button', { name: /^save$/i })).toBeVisible()
  await shot(page, '03-edit-review')
})
