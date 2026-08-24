import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    globals: true,
    // Cap the worker pool. Vitest defaults to roughly one fork per logical CPU,
    // but this suite's cost is jsdom memory, not CPU: every worker builds its
    // own DOM plus the full React module graph, so on a many-core box the pool
    // oversubscribes RAM and every worker slows down together. On a 20-core /
    // 11 GB dev box the default spawned ~20 workers and stretched the heaviest
    // AddWebhookWizard flows from ~340ms to over vitest's 5s testTimeout,
    // failing 1–4 tests per run at random — a machine-load artifact, not a
    // behavioral bug (they always passed when the file ran alone).
    //
    // Measured over the full 71-file suite on that box:
    //   default (~20 workers)  49s, 1–4 random failures
    //   8 workers              44s, green
    //   6 workers              41s, green
    //   4 workers              37s, green
    //
    // jsdom 30 increases each worker's DOM cost enough that four concurrent
    // workers again push interaction tests past 5s on the dev runner. The
    // previously failing files pass together with two workers, so retain the
    // real timeout and reduce contention instead of masking it.
    maxWorkers: 2,
  },
})
