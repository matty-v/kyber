// Conditionally start MSW. Activated only when VITE_ENABLE_MOCKS=1 at dev /
// build time — production builds can't accidentally ship the worker because
// import.meta.env.VITE_ENABLE_MOCKS is replaced at compile time and the
// tree-shaker drops this whole module when it resolves to "0".
//
// The caller (src/main.tsx) awaits enableMocks() before mounting React so
// the first GET the app makes on mount lands on the worker, not the network.

export async function enableMocks(): Promise<void> {
  if (import.meta.env.VITE_ENABLE_MOCKS !== '1') return
  const { worker } = await import('./browser')
  await worker.start({
    // Quiet — MSW prints every intercepted request by default, which buries
    // real React-Query or app logs in screenshots + e2e runs.
    quiet: true,
    // Pass unmatched requests through so MSW doesn't break images / fonts /
    // Vite HMR. Matters for dev; unused under Playwright's headless runner.
    onUnhandledRequest: 'bypass',
  })

  // Flag in the DOM so Playwright can wait on it before navigating. Using a
  // data-attribute on <html> keeps the signal out of React state.
  document.documentElement.setAttribute('data-mocks', 'ready')
}
