/*
 * Phase → visual tone lookup.
 *
 * Keeps the mapping from agent/machine phase names to semantic color tone in
 * one place. Adding a new phase is a data change here, not a UI edit in the
 * consuming components.
 */

import type { AgentPhase, MachinePhase } from '../types'

export type Tone =
  | 'success'
  | 'warn'
  | 'danger'
  | 'info'
  | 'highlight'
  | 'neutral'

export interface PhaseStyle {
  tone: Tone
  /** true when the phase is transitional and should animate. */
  pulse?: boolean
}

export type PhaseKey = AgentPhase | MachinePhase | string

const phaseMap: Record<string, PhaseStyle> = {
  Running:      { tone: 'success' },
  Ready:        { tone: 'success' },
  Creating:     { tone: 'info', pulse: true },
  Starting:     { tone: 'info', pulse: true },
  Provisioning: { tone: 'info', pulse: true },
  Replacing:    { tone: 'info', pulse: true },
  Restarting:   { tone: 'info', pulse: true },
  Stopping:     { tone: 'warn' },
  Terminating:  { tone: 'warn' },
  Preempted:    { tone: 'warn' },
  Failed:       { tone: 'danger' },
  Stopped:      { tone: 'neutral' },
  Deleted:      { tone: 'neutral' },
  NeedsAuth:        { tone: 'warn' },
  MemoryExhausted:  { tone: 'danger' },
  DiskExhausted:    { tone: 'danger' },
  BrokenRuntime:    { tone: 'danger' },
}

export function phaseStyle(phase: PhaseKey): PhaseStyle {
  return phaseMap[phase] ?? { tone: 'neutral' }
}

/**
 * Tailwind class strings per tone, for consistent ring-badge styling.
 * Kept co-located so adding a tone is a single-file edit.
 */
export const toneBadgeClasses: Record<Tone, string> = {
  success:   'bg-success-muted text-success ring-success/30',
  warn:      'bg-warn-muted text-warn ring-warn/30',
  danger:    'bg-danger-muted text-danger ring-danger/30',
  info:      'bg-info-muted text-info ring-info/30',
  highlight: 'bg-highlight-muted text-highlight ring-highlight/30',
  neutral:   'bg-surface-overlay text-text-muted ring-border-default',
}

/**
 * Tailwind class strings per tone for foreground-only consumers — the
 * Sparkline component picks up the stroke color via `currentColor`, so
 * it only needs the text-* class. Deliberately separate from
 * toneBadgeClasses to keep the badge styling intact.
 */
export const toneTextClasses: Record<Tone, string> = {
  success:   'text-success',
  warn:      'text-warn',
  danger:    'text-danger',
  info:      'text-info',
  highlight: 'text-highlight',
  neutral:   'text-text-muted',
}

/**
 * Solid tone backgrounds for filled bars (the dashboard status-distribution
 * bar and the context-pressure bar). Distinct from toneBadgeClasses, which are
 * muted ring badges — these are full-strength fills.
 */
export const toneBarClasses: Record<Tone, string> = {
  success:   'bg-success',
  warn:      'bg-warn',
  danger:    'bg-danger',
  info:      'bg-info',
  highlight: 'bg-highlight',
  neutral:   'bg-border-default',
}
