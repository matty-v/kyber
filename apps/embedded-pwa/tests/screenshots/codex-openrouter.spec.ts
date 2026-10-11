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
  await expect(page.getByText(/task receipt fail/)).toBeVisible()
  await page.getByLabel('Endpoint API key').fill('example-key-for-screenshot-only')
  await page.screenshot({ path: path.join(output, `${test.info().project.name}-codex-openrouter-create-auth.png`), fullPage: true })
  await page.getByRole('button', { name: /next/i }).click()
  await expect(page.getByText('OpenRouter (direct)')).toBeVisible()
  await expect(page.getByText(/task cancel fail/)).toBeVisible()
  await page.screenshot({ path: path.join(output, `${test.info().project.name}-codex-openrouter-create-review.png`), fullPage: true })
})

test('Codex OpenRouter agent detail shows evidence and model change stays concise', async ({ page }) => {
  await page.addInitScript(() => {
    ;(window as unknown as { __mockAgentRuntime?: string }).__mockAgentRuntime = 'codex-openrouter'
  })
  await page.goto('/agents/alice/general')
  await page.waitForFunction(() => document.documentElement.getAttribute('data-mocks') === 'ready')
  await expect(page.getByText(/task receipt fail/)).toBeVisible()
  await page.screenshot({ path: path.join(output, `${test.info().project.name}-codex-openrouter-agent-detail.png`), fullPage: true })
  await page.getByRole('button', { name: 'Model', exact: true }).click()
  await expect(page.getByLabel('Endpoint model ID')).toHaveValue('cohere/north-mini-code:free')
  const modelDialog = page.getByRole('heading', { name: 'Change model' }).locator('..')
  await expect(modelDialog.getByText(/task receipt|task cancel|dev pilot/i)).toHaveCount(0)
  await page.screenshot({ path: path.join(output, `${test.info().project.name}-codex-openrouter-model-change.png`), fullPage: true })
})
