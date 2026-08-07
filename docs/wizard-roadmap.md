# Create Agent Wizard — Roadmap

**Last updated: 2026-04-26.** Refined with han.

This document is the single source of truth for the multi-step Create Agent wizard work and its dependencies. Per-issue specs live in the linked GitHub issues; this doc captures **sequencing, decisions, and what's deferred or out**.

## North star

The current Create Agent form is a single 608-line page presenting a dozen decisions in parallel. Operators who pick api-key + Telegram (rejected at submit), or who land on an unschedulable machine, only find out late. The wizard groups decisions into logical steps, validates per step, persists state, and grounds resource choices in real machine capacity.

## What lands first — execution order

Five tracks, four sequential plus one parallel.

### 1. [#140](https://github.com/matty-v/kyber/issues/140) — Capacity discovery foundation *(bundled with `followups.md` L29)*

Foundation for resource-aware UI. Machine controller populates three new `Machine.status` fields on every reconcile:

- `ObservedCapacity` — copied from node `status.allocatable`
- `AssignableCapacity` — Observed minus a chart-configurable platform reservation (default **1 CPU / 1 GiB**)
- `AvailableCapacity` — Assignable minus sum of bound agents' `spec.resources` requests

Plus: `/create-agent` 409 check switches from `spec.capacity − sum(agents)` → `status.availableCapacity` directly. 409 body returns the live numbers.

**Bundled:** `followups.md` L29 (surface `OVERLAY_MODE` in Agent CRD status). Same `status` surface, same kind of "expose runtime metadata" change.

**No UX.** Pure backend. Existing `MachineAvailable`/`MachineAvailableExcluding` API helpers stay as guard rails until the API consumer (`/create-agent`) is migrated.

### 2. [#120](https://github.com/matty-v/kyber/issues/120) — PWA test infrastructure (RTL + jsdom)

Pulled forward in priority. Wizard work and capacity-aware sliders both want component tests:

- slider-blocks-submit-when-over
- color-band thresholds (green / yellow / red)
- per-step validation gates
- form-state persistence across Back/Next

Lands before #142 and #131.

### 3. [#142](https://github.com/matty-v/kyber/issues/142) — PWA capacity surface

**Single bundle.** All six items in the issue body ship together:

1. Fleet view: per-machine cards with `used / assignable` bars (green <70%, yellow 70-90%, red >90%)
2. Machine detail: "Capacity" card with Observed → Reservation → Assignable → Assigned (per-agent breakdown) → Available
3. Create Agent: machine picker labels options `[name] — X CPU / Y GiB avail`; resource sliders color-code as they approach available; **disabled-submit when over**
4. Create Machine (standalone): "Use full node" auto-fills from node.allocatable minus reservation
5. Create Machine (cloud): GCE VM-type dropdown driven by the existing `vmTypeCatalog` chart value (already shipped via #182 / #183)
6. Disk: soft-warning UI only (red bar when `sum(disk) > ephemeral-storage`), no block

Depends on #140 (data) and #120 (test infrastructure).

### 4. [#134](https://github.com/matty-v/kyber/issues/134) — Identity-repo dropdown + collision check *(parallel track)*

Can run **alongside** the capacity chain — different files, low conflict.

- New `GET /api/v1/github/repos` endpoint, paginated, **60s server-side cache**, backed by `GET https://api.github.com/installation/repositories`
- New `GET /api/v1/github/repos/{owner}/{name}/exists` lightweight endpoint
- "Link existing" mode swaps free-text input for a searchable dropdown; **free-text fallback stays as a tooltip** ("Can't find it? Type the slug") for repos the App can't see yet
- "Create new" mode shows a live availability badge on the computed `<agent>-agent` name; Next button disabled on collision

Lands **before #131 wizard rewrite** so the existing form benefits, then carries forward into Step 3.

### 5. [#131](https://github.com/matty-v/kyber/issues/131) — The wizard itself

Three-phase strangler refactor. **Hard cutover at the end of Phase C** — old form deleted, no feature flag.

- **Phase A** — extract the existing `CreateAgent.tsx` (608 lines, monolithic) into per-step section components **in place**. Same UX, different structure. Buys testability.
- **Phase B** — wrap with the step container, add Back/Next, validation gates, URL state (`?step=N`), and form-state persistence (lifted `WizardState` or a zustand store).
- **Phase C** — review summary, Esc / Enter key handling, deep-link guards (refuse `?step=4` if step 1 invalid), then delete the old form.

Estimate: 3-5 PRs.

#### Steps (per #131 spec)

1. **Basics** — name, description, machine picker (with avail labels from #142)
2. **Runtime + model + resources** — runtime dropdown, model dropdown, scaling toggle, resource sliders (with capacity color-coding from #142)
3. **Identity repo** — 3 modes (template / existing / none), consuming the dropdown + collision check from #134
4. **Auth** — OAuth (PKCE) or API key + **extensible channel picker** (Telegram-only at launch, designed to absorb future channels)
5. **Review + Create** — two-column summary, edit-jump back, Create button with inline spin-up progress

#### Channel picker — designed for extensibility

Step 4's Telegram toggle is a generic channel-picker component, not a hardcoded boolean:

```ts
type ChannelDef = {
  id: 'telegram'  // | 'discord' | ...
  label: string
  fields: Array<{ name: string; type: 'secret' | 'url' | 'text'; required: boolean }>
}
```

V1 ships with `[telegram]`. Future channels (#132 Phase 1 Discord webhook, Slack, Matrix, etc.) plug in by adding a row — no PWA refactor required. Server-side per-channel Secret naming pattern (`<agent>-<channel>`, already used for `-telegram`) stays.

#### Out of scope (confirmed)

- No edit-after-create wizard for existing agents — different flow
- No draft-save (operator can refresh and lose state)
- No bulk-create

## Deferred to post-wizard

These don't gate the wizard launch and have their own initiative-sized scope.

- **[#132](https://github.com/matty-v/kyber/issues/132) Phase 1 (Discord webhook)** — lands as one row added to the channel picker. Outbound-only, env-var-driven, works with any auth type. Cheap once the channel-picker plumbing exists.
- **[#132](https://github.com/matty-v/kyber/issues/132) Phase 2 (full Discord plugin)** — bi-directional via Gateway connection. Indefinite defer; revisit when there's a concrete pull (multiple operators, shared team channels).
- **[#138](https://github.com/matty-v/kyber/issues/138) — channels as MCP sidecars** — architectural refactor of every comms-channel-using agent. UX is identical; revisit when [#133](https://github.com/matty-v/kyber/issues/133) (Codex) or [#137](https://github.com/matty-v/kyber/issues/137) (OpenClaw) becomes a real ask and avoiding per-runtime channel re-implementation pays off.

## Out of scope

- **[#189](https://github.com/matty-v/kyber/issues/189) — hyphen-input regression in name fields.** Stays open for future pickup. The wizard rewrite may resolve it incidentally; if not, a small targeted fix on top.
- **`followups.md` "Telegram comms for agents out of the box" entry — managed-bot provisioning.** Won't do — current BotFather + paste-token onboarding is fine for solo operation. Revisit if operator count grows.

## Already shipped (closed via PR #186 sweep on 2026-04-26)

These were on the original "what should we do?" list but turned out to already be in the codebase, just stale in `followups.md`:

- L9 — Proactive credential sync (runtime keychain → Secret push). `pkg/tokenreport/credential_sync.go`, wired in `cmd/token-reporter/main.go`.
- L11 — api-key agents hang on "Use this API key?" prompt. `start-claude.sh:314-322` `--bare` flag.
- L13 — api-key + `--channels` is silently unsupported. API-side validation at `routes_agents.go:454-458` + PWA auto-clear.
- L15 — Replacing-phase reconciler doesn't re-detect TERMINATED. `classifyReplacing` fires `EventReplacementFailed`.
- L17 — Agent phase flashes Failed during operator restart. `classifyEvent` Running-phase routes `desiredPhase=Restarting` correctly.

## Refinement notes

Per-issue refinement decisions (channel picker shape, parallelism, cutover policy, etc.) are captured as comments on each GitHub issue with the header **"Refinement decisions (2026-04-26 with han)"**. This doc is the at-a-glance summary; the issue comments are the durable per-item record.
