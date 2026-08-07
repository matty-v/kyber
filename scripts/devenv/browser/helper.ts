// Browser-driving helper for the brought-up devenv PWA (kyber#402, Phase 2 of
// #399). A thin, documented seam over a Playwright `page`, split into two
// usage classes so each consumer knows what it is meant to call:
//
//   READ-ONLY  (product / Yoda) — observe the live PWA without mutating it:
//              navigate, read rendered text, screenshot.
//   READ-WRITE (builders / Boba Fett QA) — exercise UI flows: click, type,
//              fill, submit, and assert.
//
// HONEST BOUNDARY (see the design's Security Considerations): the read-only /
// read-write split is an *ergonomic + documented convention*, NOT an enforced
// capability sandbox. A Playwright `page` can always perform any action, so
// "read-only" is a discipline for the product consumer, not a security control.
// The real capability boundary (running the browser from an agent pod) and its
// threat model are Phase 3 / #403, out of scope here.

import { expect, type Page } from '@playwright/test'

// ─── READ-ONLY (product / Yoda) ──────────────────────────────────────────────

/**
 * READ-ONLY. Navigate to a path on the brought-up PWA (relative to the config
 * baseURL, e.g. '/' or 'agents'), waiting until the network settles so the SPA
 * has mounted and rendered. Returns the Playwright Response, if any.
 */
export async function goto(page: Page, path = '/') {
  return page.goto(path, { waitUntil: 'networkidle' })
}

/**
 * READ-ONLY. Read the trimmed visible text of the first element matching a CSS
 * selector. Throws if nothing matches within the action timeout.
 */
export async function readText(page: Page, selector: string): Promise<string> {
  const text = await page.locator(selector).first().innerText()
  return text.trim()
}

/**
 * READ-ONLY. Capture a full-page screenshot to an absolute/relative file path.
 * Animations are disabled for a stable image. For product observation and
 * artifact capture — it does not assert anything.
 */
export async function shot(page: Page, filePath: string): Promise<void> {
  await page.screenshot({ path: filePath, fullPage: true, animations: 'disabled' })
}

// ─── READ-WRITE (builders / Boba Fett QA) ────────────────────────────────────

/**
 * READ-WRITE. Click the first element matching a selector (or a Playwright
 * accessible-name role via getByRole upstream). Mutates UI state.
 */
export async function click(page: Page, selector: string): Promise<void> {
  await page.locator(selector).first().click()
}

/**
 * READ-WRITE. Fill a form field (input/textarea) matched by selector with the
 * given value, replacing any existing content.
 */
export async function fill(page: Page, selector: string, value: string): Promise<void> {
  await page.locator(selector).first().fill(value)
}

/**
 * READ-WRITE. Submit a form by clicking its submit control. Pass the selector
 * for the submit button (defaults to a button of type=submit).
 */
export async function submit(page: Page, selector = 'button[type="submit"]'): Promise<void> {
  await page.locator(selector).first().click()
}

/**
 * READ-WRITE (assertion). Assert an element matching the selector is visible.
 * Useful for builders/QA verifying a flow reached the expected surface.
 */
export async function expectVisible(page: Page, selector: string): Promise<void> {
  await expect(page.locator(selector).first()).toBeVisible()
}

/**
 * READ-WRITE (assertion). Assert that the given text is visible somewhere on the
 * page (substring match). Waits up to the action timeout for it to appear.
 */
export async function expectText(page: Page, text: string): Promise<void> {
  await expect(page.getByText(text, { exact: false }).first()).toBeVisible()
}
