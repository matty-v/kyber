// Worked example for the devenv browser harness (kyber#402, Phase 2 of #399).
//
// Navigates to the root of the brought-up PWA and asserts a REAL RENDERED
// element that only the live React SPA produces — the persistent app-shell
// chrome ("Kyber" brand + "Fleet Command Console" subtitle, rendered by
// packages/pwa-views Layout into #root). The committed placeholder index.html
// ships an EMPTY <div id="root"></div>, so this assertion fails loudly if the
// brought-up instance serves the placeholder (or a stale, pre-multi-stage
// image) instead of the real PWA — which is exactly the gating guard the design
// assigned to this spec for the `up.sh --skip-build` warm path.
//
// This doubles as the usage example: read-only observation via the helper, plus
// a read-write assertion. Run against a live bring-up:
//   scripts/devenv/up.sh
//   cd scripts/devenv/browser && npm run install-browser && npm test

import { test, expect } from '@playwright/test'
import { goto, readText, expectText, expectVisible } from '../helper'

test.describe('devenv PWA — real instance smoke', () => {
  test('root renders the real PWA app shell, not the placeholder', async ({ page }) => {
    // READ-ONLY: navigate to the brought-up PWA root.
    await goto(page, '/')

    // The SPA must have mounted something into #root — the placeholder leaves
    // it empty, so a non-empty root is the first signal the real bundle ran.
    await expect(page.locator('#root')).not.toBeEmpty()

    // READ-WRITE (assertion): the persistent desktop sidebar chrome. Rendered
    // by Layout regardless of route and independent of any API data, so it is a
    // stable real-surface marker the placeholder cannot contain.
    await expectText(page, 'Fleet Command Console')
    await expectVisible(page, 'nav')

    // READ-ONLY: the index route is FleetOverview, whose <h1> is real rendered
    // page content (again absent from the placeholder).
    const heading = await readText(page, 'h1')
    expect(heading.length).toBeGreaterThan(0)
  })
})
