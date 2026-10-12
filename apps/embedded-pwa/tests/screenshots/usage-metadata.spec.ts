import { expect, test } from '@playwright/test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const outDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', 'screenshots')

test('Metrics — observed token usage', async ({ page }) => {
  await page.goto('/settings/metrics')
  await page.waitForFunction(() => document.documentElement.getAttribute('data-mocks') === 'ready')
  const panel = page.getByText('Token usage', { exact: true }).locator('..')
  const data = test.info().project.name === 'mobile' ? panel.locator('dl') : panel.locator('table')
  await expect(data.getByText('2,000')).toBeVisible()
  await expect(test.info().project.name === 'mobile' ? panel.getByText('Unknown').first() : data.getByText('Unknown')).toBeVisible()
  await expect(panel.getByText(/cost|unpriced/i)).toHaveCount(0)
  await (test.info().project.name === 'mobile' ? data.locator('..') : panel).screenshot({
    path: path.join(outDir, `${test.info().project.name}-mat117-usage.png`), animations: 'disabled',
  })
})
