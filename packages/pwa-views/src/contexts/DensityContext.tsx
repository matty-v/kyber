// Density preference (#105). Operators on large displays want a tighter
// layout that fits more rows on screen. Operators on laptops keep the
// existing comfortable spacing. Density is a separate axis from theme and
// reduced-motion — it doesn't affect font size or accessibility, only
// padding and row heights.
//
// Implementation: a React context backed by localStorage. The provider
// writes a `data-density` attribute on <body>; CSS in index.css reads that
// attribute and overrides padding on the affected components (Card,
// DataTable cells, Sidebar nav items, StatusBadge).
//
// On viewports ≤ 768px (mobile), compact mode is silently ignored — the
// padding cuts hurt touch targets there. The user's preference is preserved
// in localStorage so resizing back to desktop restores compact.

import { createContext, useCallback, useContext, useEffect, useState } from 'react'
import type { ReactNode } from 'react'

export type Density = 'comfortable' | 'compact'

const STORAGE_KEY = 'kyber:density'
const DEFAULT_DENSITY: Density = 'comfortable'
const MOBILE_BREAKPOINT_PX = 768

interface DensityContextValue {
  /** The user's stored preference. */
  density: Density
  /**
   * What's actually applied to the DOM right now. Equals `density` on
   * desktop; collapses to 'comfortable' on mobile viewports regardless of
   * preference.
   */
  effectiveDensity: Density
  setDensity: (next: Density) => void
}

const DensityContext = createContext<DensityContextValue | null>(null)

function readStored(): Density {
  if (typeof window === 'undefined') return DEFAULT_DENSITY
  try {
    const v = window.localStorage.getItem(STORAGE_KEY)
    return v === 'compact' ? 'compact' : DEFAULT_DENSITY
  } catch {
    // Private mode / storage disabled — fall back to default.
    return DEFAULT_DENSITY
  }
}

function isMobileViewport(): boolean {
  if (typeof window === 'undefined') return false
  return window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT_PX}px)`).matches
}

interface ProviderProps {
  children: ReactNode
}

export function DensityProvider({ children }: ProviderProps) {
  const [density, setDensityState] = useState<Density>(readStored)
  const [mobile, setMobile] = useState<boolean>(isMobileViewport)

  // Track viewport width — re-evaluate effective density when crossing the
  // mobile breakpoint.
  useEffect(() => {
    if (typeof window === 'undefined') return
    const mq = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT_PX}px)`)
    const onChange = (e: MediaQueryListEvent) => setMobile(e.matches)
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])

  const effectiveDensity: Density = mobile ? 'comfortable' : density

  // Apply to <body> as a data-density attribute. CSS in index.css reads this.
  useEffect(() => {
    if (typeof document === 'undefined') return
    document.body.dataset.density = effectiveDensity
    return () => {
      // Don't strip the attribute on unmount in production — the provider
      // sits at the app root and unmounting means the page is going away.
      // But during tests with multiple renders, leaving the attribute set
      // is fine; the next mount will overwrite it.
    }
  }, [effectiveDensity])

  const setDensity = useCallback((next: Density) => {
    setDensityState(next)
    try {
      window.localStorage.setItem(STORAGE_KEY, next)
    } catch {
      // Storage unavailable — preference persists in-memory only.
    }
  }, [])

  return (
    <DensityContext.Provider value={{ density, effectiveDensity, setDensity }}>
      {children}
    </DensityContext.Provider>
  )
}

export function useDensity(): DensityContextValue {
  const ctx = useContext(DensityContext)
  if (!ctx) {
    throw new Error('useDensity must be used within a DensityProvider')
  }
  return ctx
}
