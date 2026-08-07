# Kyber PWA — Design System Proposal

**Author:** lando (frontend)
**Date:** 2026-04-19
**Status:** Direction locked — see §10 for decisions. Next step: palette/typography mockup page.
**Scope:** `pwa/` — the Kyber Fleet Command Console

---

## 1. North Star

Kyber is a **fleet command console**, not another admin panel. The name, the Gem logo, the cyan+mono wordmark, the tagline "Fleet Command Console" — the brand is already pointing at a specific aesthetic: a crystalline, sci‑fi operations console that feels *precise, fast, and inhabited by a small number of people who know what they're doing*.

The proposal below commits to that direction. The alternative — leaning generic-SaaS — is defensible but wastes the brand equity that's already there.

**Design principles:**

1. **Density before decoration.** Operators need to see a lot at once. Whitespace is earned, not free.
2. **Monospace is a feature.** IDs, resource names, phase strings, metrics — these are technical artifacts. Treat them like code.
3. **Status is structural.** Phase and health are not afterthoughts; they're primary visual hierarchy.
4. **Motion is diegetic.** Transitions hint at real systems moving — agents starting, machines provisioning — not UI gloss.
5. **One dark theme, done well.** No light mode (yet). An ops console has a natural environment; don't fight it.

---

## 2. Current State Audit

**What's working:**

- PWA shell is solid — sidebar on desktop, bottom tab bar on mobile, safe-area insets handled, `h-dvh` layout. Good bones.
- Hand-rolled `Button`, `Card`, `StatusBadge`, `ConfirmDialog` are coherent and typed. Accessibility is partially considered (focus rings, `aria-labelledby`, keyboard on `Card`).
- Tailwind v4, React Query, Zustand, react-router, lucide, xterm — a modern, uncontroversial stack.
- Brand direction is already legible: `Gem` icon in cyan, `font-mono uppercase tracking-[0.2em]` wordmark.

**What's missing or inconsistent:**

| Gap | Evidence | Impact |
|---|---|---|
| No design tokens | Raw Tailwind utilities (`bg-neutral-900`, `text-cyan-300`) everywhere | Re-skinning requires grep-and-pray |
| Status colors are a switch statement | `StatusBadge.tsx:10-36` | Adding a new phase means editing code in two places |
| Thin primitive set | No `Input`, `Select`, `Tabs`, `Tooltip`, `Toast`, `Table`, `Dropdown` | `Settings.tsx` hand-rolls inputs; `AgentDetail.tsx` hand-rolls tabs |
| Brand promise not cashed in | Pages are visually indistinct from any generic admin panel | Opportunity cost — the "command console" feel exists only in the sidebar header |
| No motion language | Only `transition-colors` on hovers | Loading, page-transition, and state-change moments feel flat |
| Typography is single-axis | System sans for everything except the brand | IDs and metrics read the same as prose |
| Dense data presented as cards | `MachineList`, `AgentList` wrap every item in a `max-w-4xl` stack of cards | Desktop users scroll through cards that could be a table |

---

## 3. The Token System (the one thing that unlocks everything else)

The single most leveraged change is introducing a **semantic token layer** using Tailwind v4's `@theme` directive. Today the codebase says `bg-neutral-900` in 40+ places; tomorrow it says `bg-surface` and we re-skin the whole app by editing one file.

### Proposed palette — "Kyber Crystal"

Dark, cool, cyan-primary. A secondary accent hue (kyber-violet) for selection states, highlights, and focus that don't carry semantic meaning. Semantic colors (success/warn/danger/info) remain distinct from accent.

```css
/* src/index.css */
@import "tailwindcss";

@theme {
  /* Surfaces — nested depth */
  --color-surface-base: oklch(14% 0.01 240);        /* page background */
  --color-surface-raised: oklch(18% 0.012 240);     /* cards, sidebar */
  --color-surface-overlay: oklch(22% 0.015 240);    /* dialogs, popovers */
  --color-surface-sunken: oklch(11% 0.008 240);     /* logs, terminal, code blocks */

  /* Borders — subtle → strong */
  --color-border-subtle: oklch(25% 0.015 240);
  --color-border-default: oklch(32% 0.02 240);
  --color-border-strong: oklch(45% 0.03 240);

  /* Text */
  --color-text-primary: oklch(96% 0.01 240);
  --color-text-secondary: oklch(72% 0.015 240);
  --color-text-muted: oklch(55% 0.015 240);
  --color-text-disabled: oklch(40% 0.01 240);

  /* Brand / accent */
  --color-accent: oklch(78% 0.17 200);              /* cyan — primary brand */
  --color-accent-muted: oklch(78% 0.17 200 / 0.15);
  --color-accent-ring: oklch(78% 0.17 200 / 0.35);

  /* Secondary highlight — for selected/focus/hero numbers */
  --color-highlight: oklch(75% 0.18 300);           /* kyber-violet */

  /* Semantic status */
  --color-success: oklch(78% 0.18 155);
  --color-warn:    oklch(82% 0.17 75);
  --color-danger:  oklch(68% 0.22 28);
  --color-info:    oklch(78% 0.17 240);

  /* Typography */
  --font-sans: "Inter", system-ui, sans-serif;
  --font-mono: "JetBrains Mono", ui-monospace, monospace;
  --font-display: "Inter Tight", "Inter", sans-serif;

  /* Motion */
  --ease-spring: cubic-bezier(0.25, 0.8, 0.4, 1);
  --ease-exit: cubic-bezier(0.6, 0, 0.8, 0.4);
}
```

