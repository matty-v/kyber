import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const output = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', 'screenshots')

test.beforeAll(() => fs.mkdirSync(output, { recursive: true }))

test('Codex OpenRouter route is clear on desktop and mobile', async ({ page }) => {
  await page.goto('/agents/new')
  await page.waitForFunction(() => document.documentElement.getAttribute('data-mocks') === 'ready')
  await page.getByLabel(/name/i).fill('codex-openrouter-demo')
  await page.getByLabel(/machine/i).selectOption('demo')
  await page.getByRole('button', { name: /next/i }).click()
  await page.getByLabel(/runtime/i).selectOption('codex')
  await page.getByRole('button', { name: /next/i }).click()
  await page.getByRole('button', { name: /next/i }).click()
  await page.getByLabel('Model source').selectOption('openrouter')
  await expect(page.getByText('OpenRouter direct route')).toBeVisible()
  await expect(page.getByLabel('Endpoint URL')).toHaveCount(0)
  await page.getByLabel('Model', { exact: true }).fill('cohere/north-mini-code:free')
  await page.getByLabel('Endpoint API key').fill('example-key-for-screenshot-only')
  await page.screenshot({ path: path.join(output, `${test.info().project.name}-codex-openrouter-create-auth.png`), fullPage: true })
  await page.getByRole('button', { name: /next/i }).click()
  await expect(page.getByText('OpenRouter (direct)')).toBeVisible()
  await page.screenshot({ path: path.join(output, `${test.info().project.name}-codex-openrouter-create-review.png`), fullPage: true })
})
