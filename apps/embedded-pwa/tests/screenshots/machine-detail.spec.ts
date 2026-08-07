// Screenshot capture for the MachineDetail page. The header is a
// single-row layout: back arrow + title + status badge + ⋯ trigger
// (#292 collapsed everything else into the More dropdown, mirroring
// AgentDetail's #262 fix). Two frames per viewport — header collapsed,
// and More dropdown open — so reviewers can confirm the one-row layout
// holds on a 390px mobile and that all lifecycle actions surface from
// the menu.

import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const MACHINE = 'demo'
const __dirname = path.dirname(fileURLToPath(import.meta.url))
const OUT_DIR = path.resolve(__dirname, '..', '..', 'screenshots')

test.beforeAll(() => {
  fs.mkdirSync(OUT_DIR, { recursive: true })
})

test.beforeEach(async ({ page }) => {
  await page.goto(`/machines/${MACHINE}`)
  await page.waitForFunction(
    () => document.documentElement.getAttribute('data-mocks') === 'ready',
  )
  await expect(page.getByRole('heading', { name: MACHINE })).toBeVisible({
    timeout: 10_000,
  })
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-machine-detail-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

test('header — single-row layout, More dropdown closed', async ({ page }) => {
  // Single row (#292): back arrow + title + status badge + ⋯ trigger.
  // No standalone primary button — Restart all agents lives in the menu.
  await expect(page.getByRole('button', { name: /more actions/i })).toBeVisible()
  await expect(
    page.getByRole('button', { name: /restart all agents/i }),
  ).toHaveCount(0)
  await shot(page, '01-header-collapsed')
})

test('header — More dropdown open shows Restart all agents at top of Lifecycle', async ({ page }) => {
  await page.getByRole('button', { name: /more actions/i }).click()
  // Running mock: Restart all agents (top), Stop, Reboot, Delete.
  await expect(page.getByRole('menuitem', { name: /restart all agents/i })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: /^stop$/i })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: /^reboot$/i })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: /^delete$/i })).toBeVisible()
  await shot(page, '02-more-open')
})
