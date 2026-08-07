// Playwright config — purpose-built for screenshot generation on UI PRs
// (see feedback_ui_pr_screenshots.md memory). Runs against a vite dev
// server with VITE_ENABLE_MOCKS=1 so every page renders against MSW
// fixtures rather than the real control-plane.

import { defineConfig, devices } from '@playwright/test'

const port = 5174 // distinct from the default dev port so both can coexist

export default defineConfig({
  testDir: './tests/screenshots',
  fullyParallel: false, // screenshots overwrite the same output files
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: [['list']],

  use: {
    baseURL: `http://localhost:${port}`,
    trace: 'off',
    video: 'off',
    screenshot: 'off', // tests call page.screenshot() explicitly
    // Wait for MSW's readiness marker before interacting — init.ts sets
    // <html data-mocks="ready"> once the worker is listening.
    navigationTimeout: 15_000,
    actionTimeout: 5_000,
  },

  projects: [
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
    {
      // Use Chrome-based mobile emulation instead of iOS WebKit to avoid a
      // second browser download. Viewport / DPR / UA match iPhone 14 closely
      // enough for UI-layout review.
      name: 'mobile',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 390, height: 844 },
        deviceScaleFactor: 3,
        isMobile: true,
        hasTouch: true,
      },
    },
  ],

  webServer: {
    command: `VITE_ENABLE_MOCKS=1 npx vite --port ${port} --strictPort`,
    url: `http://localhost:${port}`,
    timeout: 60_000,
    reuseExistingServer: !process.env.CI,
    stdout: 'pipe',
    stderr: 'pipe',
  },
})
