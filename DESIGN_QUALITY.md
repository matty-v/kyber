# DESIGN_QUALITY.md — front-end design standard (Kyber PWA)

Read this **when a change touches the frontend** (`packages/pwa-views/`, `apps/embedded-pwa/`). It's the design-quality companion to `CODE_QUALITY.md`: correct code that *looks* generic or sloppy isn't done. The goal is UI that an operator trusts at a glance — not "AI-default" frontend.

> Distilled and adapted from **Impeccable** (https://impeccable.style, github.com/pbakaus/impeccable, Apache-2.0) — its open anti-pattern catalog + design vocabulary. Attribution per `NOTICE`. The deterministic detector (`impeccable detect`) is what review runs against this.

## Who the UI serves (design for this, not for a portfolio)

Operators running an agent fleet: scanning lists of agents/machines, often on a **phone**, often **in the dark**, wanting to read state *fast*. Density, legibility, and at-a-glance status beat decoration. When a choice trades "looks impressive in a screenshot" against "an on-call operator parses it in half a second," pick the operator.

## 1. Design-system fidelity comes first

Kyber's pwa-views already has a real, mature system. **Use it; don't reinvent or override it.**

- **Color = semantic oklch tokens only.** `surface-{base,raised,overlay,sunken}`, `border-{subtle,default,strong}`, `text-{primary,secondary,muted,disabled}`, and `accent / highlight / success / warn / danger / info` (each with a `-muted` variant). **Never** raw hex, arbitrary `oklch(...)`, or a one-off color. If you need a color that isn't a token, that's a design-system change — raise it, don't inline it.
- **Reuse components.** `Button`, `Card`, `ConfirmDialog`, `EmptyState`, `CapacityBar`, `StatusBadge`, `AgentActivityBadge/Dot`, etc. Compose these before hand-rolling. A new bespoke card/button that duplicates one of these is a smell.
- **Match the surrounding screen.** Spacing scale, radius, type sizes, and elevation should read as the same app. If your addition needs a different visual language than the page it's on, you're probably wrong.

## 2. Core craft

- **Typography & hierarchy.** Establish a clear hierarchy with the existing type scale — don't make everything ~the same size (`text-text-primary` for the thing that matters, `text-secondary`/`muted` for support). One- or two-step jumps, not five competing sizes. Body line-height ≥ 1.4; line length ≤ ~75ch.
- **Spacing & rhythm.** Use the spacing scale with *intent* — group related things tightly, separate sections clearly. Don't apply one uniform gap everywhere (monotonous) or random values. Whitespace is structure, not filler.
- **Contrast & legibility (WCAG AA).** Body text meets AA contrast against its surface. Use `text-primary/secondary` on raised surfaces; reserve `text-muted/disabled` for genuinely de-emphasized/absent state. No gray text on a colored chip that washes out. No body text below ~13px.
- **Motion: restraint.** Transitions are short and functional (state change, not spectacle). No bounce/elastic easing on UI, no animating layout properties (width/height) — animate `opacity`/`transform`. `motion-safe:` for anything that moves.
- **Copy.** Terse, operator-grade, specific. No marketing buzzwords ("streamline", "empower", "seamless"), no em-dash-laden cadence, no manufactured aphorisms. Label what the thing *is/does*.
- **Mobile + dark are the default, not an afterthought.** Verify the small-viewport card layout doesn't overflow or stack awkwardly (the exact class of bug caught in #417: a badge crammed into a card header). Dark-first: lean on the surface/border tokens for depth, not glows.

## 3. Anti-"slop" checklist (what `impeccable detect` flags — avoid these)

The detector enforces ~41 deterministic rules; the ones most relevant to our UI:

- **Color/contrast:** gradient text; "AI palette" purple/violet gradients + cyan-on-dark clichés; dark-mode glowing box-shadow accents (use `accent-ring` sparingly, not glow); gray-on-colored; low-contrast text (AA fail).
- **Typography:** flat hierarchy (sizes too similar); over-used fonts (Inter/Geist/Space Grotesk/Instrument Serif as a tell); crushed or wide letter-spacing on body; all-caps body; tiny body text; tight line-height (<1.3).
- **Visual details:** frosted-glass/glassmorphism cards used decoratively; ghost cards (hairline border + wide diffuse shadow); thick colored border on rounded cards; over-rounding (44px-radius blobs); decorative repeating-gradient stripes.
- **Layout/space:** nested cards (cards-in-cards); identical icon+heading+text card grids; monotonous spacing; content overflowing its container; positioned children clipped by `overflow:hidden` (tooltips/menus); cramped padding; body text touching the viewport edge; justified text; skipped heading levels.
- **Motion:** bounce/elastic easing; animating width/height; gratuitous image hover transforms.
- **Copy:** marketing buzzwords; em-dash overuse; aphoristic/"theater" framing.
- **Imagery:** broken/placeholder `src`.

This list is a *checklist, not a cage* — a flagged pattern can be the right call with reason; say why in the PR. But the default is to avoid them.

## 4. Self-check before you open the PR

1. **Look at it rendered**, don't trust the code. Use a shared dev-env PWA (if your setup has one) or a local `npm run dev` preview — at **mobile width** and in **dark** (our defaults).
2. Run the detector locally on your changed files: `npx impeccable@2.3.2 detect <path>` (pinned; read-only; no network). Fix real findings; note any you're intentionally overriding.
3. Confirm you used **tokens + existing components**, not bespoke colors/spacing/one-off widgets.

Review runs the same detector as an **advisory** gate (warn-first) and weighs it against this standard. Real design regressions are a `merge: hold`, same as a failing test.
