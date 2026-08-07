// Vitest config — extends the vite config but excludes the Playwright
// screenshot spec so `npm test` (vitest) doesn't try to run it. Playwright
// has its own runner entry via `npm run screenshots`.

import { defineConfig, configDefaults } from 'vitest/config'
import viteConfig from './vite.config'

export default defineConfig({
  ...viteConfig,
  test: {
    // Default include is **/*.{test,spec}.{ts,tsx,js,jsx}; Playwright's
    // agent-detail.spec.ts would match without this exclude.
    exclude: [...configDefaults.exclude, 'tests/**', 'screenshots/**'],
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    globals: true,
  },
})