With that in place, every component reads like prose: `bg-surface-raised border-border-subtle text-text-primary`. Swapping the palette becomes a single-file diff.

**Note on OKLCH:** perceptually uniform, modern browsers all support it, and it makes tint/shade tweaks obvious — a lightness change of 4% *looks* like a 4% change. Safe bet for a 2026 project.

---

## 4. Typography

Three faces, each with a job:

| Face | Role | Use |
|---|---|---|
| **Inter** (sans) | UI chrome | labels, body, nav, buttons |
| **Inter Tight** (display) | Hero numerals | fleet counts, token usage, dashboard values |
| **JetBrains Mono** | Technical | IDs, phase names, zone, machineType, log output, terminal |

Load via `@fontsource` packages — fully local, no CDN dependency, no privacy concern. Adds ~150KB total, subsettable.

Size scale stays on Tailwind's defaults; I'd introduce two semantic aliases:
- `text-metric` → `text-2xl font-display font-semibold tabular-nums`
- `text-id` → `font-mono text-xs text-text-secondary`

---

## 5. Motion

Framer Motion for orchestrated transitions, plus a small set of keyframe utilities for always-on ambient motion (status badge pulse while "Provisioning", subtle glow on Running states).

**Motion budget:**
- Page transitions: 180ms fade+slide, `--ease-spring`
- Card entry: 120ms stagger on initial list render only (no re-render thrash)
- Skeleton loaders: 2s shimmer cycle, subtle
- Status-change flash: 400ms accent-ring pulse when a phase transitions in live updates
- Dialog/overlay: 200ms scale 0.96 → 1 with backdrop blur

Prefers-reduced-motion killswitch for all of it.

---

## 6. Component Library

**Recommendation: shadcn/ui primitives + Radix for headless logic, kept in `src/components/ui/`.**

Why shadcn over alternatives:

| Option | Pro | Con | Verdict |
|---|---|---|---|
| shadcn/ui | Copy-in code, we own it, Tailwind-native, pairs perfectly with v4 `@theme` | Requires curation — we decide what to adopt | **Pick this** |
| Pure Radix | Maximum control | More boilerplate, re-solves problems shadcn solved | Use for anything shadcn doesn't cover |
| Park UI / Ark UI | Framework-agnostic, nice | Less momentum, less community | No |
| Mantine / MUI | Batteries included | Heavy, fights Tailwind, generic aesthetic | No |
| Headless UI | Maintained, simple | Thinner than Radix | Fine fallback |

The hand-rolled `Button` and `Card` stay — they already fit the theme. We introduce shadcn as we hit gaps: `Select` for CreateAgent's CPU/memory/disk pickers, `Tabs` for AgentDetail, `Toast` (Sonner) for mutation feedback, `Tooltip` for action buttons, `Table` (TanStack Table under the hood) for the list views' desktop mode, `Dropdown` for overflow action menus.

---

## 7. File Architecture

Today everything sits flat in `components/`. As the primitive set grows this will get noisy. Proposed layout:

```
src/
├── components/
│   ├── ui/                  # primitives — Button, Card, Badge, Dialog, Input, Select, Tabs, Tooltip, Toast, Table
│   └── app/                 # domain — AgentCard, MachineCard, FleetSummaryCard, StatusBadge, LogViewer, ExecTerminal, TokenUsage, SecretsTab
├── lib/
│   ├── design/
│   │   ├── motion.ts        # shared motion presets
│   │   └── status.ts        # phase → token mapping (replaces the switch in StatusBadge)
│   └── ...                  # existing api.ts, pkce.ts, etc.
├── pages/                   # unchanged
└── index.css                # @theme tokens live here
```

No rename-the-world churn. Move files as they're touched.

---

## 8. A Concrete "Before / After" — StatusBadge

The switch statement in `StatusBadge.tsx` is a good microcosm.

**Today:**
```tsx
case 'Running': return 'bg-green-500/20 text-green-400 ring-green-500/30'
case 'Failed':  return 'bg-red-500/20   text-red-400   ring-red-500/30'
// ...
```

