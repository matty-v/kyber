// Preflight for the devenv browser harness (kyber#402). Runs once before any
// spec. Turns the two predictable "nothing rendered, opaque stack trace"
// failure modes into clear, actionable messages:
//
//   1. Playwright's Chromium binary is not installed (the one network
//      dependency — see scripts/devenv/README.md). → point at the fetch step.
//   2. The devenv instance is not up (no listener on the port-forward). → point
//      at scripts/devenv/up.sh.
//
// Both are operator/environment problems, not test failures, so we detect them
// up front rather than letting a spec die mid-navigation.

import { chromium } from '@playwright/test'
import fs from 'node:fs'
import { baseURL } from './playwright.config'

function fail(message: string): never {
  // A bare, framed message — globalSetup throwing aborts the run before specs,
  // and Playwright prints the thrown Error message without a spec stack trace.
  throw new Error(`\n\ndevenv browser preflight failed:\n  ${message}\n`)
}

async function checkBrowserBinary(): Promise<void> {
  // executablePath() is the path Playwright expects Chromium at; if it is empty
  // or absent, the binary was never fetched. Cheaper than a trial launch.
  const exe = chromium.executablePath()
  if (!exe || !fs.existsSync(exe)) {
    fail(
      'Chromium is not installed for Playwright.\n' +
        '  Fetch it (requires network — the one network dependency):\n' +
        '      cd scripts/devenv/browser && npm run install-browser\n' +
        '  (equivalently: npx playwright install chromium)',
    )
  }
}

async function checkInstanceUp(): Promise<void> {
  const healthURL = new URL('healthz', baseURL).toString()
  try {
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), 4_000)
    const res = await fetch(healthURL, { signal: controller.signal })
    clearTimeout(timer)
    if (!res.ok) {
      fail(
        `devenv health check at ${healthURL} returned HTTP ${res.status}.\n` +
          '  The control-plane is reachable but not healthy — check scripts/devenv/up.sh output.',
      )
    }
  } catch {
    fail(
      `no healthy devenv instance at ${healthURL}.\n` +
        '  Bring one up first:\n' +
        '      scripts/devenv/up.sh\n' +
        '  (override the target with DEVENV_API_PORT or DEVENV_PWA_URL).',
    )
  }
}

export default async function globalSetup(): Promise<void> {
  await checkBrowserBinary()
  await checkInstanceUp()
}
