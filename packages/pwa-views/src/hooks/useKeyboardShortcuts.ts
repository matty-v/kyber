import { useEffect, useRef } from 'react'
import { useNavigate } from 'react-router-dom'
import { usePrefixedPath } from '../lib/route-prefix'

// Vim-style global navigation shortcuts (#106 Slice A).
//
// Leader chord (`g` followed by `f|m|a|s` within leaderTimeoutMs) navigates to
// a top-level route. `?` opens a help overlay listing every binding. The hook
// is inactive while focus is in an input/textarea/contenteditable element so
// typing in forms is unaffected.

export interface ShortcutRoutes {
  fleet: string
  machines: string
  agents: string
  settings: string
}

export interface UseKeyboardShortcutsOptions {
  /** Called when `?` is pressed. Layout wires this to a useState toggle. */
  onShowHelp: () => void
  /**
   * When false the hook does not register its keydown listener. Layout passes
   * `enabled={!helpOpen}` so the help overlay's Esc handler is the only one
   * that fires while the overlay is up.
   */
  enabled?: boolean
  /**
   * Override route paths (treated as final — not prefixed again). Partial:
   * any key not supplied falls back to the package-relative default prefixed
   * by the active RoutePrefixContext. Caller-supplied paths are used as-is.
   */
  routes?: Partial<ShortcutRoutes>
  /**
   * Window during which the second key of a chord is accepted. Default 800ms —
   * the issue suggested ~300ms but vim users routinely take longer; 800ms is a
   * comfortable middle ground that still expires fast enough to feel modal.
   *
   * @internal Injected by tests to override the internal navigate call.
   */
  navigate?: (path: string) => void
  /**
   * Window during which the second key of a chord is accepted. Default 800ms.
   */
  leaderTimeoutMs?: number
}

const DEFAULT_LEADER_TIMEOUT_MS = 800

export function useKeyboardShortcuts({
  onShowHelp,
  enabled = true,
  routes,
  navigate: navigateProp,
  leaderTimeoutMs = DEFAULT_LEADER_TIMEOUT_MS,
}: UseKeyboardShortcutsOptions): void {
  const routerNavigate = useNavigate()
  const prefixed = usePrefixedPath()

  // Prefer caller-injected navigate (tests, future override API) over the
  // internal router navigate. This keeps the tests working without a router
  // wrapper while still using useNavigate() in production.
  const navigate = navigateProp ?? routerNavigate

  // Resolve effective routes: caller override wins; otherwise apply prefixed
  // defaults. This preserves the existing override-path API for any caller
  // that already supplies its own table.
  const effective: ShortcutRoutes = {
    fleet: routes?.fleet ?? prefixed('/'),
    machines: routes?.machines ?? prefixed('/machines'),
    agents: routes?.agents ?? prefixed('/agents'),
    settings: routes?.settings ?? prefixed('/settings'),
  }

  const leaderActiveRef = useRef(false)
  const leaderTimerRef = useRef<number | null>(null)

  useEffect(() => {
    if (!enabled) return

    function clearLeader() {
      leaderActiveRef.current = false
      if (leaderTimerRef.current !== null) {
        window.clearTimeout(leaderTimerRef.current)
        leaderTimerRef.current = null
      }
    }

    function handleKeydown(e: KeyboardEvent) {
      if (isEditableTarget(e.target)) return
      // Modifier-bearing shortcuts (cmd/ctrl/alt) belong to the browser or to
      // a future ⌘K palette (#106 Slice B). Leave them alone.
      if (e.metaKey || e.ctrlKey || e.altKey) return

      if (leaderActiveRef.current) {
        const route = chordRoute(e.key, effective)
        clearLeader()
        if (route !== null) {
          e.preventDefault()
          navigate(route)
        }
        return
      }

      if (e.key === 'g') {
        // Don't preventDefault — `g` outside form fields has no native action,
        // and we want the user's eventual non-chord follow-up to behave
        // normally if they abandon the chord.
        leaderActiveRef.current = true
        leaderTimerRef.current = window.setTimeout(clearLeader, leaderTimeoutMs)
        return
      }

      if (e.key === '?') {
        e.preventDefault()
        onShowHelp()
      }
    }

    window.addEventListener('keydown', handleKeydown)
    return () => {
      window.removeEventListener('keydown', handleKeydown)
      clearLeader()
    }
  }, [navigate, onShowHelp, enabled, effective, leaderTimeoutMs])
}

function chordRoute(key: string, routes: ShortcutRoutes): string | null {
  switch (key) {
    case 'f':
      return routes.fleet
    case 'm':
      return routes.machines
    case 'a':
      return routes.agents
    case 's':
      return routes.settings
    default:
      return null
  }
}

function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  const tag = target.tagName
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true
  // isContentEditable is the canonical API in real browsers (it resolves
  // parent-chain inheritance correctly). JSDOM returns undefined for it and
  // doesn't reflect the contentEditable IDL property to the attribute, so
  // we layer two defensive fallbacks — the property and the attribute —
  // which let JSDOM-driven tests be deterministic without changing real-
  // browser behavior.
  if (target.isContentEditable) return true
  if (target.contentEditable === 'true' || target.contentEditable === 'plaintext-only') return true
  const ce = target.getAttribute('contenteditable')
  return ce === '' || ce === 'true' || ce === 'plaintext-only'
}

// Exported for the help-overlay component so the displayed shortcuts and the
// hook's bindings can't drift apart.
export const SHORTCUT_BINDINGS: ReadonlyArray<{ keys: string; description: string }> = [
  { keys: 'g f', description: 'Go to Fleet' },
  { keys: 'g m', description: 'Go to Machines' },
  { keys: 'g a', description: 'Go to Agents' },
  { keys: 'g s', description: 'Go to Settings' },
  { keys: '⌘K / Ctrl+K', description: 'Open command palette' },
  { keys: '?', description: 'Show this help' },
  { keys: 'Esc', description: 'Close overlay' },
]
