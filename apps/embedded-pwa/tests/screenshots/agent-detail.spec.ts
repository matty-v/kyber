// Screenshot capture for PWA UI PRs — runs in both desktop and mobile
// Playwright projects (see playwright.config.ts) so each tab gets a pair of
// PNGs dumped to pwa/screenshots/ for Matt's pre-merge review.
//
// Not a regression test: the point is to produce artifacts, not to assert
// visual equality. If a future PR wants visual diffing we can layer
// toHaveScreenshot on top — but keep it opt-in per spec, since pixel-diff
// across UI iterations tends to be more noise than signal.

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
  // Go to the agent detail directly; MSW handlers key on /alice.
  await page.goto(`/agents/${AGENT}`)
  // Wait for MSW to be booted (init.ts sets this after worker.start()).
  await page.waitForFunction(
    () => document.documentElement.getAttribute('data-mocks') === 'ready',
  )
  // And for the agent data to land so we're past the loading skeleton.
  await expect(page.getByText(AGENT, { exact: false }).first()).toBeVisible({
    timeout: 10_000,
  })
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

test('overview tab', async ({ page }) => {
  await page.getByRole('tab', { name: /overview/i }).click()
  await shot(page, '01-overview')
})

// Inject a scheduling-failure override into the MSW handler via the
// window.__mockAgentScheduling knob (see src/mocks/handlers.ts). Adding
// the script via addInitScript means it runs before MSW boots on the
// reloaded page, so the very first agent fetch already includes the
// scheduling field.
async function withScheduling(
  page: import('@playwright/test').Page,
  category: 'Capacity' | 'Image',
  lastError: string,
) {
  const firstObservedAt = new Date(Date.now() - 7 * 60_000).toISOString()
  await page.addInitScript(
    ({ category, lastError, firstObservedAt }) => {
      ;(window as unknown as { __mockAgentScheduling?: unknown }).__mockAgentScheduling = {
        category,
        lastError,
        firstObservedAt,
      }
    },
    { category, lastError, firstObservedAt },
  )
  await page.reload()
  await page.waitForFunction(
    () => document.documentElement.getAttribute('data-mocks') === 'ready',
  )
}

test('overview tab — SchedulingFailureBanner (Capacity)', async ({ page }) => {
  await withScheduling(
    page,
    'Capacity',
    "0/1 nodes are available: 1 Insufficient memory. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.",
  )
  await page.getByRole('tab', { name: /overview/i }).click()
  await expect(page.getByTestId('scheduling-failure-banner')).toBeVisible()
  await shot(page, '11-overview-scheduling-capacity')
})

test('overview tab — SchedulingFailureBanner (Image)', async ({ page }) => {
  await withScheduling(
    page,
    'Image',
    'Failed to pull image "ghcr.io/matty-v/kyber-agent-claude-code:bogus-tag": rpc error: code = NotFound desc = manifest unknown',
  )
  await page.getByRole('tab', { name: /overview/i }).click()
  await expect(page.getByTestId('scheduling-failure-banner')).toBeVisible()
  await shot(page, '12-overview-scheduling-image')
})

test('secrets tab', async ({ page }) => {
  await page.getByRole('tab', { name: /secrets/i }).click()
  await shot(page, '02-secrets')
})

test('jobs tab', async ({ page }) => {
  await page.getByRole('tab', { name: /jobs/i }).click()
  await shot(page, '03-jobs')
})

test('activity tab - structured history', async ({ page }) => {
  await page.getByRole('tab', { name: /activity/i }).click()
  // The Activity tab is now just the structured history (sub-tabs removed) —
  // wait for the Recent conversation section to render before capturing.
  await page.getByText(/recent conversation/i).waitFor()
  await shot(page, '04-activity-history')
})

test('shell tab', async ({ page }) => {
  await page.getByRole('tab', { name: /^shell$/i }).click()
  await page.waitForTimeout(800)
  await shot(page, '06-shell')
})

test('shell tab with help popover open', async ({ page }) => {
  await page.getByRole('tab', { name: /^shell$/i }).click()
  // Popover trigger should be an aria-labeled button in the tab header.
  const helpTrigger = page.getByRole('button', { name: /help|aliases/i }).first()
  if (await helpTrigger.isVisible().catch(() => false)) {
    await helpTrigger.click()
    await page.waitForTimeout(200)
  }
  await shot(page, '07-shell-help-open')
})

test('header actions - single-row layout, More dropdown closed', async ({ page }) => {
  await page.getByRole('tab', { name: /overview/i }).click()
  // Single-row header (#262): back arrow + title + status/scheduling/activity badges + ⋯
  // Restart session moved into the dropdown's Lifecycle section.
  await shot(page, '08-header-actions')
})

test('header actions - More dropdown open shows Restart session at top', async ({ page }) => {
  await page.getByRole('tab', { name: /overview/i }).click()
  await page.getByRole('button', { name: /more actions/i }).click()
  await page.waitForTimeout(200)
  await shot(page, '09-header-more-open')
})

test('restart session confirmation dialog', async ({ page }) => {
  await page.getByRole('tab', { name: /overview/i }).click()
  await page.getByRole('button', { name: /more actions/i }).click()
  await page.waitForTimeout(200)
  await page.getByRole('menuitem', { name: /restart session/i }).click()
  await page.waitForTimeout(200)
  await shot(page, '10-restart-session-confirm')
})
