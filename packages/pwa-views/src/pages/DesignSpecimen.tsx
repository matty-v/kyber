/*
 * Design specimen page — renders the proposed "Kyber Crystal" palette, typography,
 * motion, and primitive samples so Matt can react to the visual direction before
 * we migrate the real app onto these tokens (see docs/design/2026-04-19-pwa-design-system.md).
 *
 * Intentionally standalone: no API hooks, no shared layout, no import of the
 * production Button/Card/StatusBadge. Sample primitives below are local so the
 * rest of the app keeps its current look until Phase 1 migration.
 */
import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import {
  Gem,
  Play,
  RotateCcw,
  Trash2,
  Activity,
  ChevronRight,
  Cpu,
  Bot,
  Server,
} from 'lucide-react'

// ───────────────────────────────────────────────────────────────────────────
// Section scaffolding
// ───────────────────────────────────────────────────────────────────────────

function Section({
  id,
  title,
  subtitle,
  children,
}: {
  id: string
  title: string
  subtitle?: string
  children: ReactNode
}) {
  return (
    <section id={id} className="mb-16 scroll-mt-20">
      <div className="mb-6 flex items-baseline justify-between border-b border-border-subtle pb-3">
        <div>
          <h2 className="font-display text-2xl font-semibold text-text-primary">{title}</h2>
          {subtitle && (
            <p className="mt-1 text-sm text-text-muted">{subtitle}</p>
          )}
        </div>
        <span className="font-mono text-[10px] uppercase tracking-[0.2em] text-text-disabled">
          §{id}
        </span>
      </div>
      {children}
    </section>
  )
}

function Caption({ children }: { children: ReactNode }) {
  return (
    <p className="font-mono text-[11px] uppercase tracking-[0.15em] text-text-muted">
      {children}
    </p>
  )
}

// ───────────────────────────────────────────────────────────────────────────
// Palette
// ───────────────────────────────────────────────────────────────────────────

interface Swatch {
  name: string
  className: string
  note?: string
  textOn?: 'light' | 'dark'
}

const surfaces: Swatch[] = [
  { name: 'surface-base', className: 'bg-surface-base', note: 'page background', textOn: 'light' },
  { name: 'surface-raised', className: 'bg-surface-raised', note: 'cards · sidebar', textOn: 'light' },
  { name: 'surface-overlay', className: 'bg-surface-overlay', note: 'dialogs · popovers', textOn: 'light' },
  { name: 'surface-sunken', className: 'bg-surface-sunken', note: 'logs · terminal', textOn: 'light' },
]

const borders: Swatch[] = [
  { name: 'border-subtle', className: 'bg-border-subtle', note: 'default divider', textOn: 'light' },
  { name: 'border-default', className: 'bg-border-default', note: 'input · selected', textOn: 'light' },
  { name: 'border-strong', className: 'bg-border-strong', note: 'hover · focus', textOn: 'light' },
]

const text: Swatch[] = [
  { name: 'text-primary', className: 'bg-text-primary', note: 'body · headings', textOn: 'dark' },
  { name: 'text-secondary', className: 'bg-text-secondary', note: 'meta · labels', textOn: 'dark' },
  { name: 'text-muted', className: 'bg-text-muted', note: 'captions · placeholder', textOn: 'dark' },
  { name: 'text-disabled', className: 'bg-text-disabled', note: 'disabled controls', textOn: 'light' },
]

const accents: Swatch[] = [
  { name: 'accent', className: 'bg-accent', note: 'brand · primary CTA', textOn: 'dark' },
  { name: 'accent-muted', className: 'bg-accent-muted', note: 'nav active · tags', textOn: 'light' },
  { name: 'highlight', className: 'bg-highlight', note: 'selection · focus', textOn: 'dark' },
  { name: 'highlight-muted', className: 'bg-highlight-muted', note: 'subtle highlight', textOn: 'light' },
]

