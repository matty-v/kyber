import { useEffect } from 'react'

// ⌘K / Ctrl+K toggle for the global command palette (#106 Slice B).
//
// Lives separate from useKeyboardShortcuts because that hook deliberately
// ignores modifier-bearing keys ("Leave them alone — meta/ctrl belong to the
// browser or to a future ⌘K palette"). This is that future. The two hooks
// listen on the same window keydown but don't overlap: one handles bare keys
// and chords, this one handles the palette accelerator.
//
// Unlike the nav-shortcuts hook, this listener does NOT focus-guard. The
// palette accelerator is meant to fire from anywhere — including from inside
// search inputs and the palette's own input (where ⌘K toggles it closed).

export interface UseCommandPaletteShortcutOptions {
  /** Toggle handler. Called with no args; caller flips its own state. */
  onToggle: () => void
  /**
   * When false the hook does not register its keydown listener. Layout passes
   * `enabled={!helpOpen}` so the help overlay can't be stacked on top of an
   * accidentally-opened palette.
   */
  enabled?: boolean
}

export function useCommandPaletteShortcut({
  onToggle,
  enabled = true,
}: UseCommandPaletteShortcutOptions): void {
  useEffect(() => {
    if (!enabled) return
    function handleKeydown(e: KeyboardEvent) {
      // Accept ⌘K (mac) and Ctrl+K (everything else). Reject if shift/alt
      // are also held — Ctrl+Shift+K is devtools in Firefox; ⌥⌘K is a Mac
      // accessibility shortcut. Conservative match keeps us out of those.
      if (e.key.toLowerCase() !== 'k') return
      if (e.shiftKey || e.altKey) return
      if (!(e.metaKey || e.ctrlKey)) return
      e.preventDefault()
      onToggle()
    }
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  }, [onToggle, enabled])
}
