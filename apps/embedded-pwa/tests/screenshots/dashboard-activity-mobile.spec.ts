import { expect, test } from '@playwright/test'
import fs from 'node:fs'
import path from 'node:path'
import { mockAgent } from '../../../../packages/pwa-views/src/mocks/fixtures'

const out = process.env.KYBER_SCREENSHOT_DIR ?? path.resolve('screenshots')
const summary = 'Continue researching and saving all species artifacts, including references at https://example.test/' + 'long-reference-'.repeat(30)

test('populated dashboard keeps activity rows and group controls inside the viewport', async ({ page }, info) => {
  fs.mkdirSync(out, { recursive: true })
  await page.addInitScript(({ agent, summary }) => {
    const original = window.fetch.bind(window)
    window.fetch = async (input, init) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.origin)
      if (url.pathname === '/api/v1/agents') {
        const now = new Date().toISOString()
        const items = ['hk-47', 'mando-' + 'a'.repeat(57)].map(id => ({ ...agent, id,
          tokenUsage: { ...agent.tokenUsage, model: 'provider/' + 'long-model-name-'.repeat(15) },
          goal: { summary, source: 'agent', acceptedAt: now },
          activity: { lastHeartbeatAt: now, work: { state: 'working', serverTime: now,
            observedAt: now, expiresAt: new Date(Date.now() + 60000).toISOString(),
            lastWorkedAt: now, foregroundCount: 1, backgroundCount: 0 } },
        }))
        return new Response(JSON.stringify({ items }), { headers: { 'Content-Type': 'application/json' } })
      }
      return original(input, init)
    }
  }, { agent: mockAgent, summary })
  test.setTimeout(90_000)
  await page.goto('/', { timeout: 60_000 })
  await expect(page.getByRole('heading', { name: 'Agent activity', exact: true })).toBeVisible({ timeout: 20_000 })
  await expect(page.locator('[data-activity-agent]')).toHaveCount(2)
  for (const width of info.project.name === 'mobile' ? [320, 390, 430] : [1024, 1440]) {
    await page.setViewportSize({ width, height: 900 })
    await page.screenshot({ path: path.join(out, `${info.project.name}-dashboard-activity-${width}.png`), fullPage: true })
    const bounds = await page.locator('[data-group-button], [data-activity-agent], [data-testid="agent-goal"]').evaluateAll(nodes => nodes.map(node => {
      const r = node.getBoundingClientRect()
      return { left: r.left, right: r.right }
    }))
    for (const r of bounds) {
      expect(r.left).toBeGreaterThanOrEqual(0)
      expect(r.right).toBeLessThanOrEqual(width)
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
    const working = page.locator('[data-group-button="working"]')
    await working.click()
    await expect(working).toHaveAttribute('aria-expanded', 'false')
    await working.click()
    await expect(working).toHaveAttribute('aria-expanded', 'true')
  }
})
