// Screenshot capture for the keyboard-shortcut help overlay (#106 Slice A).
// One frame per viewport showing the cheat sheet.

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
  // Make sure the route content has rendered before we synthesize events —
  // on mobile-emulated runs, React mounts a tick later than data-mocks=ready
  // signals MSW readiness, and the hook's keydown listener attaches in a
  // useEffect, so dispatching too early no-ops silently.
  await page.getByRole('heading', { name: /fleet overview/i }).waitFor({ state: 'visible' })
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-shortcuts-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

test('shortcut help overlay opens on ?', async ({ page }) => {
  // Dispatch the keydown directly. page.keyboard.press('?') and
  // page.keyboard.press('Shift+Slash') both produce events whose `key`
  // value depends on layout/OS; the hook listens for key === '?' so we
  // synthesize the event we actually want.
  await page.evaluate(() => {
    window.dispatchEvent(new KeyboardEvent('keydown', { key: '?', bubbles: true }))
  })
  await expect(
    page.getByRole('dialog', { name: /keyboard shortcuts/i }),
  ).toBeVisible()
  await expect(page.getByText('g f')).toBeVisible()
  await expect(page.getByText(/go to fleet/i)).toBeVisible()
  await shot(page, '01-help-overlay')
})
