import { describe, it, expect } from 'vitest'
import { render } from '@testing-library/react'
import { BrowserRouter, useLocation } from 'react-router-dom'
import { createElement } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'

// Regression guard for the react-router dual-copy bug.
//
// WHAT BROKE: pwa-views declared react-router-dom ^7.16.0 as a devDependency
// while apps/embedded-pwa depends on ^6.28.0. npm cannot satisfy both from a
// single hoisted copy, so it nested a second one under
// packages/pwa-views/node_modules. main.tsx's <BrowserRouter> then resolved to
// v6 while pwa-views' App.tsx useLocation() resolved to v7 — two distinct
// React context objects, so the hook could not see the provider and the app
// died at first render with:
//
//   useLocation() may be used only in the context of a <Router> component.
//
// The entire PWA rendered as a blank page. Introduced by #414, which widened
// the peer range to v7 (correct) but also moved the devDependency to v7 (not).
//
// WHY EXISTING TESTS MISSED IT: every test inside packages/pwa-views imports
// MemoryRouter from pwa-views' OWN react-router-dom, so the provider and the
// hook are always the same copy there and the cross-package boundary is never
// exercised. `tsc --noEmit` cannot see it either, because the peer range
// legitimately permits both majors. Only a consumer crossing the boundary
// reproduces it — which is why this guard lives in the app, not the library.

const ROUTER_CONTEXT_ERROR = /may be used only in the context of a <Router>/

function repoRoot(): string {
  // vitest runs with cwd = apps/embedded-pwa
  let dir = process.cwd()
  for (let i = 0; i < 5; i++) {
    if (existsSync(join(dir, 'package-lock.json'))) return dir
    dir = resolve(dir, '..')
  }
  throw new Error('could not locate repo root')
}

function findRouterCopies(root: string): string[] {
  const found: string[] = []
  const candidates = [
    join(root, 'node_modules'),
    ...['packages', 'apps'].flatMap((group) => {
      const groupDir = join(root, group)
      if (!existsSync(groupDir)) return []
      return readdirSync(groupDir).map((ws) => join(groupDir, ws, 'node_modules'))
    }),
  ]
  for (const nm of candidates) {
    const pkg = join(nm, 'react-router-dom', 'package.json')
    if (existsSync(pkg)) {
      const { version } = JSON.parse(readFileSync(pkg, 'utf8')) as { version: string }
      found.push(`${version} @ ${pkg.replace(root + '/', '')}`)
    }
  }
  return found
}

describe('react-router package boundary', () => {
  it('installs exactly one react-router-dom copy across the workspace', () => {
    // THIS is the load-bearing guard — verified to fail when a second copy is
    // reinstalled, and to pass once deduped.
    //
    // It asserts the invariant structurally, on the installed tree, precisely
    // because the runtime render check below CANNOT catch this bug: Vite's
    // test-time resolution collapses the two copies, so the app renders fine
    // under vitest even while the production `vite build` bundle is broken.
    // That asymmetry is the whole reason this bug reached a live cluster.
    //
    // If this fails, run `npm dedupe` and align the offending version range.
    // Note that plain `npm install` does NOT prune an already-nested copy —
    // dedupe (or deleting the nested dir) is required.
    const copies = findRouterCopies(repoRoot())
    expect(copies, `react-router-dom copies found:\n  ${copies.join('\n  ')}`).toHaveLength(1)
  })

  it('a provider satisfies useLocation() imported alongside it', () => {
    function ShowPath() {
      const location = useLocation()
      return createElement('span', { 'data-testid': 'path' }, location.pathname)
    }
    const { getByTestId } = render(
      createElement(BrowserRouter, null, createElement(ShowPath)),
    )
    expect(getByTestId('path').textContent).toBe('/')
  })

  it('App from pwa-views sees the app-side Router context', async () => {
    // The real failing path: App is compiled from the pwa-views workspace and
    // calls useLocation() internally, while BrowserRouter is imported here the
    // way main.tsx imports it.
    //
    // Supply App's required providers so this remains a real cross-workspace
    // render. Large dependency graphs can make the dynamic workspace import
    // exceed Vitest's default timeout even though the render is healthy.
    //
    // CAVEAT — measured, not assumed: this assertion still PASSES with two
    // copies installed, because Vite dedupes them at test time. It documents
    // the intended contract and would catch a genuine "App renders outside a
    // Router" mistake, but it is NOT what protects against the dual-copy
    // regression. The install-tree check above is.
    const { App, ClusterProvider, TooltipProvider } = await import('@matty-v/kyber-pwa-views')
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const cluster = {
      id: 'local',
      name: 'Kyber',
      baseURL: 'http://localhost/',
      apiKey: '',
      version: 'test',
      capabilities: [],
    }
    let message = ''
    try {
      render(
        createElement(
          BrowserRouter,
          null,
          createElement(
            QueryClientProvider,
            { client: queryClient },
            createElement(
              TooltipProvider,
              null,
              createElement(ClusterProvider, { value: cluster }, createElement(App)),
            ),
          ),
        ),
      )
    } catch (err) {
      message = err instanceof Error ? err.message : String(err)
    }
    expect(message).not.toMatch(ROUTER_CONTEXT_ERROR)
  }, 15_000)
})
