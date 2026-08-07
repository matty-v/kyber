import { createContext, useCallback, useContext, type PropsWithChildren } from 'react'

export interface BackTo {
  /** Absolute URL path to navigate to when the user clicks the back affordance. */
  href: string
  /** Visible label, e.g. "Clusters". */
  label: string
}

interface RoutePrefixContextValue {
  prefix: string
  backTo?: BackTo
}

/**
 * RoutePrefix is the URL segment that should prepend every internal navigation
 * target the views generate. Empty string (the default) means the views are
 * mounted at the URL root — current behavior of the embedded PWA, preserved.
 *
 * Holocron (Phase C) provides `/c/<cluster-id>` so navigation stays scoped to
 * the active cluster's URL space when the user clicks a nav link or the
 * package internally calls `navigate('/agents/...')`. Holocron also passes
 * a `backTo` so the Layout can render an affordance back out to the hub —
 * embedded mode (standalone PWA) leaves it undefined and no affordance shows.
 *
 * Separate from ClusterContext on purpose: routing concerns and cluster-domain
 * concerns are different.
 */
const RoutePrefixContext = createContext<RoutePrefixContextValue>({ prefix: '' })

export function RoutePrefixProvider({
  prefix,
  backTo,
  children,
}: PropsWithChildren<{ prefix: string; backTo?: BackTo }>) {
  return (
    <RoutePrefixContext.Provider value={{ prefix, backTo }}>
      {children}
    </RoutePrefixContext.Provider>
  )
}

/** Read the active route prefix string. Defaults to "" when no provider is mounted. */
export function useRoutePrefix(): string {
  return useContext(RoutePrefixContext).prefix
}

/**
 * Read the optional "back" affordance configured by the host.
 * Returns undefined in embedded mode (standalone PWA) — Layout renders nothing.
 */
export function useBackTo(): BackTo | undefined {
  return useContext(RoutePrefixContext).backTo
}

/**
 * Returns a function that prepends the active route prefix to a given path.
 * Empty prefix → no-op. Idempotent on already-prefixed paths. Segment-aware
 * (so "/c/abc" prefix doesn't accidentally match "/c/abc-other"). Callers
 * are expected to pass package-relative paths only — passing a fully-qualified
 * path for a *different* cluster's prefix would produce a double-prefixed
 * nonsense path (no-one should be doing that).
 *
 * Use this instead of writing literal "/foo" in NavLink / navigate / etc.
 */
export function usePrefixedPath(): (path: string) => string {
  const prefix = useRoutePrefix()
  return useCallback(
    (path: string) => {
      if (!prefix) return path
      // Already prefixed — segment-aware check (not raw startsWith).
      if (
        path === prefix ||
        path.startsWith(prefix + '/') ||
        path.startsWith(prefix + '?') ||
        path.startsWith(prefix + '#')
      ) {
        return path
      }
      // Normalize: strip any leading "/" on the input, then join with prefix + "/".
      const stripped = path.startsWith('/') ? path.slice(1) : path
      return prefix + '/' + stripped
    },
    [prefix],
  )
}
