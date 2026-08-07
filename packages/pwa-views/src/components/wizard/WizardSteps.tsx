import { useEffect, useRef } from 'react'
import type { WizardState } from './types'
import { WIZARD_STEPS, earliestInvalidStep } from './validation'

export interface WizardStepsProps {
  state: WizardState
  activeStep: number
  onStepClick: (stepId: number) => void
}

/**
 * WizardSteps renders the click-to-jump chip strip at the top of the wizard.
 *
 * Disabled rules:
 * - Past chips (id < activeStep) are always enabled — users can always revisit
 *   a completed step to fix something.
 * - Future chips (id > activeStep) are enabled only if every step strictly
 *   before them is valid (i.e., chip id <= earliestInvalidStep).
 * - The active chip itself is rendered as the current step (aria-current="step").
 *
 * The orchestrator wires `onStepClick` to `setSearchParams({ step: String(id) })`.
 */
export function WizardSteps({ state, activeStep, onStepClick }: WizardStepsProps) {
  const earliestInvalid = earliestInvalidStep(state)
  const navRef = useRef<HTMLElement | null>(null)

  // Keep the active chip in view as the user advances on narrow viewports.
  // No-op on desktop (the strip fits without scrolling); on mobile this scrolls
  // chip 5 into view when the user reaches Review.
  // jsdom (vitest) doesn't implement scrollIntoView, so feature-gate the call.
  useEffect(() => {
    const container = navRef.current
    if (!container) return
    const activeChip = container.querySelector<HTMLElement>('[aria-current="step"]')
    if (!activeChip || typeof activeChip.scrollIntoView !== 'function') return
    activeChip.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'center' })
  }, [activeStep])

  return (
    <nav
      ref={navRef}
      aria-label="Wizard steps"
      // Horizontal scroll on narrow viewports. The Card cap is max-w-lg (~32rem)
      // on desktop; 5 chips fit there. On phones (<24rem inside the card padding),
      // the chips overflow — let them scroll horizontally instead of wrap.
      // -mx-* extends the scrollable area edge-to-edge of the Card without
      // visually breaking the surrounding layout.
      className="flex gap-2 mb-5 overflow-x-auto -mx-4 px-4 sm:mx-0 sm:px-0 [scrollbar-width:none] [-ms-overflow-style:none] [&::-webkit-scrollbar]:hidden"
    >
      {WIZARD_STEPS.map((step) => {
        const isActive = step.id === activeStep
        const isPast = step.id < activeStep
        const isReachable = step.id <= earliestInvalid
        const disabled = !isPast && !isReachable
        const baseClass =
          'inline-flex shrink-0 items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-medium whitespace-nowrap transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring'
        const activeClass = 'bg-accent text-surface-base border-accent'
        const reachableClass =
          'bg-surface-overlay text-text-primary border-border-default hover:border-accent'
        const disabledClass =
          'bg-surface-overlay text-text-disabled border-border-default cursor-not-allowed'
        const className = isActive
          ? `${baseClass} ${activeClass}`
          : disabled
            ? `${baseClass} ${disabledClass}`
            : `${baseClass} ${reachableClass}`
        return (
          <button
            key={step.id}
            type="button"
            disabled={disabled}
            aria-disabled={disabled || undefined}
            aria-current={isActive ? 'step' : undefined}
            onClick={() => onStepClick(step.id)}
            className={className}
          >
            <span aria-hidden="true">{step.id}</span>
            <span aria-hidden="true">·</span>
            <span>{step.label}</span>
          </button>
        )
      })}
    </nav>
  )
}
