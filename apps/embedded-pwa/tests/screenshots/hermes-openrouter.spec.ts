import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const output = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', 'screenshots')

test.beforeAll(() => fs.mkdirSync(output, { recursive: true }))

async function shot(page: import('@playwright/test').Page, name: string, fullPage = true) {
  await page.screenshot({
    path: path.join(output, `${test.info().project.name}-hermes-${name}.png`),
    fullPage,
    animations: 'disabled',
  })
}

test('Hermes creation shows the direct OpenRouter route and billing boundary', async ({ page }) => {
  await page.goto('/agents/new')
  await page.waitForFunction(() => document.documentElement.getAttribute('data-mocks') === 'ready')
  await page.getByLabel(/name/i).fill('hermes-demo')
  await page.getByLabel(/machine/i).selectOption('demo')
  await page.getByRole('button', { name: /next/i }).click()
  await page.getByLabel(/runtime/i).selectOption('hermes')
  await page.getByRole('button', { name: /next/i }).click()
  await page.getByRole('button', { name: /next/i }).click()
  await expect(page.getByText('Model source: OpenRouter (direct)')).toBeVisible()
  await shot(page, 'create-auth')

  await page.getByLabel(/^OpenRouter API key$/i).fill('example-key-for-screenshot-only')
  await page.getByRole('button', { name: /next/i }).click()
  await expect(page.getByText('OpenRouter (direct)')).toBeVisible()
  await shot(page, 'create-review')
})

test('Hermes agent settings identify the model source and estimated billing', async ({ page }) => {
  await page.addInitScript(() => {
    ;(window as unknown as { __mockAgentRuntime?: string }).__mockAgentRuntime = 'hermes'
  })
  await page.goto('/agents/alice/general')
  await page.waitForFunction(() => document.documentElement.getAttribute('data-mocks') === 'ready')
  await expect(page.getByText('OpenRouter (direct)')).toBeVisible()
  await shot(page, 'agent-settings')
  await page.getByText('OpenRouter (direct)').scrollIntoViewIfNeeded()
  await shot(page, 'agent-settings-focus', false)
})
