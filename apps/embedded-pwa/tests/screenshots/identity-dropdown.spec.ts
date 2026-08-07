// Screenshot capture for the Create Agent wizard's IdentitySection (#134):
// the new dropdown of App-installed repos in 'existing' mode, and the live
// availability badge on 'template' mode that polls
// /api/v1/github/repos/{owner}/{name}/exists. The MSW mock returns true for
// any repo present in mockGitHubRepos, so naming the agent 'alice' (which
// produces target 'alice-agent', present as 'matty-v/alice-agent' in the
// fixtures) demonstrates the taken state, while a fresh name ('newbie')
// shows the available state.

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
  await expect(page.getByLabel(/name/i)).toBeVisible({ timeout: 10_000 })
})

async function shot(page: import('@playwright/test').Page, name: string) {
  const project = test.info().project.name
  const file = path.join(OUT_DIR, `${project}-identity-${name}.png`)
  await page.screenshot({ path: file, fullPage: true, animations: 'disabled' })
}

// Walk through Basics + Resources to land on Step 3 (Identity) with the
// supplied agent name. Pulled out so each scenario reads cleanly.
async function gotoIdentityStep(
  page: import('@playwright/test').Page,
  agentName: string,
) {
  await page.getByLabel(/name/i).fill(agentName)
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
  await expect(page.getByLabel(/identity repo/i)).toBeVisible()
}

test('template mode — available badge for a fresh name', async ({ page }) => {
  await gotoIdentityStep(page, 'newbie')
  await expect(page.getByTestId('identity-collision-available')).toBeVisible()
  await shot(page, '01-template-available')
})

test('template mode — taken badge when name collides', async ({ page }) => {
  // 'alice' produces 'alice-agent' which exists in mockGitHubRepos.repos.
  await gotoIdentityStep(page, 'alice')
  await expect(page.getByTestId('identity-collision-taken')).toBeVisible()
  await shot(page, '02-template-taken')
})

test('existing mode — dropdown populated from App repos', async ({ page }) => {
  await gotoIdentityStep(page, 'newbie')
  await page.getByLabel(/identity repo/i).selectOption('existing')
  await expect(page.getByLabel(/^repository$/i)).toBeVisible()
  await shot(page, '03-existing-dropdown')
})

test('existing mode — Other selected, free-text fallback', async ({ page }) => {
  await gotoIdentityStep(page, 'newbie')
  await page.getByLabel(/identity repo/i).selectOption('existing')
  await page.getByLabel(/^repository$/i).selectOption('__freeform__')
  await expect(page.getByPlaceholder(/owner\/repo/i)).toBeVisible()
  await page.getByPlaceholder(/owner\/repo/i).fill('matty-v/some-other-repo')
  await shot(page, '04-existing-freeform')
})