const statuses: Swatch[] = [
  { name: 'success', className: 'bg-success', note: 'Running · Ready', textOn: 'dark' },
  { name: 'success-muted', className: 'bg-success-muted', note: 'badge background', textOn: 'light' },
  { name: 'warn', className: 'bg-warn', note: 'Stopping · Preempted', textOn: 'dark' },
  { name: 'warn-muted', className: 'bg-warn-muted', note: 'badge background', textOn: 'light' },
  { name: 'danger', className: 'bg-danger', note: 'Failed · destructive', textOn: 'dark' },
  { name: 'danger-muted', className: 'bg-danger-muted', note: 'badge background', textOn: 'light' },
  { name: 'info', className: 'bg-info', note: 'Creating · Starting', textOn: 'dark' },
  { name: 'info-muted', className: 'bg-info-muted', note: 'badge background', textOn: 'light' },
]

function SwatchGrid({ label, items }: { label: string; items: Swatch[] }) {
  return (
    <div className="mb-8">
      <Caption>{label}</Caption>
      <div className="mt-3 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
        {items.map((s) => (
          <div
            key={s.name}
            className="overflow-hidden rounded-lg border border-border-subtle"
          >
            <div
              className={`${s.className} flex h-20 items-end p-3`}
              aria-label={s.name}
            >
              <span
                className={`font-mono text-[11px] ${
                  s.textOn === 'light' ? 'text-text-primary/80' : 'text-surface-base/80'
                }`}
              >
                {s.name}
              </span>
            </div>
            {s.note && (
              <div className="bg-surface-raised px-3 py-2 text-xs text-text-muted">
                {s.note}
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}

// ───────────────────────────────────────────────────────────────────────────
// Typography
// ───────────────────────────────────────────────────────────────────────────

function TypographySection() {
  return (
    <div className="grid gap-6 lg:grid-cols-3">
      {/* Inter — UI */}
      <div className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <Caption>Inter · UI sans</Caption>
        <div className="mt-4 space-y-3 font-sans">
          <p className="text-2xl font-semibold text-text-primary">
            Fleet Command Console
          </p>
          <p className="text-base text-text-secondary">
            Agents reconcile in real time. Machines self-heal.
          </p>
          <p className="text-sm text-text-muted">
            Secondary metadata uses weight 400 at 14px.
          </p>
          <div className="grid grid-cols-4 gap-2 pt-2">
            {[400, 500, 600, 700].map((w) => (
              <div key={w} className="text-center">
                <div
                  className="text-text-primary"
                  style={{ fontWeight: w }}
                >
                  Aa
                </div>
                <div className="font-mono text-[10px] text-text-disabled">{w}</div>
              </div>
            ))}
          </div>
        </div>
      </div>

      {/* Inter Tight — Display */}
      <div className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <Caption>Inter Tight · display numerals</Caption>
        <div className="mt-4 font-display">
          <div className="text-6xl font-semibold tabular-nums text-text-primary">
            142
          </div>
          <div className="mt-1 text-sm text-text-muted">agents running</div>
          <div className="mt-4 grid grid-cols-2 gap-3">
            <div>
              <div className="text-3xl font-semibold tabular-nums text-text-primary">
                08
              </div>
              <div className="font-mono text-[10px] uppercase tracking-[0.15em] text-text-disabled">
                machines
              </div>
            </div>
            <div>
              <div className="text-3xl font-semibold tabular-nums text-text-primary">
                99.8%
              </div>
              <div className="font-mono text-[10px] uppercase tracking-[0.15em] text-text-disabled">
                uptime
              </div>
            </div>
          </div>
        </div>
      </div>

      {/* JetBrains Mono — Technical */}
      <div className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <Caption>JetBrains Mono · technical</Caption>
        <div className="mt-4 space-y-3 font-mono text-sm">
          <div className="text-text-primary">agent-7f3a8b2c</div>
          <div className="text-text-secondary">machine · gke-node-a-01</div>
          <div className="rounded-lg bg-surface-sunken p-3 text-[12px] leading-relaxed text-text-secondary">
            <div className="text-success">
              [info]{'\u00a0'}agent started{' '}
              <span className="text-text-muted">in 2.4s</span>
            </div>
            <div className="text-info">
              [info]{'\u00a0'}reconciling spec.resources
            </div>
            <div className="text-warn">
              [warn]{'\u00a0'}spot preemption signal received
            </div>
            <div className="text-danger">
              [err ]{'\u00a0'}fuse-overlayfs unavailable
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

// ───────────────────────────────────────────────────────────────────────────
// Sample primitives (local to the specimen page)
// ───────────────────────────────────────────────────────────────────────────

type SampleButtonVariant = 'primary' | 'secondary' | 'danger' | 'ghost'
type SampleButtonSize = 'sm' | 'md' | 'lg'

const sampleButtonVariants: Record<SampleButtonVariant, string> = {
  primary:
    'bg-accent text-surface-base hover:brightness-110 disabled:opacity-40',
  secondary:
    'bg-surface-overlay text-text-primary hover:bg-border-default disabled:opacity-40',
  danger:
    'bg-danger text-surface-base hover:brightness-110 disabled:opacity-40',
  ghost:
    'bg-transparent text-text-secondary hover:text-text-primary hover:bg-surface-overlay disabled:opacity-40',
}

const sampleButtonSizes: Record<SampleButtonSize, string> = {
  sm: 'px-2.5 py-1.5 text-xs min-h-[32px]',
  md: 'px-3.5 py-2 text-sm min-h-[40px]',
  lg: 'px-5 py-2.5 text-base min-h-[44px]',
}

function SampleButton({
  variant = 'secondary',
  size = 'md',
  children,
  className = '',
  ...rest
}: {
  variant?: SampleButtonVariant
  size?: SampleButtonSize
  children: ReactNode
  className?: string
} & React.ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button
      {...rest}
      className={`inline-flex items-center justify-center gap-1.5 rounded-lg font-medium transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-ring ${sampleButtonVariants[variant]} ${sampleButtonSizes[size]} ${className}`}
    >
      {children}
    </button>
  )
}

type SamplePhase =
  | 'Running'
  | 'Ready'
  | 'Creating'
  | 'Starting'
  | 'Provisioning'
  | 'Stopping'
  | 'Preempted'
  | 'Failed'
  | 'Stopped'

const phaseTone: Record<
  SamplePhase,
  { bg: string; text: string; ring: string; pulse?: boolean }
> = {
  Running: { bg: 'bg-success-muted', text: 'text-success', ring: 'ring-success/30' },
  Ready: { bg: 'bg-success-muted', text: 'text-success', ring: 'ring-success/30' },
  Creating: { bg: 'bg-info-muted', text: 'text-info', ring: 'ring-info/30', pulse: true },
  Starting: { bg: 'bg-info-muted', text: 'text-info', ring: 'ring-info/30', pulse: true },
  Provisioning: { bg: 'bg-info-muted', text: 'text-info', ring: 'ring-info/30', pulse: true },
  Stopping: { bg: 'bg-warn-muted', text: 'text-warn', ring: 'ring-warn/30' },
  Preempted: { bg: 'bg-warn-muted', text: 'text-warn', ring: 'ring-warn/30' },
  Failed: { bg: 'bg-danger-muted', text: 'text-danger', ring: 'ring-danger/30' },
  Stopped: { bg: 'bg-surface-overlay', text: 'text-text-muted', ring: 'ring-border-default' },
}

function SampleBadge({ phase }: { phase: SamplePhase }) {
  const tone = phaseTone[phase]
  const pulse = tone.pulse
    ? 'before:absolute before:inset-0 before:rounded-full before:[animation:kyber-pulse-ring_1.6s_var(--ease-spring)_infinite]'
    : ''
  return (
    <span
      className={`relative inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 font-mono text-[11px] font-medium uppercase tracking-[0.05em] ring-1 ring-inset ${tone.bg} ${tone.text} ${tone.ring} ${pulse}`}
    >
      {tone.pulse && (
        <span className="h-1.5 w-1.5 rounded-full bg-current opacity-80" />
      )}
      {phase}
    </span>
  )
}

// ───────────────────────────────────────────────────────────────────────────
// Hero numerals + motion samples
// ───────────────────────────────────────────────────────────────────────────

function HeroNumerals() {
  return (
    <div className="rounded-2xl border border-border-subtle bg-surface-raised p-6">
      <div className="mb-4 flex items-center gap-2">
        <Gem className="h-4 w-4 text-accent" strokeWidth={1.5} />
        <Caption>Fleet at a glance</Caption>
      </div>
      <div className="grid gap-6 sm:grid-cols-3">
        <HeroMetric value="142" label="agents" trend="+4" trendPositive />
        <HeroMetric value="08" label="machines" trend="±0" />
        <HeroMetric value="99.8%" label="uptime 7d" trend="+0.2" trendPositive />
      </div>
    </div>
  )
}

function HeroMetric({
  value,
  label,
  trend,
  trendPositive,
}: {
  value: string
  label: string
  trend?: string
  trendPositive?: boolean
}) {
  return (
    <div>
      <div className="flex items-baseline gap-3">
        <div className="font-display text-5xl font-semibold tabular-nums text-text-primary">
          {value}
        </div>
        {trend && (
          <div
            className={`font-mono text-xs ${
              trendPositive ? 'text-success' : 'text-text-muted'
            }`}
          >
            {trend}
          </div>
        )}
      </div>
      <div className="mt-1 font-mono text-[10px] uppercase tracking-[0.2em] text-text-disabled">
        {label}
      </div>
    </div>
  )
}

function MotionSample() {
  const [phase, setPhase] = useState<SamplePhase>('Provisioning')
  const [flashKey, setFlashKey] = useState(0)

  useEffect(() => {
    const order: SamplePhase[] = ['Provisioning', 'Starting', 'Running', 'Running', 'Stopping', 'Stopped']
    let i = 0
    const tick = setInterval(() => {
      i = (i + 1) % order.length
      setPhase(order[i])
      setFlashKey((k) => k + 1)
    }, 2200)
    return () => clearInterval(tick)
  }, [])

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <div className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <Caption>Skeleton shimmer</Caption>
        <div className="mt-4 space-y-3">
          {[0, 1, 2].map((i) => (
            <div
              key={i}
              className="h-4 rounded-md"
              style={{
                backgroundImage:
                  'linear-gradient(90deg, var(--color-surface-overlay) 0%, var(--color-border-default) 50%, var(--color-surface-overlay) 100%)',
                backgroundSize: '200% 100%',
                animation: 'kyber-shimmer 1.8s linear infinite',
                width: `${70 - i * 10}%`,
              }}
            />
          ))}
        </div>
      </div>
      <div className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <Caption>Status-change flash</Caption>
        <div
          key={flashKey}
          className="mt-4 flex items-center justify-between rounded-lg border border-border-subtle px-3 py-2"
          style={{ animation: 'kyber-flash 800ms ease-out' }}
        >
          <span className="font-mono text-sm text-text-primary">
            agent-7f3a8b2c
          </span>
          <SampleBadge phase={phase} />
        </div>
        <p className="mt-3 text-xs text-text-muted">
          Background flashes accent-muted when the phase changes — gives operators a
          peripheral-vision cue that something moved.
        </p>
      </div>
    </div>
  )
}

// ───────────────────────────────────────────────────────────────────────────
// Primitives
// ───────────────────────────────────────────────────────────────────────────

function PrimitivesSection() {
  return (
    <div className="space-y-8">
      {/* Buttons */}
      <div>
        <Caption>Buttons — variant · size</Caption>
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <SampleButton variant="primary">
            <Play className="h-4 w-4" /> Start agent
          </SampleButton>
          <SampleButton variant="secondary">
            <RotateCcw className="h-4 w-4" /> Restart
          </SampleButton>
          <SampleButton variant="danger">
            <Trash2 className="h-4 w-4" /> Delete
          </SampleButton>
          <SampleButton variant="primary" disabled>
            Disabled
          </SampleButton>
        </div>
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <SampleButton variant="primary" size="sm">Small</SampleButton>
          <SampleButton variant="primary" size="md">Medium</SampleButton>
          <SampleButton variant="primary" size="lg">Large</SampleButton>
        </div>
      </div>

      {/* Status badges */}
      <div>
        <Caption>Status badges — all agent/machine phases</Caption>
        <div className="mt-3 flex flex-wrap gap-2">
          {(Object.keys(phaseTone) as SamplePhase[]).map((p) => (
            <SampleBadge key={p} phase={p} />
          ))}
        </div>
      </div>

      {/* Cards — agent-style + machine-style */}
      <div>
        <Caption>Cards — list item density</Caption>
        <div className="mt-3 grid gap-3 lg:grid-cols-2">
          <SampleAgentCard
            id="agent-7f3a8b2c"
            model="claude-opus-4-7"
            machine="gke-node-a-01"
            phase="Running"
          />
          <SampleAgentCard
            id="agent-9c2e1d44"
            model="claude-sonnet-4-6"
            machine="gke-node-b-03"
            phase="Provisioning"
          />
          <SampleMachineCard
            id="gke-node-a-01"
            type="n2d-standard-4"
            zone="us-central1-a"
            phase="Ready"
            agents={4}
            spot
          />
          <SampleMachineCard
            id="gke-node-b-03"
            type="n2d-standard-8"
            zone="us-central1-b"
            phase="Failed"
            agents={0}
          />
        </div>
      </div>

      {/* Dialog preview */}
      <div>
        <Caption>Dialog — rendered inline (not modal)</Caption>
        <div className="mt-3 rounded-xl border border-border-subtle bg-surface-base p-6">
          <div className="mx-auto w-full max-w-sm rounded-xl border border-border-default bg-surface-overlay p-6 shadow-xl">
            <h3 className="font-display text-base font-semibold text-text-primary">
              Delete agent?
            </h3>
            <p className="mt-2 text-sm text-text-muted">
              This will permanently delete agent{' '}
              <span className="font-mono text-text-secondary">agent-7f3a8b2c</span>
              .
            </p>
            <div className="mt-5 flex justify-end gap-3">
              <SampleButton variant="ghost" size="sm">Cancel</SampleButton>
              <SampleButton variant="danger" size="sm">Delete</SampleButton>
            </div>
          </div>
        </div>
      </div>

      {/* Input sample */}
      <div>
        <Caption>Input · select — surface-sunken on surface-raised</Caption>
        <div className="mt-3 grid gap-3 sm:grid-cols-2">
          <label className="block">
            <span className="mb-1 block font-mono text-[11px] uppercase tracking-[0.15em] text-text-muted">
              Server URL
            </span>
            <input
              defaultValue="https://kyber.example.com"
              className="w-full rounded-lg border border-border-default bg-surface-sunken px-3 py-2 font-mono text-sm text-text-primary placeholder-text-disabled focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent-ring"
            />
          </label>
          <label className="block">
            <span className="mb-1 block font-mono text-[11px] uppercase tracking-[0.15em] text-text-muted">
              Model
            </span>
            <select
              defaultValue="claude-opus-4-7"
              className="w-full rounded-lg border border-border-default bg-surface-sunken px-3 py-2 font-mono text-sm text-text-primary focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent-ring"
            >
              <option>claude-opus-4-7</option>
              <option>claude-sonnet-4-6</option>
              <option>claude-haiku-4-5</option>
            </select>
          </label>
        </div>
      </div>
    </div>
  )
}

function SampleAgentCard({
  id,
  model,
  machine,
  phase,
}: {
  id: string
  model: string
  machine: string
  phase: SamplePhase
}) {
  return (
    <div className="group cursor-pointer rounded-xl border border-border-subtle bg-surface-raised p-4 transition-colors hover:border-border-default">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <Bot className="h-4 w-4 text-accent" strokeWidth={1.5} />
            <span className="truncate font-mono text-sm text-text-primary">
              {id}
            </span>
            <SampleBadge phase={phase} />
          </div>
          <p className="mt-2 font-mono text-xs text-text-muted">
            {model} · {machine}
          </p>
        </div>
        <ChevronRight className="h-4 w-4 shrink-0 text-text-disabled transition-transform group-hover:translate-x-0.5 group-hover:text-text-secondary" />
      </div>
    </div>
  )
}

function SampleMachineCard({
  id,
  type,
  zone,
  phase,
  agents,
  spot,
}: {
  id: string
  type: string
  zone: string
  phase: SamplePhase
  agents: number
  spot?: boolean
}) {
  return (
    <div className="group cursor-pointer rounded-xl border border-border-subtle bg-surface-raised p-4 transition-colors hover:border-border-default">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <Server className="h-4 w-4 text-accent" strokeWidth={1.5} />
            <span className="truncate font-mono text-sm text-text-primary">
              {id}
            </span>
            <SampleBadge phase={phase} />
          </div>
          <p className="mt-2 font-mono text-xs text-text-muted">
            {type} · {zone}
            {spot && ' · spot'}
          </p>
          <div className="mt-2 flex items-center gap-3 font-mono text-[11px] text-text-muted">
            <span className="inline-flex items-center gap-1">
              <Activity className="h-3 w-3" /> {agents} agents
            </span>
            <span className="inline-flex items-center gap-1">
              <Cpu className="h-3 w-3" /> 4 cpu · 16 Gi
            </span>
          </div>
        </div>
        <ChevronRight className="h-4 w-4 shrink-0 text-text-disabled transition-transform group-hover:translate-x-0.5 group-hover:text-text-secondary" />
      </div>
    </div>
  )
}

// ───────────────────────────────────────────────────────────────────────────
// Page
// ───────────────────────────────────────────────────────────────────────────

export function DesignSpecimen() {
  return (
    <div className="min-h-dvh bg-surface-base font-sans text-text-primary antialiased">
      <div className="mx-auto max-w-5xl px-6 py-10">
        {/* Header */}
        <header className="mb-12 flex items-start justify-between border-b border-border-subtle pb-8">
          <div>
            <div className="flex items-center gap-2">
              <Gem className="h-5 w-5 text-accent" strokeWidth={1.5} />
              <span className="font-mono text-xs uppercase tracking-[0.3em] text-text-muted">
                Kyber Crystal
              </span>
            </div>
            <h1 className="mt-3 font-display text-4xl font-semibold text-text-primary">
              Design Specimen
            </h1>
            <p className="mt-2 max-w-xl text-sm text-text-secondary">
              Proposed palette, typography, and primitives for the Kyber PWA. Review
              the visual direction here before Phase&nbsp;1 token migration. See{' '}
              <span className="font-mono text-text-muted">
                docs/design/2026-04-19-pwa-design-system.md
              </span>
              .
            </p>
          </div>
          <nav className="hidden shrink-0 lg:block">
            <Caption>On this page</Caption>
            <ul className="mt-2 space-y-1 font-mono text-xs text-text-muted">
              <li><a className="hover:text-text-primary" href="#palette">§palette</a></li>
              <li><a className="hover:text-text-primary" href="#typography">§typography</a></li>
              <li><a className="hover:text-text-primary" href="#primitives">§primitives</a></li>
              <li><a className="hover:text-text-primary" href="#hero">§hero</a></li>
              <li><a className="hover:text-text-primary" href="#motion">§motion</a></li>
            </ul>
          </nav>
        </header>

        <Section
          id="palette"
          title="Palette"
          subtitle="Semantic tokens — every color in the app references one of these."
        >
          <SwatchGrid label="Surfaces" items={surfaces} />
          <SwatchGrid label="Borders" items={borders} />
          <SwatchGrid label="Text" items={text} />
          <SwatchGrid label="Accent · highlight" items={accents} />
          <SwatchGrid label="Status" items={statuses} />
        </Section>

        <Section
          id="typography"
          title="Typography"
          subtitle="Three faces, three jobs. Inter · Inter Tight · JetBrains Mono."
        >
          <TypographySection />
        </Section>

        <Section
          id="primitives"
          title="Primitives"
          subtitle="Buttons, cards, badges, dialog, input — rendered on the new tokens."
        >
          <PrimitivesSection />
        </Section>

        <Section
          id="hero"
          title="Hero numerals"
          subtitle="Display face + tabular figures for fleet-level metrics."
        >
          <HeroNumerals />
        </Section>

        <Section
          id="motion"
          title="Motion"
          subtitle="Ambient motion — skeletons shimmer while loading; rows flash when state changes."
        >
          <MotionSample />
        </Section>

        <footer className="mt-16 border-t border-border-subtle pt-6">
          <p className="font-mono text-[11px] uppercase tracking-[0.15em] text-text-disabled">
            Specimen · no production routing or API · safe to delete post-review
          </p>
        </footer>
      </div>
    </div>
  )
}
