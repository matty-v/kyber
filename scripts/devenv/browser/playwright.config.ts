// Playwright config for the devenv browser harness (kyber#402, Phase 2 of #399).
//
// Unlike apps/embedded-pwa/playwright.config.ts — which spawns a vite dev server
// with VITE_ENABLE_MOCKS=1 and drives the MSW-mocked frontend on :5174 — this
// project targets the *already-running* brought-up instance: the real
// control-plane (mock compute provider) serving the embedded PWA at the root
// path over the Phase-1 port-forward. There is therefore NO `webServer` block;
// the bring-up is owned by scripts/devenv/up.sh, not by Playwright.
//
// baseURL resolution (matches the Phase-1 contract, scripts/devenv/lib.sh):
//   - DEVENV_PWA_URL — explicit full URL override, for a non-localhost bring-up
//     (e.g. a shared remote devenv). Takes precedence when set.
//   - otherwise http://localhost:${DEVENV_API_PORT ?? 18080}/ — the control-plane
//     Service :8080 port-forwarded to localhost (honors up.sh --api-port, which
//     exports DEVENV_API_PORT via lib.sh).

import { defineConfig, devices } from '@playwright/test'

const port = process.env.DEVENV_API_PORT ?? '18080'
export const baseURL = process.env.DEVENV_PWA_URL ?? `http://localhost:${port}/`

export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: [['list']],

  // Preflight: fail fast (with an actionable message) when the bring-up is not
  // up or the browser binary is missing, rather than emitting an opaque trace.
  globalSetup: './global-setup.ts',

  use: {
    baseURL,
    trace: 'off',
    video: 'off',
    screenshot: 'off', // the helper's shot() calls page.screenshot() explicitly
    navigationTimeout: 15_000,
    actionTimeout: 5_000,
  },

  projects: [
    {
      // Desktop Chrome at 1440x900 — the lg: breakpoint, so the PWA renders its
      // full desktop sidebar (the worked example asserts shell chrome there).
      name: 'desktop',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
  ],
})