**Proposed:**
```ts
// src/lib/design/status.ts
export const phaseTokens: Record<Phase, { tone: 'success' | 'warn' | 'danger' | 'info' | 'neutral' | 'highlight'; pulse?: boolean }> = {
  Running:      { tone: 'success' },
  Ready:        { tone: 'success' },
  Creating:     { tone: 'info', pulse: true },
  Starting:     { tone: 'info', pulse: true },
  Provisioning: { tone: 'info', pulse: true },
  Stopping:     { tone: 'warn' },
  Preempted:    { tone: 'warn' },
  Failed:       { tone: 'danger' },
  Suspended:    { tone: 'highlight' },
  Stopped:      { tone: 'neutral' },
  Deleted:      { tone: 'neutral' },
  // ...
}
```

Badge consumes the token, picks classes from a single `toneClasses` map, and we get `pulse` for free on transitional phases. New phases are a data change, not a UI change.

---

## 9. Rollout Plan

Four phases, each independently shippable. Nothing in this plan is a big-bang rewrite.

### Phase 1 — Foundation (invisible to users, unblocks everything)
- Introduce `@theme` tokens in `index.css`
- Add `@fontsource/inter`, `@fontsource/inter-tight`, `@fontsource/jetbrains-mono`
- Migrate `Button`, `Card`, `StatusBadge`, `ConfirmDialog` to semantic tokens
- Extract `status.ts` lookup table
- **No visual regressions** — tokens match today's palette at first, then we tune

### Phase 2 — Primitives (fill the gaps)
- Install shadcn via CLI, adopt `Input`, `Select`, `Tabs`, `Tooltip`, `Toast` (Sonner), `Dropdown`
- Refactor `Settings`, `CreateAgent`, `CreateMachine`, `AgentDetail` tabs onto new primitives
- Wire Sonner toasts into React Query's mutation lifecycle — every action gives feedback

### Phase 3 — Command console pass (the visible redesign)
- Tune the token palette toward the "Kyber Crystal" direction — cooler surfaces, tighter cyan, the violet highlight
- Adopt Inter Tight for hero metrics on `FleetOverview`
- Add TanStack Table desktop view for `MachineList` / `AgentList`, keep card view for mobile
- Motion pass — page transitions, skeleton shimmer, status-change flash

### Phase 4 — Polish
- Sparklines on fleet summary (phase counts over time, if telemetry exposes it)
- Compact density mode toggle (user preference)
- Empty-state illustrations — small, restrained, on-brand
- Systematic keyboard shortcuts (`g a` → agents, `g m` → machines, `⌘k` → command palette)

---

## 10. Decisions (resolved 2026-04-19)

1. **Palette commit** — Build a standalone mockup page rendering the proposed "Kyber Crystal" palette, typography, and primitives **before** migrating tokens. Matt reviews the mockup, then Phase 1 migrates to the tuned values directly.
2. **Fonts** — Adopt all three: **Inter** (UI sans), **Inter Tight** (display / hero numerals), **JetBrains Mono** (IDs, logs, terminal). Self-hosted via `@fontsource`.
3. **Light mode** — Not supported. Dark-only for the foreseeable future. Token system keeps light as a future config change, not a rewrite.
4. **Command palette** — Stays a Phase 4 stretch. Revisit with a concrete proposal when we get there; Matt will judge on concrete mockups rather than concept.
5. **Data viz** — No library dependency. Hand-roll SVG sparklines when Phase 4 needs them. Revisit `visx` only if scope grows past small charts.
6. **Shipping model** — Small PRs against `main`. Each phase (and typically each sub-slice) is its own PR. No long-lived `redesign/` branch.

## 11. Immediate Next Step

Per decision #1, the next deliverable is a **standalone mockup/specimen page** — a new route (e.g. `/_design`) rendering:

- The "Kyber Crystal" palette as swatches, with token names
- Typography specimen (Inter, Inter Tight, JetBrains Mono at representative sizes)
- Button/Card/StatusBadge/Dialog rendered against the proposed surfaces
- Hero-number treatment (tabular-nums + Inter Tight) on a fleet-summary-style block
- A small motion sample (skeleton shimmer, status-change pulse)

The mockup is visual-only — no API calls, no routing integration beyond an opt-in path. Build on a branch, deploy-preview for Matt, iterate on the palette until it lands, then start Phase 1.

---

## Appendix A — Mock palette swatches

Rendered approximations of the proposed tokens (hex equivalents of the OKLCH values above for quick reference):

| Token | Hex | Note |
|---|---|---|
| surface-base | `#0a0e13` | slightly cooler than current `neutral-950` |
| surface-raised | `#121821` | cards |
| surface-overlay | `#1a2230` | dialogs |
| surface-sunken | `#07090d` | logs/terminal |
| accent (cyan) | `#4ed9e8` | brand — close to current `cyan-400` |
| highlight (violet) | `#c49dff` | selection, focus rings on interactive surfaces |
| success | `#4fd89a` | |
| warn | `#f4c26b` | |
| danger | `#f56a7e` | softer than current `red-500` for dark surfaces |

## Appendix B — What I'd cut from scope

- Light theme — defer indefinitely
- i18n — defer until someone asks
- Rich text editing anywhere in the PWA — not needed
- Custom illustration system — out of scope; stock is fine for empty states
